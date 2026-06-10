package cctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
// The mapping to cmux's model is one sidebar GROUP per repo, one
// workspace per WORKTREE, and one tab (surface) per SESSION. Spawning
// into a worktree that already has a workspace adds a tab there instead
// of creating another workspace; workspaces are added to their repo's
// group as they're created.
type Spawner interface {
	Name() string
	Available() bool
	Spawn(spec SpawnSpec) error
}

// SpawnSpec describes one open-this-session request to a Spawner.
type SpawnSpec struct {
	// Script is the wrapper-script path; the only thing that must run.
	Script string
	// Cwd is the working-directory hint for a newly created workspace
	// (cmux uses it for the sidebar directory label + Files panel root
	// on local servers).
	Cwd string
	// WsTitle names the worktree's workspace: "repo/worktree".
	WsTitle string
	// TabTitle names the session's tab inside the workspace.
	TabTitle string
	// GroupTitle names the sidebar group the workspace belongs to — the
	// repo ("rxtx.dev" locally, "workspace/olympus" for remotes). Empty
	// disables grouping.
	GroupTitle string
	// GroupCwd seeds the group anchor's working directory when the group
	// is first created (the repo checkout path on local servers). cmux
	// keys per-group cmux.json customization on this path.
	GroupCwd string
	// FocusExisting says reaching the existing workspace is enough —
	// don't run the script at all. Only the attach-to-a-live-session
	// path sets it: there a tab in that workspace already holds the
	// attached tmux client, and running a second client would just
	// mirror it. Everywhere else (new session, detached resurrect) the
	// wrapper script MUST actually run.
	FocusExisting bool
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

// Spawn maps cctl's tree onto cmux: one sidebar group per repo, one
// workspace per worktree (named `WsTitle`, "repo/worktree"), one tab per
// session (named `TabTitle`).
//
//   - workspace exists + FocusExisting → just select it (a tab in there
//     already holds the attached tmux client; running the script again
//     would only mirror the session).
//   - workspace exists → add a tab: new-surface, run the wrapper in it
//     (respawn-pane), rename the tab to the session name.
//   - no workspace → create one via `new-workspace --layout` with a single
//     terminal surface running the wrapper (bypasses cmux's default
//     template with its Files panel), then best-effort rename its tab.
//
// After either path the workspace is added to its repo's sidebar group
// (created on first use, anchored at the repo path). Every cmux call past
// the first is best-effort with logging: if adding a tab to an existing
// workspace fails (cmux version drift, surface-id parse failure, …) we
// fall back to creating a separate workspace so the session always opens
// SOMEWHERE, and a failed grouping never blocks the spawn.
//
// Auth: cmux's socket only accepts processes started inside cmux. When run
// from outside, the call errors and the TUI falls back to inline ExecProcess.
func (cmuxSpawner) Spawn(spec SpawnSpec) error {
	cli := cmuxCLIPath()
	if cli == "" {
		return fmt.Errorf("cmux CLI not found (install /Applications/cmux.app)")
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if spec.WsTitle != "" {
		if id, ok := findCmuxWorkspaceByName(cli, spec.WsTitle); ok {
			// Workspaces created before grouping existed (or whose group
			// was dissolved) get healed into their repo's group here.
			ensureCmuxGroupMembership(cli, spec.GroupTitle, spec.GroupCwd, id)
			if spec.FocusExisting {
				out, err := exec.Command(cli, "select-workspace", "--workspace", id).CombinedOutput()
				if err == nil {
					log().Info("cmux-focus-existing", "id", id, "ws", spec.WsTitle)
					return nil
				}
				log().Debug("cmux-select-workspace-fail", "id", id, "ws", spec.WsTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
				// fall through to add-tab: running the wrapper is still
				// correct, just less elegant than focusing.
			}
			if err := addCmuxTab(cli, id, spec.Script, spec.TabTitle); err == nil {
				log().Info("cmux-add-tab", "id", id, "ws", spec.WsTitle, "tab", spec.TabTitle)
				return nil
			} else {
				log().Warn("cmux-add-tab-fail (falling back to new workspace)",
					"id", id, "ws", spec.WsTitle, "tab", spec.TabTitle, "err", err.Error())
			}
		}
	}
	args, err := cmuxNewWorkspaceArgs(spec.Script, cwd, spec.WsTitle)
	if err != nil {
		return fmt.Errorf("cmux build args: %w", err)
	}
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cmux new-workspace: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if spec.WsTitle != "" {
		if id, ok := findCmuxWorkspaceByName(cli, spec.WsTitle); ok {
			// Best-effort: label the fresh workspace's only tab with the
			// session name so the tab bar reads "<session>" instead of a
			// shell default, and file the workspace under its repo group.
			if spec.TabTitle != "" {
				out, err := exec.Command(cli, "rename-tab", "--workspace", id, "--tab", "tab:1", spec.TabTitle).CombinedOutput()
				if err != nil {
					log().Debug("cmux-rename-tab-fail", "id", id, "tab", spec.TabTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
				}
			}
			ensureCmuxGroupMembership(cli, spec.GroupTitle, spec.GroupCwd, id)
		}
	}
	return nil
}

// ensureCmuxGroupMembership files a workspace under the sidebar group
// named `group`, creating the group when it doesn't exist yet (anchored
// at groupCwd — the repo checkout). Adding a workspace that's already a
// member is harmless. Purely cosmetic, so failures only log.
//
// Note: `workspace-group create` defaults --from to the user's current
// sidebar selection, so we always pass the workspace id explicitly.
func ensureCmuxGroupMembership(cli, group, groupCwd, wsID string) {
	if group == "" || wsID == "" {
		return
	}
	if gid, ok := findCmuxGroupByName(cli, group); ok {
		out, err := exec.Command(cli, "workspace-group", "add", "--group", gid, "--workspace", wsID).CombinedOutput()
		if err != nil {
			log().Debug("cmux-group-add-fail", "group", group, "gid", gid, "ws", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			return
		}
		log().Debug("cmux-group-add", "group", group, "gid", gid, "ws", wsID)
		return
	}
	args := []string{"workspace-group", "create", "--name", group, "--from", wsID}
	if groupCwd != "" {
		args = append(args, "--cwd", groupCwd)
	}
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		log().Debug("cmux-group-create-fail", "group", group, "ws", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		return
	}
	log().Info("cmux-group-create", "group", group, "ws", wsID)
}

// findCmuxGroupByName looks a sidebar group up by exact name via
// `workspace-group list --json` and returns its id. The JSON payload is
// {"groups":[{"id":…,"name":…,…}]} (see cmux's v2WorkspaceGroupPayload).
func findCmuxGroupByName(cli, name string) (string, bool) {
	out, err := exec.Command(cli, "workspace-group", "list", "--json").Output()
	if err != nil {
		return "", false
	}
	return parseCmuxGroupList(string(out), name)
}

// parseCmuxGroupList extracts the id of the group with the given name
// from `workspace-group list --json` output. Split from the exec call so
// the parsing is unit-testable. Falls back to the "ref" handle when "id"
// is absent — both are accepted by group-targeting commands.
func parseCmuxGroupList(raw, name string) (string, bool) {
	var payload struct {
		Groups []struct {
			ID   string `json:"id"`
			Ref  string `json:"ref"`
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		log().Debug("cmux-group-list-parse-fail", "err", err.Error())
		return "", false
	}
	for _, g := range payload.Groups {
		if g.Name != name {
			continue
		}
		if g.ID != "" {
			return g.ID, true
		}
		if g.Ref != "" {
			return g.Ref, true
		}
	}
	return "", false
}

// addCmuxTab opens a new terminal tab (surface) in the given workspace,
// runs the wrapper script in it, and names it after the session. Returns
// an error if the surface can't be created or its id can't be determined —
// the caller falls back to a separate workspace in that case.
func addCmuxTab(cli, wsID, script, tabTitle string) error {
	out, err := exec.Command(cli, "new-surface", "--workspace", wsID, "--focus", "true").CombinedOutput()
	if err != nil {
		return fmt.Errorf("new-surface: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sid := parseCmuxSurfaceID(string(out))
	if sid == "" {
		// new-surface's output didn't include a recognizable id; ask for
		// the workspace's surfaces and take the newest (listed last).
		lout, lerr := exec.Command(cli, "--id-format", "uuids", "list-pane-surfaces", "--workspace", wsID).Output()
		if lerr == nil {
			sid = lastCmuxListedID(string(lout))
		}
	}
	if sid == "" {
		return fmt.Errorf("could not determine new surface id (new-surface said: %s)", strings.TrimSpace(string(out)))
	}
	// The fresh surface runs the user's default shell; respawn-pane sends
	// it the wrapper-script path to execute. When the wrapper exits the
	// shell survives, so the tab stays usable.
	if out, err := exec.Command(cli, "respawn-pane", "--workspace", wsID, "--surface", sid, "--command", script).CombinedOutput(); err != nil {
		return fmt.Errorf("respawn-pane: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if tabTitle != "" {
		if out, err := exec.Command(cli, "rename-tab", "--workspace", wsID, "--tab", sid, tabTitle).CombinedOutput(); err != nil {
			log().Debug("cmux-rename-tab-fail", "surface", sid, "tab", tabTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		}
	}
	// Land the user on the workspace; the new surface was created with
	// --focus true so the right tab is already selected within it.
	if out, err := exec.Command(cli, "select-workspace", "--workspace", wsID).CombinedOutput(); err != nil {
		log().Debug("cmux-select-workspace-fail", "id", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
	}
	return nil
}

// cmuxUUIDRe / cmuxSurfaceRefRe match the two id shapes the cmux CLI
// prints depending on --id-format: full UUIDs and short "surface:N" refs.
var (
	cmuxUUIDRe       = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	cmuxSurfaceRefRe = regexp.MustCompile(`surface:\d+`)
)

// parseCmuxSurfaceID pulls a surface identifier out of `new-surface`
// output, accepting either a UUID or a "surface:N" short ref anywhere in
// the text. Returns "" when neither appears.
func parseCmuxSurfaceID(out string) string {
	if m := cmuxUUIDRe.FindString(out); m != "" {
		return m
	}
	if m := cmuxSurfaceRefRe.FindString(out); m != "" {
		return m
	}
	return ""
}

// lastCmuxListedID returns the first token of the last non-empty line —
// the id column of the newest entry in a cmux list-* output.
func lastCmuxListedID(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return strings.Fields(line)[0]
	}
	return ""
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

// notifyCmux posts a native cmux notification (sidebar badge +
// notification center) so task outcomes reach the user even when the
// cctl tab isn't focused. When wsTitle resolves to a workspace, the
// notification is attached to it (clicking jumps there). Fire-and-forget
// and best-effort: cctl works fine without a cmux socket.
func notifyCmux(title, body, wsTitle string) {
	cli := cmuxCLIPath()
	if cli == "" {
		return
	}
	args := []string{"notify", "--title", title}
	if body != "" {
		args = append(args, "--body", abbrev(body, 300))
	}
	if wsTitle != "" {
		if id, ok := findCmuxWorkspaceByName(cli, wsTitle); ok {
			args = append(args, "--workspace", id)
		}
	}
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		log().Debug("cmux-notify-fail", "title", title, "err", err.Error(), "out", strings.TrimSpace(string(out)))
	}
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
