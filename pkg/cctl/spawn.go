package cctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Spawner launches a wrapper script in a new tab/window of a terminal
// emulator (fire-and-forget). Implementations return nil on successful
// launch, an error otherwise; the TUI falls back to inline tea.ExecProcess
// when no Spawner succeeds.
//
// Each Spawner declares an Available() check so detection prefers
// terminals that are actually installed, and a Name() that matches the
// values accepted by `defaults.spawn` ("cmux" or "inline").
//
// `cwd` is a hint at the working directory the new tab/window should
// reflect. cmux uses it as the workspace path so the sidebar entry shows
// the worktree name. The inline spawner ignores it; the wrapper script
// does its own cd anyway.
//
// `title` is the human label the new tab/workspace should display. cmux
// uses it via rename-workspace.
//
// `focusExisting` says a workspace with this title, if one exists, can be
// focused instead of creating a new one. Only the attach-to-a-live-session
// path sets it: there the existing tab holds the attached tmux client, so
// focusing is the right move. Everywhere else (new session, resurrect) the
// wrapper script MUST actually run, and an existing same-named workspace is
// just a leftover whose wrapper already exited — focusing it would silently
// do nothing.
type Spawner interface {
	Name() string
	Available() bool
	Spawn(scriptPath, cwd, title string, focusExisting bool) error
}

// ---- cmux ------------------------------------------------------------------

type cmuxSpawner struct{}

func (cmuxSpawner) Name() string { return "cmux" }

func (cmuxSpawner) Available() bool {
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/Applications/cmux.app", "/Applications/Cmux.app"} {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
	}
	_, err := exec.LookPath("cmux")
	return err == nil
}

// Spawn either focuses an existing cmux workspace whose name matches the
// requested title (only when the caller allows it via focusExisting), or
// creates a new one-pane workspace running our wrapper.
//
// Focus-existing: when the user hits Enter on an attached session that we
// previously spawned in a tab, the cmux workspace for it usually still
// exists. Re-opening it as a new tab leaves the original tab orphaned
// with a duplicate. So we ask cmux to list workspaces, find one whose
// name equals our title (we set `--name <title>` on creation), and call
// `select-workspace` to focus it. Only on no-match do we create. The
// caller gates this: for detached/new/resurrected sessions a same-named
// workspace is a dead leftover and focusing it would skip running the
// wrapper entirely (the tmux session would never start).
//
// Creating uses `new-workspace --layout` so cmux's default template
// (which adds a Files panel beside the terminal) is bypassed and the
// workspace contains exactly one terminal surface.
//
// Auth: cmux's socket only accepts processes started inside cmux. When run
// from outside, the call errors and the TUI falls back to inline ExecProcess.
func (cmuxSpawner) Spawn(script, cwd, title string, focusExisting bool) error {
	cli := cmuxCLIPath()
	if cli == "" {
		return fmt.Errorf("cmux CLI not found (install /Applications/cmux.app)")
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	// Try focus-existing first. Errors here aren't fatal — we just fall
	// through to creation; the existing-workspace fast path is a UX
	// optimization, not a correctness requirement.
	if focusExisting && title != "" {
		if id, ok := findCmuxWorkspaceByName(cli, title); ok {
			out, err := exec.Command(cli, "select-workspace", "--workspace", id).CombinedOutput()
			if err == nil {
				log().Info("cmux-focus-existing", "id", id, "title", title)
				return nil
			}
			log().Debug("cmux-select-workspace-fail", "id", id, "title", title, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		}
	}
	args, err := cmuxNewWorkspaceArgs(script, cwd, title)
	if err != nil {
		return fmt.Errorf("cmux build args: %w", err)
	}
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cmux new-workspace: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// findCmuxWorkspaceByName returns the UUID of the first workspace whose
// name matches the given string, or "" + false when none match (including
// the case where the cmux socket isn't reachable from this process).
//
// Output format from `cmux --id-format uuids list-workspaces` is one
// workspace per line; each line starts with the UUID and is followed by
// the workspace name and any extra metadata cmux chooses to include.
// We accept "uuid<sep>name" with whitespace separation and trim the
// match candidate before comparison.
func findCmuxWorkspaceByName(cli, name string) (string, bool) {
	out, err := exec.Command(cli, "--id-format", "uuids", "list-workspaces").Output()
	if err != nil {
		return "", false
	}
	target := strings.TrimSpace(name)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		id := fields[0]
		// Name may contain spaces; rejoin every field past the UUID.
		candidate := strings.TrimSpace(strings.Join(fields[1:], " "))
		if candidate == target {
			return id, true
		}
	}
	return "", false
}

// cmuxNewWorkspaceArgs builds the argv for `cmux new-workspace ...` with a
// single-terminal layout (no Files panel). Split out so the args are testable
// without spawning cmux.
func cmuxNewWorkspaceArgs(script, cwd, title string) ([]string, error) {
	layout, err := buildCmuxLayout(script)
	if err != nil {
		return nil, err
	}
	args := []string{"new-workspace"}
	if title != "" {
		args = append(args, "--name", title)
	}
	args = append(args,
		"--cwd", cwd,
		"--layout", layout,
		"--focus", "true",
	)
	return args, nil
}

// buildCmuxLayout returns a JSON layout describing a single pane containing
// one terminal surface running `command`. This matches the schema used by
// `cmux new-workspace --layout` and `cmux.json` layout definitions.
func buildCmuxLayout(command string) (string, error) {
	layout := map[string]any{
		"pane": map[string]any{
			"surfaces": []map[string]any{
				{"type": "terminal", "command": command},
			},
		},
	}
	b, err := json.Marshal(layout)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// cmuxCLIPath finds the cmux CLI binary. On macOS it's bundled inside the
// app at Contents/Resources/bin/cmux; users don't always have the symlink
// in $PATH (cmux ships a "Install CLI" action they may not have run).
func cmuxCLIPath() string {
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/cmux.app/Contents/Resources/bin/cmux",
			"/Applications/Cmux.app/Contents/Resources/bin/cmux",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("cmux"); err == nil {
		return p
	}
	return ""
}

// ---- registry + detection --------------------------------------------------

// allSpawners returns every concrete provider in a deterministic order.
// Today cctl ships only one — cmux — because "Enter always opens a new
// tab" is the design and cmux is the supported terminal. The interface
// is kept so future providers can plug in without touching call sites.
func allSpawners() []Spawner {
	return []Spawner{cmuxSpawner{}}
}

// detectSpawner returns the active Spawner. With only cmux supported,
// this is trivial — but kept as a function so the caller still gets a
// "reason" string for logging and we can plug new providers in later.
func detectSpawner(pref string) (Spawner, string) {
	pref = strings.ToLower(strings.TrimSpace(pref))
	if pref != "" && pref != "auto" {
		for _, s := range allSpawners() {
			if s.Name() == pref {
				return s, "config:" + pref
			}
		}
		log().Warn("unknown spawn provider in config, falling back to auto", "given", pref)
	}
	if looksLikeCmux() {
		return cmuxSpawner{}, "env=cmux(bundle)"
	}
	if strings.EqualFold(os.Getenv("TERM_PROGRAM"), "cmux") {
		return cmuxSpawner{}, "env=cmux"
	}
	return cmuxSpawner{}, "default"
}

// looksLikeCmux returns true when the current process appears to be running
// inside cmux even though $TERM_PROGRAM advertises "ghostty" (cmux is
// libghostty-based and inherits ghostty's TERM_PROGRAM). Heuristics:
//
//   - $__CFBundleIdentifier — macOS sets this for processes spawned by an
//     .app bundle, so cmux's "com.cmuxterm.app" identifies us.
//   - any CMUX_* environment variable — cmux sets workspace metadata as
//     env when it spawns shells.
func looksLikeCmux() bool {
	if id := strings.ToLower(os.Getenv("__CFBundleIdentifier")); strings.Contains(id, "cmux") {
		return true
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "CMUX_") {
			return true
		}
	}
	return false
}
