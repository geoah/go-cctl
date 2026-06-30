package cctl

import (
	"encoding/base64"
	"strings"
	"testing"
)

// wantMsOverride is the OSC 52 Ms capability override the tmux block must
// carry verbatim. tmux only emits OSC 52 when the terminal advertises Ms, and
// xterm-256color (mosh's default $TERM) ships none — so without this line
// set-clipboard is a silent no-op on remotes. The `c%p1%.0s` prefix forces the
// "c" (CLIPBOARD) selector (the only one mosh-server forwards) while discarding
// tmux's own selector param.
//
// The backslashes are DOUBLED on purpose: tmux's config-string lexer collapses
// `\\E`->`\E` and `\\007`->`\007` before terminfo compiles them to ESC/BEL.
// A single backslash makes tmux reject the line ("invalid octal escape"), so
// this test guards the doubling that a careless edit would undo.
const wantMsOverride = `set -ag terminal-overrides ",*:Ms=\\E]52;c%p1%.0s;%p2%s\\007"`

// TestTmuxManagedBody_ClipboardDirectives pins the copy/clipboard-critical
// lines so a future edit can't silently drop the OSC 52 fix (the bug that
// broke clipboard copy over mosh) or the supporting directives.
func TestTmuxManagedBody_ClipboardDirectives(t *testing.T) {
	must := []struct{ what, line string }{
		{"set-clipboard enables OSC 52 emit", "set -s set-clipboard on"},
		{"Ms override (defines cap + forces c selector)", wantMsOverride},
		{"allow-passthrough for inner-app escapes", "set -g allow-passthrough on"},
		{"copy-pipe binds y to system clipboard", "bind -T copy-mode-vi y send-keys -X copy-pipe-and-cancel"},
		{"drag-end copy keeps scroll position", "bind -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe-no-clear"},
		{"focus-events for TUIs", "set -g focus-events on"},
		{"aggressive-resize for roamed/multi-client", "set -g aggressive-resize on"},
		{"large scrollback", "set -g history-limit 100000"},
	}
	for _, m := range must {
		if !strings.Contains(tmuxManagedBody, m.line) {
			t.Errorf("tmuxManagedBody missing %s:\n  want line: %q", m.what, m.line)
		}
	}

	// The Ms override must use a real ESC (\E) and BEL (\7) escape — written as
	// literal backslash sequences in the tmux file, not Go-interpreted control
	// bytes. Guard against an accidental \x1b creeping in.
	if strings.ContainsRune(tmuxManagedBody, '\x1b') || strings.ContainsRune(tmuxManagedBody, '\x07') {
		t.Errorf("tmuxManagedBody contains a raw control byte; tmux escapes must be the literal text \\E / \\7")
	}
}

// TestManagedBlockRemote_Base64PreservesBackslashes proves the remote writer's
// transport (base64 encode -> decode) round-trips the tmux block byte-for-byte,
// including the backslashes in the Ms override. This is the guarantee that
// `cctl init tmux --server X` lands the same OSC 52 line as the local path,
// rather than mangling \E / \7 through ssh shell quoting.
func TestManagedBlockRemote_Base64PreservesBackslashes(t *testing.T) {
	merged, changed := mergeManagedBlock("", tmuxManagedBody)
	if !changed {
		t.Fatal("mergeManagedBlock reported no change against empty input")
	}
	if !strings.Contains(merged, wantMsOverride) {
		t.Fatalf("merged block dropped the Ms override line")
	}

	encoded := encodeBase64(merged)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("encodeBase64 produced undecodable output: %v", err)
	}
	if string(decoded) != merged {
		t.Fatalf("base64 round-trip altered the block:\n  len(decoded)=%d len(merged)=%d", len(decoded), len(merged))
	}
	if !strings.Contains(string(decoded), wantMsOverride) {
		t.Errorf("base64 round-trip lost the Ms override line (backslashes mangled)")
	}
}

// TestCmuxConfigDefaults_TerminalScrollSpeed pins the cmux scroll multiplier so
// it stays aligned with the ghostty mouse-scroll-multiplier `cctl init ghostty`
// writes (otherwise scrolling feels slower in cmux's own terminals).
func TestCmuxConfigDefaults_TerminalScrollSpeed(t *testing.T) {
	term, ok := cmuxConfigDefaults["terminal"]
	if !ok {
		t.Fatal("cmuxConfigDefaults missing the terminal section")
	}
	if got := term["scrollSpeed"]; got != 3 {
		t.Errorf("terminal.scrollSpeed = %v, want 3 (match ghostty mouse-scroll-multiplier)", got)
	}
}
