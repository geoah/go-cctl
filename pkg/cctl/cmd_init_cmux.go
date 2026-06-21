package cctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// cmuxConfigDefaults are the cmux.json keys we set on the user's behalf so
// cmux behaves nicely for the mosh+tmux+claude workflow cctl orchestrates.
//
// Why each:
//   - app.reorderOnNotification: when claude finishes (or any agent fires
//     a notification), the workspace bubbles to the top of the sidebar —
//     so the cctl session you care about is always glanceable.
//   - app.workspaceInheritWorkingDirectory: each cctl session has its
//     own worktree path; we don't want a new workspace to start in
//     wherever the previous one's cwd was.
//   - app.commandPaletteSearchesAllSurfaces: across many concurrent
//     sessions the palette is the fastest way to jump between them.
//   - sidebar.showNotificationMessage + showBranchDirectory: surfaces
//     "what happened recently" + "what worktree is this" inline so you
//     don't have to focus a workspace just to remember its context.
//   - notifications.{dockBadge, unreadPaneRing, paneFlash, showInMenuBar}:
//     all four are how cmux says "agent is asking for input / done";
//     defaulting them on means claude prompts don't get lost when you've
//     switched away to read PRs or browse code.
var cmuxConfigDefaults = map[string]map[string]any{
	"app": {
		"reorderOnNotification":             true,
		"workspaceInheritWorkingDirectory":  false,
		"commandPaletteSearchesAllSurfaces": true,
	},
	"sidebar": {
		"showNotificationMessage": true,
		"showBranchDirectory":     true,
	},
	"notifications": {
		"dockBadge":      true,
		"unreadPaneRing": true,
		"paneFlash":      true,
		"showInMenuBar":  true,
	},
}

func newInitCmuxCmd() *cobra.Command {
	var (
		force       bool
		skipHooks   bool
		skipReload  bool
		skipControl bool
	)
	cmd := &cobra.Command{
		Use:   "cmux",
		Short: "Apply cmux best-practice config for mosh/tmux/claude workflows",
		Long: `init cmux upserts a curated set of keys in ~/.config/cmux/cmux.json so
cmux plays nicely with cctl's session orchestration:

  app.reorderOnNotification, app.workspaceInheritWorkingDirectory,
  app.commandPaletteSearchesAllSurfaces, sidebar.showNotificationMessage,
  sidebar.showBranchDirectory, notifications.dockBadge,
  notifications.unreadPaneRing, notifications.paneFlash,
  notifications.showInMenuBar.

By default we only set a key if it's absent (so your customizations are
preserved); pass --force to overwrite.

Also runs ` + "`cmux hooks setup`" + ` so cmux's notification hooks attach to any
agents on $PATH (codex/opencode/pi/cursor/gemini/...). Claude Code's hooks
are injected automatically by cmux's own claude wrapper — there's no
explicit "claude" hook target. Then ` + "`cmux reload-config`" + ` applies the
settings without restarting cmux. Disable either step with --no-hooks /
--no-reload.

Terminal rendering settings (font, theme, transparency, scrollback)
belong in ~/.config/ghostty/config — run ` + "`cctl init ghostty`" + ` for
those. For tmux on a remote server, run ` + "`cctl init tmux --server <name>`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitCmux(force, skipHooks, skipReload, skipControl)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite cmux.json keys even if already present")
	cmd.Flags().BoolVar(&skipHooks, "no-hooks", false, "don't run `cmux hooks setup --agent claude`")
	cmd.Flags().BoolVar(&skipReload, "no-reload", false, "don't call `cmux reload-config` afterwards")
	cmd.Flags().BoolVar(&skipControl, "no-control", false, "don't set up the persistent cctl control workspace (keeps the TUI alive across cmux restarts/reboots)")
	return cmd
}

func runInitCmux(force, skipHooks, skipReload, skipControl bool) error {
	cli := cmuxCLIPath()
	if cli == "" {
		return fmt.Errorf("cmux CLI not found — install cmux from https://cmux.com or via `brew tap manaflow-ai/cmux && brew install --cask cmux`")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve $HOME: %w", err)
	}
	path := filepath.Join(home, ".config", "cmux", "cmux.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	cfg := map[string]any{}
	if len(existing) > 0 {
		// cmux ships a JSONC template (JSON with // comments). Strip them
		// before parsing — we re-emit as plain JSON afterwards so the
		// resulting file is round-trippable.
		stripped := stripJSONCComments(string(existing))
		if err := json.Unmarshal([]byte(stripped), &cfg); err != nil {
			return fmt.Errorf("parse %s (after stripping JSONC comments): %w", path, err)
		}
		// One-time .bak before we change it. Idempotent — the .bak only
		// captures the pre-cctl state, so we don't overwrite once it
		// exists.
		bak := path + ".bak-cctl"
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(bak, existing, 0o644); err != nil {
				return fmt.Errorf("backup to %s: %w", bak, err)
			}
			fmt.Fprintf(os.Stderr, "backed up existing config to %s\n", bak)
		}
	}

	// Apply our recommended defaults section-by-section.
	var changed []string
	for section, keys := range cmuxConfigDefaults {
		secMap, _ := cfg[section].(map[string]any)
		if secMap == nil {
			secMap = map[string]any{}
		}
		for k, want := range keys {
			cur, present := secMap[k]
			switch {
			case !present:
				secMap[k] = want
				changed = append(changed, fmt.Sprintf("set %s.%s = %v", section, k, want))
			case force && !equalAny(cur, want):
				secMap[k] = want
				changed = append(changed, fmt.Sprintf("override %s.%s = %v (was %v)", section, k, want, cur))
			default:
				// already set; leave it alone unless --force
			}
		}
		cfg[section] = secMap
	}
	// Encourage editor support: schema URL is harmless to drop in.
	if _, ok := cfg["$schema"]; !ok {
		cfg["$schema"] = "https://raw.githubusercontent.com/manaflow-ai/cmux/main/web/data/cmux.schema.json"
		changed = append(changed, "set $schema (for editor completion)")
	}

	if len(changed) == 0 {
		fmt.Fprintln(os.Stderr, "cmux: config already up to date")
	} else {
		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		out = append(out, '\n')
		tmp := path + ".cctl-tmp"
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("rename to %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "cmux: updated %s\n", path)
		for _, c := range changed {
			fmt.Fprintf(os.Stderr, "  - %s\n", c)
		}
	}

	if !skipHooks {
		// `cmux hooks setup` (no --agent) installs hooks for every agent
		// cmux supports that's on $PATH. Claude Code in particular is
		// wrapped automatically by cmux's claude shim, so there's no
		// "claude" target — passing one yields "Unknown hooks target".
		// `--yes` skips confirmation prompts.
		fmt.Fprintln(os.Stderr, "cmux: installing agent notification hooks for whichever agents are on $PATH...")
		if out, err := exec.Command(cli, "hooks", "setup", "--yes").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "  hooks setup failed: %v\n  %s\n", err, out)
		} else {
			fmt.Fprintf(os.Stderr, "%s", out)
		}
	}

	if !skipReload {
		fmt.Fprintln(os.Stderr, "cmux: reloading config...")
		if out, err := exec.Command(cli, "reload-config").CombinedOutput(); err != nil {
			// reload-config requires cctl to be running inside cmux (socket
			// auth). If not, no big deal — settings will apply at next launch.
			fmt.Fprintf(os.Stderr, "  reload skipped (%v) — restart cmux or run `cmux reload-config` from inside cmux to apply.\n", err)
			_ = out
		}
	}

	// Clear any legacy cctl resume-command bindings that made cmux pop up
	// "auto restore?" prompts — cctl no longer creates them.
	if n := pruneCctlResumeCommands(cli); n > 0 {
		fmt.Fprintf(os.Stderr, "cmux: removed %d stale cctl resume binding(s) (restore-prompt spam)\n", n)
	}

	if !skipControl {
		// Make the cctl TUI a durable cmux workspace so it survives cmux
		// restarts and relaunches (and reconciles) after a reboot. Requires
		// running from inside cmux for socket auth.
		fmt.Fprintln(os.Stderr, "cmux: setting up the persistent cctl control workspace...")
		if err := ensureCctlControlWorkspace(cli); err != nil {
			fmt.Fprintf(os.Stderr, "  control workspace skipped (%v) — run `cctl init cmux` from inside cmux to enable it.\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "  cctl now reopens automatically with cmux (and syncs your tabs on launch).")
		}
	}

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "next steps:")
	fmt.Fprintln(os.Stderr, "  cctl init ghostty           # terminal rendering (cmux uses libghostty)")
	fmt.Fprintln(os.Stderr, "  cctl init tmux              # tmux defaults locally")
	fmt.Fprintln(os.Stderr, "  cctl init tmux --server X   # …and on each remote you mosh to")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "and in ~/.cctl.yaml, set (optional — cmux is the default):")
	fmt.Fprintln(os.Stderr, "  defaults:")
	fmt.Fprintln(os.Stderr, "    spawn: cmux")

	return nil
}

// stripJSONCComments removes // line-comments and /* block-comments */ from
// JSON-with-comments source so encoding/json (which is strict) can parse it.
// Also drops trailing commas before `}`/`]`, which cmux's template tends to
// leave behind once you delete the trailing entry. Quoted strings are passed
// through verbatim — a "//" inside a JSON string value survives intact.
func stripJSONCComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(src) {
			switch src[i+1] {
			case '/':
				for i < len(src) && src[i] != '\n' {
					i++
				}
				if i < len(src) {
					b.WriteByte('\n')
				}
				continue
			case '*':
				i += 2
				for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
					i++
				}
				if i+1 < len(src) {
					i++ // consume '/'
				}
				continue
			}
		}
		b.WriteByte(c)
	}
	return removeTrailingCommas(b.String())
}

// removeTrailingCommas walks the (comment-free) JSON source and drops any
// `,` followed only by whitespace before `}` or `]`. Strict JSON doesn't
// allow trailing commas; cmux's JSONC template does.
func removeTrailingCommas(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			// Peek past whitespace; if next non-space byte is } or ],
			// the comma is trailing — drop it.
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func equalAny(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	// Cheap structural compare via JSON round-trip; values from JSON
	// unmarshal as float64/string/bool, which match the literals in
	// cmuxConfigDefaults closely enough.
	pa, _ := json.Marshal(a)
	pb, _ := json.Marshal(b)
	return string(pa) == string(pb)
}
