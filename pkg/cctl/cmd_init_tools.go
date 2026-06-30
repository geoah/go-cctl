package cctl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// newInitToolsCmds returns the per-tool subcommands attached under `cctl init`
// so the cmd_init.go entry point can register them in one call.
func newInitToolsCmds() []*cobra.Command {
	return []*cobra.Command{
		newInitTmuxCmd(),
		newInitGhostlyCmd(),
		newInitMoshCmd(),
		newInitClaudeCmd(),
		newInitCmuxCmd(),
		newInitAllCmd(),
	}
}

// ---- tmux ------------------------------------------------------------------

const tmuxManagedBody = `# cctl best practices for scroll/select/copy across mosh + ghostty + tmux.

# enable mouse for scroll-wheel + drag selection
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
#      OSC 52 writes (` + "`cctl init ghostty`" + ` sets clipboard-write = allow,
#      which cmux's terminals also read).
set -s set-clipboard on
set -ag terminal-overrides ",*:Ms=\\E]52;c%p1%.0s;%p2%s\\007"

# allow inner TUIs/agents (claude, codex, vim) to emit their own passthrough
# escape sequences (OSC 52, images, ...) out to the terminal
set -g allow-passthrough on

# y in copy mode copies the selection to the system clipboard
bind -T copy-mode-vi y send-keys -X copy-pipe-and-cancel

# mouse drag-end copies without exiting copy-mode (so the view doesn't snap
# back to the bottom mid-scroll)
bind -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe-no-clear

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

func newInitTmuxCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "tmux",
		Short: "Ensure tmux is configured for mouse/scroll/copy across mosh + ghostty",
		Long: `init tmux upserts a clearly-marked block in ~/.tmux.conf with cctl's
best-practice defaults for the mosh + tmux + ghostty stack:

  - mouse on, vim-style copy mode
  - OSC 52 set-clipboard with the Ms-capability + "c" selector override that
    makes copy actually reach the system clipboard over ssh/mosh (without it,
    set-clipboard is a silent no-op on xterm-256color / mosh)
  - allow-passthrough so agent TUIs can emit their own escape sequences
  - drag-end copy that keeps your scroll position
  - focus-events + aggressive-resize for nicer multi-client / roamed sessions
  - tmux session name broadcast as the terminal window title
  - 100k history, zero escape-time, true color

Re-run any time to refresh the block; lines outside the markers are not
touched. Use --server <name> to apply the same block on a remote you mosh to.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return applyManaged("tmux", "~/.tmux.conf", tmuxManagedBody, server)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "apply on this configured cctl server (default: local machine)")
	return cmd
}

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

func newInitGhostlyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ghostty",
		Short: "Configure ghostty's clipboard + mouse-capture for tmux interop",
		Long: `init ghostty upserts a managed block in ~/.config/ghostty/config so OSC 52
clipboard writes from tmux land in the system clipboard, and so shift+drag
bypasses tmux's mouse handling for native selection.

Local only — ghostty is the terminal emulator on your laptop.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return applyManaged("ghostty", "~/.config/ghostty/config", ghostlyManagedBody, "")
		},
	}
	return cmd
}

// ---- mosh ------------------------------------------------------------------

func newInitMoshCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "mosh",
		Short: "Check mosh version + report scrollback / setup hints",
		Long: `init mosh has no file to edit — mosh is configured via flags and TERM —
but it does verify that the local (and optionally remote) installs are
recent enough to play nicely with tmux mouse mode, OSC 52, and scrollback
(mosh >= 1.4).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportMosh(server)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "also check mosh-server on this configured cctl server")
	return cmd
}

func reportMosh(serverName string) error {
	v, err := exec.Command("mosh", "--version").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mosh: not found on local PATH — install via your package manager (brew install mosh / apt install mosh).")
	} else {
		fmt.Fprintf(os.Stderr, "mosh (local): %s\n", firstLine(string(v)))
	}
	if serverName == "" {
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
		return nil
	}
	out, rerr := runRemote(srv, `mosh-server -v 2>&1 | head -1 || mosh --version 2>&1 | head -1 || echo "mosh not found"`)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "mosh (%s): error — %v\n", serverName, rerr)
		return nil
	}
	fmt.Fprintf(os.Stderr, "mosh (%s): %s\n", serverName, firstLine(out))
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Hint: mosh >= 1.4 has working scrollback. If yours is older, attach the")
	fmt.Fprintln(os.Stderr, "tmux session and scroll inside tmux's copy-mode instead.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Hint: copy-to-clipboard over mosh needs the OSC 52 Ms override in tmux")
	fmt.Fprintf(os.Stderr, "(mosh only forwards the \"c\" selector). Run `cctl init tmux --server %s`.\n", serverName)
	return nil
}

// ---- claude ----------------------------------------------------------------

func newInitClaudeCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Sanity-check claude code + report cctl-relevant config",
		Long: `init claude does not edit claude's config — its conversations live in
~/.claude/projects/<encoded-cwd> and cctl's "resume" detection relies on
that path being present. This command just verifies claude is on PATH and
reports the projects dir.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportClaude(server)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "also check claude on this configured cctl server")
	return cmd
}

func reportClaude(serverName string) error {
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
	if serverName == "" {
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
		return nil
	}
	out, rerr := runRemote(srv, `command -v claude || echo "(missing)"; test -d "$HOME/.claude/projects" && echo "projects: yes" || echo "projects: no"`)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "claude (%s): error — %v\n", serverName, rerr)
		return nil
	}
	fmt.Fprintf(os.Stderr, "claude (%s):\n%s\n", serverName, strings.TrimSpace(out))
	return nil
}

// ---- all -------------------------------------------------------------------

func newInitAllCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "all",
		Short: "Run init tmux + ghostty + mosh + claude (in that order)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyManaged("tmux", "~/.tmux.conf", tmuxManagedBody, server); err != nil {
				return err
			}
			if err := applyManaged("ghostty", "~/.config/ghostty/config", ghostlyManagedBody, ""); err != nil {
				return err
			}
			if err := reportMosh(server); err != nil {
				return err
			}
			if err := reportClaude(server); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "apply tmux + check mosh/claude on this configured cctl server too")
	return cmd
}

// ---- shared helpers --------------------------------------------------------

// applyManaged is the common path for tools whose config we edit. It targets
// local by default and the named server if --server is given. The body should
// NOT include the cctl markers; they're added by upsertManagedBlock*.
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
