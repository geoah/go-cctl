package cctl

import "testing"

func TestClaudeHookSubcommand(t *testing.T) {
	cases := map[string]string{
		"Notification":     "notification",
		"Stop":             "stop",
		"SessionStart":     "session-start",
		"SessionEnd":       "session-end",
		"UserPromptSubmit": "prompt-submit",
		"PreToolUse":       "pre-tool-use",
		"Bogus":            "",
		"":                 "",
	}
	for in, want := range cases {
		if got := claudeHookSubcommand(in); got != want {
			t.Errorf("claudeHookSubcommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCmuxHookEnv(t *testing.T) {
	base := []string{
		"PATH=/bin",
		"CMUX_WORKSPACE_ID=control-pane",
		"CMUX_SURFACE_ID=control-surface",
		"CMUX_SOCKET_PATH=/tmp/x.sock",
	}
	out := cmuxHookEnv(base, "ws-123", "surf-456")
	// The control-pane values must be replaced, not duplicated, so the replayed
	// hook targets the session's surface — and the socket path is preserved.
	var ws, surf, sock int
	for _, kv := range out {
		switch kv {
		case "CMUX_WORKSPACE_ID=ws-123":
			ws++
		case "CMUX_SURFACE_ID=surf-456":
			surf++
		case "CMUX_SOCKET_PATH=/tmp/x.sock":
			sock++
		case "CMUX_WORKSPACE_ID=control-pane", "CMUX_SURFACE_ID=control-surface":
			t.Errorf("stale control-pane id leaked through: %q", kv)
		}
	}
	if ws != 1 || surf != 1 {
		t.Errorf("target ids not set exactly once: ws=%d surf=%d", ws, surf)
	}
	if sock != 1 {
		t.Errorf("socket path must be preserved exactly once, got %d", sock)
	}
}

func TestBetaRemoteClaudeStatusDefault(t *testing.T) {
	if (&Config{}).betaRemoteClaudeStatus() {
		t.Error("beta_remote_claude_status should default to false")
	}
	tr := true
	if !(&Config{Defaults: Defaults{BetaRemoteClaudeStatus: &tr}}).betaRemoteClaudeStatus() {
		t.Error("explicit true should enable")
	}
}
