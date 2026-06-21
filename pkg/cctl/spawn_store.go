package cctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file makes cctl's session tabs survive a machine reboot.
//
// The problem it fixes: cmux persists each tab and re-runs its "resume
// command" when it restores a workspace (after an app restart or a full
// reboot). cctl used to hand cmux a wrapper script written to $TMPDIR —
// which macOS clears on reboot — so the persisted path pointed at a file
// that no longer existed, and the pane came up dead. Worse, older tabs had
// a bare `tmux attach` baked in, which errors with "no sessions" once the
// reboot has wiped the tmux server.
//
// The fix has two halves, both implemented here:
//
//   1. Durable wrapper scripts. Instead of a random $TMPDIR temp file, each
//      session gets a stable path under ~/.cctl/spawn/ derived from its
//      identity. It survives reboot, is overwritten in place on every
//      respawn (no temp-file accumulation), and runs the idempotent
//      `tmux new-session -A` that resurrects+resumes the session.
//
//   2. Explicit cmux resume bindings. After opening a tab cctl tells cmux,
//      via `cmux surface resume set`, exactly what to replay on restore:
//      the durable wrapper above. This replaces whatever stale command cmux
//      had cached and guarantees a reboot resurrects the session instead of
//      dying. (cmux >= the build shipping surface.resume.set; older cmux
//      just keeps using the layout command, which the durable path already
//      fixes.)

// cctlHome returns ~/.cctl, creating it if needed. This is cctl's local
// state root (alongside the existing remotes/ stand-in dirs).
func cctlHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cctl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// spawnScriptDir returns (and creates) ~/.cctl/spawn — the home for the
// durable wrapper scripts cmux replays on restore.
func spawnScriptDir() (string, error) {
	h, err := cctlHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(h, "spawn")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// fsSlug turns an arbitrary identity component into a filename-safe token:
// ASCII alphanumerics plus . _ - survive, everything else collapses to '-'.
// Used only for the on-disk wrapper filename, never for tmux/cmux targeting.
func fsSlug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	return out
}

// hasIdentity reports whether a spec carries the full session identity
// needed for a stable wrapper path + manifest entry. Ad-hoc spawns without
// it fall back to a disposable $TMPDIR script.
func (s SpawnSpec) hasIdentity() bool {
	return s.Server != "" && s.Repo != "" && s.Worktree != "" && s.Session != ""
}

// spawnScriptPath is the deterministic wrapper path for a session. The
// server is part of the key so the same repo/worktree/session on two hosts
// never collide on one filename.
func spawnScriptPath(spec SpawnSpec) (string, error) {
	dir, err := spawnScriptDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s__%s__%s__%s.sh",
		fsSlug(spec.Server), fsSlug(spec.Repo), fsSlug(spec.Worktree), fsSlug(spec.Session))
	return filepath.Join(dir, name), nil
}

// writeScriptAtomic writes an executable script via temp-file + rename so a
// concurrent cmux restore never reads a half-written file. The temp lives in
// the destination dir so the rename stays on one filesystem.
func writeScriptAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cctl-spawn-*.tmp")
	if err != nil {
		return fmt.Errorf("create wrapper temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write wrapper: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close wrapper: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod wrapper: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename wrapper: %w", err)
	}
	return nil
}

// sweepLegacySpawnScripts best-effort removes the old disposable wrapper
// scripts cctl used to drop in $TMPDIR. They're obsolete now that wrappers
// live under ~/.cctl/spawn, and clearing them stops a vanished $TMPDIR path
// from being replayed. Errors are ignored — this is pure tidying.
func sweepLegacySpawnScripts() {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "cctl-spawn-*.sh"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// ---- cmux resume bindings --------------------------------------------------

// cmuxCmd builds a cmux invocation with CMUX_QUIET=1 so deprecation notices
// (e.g. "list-workspaces is now an alias…") stay out of our parsed output
// and logs. Used by the resume/introspection helpers added here; the older
// call sites read stdout only, where the notices (stderr) never appeared.
func cmuxCmd(cli string, args ...string) *exec.Cmd {
	c := exec.Command(cli, args...)
	c.Env = append(os.Environ(), "CMUX_QUIET=1")
	return c
}

// pruneCctlResumeCommands removes cmux "resume command" bindings cctl created
// in earlier versions (source=cctl). Those accumulate in cmux.json and make
// cmux pop up "auto restore?" prompts on launch; cctl no longer creates them
// (it relies on cmux replaying a surface's own durable-wrapper command), so
// this cleans up the legacy cruft. Best-effort; writes cmux.json + reloads.
func pruneCctlResumeCommands(cli string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	path := filepath.Join(home, ".config", "cmux", "cmux.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	out, removed := stripCctlResumeCommands(raw)
	if removed == 0 {
		return 0
	}
	tmp := path + ".cctl-tmp"
	if os.WriteFile(tmp, out, 0o644) != nil {
		return 0
	}
	if os.Rename(tmp, path) != nil {
		os.Remove(tmp)
		return 0
	}
	_ = cmuxCmd(cli, "reload-config").Run()
	log().Info("cmux-prune-resume-commands", "removed", removed)
	return removed
}

// stripCctlResumeCommands removes terminal.resumeCommands entries with
// source=="cctl" from a cmux.json document, returning the rewritten JSON
// (indented, trailing newline) and how many were removed. Returns (nil, 0)
// when nothing changed or the document can't be parsed as strict JSON (so a
// JSONC file with comments is left untouched). Pure — unit-testable.
func stripCctlResumeCommands(raw []byte) ([]byte, int) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil, 0
	}
	termRaw, ok := doc["terminal"]
	if !ok {
		return nil, 0
	}
	var term map[string]json.RawMessage
	if json.Unmarshal(termRaw, &term) != nil {
		return nil, 0
	}
	rcRaw, ok := term["resumeCommands"]
	if !ok {
		return nil, 0
	}
	var rc []map[string]any
	if json.Unmarshal(rcRaw, &rc) != nil {
		return nil, 0
	}
	kept := make([]map[string]any, 0, len(rc))
	for _, c := range rc {
		if s, _ := c["source"].(string); s == "cctl" {
			continue
		}
		kept = append(kept, c)
	}
	removed := len(rc) - len(kept)
	if removed == 0 {
		return nil, 0
	}
	if len(kept) == 0 {
		delete(term, "resumeCommands")
	} else {
		b, _ := json.Marshal(kept)
		term["resumeCommands"] = b
	}
	tb, _ := json.Marshal(term)
	doc["terminal"] = tb
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, 0
	}
	return append(out, '\n'), removed
}

// ---- cmux surface enumeration ----------------------------------------------

// cmuxSurface is one tab (surface) inside a workspace: its UUID + tab title.
type cmuxSurface struct {
	id    string
	title string
}

// listCmuxSurfaces returns the surfaces of a workspace with their tab
// titles, parsed from `list-pane-surfaces`. Lines look like:
//
//   - <uuid>  <title…>  [selected]
//     <uuid>  <title…>
//
// The leading "*"/selected markers and trailing "[…]" bracket are stripped;
// the title is whatever sits between the UUID and any trailing marker.
func listCmuxSurfaces(cli, wsID string) []cmuxSurface {
	out, err := cmuxCmd(cli, "--id-format", "uuids", "list-pane-surfaces", "--workspace", wsID).Output()
	if err != nil {
		return nil
	}
	return parseCmuxSurfaceLines(string(out))
}

// parseCmuxSurfaceLines parses `list-pane-surfaces` output (see
// listCmuxSurfaces' doc for the line shapes). Split out for unit testing.
func parseCmuxSurfaceLines(out string) []cmuxSurface {
	var res []cmuxSurface
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		if line == "" {
			continue
		}
		id := cmuxUUIDRe.FindString(line)
		if id == "" {
			continue
		}
		rest := strings.TrimSpace(line[strings.Index(line, id)+len(id):])
		// Drop a trailing "[selected]"/"[focused]" style marker.
		if i := strings.LastIndex(rest, "["); i >= 0 && strings.HasSuffix(rest, "]") {
			rest = strings.TrimSpace(rest[:i])
		}
		res = append(res, cmuxSurface{id: id, title: rest})
	}
	return res
}

// findCmuxSurfaceByTitle returns the UUID of the first surface in a
// workspace whose tab title matches exactly. Used to heal/respawn the right
// existing tab instead of piling on a duplicate.
func findCmuxSurfaceByTitle(cli, wsID, title string) (string, bool) {
	for _, s := range listCmuxSurfaces(cli, wsID) {
		if s.title == title {
			return s.id, true
		}
	}
	return "", false
}

// ---- cctl control pane (the "keep cctl running" half) ----------------------

// controlSessionName is the tmux session the cctl TUI lives in when set up
// as a durable cmux control pane. Its name has no repo/worktree/session
// shape, so parseTmuxName rejects it and it never appears as a managed row.
const controlSessionName = sessionPrefix + "_control" // "cctl/_control"

// writeControlScript materializes the durable wrapper that keeps the cctl
// TUI alive. Wrapped in tmux: a cmux-only restart re-attaches the still-
// running TUI (zero downtime), and after a reboot tmux is gone so it's
// recreated and relaunched — whereupon cctl's startup pass reconciles cmux.
func writeControlScript() (string, error) {
	dir, err := spawnScriptDir()
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "cctl" // fall back to a PATH lookup
	}
	content := "#!/bin/sh\n" +
		"# cctl control pane — keeps the cctl TUI alive across cmux restarts\n" +
		"# and relaunches it after a reboot (it then reconciles cmux on start).\n" +
		"exec tmux new-session -A -s " + shellQuote(controlSessionName) + " " + shellQuote(self) + "\n"
	path := filepath.Join(dir, "_control.sh")
	if err := writeScriptAtomic(path, content); err != nil {
		return "", err
	}
	return path, nil
}

// ensureCctlControlWorkspace makes the cctl TUI a persistent cmux workspace
// so it reopens with cmux: a "cctl" workspace whose surface command is the
// durable control wrapper, which cmux replays on restart/reboot. No resume
// binding (those spam cmux's restore prompts) — just the surface command.
//
// If a "cctl" workspace already exists it's left alone (it already runs cctl,
// which cmux replays); we never respawn it, so this won't restart a running
// TUI. Needs to run from inside cmux (socket auth).
func ensureCctlControlWorkspace(cli string) error {
	if cli == "" {
		return fmt.Errorf("cmux CLI not found")
	}
	if _, ok := findCmuxWorkspaceByName(cli, "cctl"); ok {
		log().Info("cctl-control-workspace-exists")
		return nil
	}
	script, err := writeControlScript()
	if err != nil {
		return fmt.Errorf("write control wrapper: %w", err)
	}
	home, _ := os.UserHomeDir()
	if out, err := cmuxCmd(cli, "new-workspace", "--name", "cctl", "--cwd", home,
		"--command", script, "--focus", "false").CombinedOutput(); err != nil {
		return fmt.Errorf("create cctl workspace: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log().Info("cctl-control-workspace-created", "script", script)
	return nil
}
