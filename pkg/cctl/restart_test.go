package cctl

import (
	"strings"
	"testing"
)

// TestRestartTargets_SkipsUnknownServer verifies --restart-all only targets
// tracked sessions whose server still exists in the config — killing a session
// whose server was removed would leave it dead (nothing can restore it).
func TestRestartTargets_SkipsUnknownServer(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{
		"local":     {Local: true},
		"workspace": {Host: "10.0.0.1"},
	}}
	entries := []wsEntry{
		{Server: "local", Repo: "app", Worktree: "main", Session: "a", TmuxName: "cctl/app/main/a"},
		{Server: "workspace", Repo: "svc", Worktree: "wt", Session: "b", TmuxName: "cctl/svc/wt/b"},
		{Server: "retired", Repo: "old", Worktree: "main", Session: "c", TmuxName: "cctl/old/main/c"},
	}
	got := restartTargets(cfg, entries)
	if len(got) != 2 {
		t.Fatalf("restartTargets returned %d entries, want 2 (the 'retired' server is gone): %+v", len(got), got)
	}
	for _, e := range got {
		if e.Server == "retired" {
			t.Errorf("restartTargets kept an entry on the removed server %q", e.Server)
		}
	}
}

// TestRestartTargets_Empty covers the no-tracked-sessions case (config reload
// only) without panicking.
func TestRestartTargets_Empty(t *testing.T) {
	cfg := &Config{Servers: map[string]Server{"local": {Local: true}}}
	if got := restartTargets(cfg, nil); len(got) != 0 {
		t.Errorf("restartTargets(nil entries) = %+v, want empty", got)
	}
}

// TestTmuxReloadCmd pins the reload command: it must source ~/.tmux.conf and be
// non-fatal (so a host with no running tmux server doesn't abort the pass).
func TestTmuxReloadCmd(t *testing.T) {
	if !strings.Contains(tmuxReloadCmd, "source-file ~/.tmux.conf") {
		t.Errorf("tmuxReloadCmd should source ~/.tmux.conf, got %q", tmuxReloadCmd)
	}
	if !strings.Contains(tmuxReloadCmd, "|| true") {
		t.Errorf("tmuxReloadCmd should be non-fatal (|| true) so a missing server doesn't abort, got %q", tmuxReloadCmd)
	}
}
