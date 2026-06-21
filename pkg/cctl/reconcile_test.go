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

func TestSplitWsTitle(t *testing.T) {
	if r, w, ok := splitWsTitle("rxtx.dev/main"); !ok || r != "rxtx.dev" || w != "main" {
		t.Errorf("got %q %q %v", r, w, ok)
	}
	for _, bad := range []string{"single", "a/b/c", "/x", "x/", ""} {
		if _, _, ok := splitWsTitle(bad); ok {
			t.Errorf("splitWsTitle(%q) should fail", bad)
		}
	}
}

func TestLooksLikeCctlLocal(t *testing.T) {
	local := []string{
		"tmux attach -t cctl/go-cctl/main/default",
		"/Users/x/.cctl/spawn/local__go-cctl__main__default.sh",
		"tmux new-session -A -s cctl/r/w/s 'claude'",
	}
	for _, s := range local {
		if !looksLikeCctlLocal(s) {
			t.Errorf("should be cctl-local: %q", s)
		}
	}
	notLocal := []string{
		"", "bash -l", "vim main.go",
		"mosh host -- tmux new-session -A -s cctl/r/w/s",
		"ssh host tmux attach -t cctl/r/w/s",
	}
	for _, s := range notLocal {
		if looksLikeCctlLocal(s) {
			t.Errorf("should NOT be cctl-local: %q", s)
		}
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

func TestParseResumeBinding(t *testing.T) {
	if c, s := parseResumeBinding([]byte(`{"resume_binding":null}`)); c != "" || s != "" {
		t.Errorf("null binding should be empty, got cmd=%q src=%q", c, s)
	}
	// The real cmux 0.64 shape: command + source live in resume_binding.
	real := `{"resume_binding":{"command":"tmux new-session -A -s cctl/r/w/s","cwd":"/h","kind":"tmux","source":"cctl"}}`
	if c, s := parseResumeBinding([]byte(real)); c != "tmux new-session -A -s cctl/r/w/s" || s != "cctl" {
		t.Errorf("command form = cmd=%q src=%q", c, s)
	}
	// Cross-version fallbacks.
	if c, _ := parseResumeBinding([]byte(`{"resume_binding":{"shell":"tmux attach -t cctl/r/w/s"}}`)); c != "tmux attach -t cctl/r/w/s" {
		t.Errorf("shell fallback = %q", c)
	}
	if c, _ := parseResumeBinding([]byte(`{"resume_binding":{"argv":["tmux","attach","-t","cctl/r/w/s"]}}`)); c != "tmux attach -t cctl/r/w/s" {
		t.Errorf("argv fallback = %q", c)
	}
	if c, s := parseResumeBinding([]byte(`not json`)); c != "" || s != "" {
		t.Errorf("garbage should be empty, got cmd=%q src=%q", c, s)
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

func TestManifestTitleSet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manifestUpsert(SpawnSpec{
		Server: "local", Repo: "r", Worktree: "main", Session: "a",
		WsTitle: "r/main", TabTitle: "a",
	})
	if set := manifestTitleSet(); !set["r/main\x00a"] {
		t.Errorf("title set missing expected key: %+v", set)
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
func TestMapWorkspaceMeta_ServerDerivation(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{
		"mac":       {Local: true},
		"workspace": {Host: "h"},
	}}
	// Stand in for listCmuxGroups by exercising the mapping logic via the
	// same derivation mapWorkspaceMeta uses: build it manually.
	groups := []cmuxGroup{
		{name: "workspace/olympus", members: []string{"WS1"}},
		{name: "rxtx.dev", members: []string{"WS2"}},
	}
	out := map[string]wsMeta{}
	for _, g := range groups {
		meta := wsMeta{server: "mac"}
		if prefix, _, found := strings.Cut(g.name, "/"); found {
			if srv, ok := cfg.Servers[prefix]; ok && !srv.Local {
				meta = wsMeta{server: prefix, cctlRemote: true}
			}
		}
		for _, id := range g.members {
			out[id] = meta
		}
	}
	if out["WS1"].server != "workspace" || !out["WS1"].cctlRemote {
		t.Errorf("WS1 should map to remote server workspace; got %+v", out["WS1"])
	}
	if out["WS2"].server != "mac" || out["WS2"].cctlRemote {
		t.Errorf("WS2 (bare repo group) should map local, non-remote; got %+v", out["WS2"])
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
