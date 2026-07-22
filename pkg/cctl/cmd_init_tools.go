package cctl

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// This file holds the per-tool init logic that `cctl init` (see cmd_init.go)
// drives. There are no cobra subcommands any more — `cctl init` is a single
// interactive/flag-driven command that calls these helpers for the tools and
// servers the user picked.

// sortedRemoteNames returns the names of every configured non-local server, in
// stable (sorted) order. Local is handled separately by each tool's local path.
func sortedRemoteNames(cfg *Config) []string {
	var out []string
	for name, s := range cfg.Servers {
		if s.Local {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---- tmux ------------------------------------------------------------------

const tmuxManagedBody = `# cctl best practices for scroll/select/copy across mosh + ghostty + tmux.

# Mouse ON — required for wheel-scroll: tmux occupies the terminal's
# alternate screen, so if tmux doesn't capture the mouse the emulator
# translates the wheel into arrow keys aimed at whatever app is focused
# (claude eats them as input-history). The cost is that tmux also captures
# drag-selection, so copying is split by where the tmux server runs:
#   - LOCAL sessions: plain drag → copy-mode → the copy-pipe binding below
#     pipes to pbcopy → system clipboard. Just drag.
#   - REMOTE (mosh) sessions: pbcopy runs on the wrong machine and tmux's
#     OSC 52 fallback is dropped by cmux — so hold SHIFT while dragging.
#     Shift bypasses tmux's mouse capture (libghostty behavior, which cmux
#     and ghostty share) and the terminal selects+copies natively.
set -g mouse on

# vim-style copy mode (Ctrl-b [ then v / y)
setw -g mode-keys vi

# OSC 52: let tmux push its copy buffer to the terminal's clipboard, so copy
# works even over ssh/mosh where there's no local pbcopy. Two parts:
#   1. set-clipboard on -> tmux emits OSC 52 when you copy.
#   2. the Ms override is REQUIRED, not optional: tmux only emits OSC 52 if the
#      terminal's terminfo advertises the "Ms" capability, and xterm-256color
#      (mosh's default $TERM) ships none -- so without this, set-clipboard is a
#      silent no-op on remotes. It also forces the "c" (CLIPBOARD) selector,
#      the only one mosh-server forwards. The terminal emulator must still allow
#      OSC 52 writes (` + "`cctl init`" + ` (ghostty) sets clipboard-write = allow,
#      which cmux's terminals also read).
set -s set-clipboard on
set -ag terminal-overrides ",*:Ms=\\E]52;c%p1%.0s;%p2%s\\007"

# allow inner TUIs/agents (claude, codex, vim) to emit their own passthrough
# escape sequences (OSC 52, images, ...) out to the terminal
set -g allow-passthrough on

# Plain drag ALWAYS starts a tmux selection — even over apps that grabbed the
# mouse (claude enables mouse reporting for scroll, and by default tmux would
# forward the drag to it; claude then renders its own selection whose copy
# path is OSC 52, dead inside cmux). Hijacking the drag costs nothing real —
# claude keeps clicks and scroll — and makes drag behave the same over every
# pane. Copy lands via the copy-pipe bindings below.
bind -n MouseDrag1Pane copy-mode -M

# Copy to the system clipboard. Two independent mechanisms, because OSC 52
# alone isn't enough inside cmux: cmux's terminals don't reliably honor an
# inbound OSC 52 clipboard-write, so a tmux-copy-mode selection would land in a
# tmux buffer that never reaches the Mac clipboard. Piping to pbcopy fixes the
# LOCAL case (the tmux server is on the Mac); the "|| cat" swallows stdin on
# remotes where pbcopy is absent, and set-clipboard's OSC 52 (above) still
# carries the copy back over mosh — visible in ghostty, dropped by cmux. So:
# local copy = plain drag (pbcopy); remote copy INSIDE CMUX = hold SHIFT while
# dragging (bypasses tmux; cmux selects natively and copyOnSelect copies).
bind -T copy-mode-vi y send-keys -X copy-pipe-and-cancel "pbcopy 2>/dev/null || cat >/dev/null"

# mouse drag-end copies without exiting copy-mode (so the view doesn't snap
# back to the bottom mid-scroll)
bind -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe-no-clear "pbcopy 2>/dev/null || cat >/dev/null"

# big scrollback so claude output is still scrollable hours later
set -g history-limit 100000

# zero escape time helps vim, fzf, and the claude TUI feel snappy
set -sg escape-time 0

# let vim/nvim and agent TUIs react to focus gain/lose
set -g focus-events on

# when re-attached from a smaller (e.g. roamed mosh) client, resize to the
# active client instead of locking the whole session to the smallest one
set -g aggressive-resize on

# broadcast the tmux session name as the terminal window title so ghostty's
# tab title is consistent whether you're attached locally or via mosh.
set -g set-titles on
set -g set-titles-string "#{session_name}"

# true color for ghostty / mosh / most modern terminals
set -g default-terminal "tmux-256color"
set -ga terminal-overrides ",*256col*:Tc,xterm-ghostty:Tc"
`

// ---- ghostty ---------------------------------------------------------------

const ghostlyManagedBody = `# cctl best practices for clipboard + tmux interop.

# let apps (tmux via OSC 52) write the system clipboard
clipboard-write = allow

# holding shift while selecting bypasses tmux's mouse capture so you can do
# a "raw" terminal selection that lands directly in the system clipboard.
mouse-shift-capture = false

# slightly nicer scroll feel inside long terminal output
mouse-scroll-multiplier = 3
`

// ---- mosh ------------------------------------------------------------------
//
// mosh has no file to edit — it's configured via flags and TERM — so init just
// verifies the installs are recent enough (mosh >= 1.4) for tmux mouse mode,
// OSC 52, and scrollback.

func reportMoshLocal() {
	v, err := exec.Command("mosh", "--version").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mosh: not found on local PATH — install via your package manager (brew install mosh / apt install mosh).")
		return
	}
	fmt.Fprintf(os.Stderr, "mosh (local): %s\n", firstLine(string(v)))
}

func reportMoshRemote(serverName string, srv Server) error {
	out, err := runRemote(srv, `mosh-server -v 2>&1 | head -1 || mosh --version 2>&1 | head -1 || echo "mosh not found"`)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mosh (%s): %s\n", serverName, firstLine(out))
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Hint: mosh >= 1.4 has working scrollback. If yours is older, attach the")
	fmt.Fprintln(os.Stderr, "tmux session and scroll inside tmux's copy-mode instead.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Hint: copy-to-clipboard over mosh needs the OSC 52 Ms override in tmux")
	fmt.Fprintf(os.Stderr, "(mosh only forwards the \"c\" selector). Run `cctl init --tools tmux --servers %s`.\n", serverName)
	return nil
}

// ---- claude ----------------------------------------------------------------
//
// claude has no config to edit — its conversations live in
// ~/.claude/projects/<encoded-cwd> and cctl's resume detection relies on that
// path. init just verifies claude is on PATH and reports the projects dir.

func reportClaudeLocal() {
	if path, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(os.Stderr, "claude (local): not found on PATH — install Claude Code or add it to ~/.local/bin")
	} else {
		fmt.Fprintf(os.Stderr, "claude (local): %s\n", path)
	}
	home, _ := os.UserHomeDir()
	projects := home + "/.claude/projects"
	if _, err := os.Stat(projects); err == nil {
		fmt.Fprintf(os.Stderr, "claude projects (local): %s exists — `cctl claude` will resume past conversations per worktree.\n", projects)
	} else {
		fmt.Fprintf(os.Stderr, "claude projects (local): %s missing — first `cctl claude` will start fresh.\n", projects)
	}
}

func reportClaudeRemote(serverName string, srv Server) error {
	out, err := runRemote(srv, `command -v claude || echo "(missing)"; test -d "$HOME/.claude/projects" && echo "projects: yes" || echo "projects: no"`)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "claude (%s):\n%s\n", serverName, strings.TrimSpace(out))
	return nil
}

// ---- shared helpers --------------------------------------------------------

// applyManaged is the common path for tools whose config we edit (tmux,
// ghostty). serverName == "" targets the local machine; a configured name
// targets that server (a local alias falls back to the local path). The body
// should NOT include the cctl markers; they're added by upsertManagedBlock*.
func applyManaged(tool, path, body, serverName string) error {
	if serverName == "" {
		changed, err := upsertManagedBlockLocal(path, body)
		if err != nil {
			return fmt.Errorf("%s (local): %w", tool, err)
		}
		if changed {
			fmt.Fprintf(os.Stderr, "%s (local): updated %s\n", tool, expandPath(path))
		} else {
			fmt.Fprintf(os.Stderr, "%s (local): %s already up to date\n", tool, expandPath(path))
		}
		return nil
	}
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	srv, ok := cfg.Servers[serverName]
	if !ok {
		return fmt.Errorf("unknown server %q", serverName)
	}
	if srv.Local {
		// Local server alias — same code path as no --server, but use the
		// server alias for log clarity.
		changed, err := upsertManagedBlockLocal(path, body)
		if err != nil {
			return fmt.Errorf("%s (%s): %w", tool, serverName, err)
		}
		if changed {
			fmt.Fprintf(os.Stderr, "%s (%s): updated %s\n", tool, serverName, expandPath(path))
		} else {
			fmt.Fprintf(os.Stderr, "%s (%s): %s already up to date\n", tool, serverName, expandPath(path))
		}
		return nil
	}
	changed, err := upsertManagedBlockRemote(srv, path, body)
	if err != nil {
		return fmt.Errorf("%s (%s): %w", tool, serverName, err)
	}
	if changed {
		fmt.Fprintf(os.Stderr, "%s (%s): updated %s\n", tool, serverName, path)
	} else {
		fmt.Fprintf(os.Stderr, "%s (%s): %s already up to date\n", tool, serverName, path)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
