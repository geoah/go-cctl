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

// A dotted worktree ("ecr.b1") is stored real by a spawn but sanitized
// ("ecr_b1") by the legacy adopt path, leaving two rows for one tmux session.
// dd targets the real name; it must still purge the sanitized twin (matched by
// canonical tmux name) so the next reconcile can't revive the session.
func TestManifestRemove_PurgesSanitizedTwin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// real-name entry (spawn) + sanitized twin (adopt) — same tmux session.
	manifestUpsert(SpawnSpec{Server: "dev", Repo: "olympus", Worktree: "ecr.b1", Session: "2"})
	manifestUpsertEntry(wsEntry{
		Server: "dev", Repo: "olympus", Worktree: "ecr_b1", Session: "2",
		TmuxName: tmuxName("olympus", "ecr.b1", "2"),
		WsTitle:  cmuxWsTitle("olympus", "ecr_b1", "2"), TabTitle: "2",
	})
	// A different session on the same worktree must survive.
	manifestUpsert(SpawnSpec{Server: "dev", Repo: "olympus", Worktree: "ecr.b1", Session: "1"})

	manifestRemove("dev", "olympus", "ecr.b1", "2")

	es := loadManifestEntries()
	if len(es) != 1 || es[0].Session != "1" {
		t.Fatalf("after remove want only session 1, got %+v", es)
	}
}

// The reconcile's dedup collapses the real+sanitized twin pair, keeping the
// real-name row (its worktree changes under sanitization) so its WsTitle still
// matches the actual cmux workspace.
func TestManifestDedupByTmux_KeepsRealName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manifestUpsertEntry(wsEntry{
		Server: "dev", Repo: "olympus", Worktree: "ecr_b1", Session: "1",
		TmuxName: tmuxName("olympus", "ecr.b1", "1"),
		WsTitle:  cmuxWsTitle("olympus", "ecr_b1", "1"),
	})
	manifestUpsertEntry(wsEntry{
		Server: "dev", Repo: "olympus", Worktree: "ecr.b1", Session: "1",
		TmuxName: tmuxName("olympus", "ecr.b1", "1"),
		WsTitle:  cmuxWsTitle("olympus", "ecr.b1", "1"),
	})
	// An unrelated single session must be left untouched.
	manifestUpsertEntry(wsEntry{
		Server: "dev", Repo: "olympus", Worktree: "n2d-vs-c4d", Session: "1",
		TmuxName: tmuxName("olympus", "n2d-vs-c4d", "1"),
	})

	if dropped := manifestDedupByTmux(); dropped != 1 {
		t.Fatalf("want 1 dropped, got %d", dropped)
	}
	es := loadManifestEntries()
	if len(es) != 2 {
		t.Fatalf("want 2 entries after dedup, got %d: %+v", len(es), es)
	}
	for _, e := range es {
		if e.Worktree == "ecr_b1" {
			t.Errorf("sanitized twin should have been dropped, kept: %+v", e)
		}
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
	desiredTmux := map[string]bool{tracked: true}
	remoteMeta := wsMeta{server: "ws", remote: true, repoRoot: "olympus"}
	localMeta := wsMeta{server: "local", repoRoot: "rxtx.dev"}

	// alive tracked → keep
	live := map[string]bool{tracked: true}
	if shouldCloseDeadWorkspace("rxtx.dev/main/audit", localMeta, live, desired, desiredTmux, false) {
		t.Error("alive session must never be closed")
	}
	// dead + tracked → keep (the reconcile REVIVES tracked sessions; only dd,
	// which removes the manifest entry, makes a session closeable)
	if shouldCloseDeadWorkspace("rxtx.dev/main/audit", localMeta, map[string]bool{}, desired, desiredTmux, false) {
		t.Error("dead tracked workspace must be kept (it gets revived, not closed)")
	}
	// dead + untracked + flag OFF → keep (protects manual tabs)
	if shouldCloseDeadWorkspace("rxtx.dev/main/scratch", localMeta, map[string]bool{}, desired, desiredTmux, false) {
		t.Error("dead untracked workspace must be kept when close-unmatched is off")
	}
	// dead + untracked + flag ON → close (opt-in pruning), but only cctl-shaped
	if !shouldCloseDeadWorkspace("rxtx.dev/main/scratch", localMeta, map[string]bool{}, desired, desiredTmux, true) {
		t.Error("dead untracked 3-part workspace should close when close-unmatched is on")
	}
	// non-cctl-shaped (single component) → never closed, even with flag on
	if shouldCloseDeadWorkspace("my-notes", localMeta, map[string]bool{}, desired, desiredTmux, true) {
		t.Error("non-cctl-shaped workspace must never be auto-closed")
	}
	// 2-part: closed only in a cctl remote group, never via close-unmatched
	if shouldCloseDeadWorkspace("olympus/lethe", localMeta, map[string]bool{}, desired, desiredTmux, true) {
		t.Error("2-part local workspace must not be closed even with flag on")
	}
	if !shouldCloseDeadWorkspace("olympus/lethe", remoteMeta, map[string]bool{}, desired, desiredTmux, false) {
		t.Error("dead 2-part workspace in a cctl remote group should close")
	}
	// 2-part in remote group but a session there is alive → keep
	liveWt := map[string]bool{tmuxName("olympus", "lethe", "poc"): true}
	if shouldCloseDeadWorkspace("olympus/lethe", remoteMeta, liveWt, desired, desiredTmux, false) {
		t.Error("2-part workspace with a live session must be kept")
	}

	// Stale sanitized twin: "olympus/ecr_b1/1" and the tracked canonical
	// "olympus/ecr.b1/1" share one tmux session (both sanitize to ecr_b1). The
	// canonical tab is tracked; the sanitized dup must be closed even though the
	// shared session is live — otherwise the legacy duplicate never collapses.
	twinDesired := map[string]bool{"olympus/ecr.b1/1": true}
	twinDesiredTmux := map[string]bool{tmuxName("olympus", "ecr.b1", "1"): true}
	sharedLive := map[string]bool{tmuxName("olympus", "ecr_b1", "1"): true}
	if !shouldCloseDeadWorkspace("olympus/ecr_b1/1", localMeta, sharedLive, twinDesired, twinDesiredTmux, false) {
		t.Error("stale sanitized twin of a tracked canonical workspace should be closed")
	}
	// ...but the canonical real-name workspace itself must never be closed.
	if shouldCloseDeadWorkspace("olympus/ecr.b1/1", localMeta, sharedLive, twinDesired, twinDesiredTmux, false) {
		t.Error("canonical tracked workspace must never be closed")
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

// TestParseClaudeRunningTree pins the claude-vs-shell detection by process
// subtree: a pane's own pid is its shell, so the session counts as "running"
// only when a non-shell descendant (claude/node, or a tool) exists. This is the
// fix for tmux reporting pane_current_command=zsh while claude (its own process
// group) runs underneath — the old pane-command check saw only the shell and
// got every live session killed+respawned.
func TestParseClaudeRunningTree(t *testing.T) {
	// session "x": pane shell 100 has a claude (node) child 101 -> running.
	// session "y": pane shell 200 is idle (no children)          -> not running.
	// session "z": pane shell 300 -> zsh 301 (subshell) -> claude 302 (nested) -> running.
	// session "w": pane shell 400 -> claude 401 -> a bash tool 402: still
	//              running, because node(claude) sits above the bash child.
	panes := strings.Join([]string{
		"cctl/olympus/main/x 100",
		"cctl/olympus/main/y 200",
		"cctl/olympus/main/z 300",
		"cctl/olympus/main/w 400",
		"garbage", "",
	}, "\n")
	procs := strings.Join([]string{
		"100 1 zsh",
		"101 100 claude",
		"200 1 zsh",
		"300 1 zsh",
		"301 300 zsh",
		"302 301 node",
		"400 1 -bash",
		"401 400 node",
		"402 401 bash",
		"garbage line", "",
	}, "\n")
	up := parseClaudeRunningTree(panes, procs)
	if !up["cctl/olympus/main/x"] || !up["cctl/olympus/main/z"] || !up["cctl/olympus/main/w"] {
		t.Errorf("sessions with a non-shell in the subtree should be running: %+v", up)
	}
	if up["cctl/olympus/main/y"] {
		t.Errorf("an idle-shell session must NOT count as running: %+v", up)
	}
	for _, sh := range []string{"zsh", "bash", "-zsh", "sh", "fish", "dash"} {
		if !isShellCommand(sh) {
			t.Errorf("isShellCommand(%q) should be true", sh)
		}
	}
	for _, x := range []string{"node", "claude", "nvim"} {
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

func TestSurfacesToPrune(t *testing.T) {
	titles := sessionTabTitles(wsEntry{Session: "docs", TmuxName: "cctl/olympus/gb300-k8s/docs"})
	// Junk cwd shells alongside the real "docs" tab → prune the shells only.
	w := cmuxWsView{id: "w1", name: "olympus/gb300-k8s/docs", surfaces: []cmuxSurface{
		{id: "s1", title: "…/github.com/geoah/rxtx.dev"},
		{id: "s2", title: "…/workspace/olympus/gb300-k8s"},
		{id: "s3", title: "docs"},
	}}
	got := surfacesToPrune(w, titles)
	if len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("want [s1 s2] pruned, got %v", got)
	}
	// The full tmux name also counts as the session tab (local flavor).
	w2 := cmuxWsView{id: "w2", name: "go-cctl/main/default", surfaces: []cmuxSurface{
		{id: "a", title: "cctl/go-cctl/main/default"},
	}}
	if got := surfacesToPrune(w2, sessionTabTitles(wsEntry{Session: "default", TmuxName: "cctl/go-cctl/main/default"})); got != nil {
		t.Fatalf("single session tab (full-name titled) must not be pruned, got %v", got)
	}
	// No identifiable session tab → prune NOTHING (never nuke a settling tab).
	w3 := cmuxWsView{id: "w3", name: "rxtx.dev/mneme/default", surfaces: []cmuxSurface{
		{id: "x", title: "✳ Some other conversation"},
		{id: "y", title: "…/worktrees/rxtx.dev/mneme"},
	}}
	if got := surfacesToPrune(w3, sessionTabTitles(wsEntry{Session: "default", TmuxName: "cctl/rxtx_dev/mneme/default"})); got != nil {
		t.Fatalf("no session tab present → must prune nothing, got %v", got)
	}
}

func TestMergeWsEntry_PreservesRichFieldsDropsPrompt(t *testing.T) {
	old := wsEntry{
		Server: "workspace", Repo: "olympus", Worktree: "gb300-k8s", Session: "docs",
		TmuxName: "cctl/olympus/gb300-k8s/docs", WsTitle: "olympus/gb300-k8s/docs",
		TabTitle: "docs", Group: "workspace/olympus", GroupCwd: "/x", Cwd: "/y",
		Script: "/Users/me/.cctl/spawn/w.sh", Agent: "claude", Remote: true,
		Prompt: "stale pending prompt",
	}
	// A sparse attach-style upsert: identity only.
	in := wsEntry{Server: "workspace", Repo: "olympus", Worktree: "gb300-k8s", Session: "docs",
		TmuxName: "cctl/olympus/gb300-k8s/docs", Updated: 42}
	got := mergeWsEntry(old, in)
	if got.Script != old.Script {
		t.Errorf("Script clobbered: %q (dd would leak the wrapper)", got.Script)
	}
	if got.Agent != "claude" || !got.Remote || got.Group != old.Group || got.Cwd != "/y" {
		t.Errorf("rich fields lost: %+v", got)
	}
	if got.Prompt != "" {
		t.Errorf("stale Prompt inherited (%q) — a dead session would relaunch with it", got.Prompt)
	}
	// Incoming non-empty values win.
	in2 := wsEntry{Server: "workspace", Repo: "olympus", Worktree: "gb300-k8s", Session: "docs",
		Agent: "codex", Prompt: "fresh"}
	got2 := mergeWsEntry(old, in2)
	if got2.Agent != "codex" || got2.Prompt != "fresh" {
		t.Errorf("incoming values should win: %+v", got2)
	}
}

func TestCctlRepoGroupNames(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{
		"local":     {Local: true},
		"workspace": {},
	}}
	entries := []wsEntry{
		{Server: "local", Repo: "rxtx.dev"},
		{Server: "workspace", Repo: "olympus"},
		{Server: "gone", Repo: "zombie"}, // server removed from config → skipped
	}
	got := cctlRepoGroupNames(cfg, entries)
	if !got["rxtx.dev"] || !got["workspace/olympus"] {
		t.Fatalf("missing expected group names: %v", got)
	}
	if got["zombie"] || got["gone/zombie"] {
		t.Fatalf("entry for a removed server must not own a group name: %v", got)
	}
}

func TestOrderedCmuxWorkspaceIDs(t *testing.T) {
	// Out of order, control not first, with a USER workspace ("scratch", not
	// cctl-shaped) between them → the cctl block sorts into the cctl-occupied
	// slots; the user workspace keeps its exact position.
	wss := []cmuxWorkspace{
		{id: "B", name: "rxtx.dev/main/x"},
		{id: "U", name: "scratch"},
		{id: "C", name: "cctl"},
		{id: "A", name: "go-cctl/main/default"},
	}
	got := orderedCmuxWorkspaceIDs(wss)
	want := []string{"C", "U", "A", "B"} // slots 0,2,3 are cctl's; slot 1 stays U
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// Only user workspaces "out of order" → nil (we never touch them).
	if got := orderedCmuxWorkspaceIDs([]cmuxWorkspace{
		{id: "Z", name: "zeta"}, {id: "Y", name: "alpha"},
	}); got != nil {
		t.Fatalf("user-only workspaces must never be reordered, got %v", got)
	}
	// Already ordered → nil (caller skips the reorder call).
	if got := orderedCmuxWorkspaceIDs([]cmuxWorkspace{
		{id: "C", name: "cctl"}, {id: "A", name: "a/b/c"}, {id: "B", name: "b/c/d"},
	}); got != nil {
		t.Fatalf("already-ordered should return nil, got %v", got)
	}
	if got := orderedCmuxWorkspaceIDs([]cmuxWorkspace{{id: "X", name: "solo"}}); got != nil {
		t.Fatalf("single workspace should return nil, got %v", got)
	}
}

func TestEntriesToSpawnMatchesByWsID(t *testing.T) {
	live := map[string]map[string]bool{"local": {"cctl/r/main/a": true}}
	// Workspace was RENAMED (title no longer matches) but the UUID is
	// recorded — the entry must still count as present.
	views := []cmuxWsView{{id: "UUID-1", name: "renamed-by-user"}}
	entries := []wsEntry{{
		Server: "local", Repo: "r", Worktree: "main", Session: "a",
		TmuxName: "cctl/r/main/a", WsID: "UUID-1",
	}}
	if out := entriesToSpawn(views, live, entries); len(out) != 0 {
		t.Fatalf("UUID-matched workspace treated as missing: %+v", out)
	}
	// Same entry without the UUID → title mismatch → spawn.
	entries[0].WsID = ""
	if out := entriesToSpawn(views, live, entries); len(out) != 1 {
		t.Fatalf("title-mismatched workspace without UUID should spawn, got %d", len(out))
	}
}

func TestBackfillWsIDs(t *testing.T) {
	views := []cmuxWsView{
		{id: "UUID-A", name: "r/main/a"},
		{id: "UUID-B", name: "r/main/b"},
	}
	entries := []wsEntry{
		{Server: "s", Repo: "r", Worktree: "main", Session: "a"},                 // no id → backfill A
		{Server: "s", Repo: "r", Worktree: "main", Session: "b", WsID: "STALE"},  // dead id → re-backfill B
		{Server: "s", Repo: "r", Worktree: "main", Session: "c", WsID: "UUID-A"}, // hmm: live id, title absent → keep
	}
	got := backfillWsIDs(views, entries)
	if got[0].WsID != "UUID-A" {
		t.Errorf("entry a: want UUID-A, got %q", got[0].WsID)
	}
	if got[1].WsID != "UUID-B" {
		t.Errorf("entry b: stale id not re-backfilled, got %q", got[1].WsID)
	}
	if got[2].WsID != "UUID-A" {
		t.Errorf("entry c: live recorded id must never be moved, got %q", got[2].WsID)
	}
}

func TestParseCmuxWorkspaceClosed(t *testing.T) {
	// Captured from cmux 0.64.18 `cmux events`.
	frame := `{"boot_id":"B","category":"workspace","id":"B-42","name":"workspace.closed","payload":{"custom_title":"r/main/a","cwd":"/x","index":null,"selected":false,"tab_count":12,"title":"r/main/a","workspace_id":"323ADC8F-1311-4CD8-8FD0-2DF66C2E7087"}}`
	id, title, ok := parseCmuxWorkspaceClosed([]byte(frame))
	if !ok || id != "323ADC8F-1311-4CD8-8FD0-2DF66C2E7087" || title != "r/main/a" {
		t.Fatalf("parse = (%q, %q, %v)", id, title, ok)
	}
	// Other event names and garbage are rejected.
	if _, _, ok := parseCmuxWorkspaceClosed([]byte(`{"name":"workspace.created","payload":{"workspace_id":"X"}}`)); ok {
		t.Fatal("workspace.created must not parse as a close")
	}
	if _, _, ok := parseCmuxWorkspaceClosed([]byte(`not json`)); ok {
		t.Fatal("garbage must not parse")
	}
}

func TestHealSanitizedRepoNames(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{
		"kv-dev": {Repos: map[string]Repo{"rxtx.dev": {}}},
		"ambig":  {Repos: map[string]Repo{"a.b": {}, "a:b": {}}}, // both sanitize to a_b
	}}
	entries := []wsEntry{
		{Server: "kv-dev", Repo: "rxtx_dev", Worktree: "phoenix", Session: "poc"},
		{Server: "kv-dev", Repo: "rxtx.dev", Worktree: "main", Session: "x"}, // already canonical
		{Server: "ambig", Repo: "a_b", Worktree: "w", Session: "s"},          // ambiguous — untouched
	}
	got := healSanitizedRepoNames("", cfg, nil, entries)
	if got[0].Repo != "rxtx.dev" || got[0].WsTitle != "rxtx.dev/phoenix/poc" {
		t.Errorf("alias not healed: %+v", got[0])
	}
	if got[1].Repo != "rxtx.dev" {
		t.Errorf("canonical entry disturbed: %+v", got[1])
	}
	if got[2].Repo != "a_b" {
		t.Errorf("ambiguous alias must not be guessed: %+v", got[2])
	}
}
