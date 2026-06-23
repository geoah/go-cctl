package cctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFsSlug(t *testing.T) {
	cases := map[string]string{
		"rxtx.dev":    "rxtx.dev",
		"go-cctl":     "go-cctl",
		"feat/foo":    "feat-foo",
		"a b:c":       "a-b-c",
		"":            "_",
		"under_score": "under_score",
	}
	for in, want := range cases {
		if got := fsSlug(in); got != want {
			t.Errorf("fsSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpawnSpecHasIdentity(t *testing.T) {
	if !(SpawnSpec{Server: "s", Repo: "r", Worktree: "w", Session: "x"}).hasIdentity() {
		t.Error("full spec should have identity")
	}
	if (SpawnSpec{Server: "s", Repo: "r", Worktree: "w"}).hasIdentity() {
		t.Error("missing session should not have identity")
	}
	if (SpawnSpec{}).hasIdentity() {
		t.Error("empty spec should not have identity")
	}
}

func TestSpawnScriptPathDeterministic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	spec := SpawnSpec{Server: "local", Repo: "rxtx.dev", Worktree: "main", Session: "default"}
	p1, err := spawnScriptPath(spec)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := spawnScriptPath(spec)
	if p1 != p2 {
		t.Errorf("non-deterministic: %q vs %q", p1, p2)
	}
	if base := filepath.Base(p1); base != "local__rxtx.dev__main__default.sh" {
		t.Errorf("unexpected filename %q", base)
	}
	if !strings.Contains(p1, filepath.Join(".cctl", "spawn")) {
		t.Errorf("path not under ~/.cctl/spawn: %q", p1)
	}
	if _, err := os.Stat(filepath.Dir(p1)); err != nil {
		t.Errorf("spawn dir not created: %v", err)
	}
}

func TestWsTitle(t *testing.T) {
	if got := cmuxWsTitle("rxtx.dev", "main", "audit"); got != "rxtx.dev/main/audit" {
		t.Errorf("cmuxWsTitle = %q", got)
	}
	r, w, s, ok := parseWsTitle("rxtx.dev/main/audit")
	if !ok || r != "rxtx.dev" || w != "main" || s != "audit" {
		t.Errorf("parseWsTitle got %q %q %q %v", r, w, s, ok)
	}
	// Round-trips.
	if r2, w2, s2, ok := parseWsTitle(cmuxWsTitle("repo", "wt", "sess")); !ok || r2 != "repo" || w2 != "wt" || s2 != "sess" {
		t.Errorf("round-trip failed: %q %q %q %v", r2, w2, s2, ok)
	}
	// Only the exact three-part shape is accepted.
	for _, bad := range []string{"single", "a/b", "/x/y", "x//y", "a/b/c/d", ""} {
		if _, _, _, ok := parseWsTitle(bad); ok {
			t.Errorf("parseWsTitle(%q) should fail", bad)
		}
	}
}

// TestSessionForWorkspace pins the migration's session-derivation: from a
// tmux name embedded in a (terminal-overwritten) tab title, from a clean tab
// title matching a tracked session, or the sole manifest session.
func TestSessionForWorkspace(t *testing.T) {
	tracked := map[string]bool{tmuxName("olympus", "gb300-k8s", "docs"): true}

	// 1. tmux name embedded in a mosh/tmux title.
	w := cmuxWsView{surfaces: []cmuxSurface{{title: `[mosh] cctl/olympus/gb300-k8s/nfs-temp:0:bash - "…"`}}}
	if s := sessionForWorkspace(w, "olympus", "gb300-k8s", nil, nil, nil); s != "nfs-temp" {
		t.Errorf("embedded tmux name: got %q want nfs-temp", s)
	}
	// 2. clean tab title that matches a tracked session.
	w = cmuxWsView{surfaces: []cmuxSurface{{title: "docs"}}}
	if s := sessionForWorkspace(w, "olympus", "gb300-k8s", nil, tracked, nil); s != "docs" {
		t.Errorf("clean tracked title: got %q want docs", s)
	}
	// 3. fall back to the sole manifest session for the worktree.
	w = cmuxWsView{surfaces: []cmuxSurface{{title: "Terminal"}}}
	if s := sessionForWorkspace(w, "olympus", "main", []string{"random"}, nil, nil); s != "random" {
		t.Errorf("sole manifest session: got %q want random", s)
	}
	// Ambiguous / unknown → empty (don't rename).
	w = cmuxWsView{surfaces: []cmuxSurface{{title: "Terminal"}}}
	if s := sessionForWorkspace(w, "olympus", "k8s", []string{"a", "b"}, nil, nil); s != "" {
		t.Errorf("ambiguous: got %q want empty", s)
	}
}

func TestParseCmuxSurfaceLines(t *testing.T) {
	out := strings.Join([]string{
		"* 528A9A76-E0FD-4BFE-A342-5571F2C3D318  cctl  [selected]",
		"  AA289062-104A-4FD6-9878-427B297B528D  default",
		"  6912DFDD-8917-40C4-ADC5-70A73995CE49  my feature tab",
		"garbage line without uuid",
		"",
	}, "\n")
	got := parseCmuxSurfaceLines(out)
	if len(got) != 3 {
		t.Fatalf("want 3 surfaces, got %d: %+v", len(got), got)
	}
	if got[0].id != "528A9A76-E0FD-4BFE-A342-5571F2C3D318" || got[0].title != "cctl" {
		t.Errorf("surface[0] = %+v (markers should be stripped)", got[0])
	}
	if got[1].title != "default" {
		t.Errorf("surface[1].title = %q", got[1].title)
	}
	if got[2].title != "my feature tab" {
		t.Errorf("surface[2].title = %q (spaces should survive)", got[2].title)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if manifestFileExists() {
		t.Fatal("manifest should not exist yet")
	}

	dir, _ := spawnScriptDir()
	scriptA := filepath.Join(dir, "a.sh")
	if err := os.WriteFile(scriptA, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	manifestUpsert(SpawnSpec{Server: "local", Repo: "r", Worktree: "main", Session: "a", Script: scriptA})
	manifestUpsert(SpawnSpec{Server: "local", Repo: "r", Worktree: "main", Session: "b"})
	if !manifestFileExists() {
		t.Fatal("manifest should exist after upsert")
	}
	if es := loadManifestEntries(); len(es) != 2 {
		t.Fatalf("want 2 entries, got %d", len(es))
	}

	// Upsert with the same identity updates in place (no duplicate). Real
	// spawns always carry Script, so the round-trip keeps it.
	manifestUpsert(SpawnSpec{Server: "local", Repo: "r", Worktree: "main", Session: "a", TabTitle: "a2", Script: scriptA})
	es := loadManifestEntries()
	if len(es) != 2 {
		t.Fatalf("upsert same key should not add; got %d", len(es))
	}
	for _, e := range es {
		if e.Session == "a" && e.TmuxName != "cctl/r/main/a" {
			t.Errorf("TmuxName = %q, want cctl/r/main/a", e.TmuxName)
		}
	}

	// Remove drops the entry and its durable script file.
	manifestRemove("local", "r", "main", "a")
	if _, err := os.Stat(scriptA); !os.IsNotExist(err) {
		t.Errorf("script should be deleted on remove (err=%v)", err)
	}
	if es := loadManifestEntries(); len(es) != 1 || es[0].Session != "b" {
		t.Fatalf("after remove want [b], got %+v", es)
	}

	// removeWorktree drops every session on that worktree, leaving others.
	manifestUpsert(SpawnSpec{Server: "local", Repo: "r", Worktree: "main", Session: "c"})
	manifestUpsert(SpawnSpec{Server: "local", Repo: "r", Worktree: "feat", Session: "d"})
	manifestRemoveWorktree("local", "r", "main")
	if es := loadManifestEntries(); len(es) != 1 || es[0].Session != "d" {
		t.Fatalf("after removeWorktree want [d], got %+v", es)
	}
}

func TestManifestWsSet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manifestUpsert(SpawnSpec{
		Server: "local", Repo: "r", Worktree: "main", Session: "a",
		WsTitle: "r/main/a", TabTitle: "a",
	})
	if set := manifestWsSet(); !set["r/main/a"] {
		t.Errorf("ws set missing expected per-session name: %+v", set)
	}
}

func TestAllServersSettled(t *testing.T) {
	m := &tuiModel{
		serverNames: []string{"local", "remote"},
		state: map[string]*serverState{
			"local":  {conn: connConnected, sessionsLoaded: true, reposLoaded: true, worktreesLoaded: true},
			"remote": {conn: connConnecting},
		},
	}
	if m.allServersSettled() {
		t.Error("a still-connecting server means not settled")
	}
	// Connected but mid-fetch: still not settled.
	m.state["remote"].conn = connConnected
	if m.allServersSettled() {
		t.Error("connected-but-unloaded means not settled")
	}
	// Fully loaded: settled.
	m.state["remote"].sessionsLoaded = true
	m.state["remote"].reposLoaded = true
	m.state["remote"].worktreesLoaded = true
	if !m.allServersSettled() {
		t.Error("all servers loaded should be settled")
	}
	// A server that gave up (disconnected) counts as settled — don't block
	// startup on an unreachable remote.
	m.state["remote"].conn = connDisconnected
	m.state["remote"].sessionsLoaded = false
	if !m.allServersSettled() {
		t.Error("a disconnected (gave-up) server should count as settled")
	}
}

func TestSyncAllServersDefault(t *testing.T) {
	if !(&Config{}).syncAllServers() {
		t.Error("sync_all_servers should default to true")
	}
	f := false
	if (&Config{Defaults: Defaults{SyncAllServers: &f}}).syncAllServers() {
		t.Error("explicit false should disable")
	}
	tr := true
	if !(&Config{Defaults: Defaults{SyncAllServers: &tr}}).syncAllServers() {
		t.Error("explicit true should enable")
	}
}

func TestParseCmuxGroups(t *testing.T) {
	raw := []byte(`{"groups":[
		{"name":"workspace/olympus","member_workspace_ids":["A","B"]},
		{"name":"rxtx.dev","member_workspace_ids":["C"]}
	]}`)
	groups := parseCmuxGroups(raw)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	if groups[0].name != "workspace/olympus" || len(groups[0].members) != 2 {
		t.Errorf("group0 = %+v", groups[0])
	}
	if groups[1].name != "rxtx.dev" || groups[1].members[0] != "C" {
		t.Errorf("group1 = %+v", groups[1])
	}
	if parseCmuxGroups([]byte("not json")) != nil {
		t.Error("garbage should parse to nil")
	}
}

// TestMapWorkspaceMeta_ServerDerivation: a "<remoteServer>/repo" group marks
// members as that remote server's (and cctl-owned); a bare-repo group → local.
func TestSyncCloseUnmatchedDefault(t *testing.T) {
	if (&Config{}).syncCloseUnmatched() {
		t.Error("sync_close_unmatched should default to false")
	}
	tr := true
	if !(&Config{Defaults: Defaults{SyncCloseUnmatched: &tr}}).syncCloseUnmatched() {
		t.Error("explicit true should enable")
	}
}

func TestShouldCloseDeadWorkspace(t *testing.T) {
	tracked := tmuxName("rxtx.dev", "main", "audit")
	desired := map[string]bool{"rxtx.dev/main/audit": true}
	remoteMeta := wsMeta{server: "ws", remote: true, repoRoot: "olympus"}
	localMeta := wsMeta{server: "local", repoRoot: "rxtx.dev"}

	// alive tracked → keep
	live := map[string]bool{tracked: true}
	if shouldCloseDeadWorkspace("rxtx.dev/main/audit", localMeta, live, desired, false) {
		t.Error("alive session must never be closed")
	}
	// dead + tracked → keep (the reconcile REVIVES tracked sessions; only dd,
	// which removes the manifest entry, makes a session closeable)
	if shouldCloseDeadWorkspace("rxtx.dev/main/audit", localMeta, map[string]bool{}, desired, false) {
		t.Error("dead tracked workspace must be kept (it gets revived, not closed)")
	}
	// dead + untracked + flag OFF → keep (protects manual tabs)
	if shouldCloseDeadWorkspace("rxtx.dev/main/scratch", localMeta, map[string]bool{}, desired, false) {
		t.Error("dead untracked workspace must be kept when close-unmatched is off")
	}
	// dead + untracked + flag ON → close (opt-in pruning), but only cctl-shaped
	if !shouldCloseDeadWorkspace("rxtx.dev/main/scratch", localMeta, map[string]bool{}, desired, true) {
		t.Error("dead untracked 3-part workspace should close when close-unmatched is on")
	}
	// non-cctl-shaped (single component) → never closed, even with flag on
	if shouldCloseDeadWorkspace("my-notes", localMeta, map[string]bool{}, desired, true) {
		t.Error("non-cctl-shaped workspace must never be auto-closed")
	}
	// 2-part: closed only in a cctl remote group, never via close-unmatched
	if shouldCloseDeadWorkspace("olympus/lethe", localMeta, map[string]bool{}, desired, true) {
		t.Error("2-part local workspace must not be closed even with flag on")
	}
	if !shouldCloseDeadWorkspace("olympus/lethe", remoteMeta, map[string]bool{}, desired, false) {
		t.Error("dead 2-part workspace in a cctl remote group should close")
	}
	// 2-part in remote group but a session there is alive → keep
	liveWt := map[string]bool{tmuxName("olympus", "lethe", "poc"): true}
	if shouldCloseDeadWorkspace("olympus/lethe", remoteMeta, liveWt, desired, false) {
		t.Error("2-part workspace with a live session must be kept")
	}
}

// TestEntriesToSpawn pins what the reconcile (re)spawns to converge to the
// manifest: open a tab for a live session missing one, AND revive a dead
// tracked session (reboot/kill). Skip ones already up and unreachable servers.
func TestEntriesToSpawn(t *testing.T) {
	entries := []wsEntry{
		{Server: "ws", Repo: "olympus", Worktree: "main", Session: "random", TmuxName: tmuxName("olympus", "main", "random")},       // live, no tab -> open
		{Server: "ws", Repo: "olympus", Worktree: "gb300-k8s", Session: "docs", TmuxName: tmuxName("olympus", "gb300-k8s", "docs")}, // live + tab -> skip (up)
		{Server: "ws", Repo: "olympus", Worktree: "old", Session: "poc", TmuxName: tmuxName("olympus", "old", "poc")},               // dead -> revive
		{Server: "down", Repo: "r", Worktree: "main", Session: "x", TmuxName: tmuxName("r", "main", "x")},                           // unreachable -> skip
	}
	live := map[string]map[string]bool{
		"ws": {
			tmuxName("olympus", "main", "random"):    true,
			tmuxName("olympus", "gb300-k8s", "docs"): true,
			// "old/poc" intentionally absent => dead (reboot/kill) -> revive
		},
		// "down" intentionally absent => unreachable
	}
	views := []cmuxWsView{{name: "olympus/gb300-k8s/docs"}} // docs already has a tab

	got := entriesToSpawn(views, live, entries)
	want := map[string]bool{"olympus/main/random": true, "olympus/old/poc": true}
	if len(got) != len(want) {
		t.Fatalf("want %d (re)spawn targets, got %d: %+v", len(want), len(got), got)
	}
	for _, e := range got {
		if name := cmuxWsTitle(e.Repo, e.Worktree, e.Session); !want[name] {
			t.Errorf("unexpected spawn target %q (up + unreachable must be skipped)", name)
		}
	}
}

// TestParseClaudeRunning pins the claude-vs-shell detection: a running claude
// renames its pane to its version (e.g. "2.1.186"), a dead/never-started one
// sits at a shell. Only shells count as "not running" (so we never respawn
// over a live claude).
func TestParseClaudeRunning(t *testing.T) {
	out := strings.Join([]string{
		"cctl/rxtx_dev/main/x 2.1.186",    // claude running (version title)
		"cctl/rxtx_dev/ergogen/y zsh",     // shell → not running
		"cctl/go-cctl/main/default -bash", // login shell → not running
		"cctl/olympus/main/z node",        // node (claude) → running
		"cctl/foo/bar/w nvim",             // a tool → leave (running)
		"garbage",                         // ignored
		"",                                // ignored
	}, "\n")
	up := parseClaudeRunning(out)
	if !up["cctl/rxtx_dev/main/x"] || !up["cctl/olympus/main/z"] || !up["cctl/foo/bar/w"] {
		t.Errorf("non-shell panes should count as running: %+v", up)
	}
	if up["cctl/rxtx_dev/ergogen/y"] || up["cctl/go-cctl/main/default"] {
		t.Errorf("shell panes must NOT count as running (they get respawned): %+v", up)
	}
	for _, sh := range []string{"zsh", "bash", "-zsh", "sh", "fish", "dash"} {
		if !isShellCommand(sh) {
			t.Errorf("isShellCommand(%q) should be true", sh)
		}
	}
	for _, x := range []string{"node", "claude", "2.1.186", "nvim"} {
		if isShellCommand(x) {
			t.Errorf("isShellCommand(%q) should be false", x)
		}
	}
}

func TestDeriveWsMeta(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{
		"mac":       {Local: true},
		"workspace": {Host: "h"},
	}}
	// A "<remoteServer>/<repo>" group → that server, marked remote+owned.
	if m := deriveWsMeta("workspace/olympus", cfg, "mac"); m.server != "workspace" || !m.remote || m.repoRoot != "olympus" {
		t.Errorf("remote group: got %+v", m)
	}
	// A bare local repo group → local, not remote.
	if m := deriveWsMeta("rxtx.dev", cfg, "mac"); m.server != "mac" || m.remote || m.repoRoot != "rxtx.dev" {
		t.Errorf("local group: got %+v", m)
	}
	// A "/"-containing name whose prefix isn't a known remote server → local.
	if m := deriveWsMeta("a/b", cfg, "mac"); m.server != "mac" || m.remote {
		t.Errorf("unknown-prefix group should be local: got %+v", m)
	}
}

// TestStripCctlResumeCommands pins the cmux.json cleanup that kills the
// "auto restore?" prompt spam: drop source=cctl resume bindings, keep
// everything else, idempotent, and never corrupt the file.
func TestStripCctlResumeCommands(t *testing.T) {
	in := []byte(`{
		"app": {"x": 1},
		"terminal": {
			"autoResumeAgentSessions": true,
			"resumeCommands": [
				{"name": "a", "source": "cctl"},
				{"name": "b", "source": "claude"},
				{"name": "c", "source": "cctl"}
			]
		}
	}`)
	out, removed := stripCctlResumeCommands(in)
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	term := doc["terminal"].(map[string]any)
	if rc := term["resumeCommands"].([]any); len(rc) != 1 {
		t.Fatalf("want 1 non-cctl entry kept, got %d", len(rc))
	}
	if term["autoResumeAgentSessions"] != true {
		t.Error("unrelated terminal keys must be preserved")
	}
	if doc["app"] == nil {
		t.Error("unrelated top-level keys must be preserved")
	}

	// Idempotent: a second pass changes nothing.
	if _, r := stripCctlResumeCommands(out); r != 0 {
		t.Errorf("second pass removed=%d, want 0", r)
	}

	// All-cctl → the empty resumeCommands key is dropped entirely.
	o2, r := stripCctlResumeCommands([]byte(`{"terminal":{"resumeCommands":[{"source":"cctl"}]}}`))
	if r != 1 {
		t.Fatalf("all-cctl removed=%d, want 1", r)
	}
	var d2 map[string]any
	_ = json.Unmarshal(o2, &d2)
	if _, has := d2["terminal"].(map[string]any)["resumeCommands"]; has {
		t.Error("emptied resumeCommands should be removed")
	}

	// No terminal / no resumeCommands / garbage → no change, no panic.
	for _, raw := range []string{`{"app":{}}`, `{"terminal":{}}`, `not json`} {
		if _, r := stripCctlResumeCommands([]byte(raw)); r != 0 {
			t.Errorf("input %q: removed=%d, want 0", raw, r)
		}
	}
}

func TestFindLocalServer(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{
		"remote": {Host: "h"},
		"mac":    {Local: true},
	}}
	name, _, ok := findLocalServer(cfg)
	if !ok || name != "mac" {
		t.Errorf("findLocalServer = %q %v, want mac true", name, ok)
	}
	none := &Config{Servers: map[string]Server{"remote": {Host: "h"}}}
	if _, _, ok := findLocalServer(none); ok {
		t.Error("findLocalServer should report false when no local server")
	}
}
