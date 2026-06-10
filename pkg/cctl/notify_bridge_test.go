package cctl

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseNotifyLine covers the local half of the claude→cmux bridge:
// notify.jsonl lines become notification content; non-cctl sessions and
// junk are dropped.
func TestParseNotifyLine(t *testing.T) {
	title, body, ws, key, ok := parseNotifyLine(
		`{"ts":1,"event":"Notification","session":"cctl/rxtx_dev/main/mneme","payload":{"message":"Claude needs your permission"}}`)
	if !ok {
		t.Fatalf("valid Notification line must parse")
	}
	if !strings.Contains(title, "needs attention") || !strings.Contains(title, "rxtx_dev/main/mneme") {
		t.Errorf("title = %q", title)
	}
	if body != "Claude needs your permission" {
		t.Errorf("body = %q", body)
	}
	if ws != "rxtx_dev/main" {
		t.Errorf("wsTitle = %q, want sanitized repo/worktree", ws)
	}
	if key != "cctl/rxtx_dev/main/mneme/Notification" {
		t.Errorf("throttle key = %q", key)
	}

	title, _, _, _, ok = parseNotifyLine(
		`{"ts":2,"event":"Stop","session":"cctl/r/wt/poc","payload":null}`)
	if !ok || !strings.Contains(title, "finished") {
		t.Errorf("Stop event: title = %q ok=%v", title, ok)
	}

	for _, bad := range []string{
		"", "   ", "not json",
		`{"ts":3,"event":"Stop","session":"someones-other-session","payload":null}`, // non-cctl session
	} {
		if _, _, _, _, ok := parseNotifyLine(bad); ok {
			t.Errorf("line %q must not parse", bad)
		}
	}
}

// TestNotifyHookScript_EndToEnd runs the actual remote hook script
// through sh with a sample claude hook payload and verifies the JSONL it
// appends parses back through parseNotifyLine. HOME is pointed at a temp
// dir; TMUX is unset so the session field is empty (tolerated — the line
// parses but is dropped at the non-cctl-session gate, which we bypass by
// injecting the session via a fake tmux shim on PATH).
func TestNotifyHookScript_EndToEnd(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fake `tmux display-message -p '#S'` returning a cctl session name.
	shim := "#!/bin/sh\necho 'cctl/olympus/b300/poc'\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, "hook.sh")
	if err := os.WriteFile(script, []byte(notifyHookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", script, "Notification")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"TMUX=fake", // truthy so the script asks (our shim) for the session
	)
	cmd.Stdin = strings.NewReader(`{"message":"needs input","session_id":"abc"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook script failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(home, ".cctl", "notify.jsonl"))
	if err != nil {
		t.Fatalf("notify.jsonl not written: %v", err)
	}
	line := strings.TrimSpace(string(data))
	var ev notifyEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("hook output is not valid JSON: %v\n%s", err, line)
	}
	title, body, ws, _, ok := parseNotifyLine(line)
	if !ok {
		t.Fatalf("hook output must round-trip through parseNotifyLine: %s", line)
	}
	if !strings.Contains(title, "olympus/b300/poc") || body != "needs input" || ws != "olympus/b300" {
		t.Errorf("round-trip: title=%q body=%q ws=%q", title, body, ws)
	}
}

// TestClaudeLaunchScript_BridgeSurvivesQuoting syntax-checks the whole
// chain: the bridge launch script (heredocs with single quotes inside)
// gets shellQuoted into a tmux new-session command — both layers must
// stay valid shell.
func TestClaudeLaunchScript_BridgeSurvivesQuoting(t *testing.T) {
	launch := claudeLaunchScript("/wt", []string{"--dangerously-skip-permissions"}, "", true)
	inner := filepath.Join(t.TempDir(), "inner.sh")
	if err := os.WriteFile(inner, []byte(launch), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", inner).CombinedOutput(); err != nil {
		t.Fatalf("launch script is not valid shell: %v\n%s", err, out)
	}
	full := "tmux new-session -A -s cctl/r/wt/s " + shellQuote(launch) + "\n"
	outer := filepath.Join(t.TempDir(), "outer.sh")
	if err := os.WriteFile(outer, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", outer).CombinedOutput(); err != nil {
		t.Fatalf("tmux-quoted command is not valid shell: %v\n%s", err, out)
	}
}

// TestClaudeLaunchScript_NotifyBridge pins the remote-session wiring:
// bridge=true installs the hook + settings overlay and adds --settings
// to the claude invocation; bridge=false (local) leaves everything out.
func TestClaudeLaunchScript_NotifyBridge(t *testing.T) {
	got := claudeLaunchScript("/wt", nil, "", true)
	for _, want := range []string{
		"cmux-claude-hook.sh",
		"claude-cmux-hooks.json",
		`--settings "$HOME/.cctl/claude-cmux-hooks.json"`,
		"CCTL_NOTIFY_HOOK",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bridge launch script missing %q", want)
		}
	}
	local := claudeLaunchScript("/wt", nil, "", false)
	if strings.Contains(local, "cmux-claude-hook") || strings.Contains(local, "--settings") {
		t.Errorf("local launch script must not carry the bridge:\n%s", local)
	}
	// The settings overlay itself must be valid JSON with both hooks.
	var cfg map[string]any
	if err := json.Unmarshal([]byte(notifyHookSettings), &cfg); err != nil {
		t.Fatalf("notifyHookSettings is not valid JSON: %v", err)
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks["Notification"] == nil || hooks["Stop"] == nil {
		t.Errorf("settings overlay must wire Notification and Stop hooks")
	}
}
