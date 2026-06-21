package cctl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// This file is the one reconcile pass that keeps cmux and cctl in lockstep.
// It backs the R/S keys AND runs on startup, so "restore", "sync", and
// "make-them-match" are all the same operation:
//
//   1. Adopt  — record live tmux sessions and existing cctl tabs into the
//               manifest, so they survive the next reboot.
//   2. Heal   — pin every cctl tab's cmux resume binding to its durable
//               wrapper (fixes stale `tmux attach` / vanished $TMPDIR paths).
//   3. Restore— for manifest sessions whose tab is missing, or whose local
//               tmux session has died, (re)open/revive the tab — resuming
//               claude via the wrapper's `new-session -A` + `--continue`.
//   4. Close  — close cctl tabs that are gone for good (deleted, or dead and
//               no longer tracked) so cmux matches cctl.
//
// Safety rails on Close: never touch a tab whose tmux session is still
// alive; only act on tabs we can positively identify as cctl-local; and on
// the very first run (no manifest yet) adopt instead of closing, so an
// upgrade never nukes a screen full of existing work.

type syncResult struct {
	adopted  int
	healed   int
	restored int
	closed   int
	errs     int
}

func (r syncResult) touched() bool {
	return r.adopted+r.healed+r.restored+r.closed+r.errs > 0
}

func (r syncResult) summary() string {
	return fmt.Sprintf("synced cmux — restored %d, healed %d, closed %d, adopted %d",
		r.restored, r.healed, r.closed, r.adopted)
}

// findLocalServer returns the (first) server marked local, the only one
// whose tmux liveness cctl can probe cheaply (no ssh). Sync limits its
// liveness-gated revive/close decisions to local sessions; remote
// (cmux-ssh) workspaces are left to cmux's own ssh restore.
func findLocalServer(cfg *Config) (string, Server, bool) {
	for name, s := range cfg.Servers {
		if s.Local {
			return name, s, true
		}
	}
	return "", Server{}, false
}

// looksLikeCctlLocal reports whether a surface's cmux resume-binding command
// belongs to a cctl LOCAL session: it targets the cctl/ tmux namespace (or
// runs one of our ~/.cctl/spawn wrappers) and isn't an ssh/mosh remote.
func looksLikeCctlLocal(shell string) bool {
	if shell == "" {
		return false
	}
	if strings.Contains(shell, "ssh ") || strings.Contains(shell, "mosh") {
		return false
	}
	return strings.Contains(shell, "/.cctl/spawn/") || strings.Contains(shell, sessionPrefix)
}

// isCctlLocalSurface reports whether a surface is a cctl-owned LOCAL tab.
// The strongest signal is the resume binding's source label, which
// setCmuxResumeBinding stamps "cctl" (local tabs only); for legacy tabs
// predating that, fall back to the shape of the bound command.
func isCctlLocalSurface(cli, wsID, surfaceID string) bool {
	cmd, source := cmuxResumeBinding(cli, wsID, surfaceID)
	if source == "cctl" {
		return true
	}
	return looksLikeCctlLocal(cmd)
}

// cmuxWsView is a workspace plus its surfaces, fetched once per sync.
type cmuxWsView struct {
	id       string
	name     string
	surfaces []cmuxSurface
}

// syncCmuxState runs the full reconcile and returns what it did. Safe to
// call with no cmux (returns a zero result) — cctl works fine headless.
func syncCmuxState(cfg *Config) syncResult {
	var res syncResult
	cli := cmuxCLIPath()
	if cli == "" {
		return res
	}
	bootstrap := !manifestFileExists()
	localName, localSrv, hasLocal := findLocalServer(cfg)

	// Local tmux liveness (sanitized full names) — cheap, no ssh.
	liveLocal := map[string]bool{}
	var localSessions []SessionInfo
	if hasLocal {
		if sessions, err := listSessions(localName, localSrv); err == nil {
			localSessions = sessions
			for _, s := range sessions {
				liveLocal[s.Name] = true
			}
		}
	}

	// Snapshot cmux: every workspace and its tabs.
	var views []cmuxWsView
	for _, w := range listCmuxWorkspaces(cli) {
		views = append(views, cmuxWsView{id: w.id, name: w.name, surfaces: listCmuxSurfaces(cli, w.id)})
	}

	res.adopted += adoptFromCmux(cli, localName, hasLocal, views)
	if hasLocal {
		res.adopted += adoptFromTmux(localName, localSrv, localSessions)
	}

	// Re-read after adoption so heal/restore see the full desired set.
	entries := loadManifestEntries()
	healRestore(cfg, cli, views, liveLocal, &res, entries)

	if !bootstrap {
		closeOrphans(cli, views, liveLocal, &res)
	}
	return res
}

// adoptFromCmux records existing cctl-local tabs (identified by their resume
// binding) that aren't in the manifest yet, so they survive the next reboot.
func adoptFromCmux(cli, localName string, hasLocal bool, views []cmuxWsView) int {
	if !hasLocal {
		return 0
	}
	have := manifestKeySet()
	n := 0
	for _, w := range views {
		repo, wt, ok := splitWsTitle(w.name)
		if !ok {
			continue
		}
		for _, sf := range w.surfaces {
			if sf.title == "" {
				continue
			}
			key := manifestKey(localName, repo, wt, sf.title)
			if have[key] {
				continue
			}
			if !isCctlLocalSurface(cli, w.id, sf.id) {
				continue
			}
			manifestUpsertEntry(wsEntry{
				Server: localName, Repo: repo, Worktree: wt, Session: sf.title,
				TmuxName: tmuxName(repo, wt, sf.title),
				WsTitle:  w.name, TabTitle: sf.title, Remote: false,
			})
			have[key] = true
			n++
		}
	}
	return n
}

// adoptFromTmux records live local tmux sessions missing from the manifest
// (e.g. ones whose cmux tab cctl couldn't fingerprint). Repo names are
// de-sanitized via the local repo list so "rxtx_dev" → "rxtx.dev".
func adoptFromTmux(localName string, localSrv Server, sessions []SessionInfo) int {
	if len(sessions) == 0 {
		return 0
	}
	repos := mergeRepos(localSrv)
	if discovered, err := discoverRepos(localSrv); err == nil {
		for k, v := range discovered {
			repos[k] = v
		}
	}
	bySafe := map[string]string{}
	for name := range repos {
		bySafe[tmuxSafeName(name)] = name
	}
	have := manifestKeySet()
	n := 0
	for _, s := range sessions {
		repo := s.Repo
		if real, ok := bySafe[s.Repo]; ok {
			repo = real
		}
		key := manifestKey(localName, repo, s.Worktree, s.Session)
		if have[key] {
			continue
		}
		manifestUpsertEntry(wsEntry{
			Server: localName, Repo: repo, Worktree: s.Worktree, Session: s.Session,
			TmuxName: s.Name,
			WsTitle:  fmt.Sprintf("%s/%s", repo, s.Worktree), TabTitle: s.Session, Remote: false,
		})
		have[key] = true
		n++
	}
	return n
}

// healRestore pins resume bindings on existing tabs (reviving dead local
// ones), and (re)opens tabs that are missing entirely.
func healRestore(cfg *Config, cli string, views []cmuxWsView, liveLocal map[string]bool, res *syncResult, entries []wsEntry) {
	byName := map[string]cmuxWsView{}
	for _, w := range views {
		byName[w.name] = w
	}
	for _, e := range entries {
		w, hasWs := byName[e.WsTitle]
		sid, hasTab := "", false
		if hasWs {
			sid, hasTab = surfaceIDByTitle(w.surfaces, e.TabTitle)
		}
		if !hasTab {
			// Tab (or whole workspace) gone — recreate it. spawnInNewWindow
			// heals an existing same-titled tab or creates a fresh
			// workspace, and re-binds + re-records the manifest.
			if err := restoreSpawn(cfg, e); err != nil {
				log().Warn("sync-restore-fail", "ws", e.WsTitle, "tab", e.TabTitle, "err", err.Error())
				res.errs++
				continue
			}
			res.restored++
			continue
		}
		// Tab exists: rebind it to the durable wrapper (the core fix), and
		// if it's a local session whose tmux has died, revive the pane now.
		if e.Remote {
			continue
		}
		script, updated, err := ensureDurableScript(cfg, e)
		if err != nil || script == "" {
			if err != nil {
				log().Debug("sync-script-ensure-fail", "ws", e.WsTitle, "tab", e.TabTitle, "err", err.Error())
			}
			continue
		}
		setCmuxResumeBinding(cli, w.id, sid, updated.Cwd, e.TabTitle, script)
		res.healed++
		if !liveLocal[e.TmuxName] {
			if out, err := exec.Command(cli, "respawn-pane", "--workspace", w.id, "--surface", sid, "--command", script).CombinedOutput(); err != nil {
				log().Debug("sync-revive-fail", "ws", e.WsTitle, "tab", e.TabTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			} else {
				res.restored++
			}
		}
	}
}

// closeOrphans closes cctl-local tabs that should no longer exist: not in
// the manifest and not currently alive in tmux. Workspaces emptied as a
// result are closed too.
func closeOrphans(cli string, views []cmuxWsView, liveLocal map[string]bool, res *syncResult) {
	desired := manifestTitleSet()
	for _, w := range views {
		repo, wt, ok := splitWsTitle(w.name)
		if !ok {
			continue
		}
		closedHere := 0
		for _, sf := range w.surfaces {
			if sf.title == "" {
				continue
			}
			if desired[w.name+"\x00"+sf.title] {
				continue
			}
			if liveLocal[tmuxName(repo, wt, sf.title)] {
				continue // never close a live session's tab
			}
			if !isCctlLocalSurface(cli, w.id, sf.id) {
				continue // only close tabs we own
			}
			if out, err := exec.Command(cli, "close-surface", "--workspace", w.id, "--surface", sf.id).CombinedOutput(); err != nil {
				log().Debug("sync-close-surface-fail", "ws", w.name, "tab", sf.title, "err", err.Error(), "out", strings.TrimSpace(string(out)))
				continue
			}
			log().Info("sync-close-orphan-tab", "ws", w.name, "tab", sf.title)
			res.closed++
			closedHere++
		}
		// Whole workspace went away — close the now-empty shell too.
		if closedHere > 0 && closedHere == len(w.surfaces) {
			if out, err := exec.Command(cli, "close-workspace", "--workspace", w.id).CombinedOutput(); err != nil {
				log().Debug("sync-close-workspace-fail", "ws", w.name, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			}
		}
	}
}

// ensureDurableScript returns the durable wrapper path for an entry,
// regenerating it (and backfilling the manifest with the script/cwd/group)
// when it's missing. Remote entries have no local script.
func ensureDurableScript(cfg *Config, e wsEntry) (string, wsEntry, error) {
	if e.Remote {
		return "", e, nil
	}
	if e.Script != "" {
		if _, err := os.Stat(e.Script); err == nil {
			return e.Script, e, nil
		}
	}
	r, err := cfg.resolve(e.Server, e.Repo)
	if err != nil {
		return "", e, err
	}
	cwd := r.Repo.Path
	if e.Worktree != "" && e.Worktree != "main" {
		cwd = worktreePath(r.WorktreeBase, r.RepoName, e.Worktree)
	}
	tname := tmuxName(r.RepoName, e.Worktree, e.Session)
	inner, err := interactiveCmd(r.Server, r.UseMosh, attachOrRespawn(r, tname, cwd))
	if err != nil {
		return "", e, err
	}
	spec := SpawnSpec{Server: e.Server, Repo: e.Repo, Worktree: e.Worktree, Session: e.Session}
	path, err := writeSpawnScript(inner, spec)
	if err != nil {
		return "", e, err
	}
	e.Script = path
	e.Cwd = workspaceCwd(r, e.Worktree)
	e.Group, e.GroupCwd = repoGroup(r)
	manifestUpsertEntry(e)
	return path, e, nil
}

// restoreSpawn (re)opens a manifest entry's tab via the normal spawn path,
// which heals an existing same-titled tab or creates the workspace, binds
// the durable wrapper, and refreshes the manifest record.
func restoreSpawn(cfg *Config, e wsEntry) error {
	r, err := cfg.resolve(e.Server, e.Repo)
	if err != nil {
		return err
	}
	cwd := r.Repo.Path
	if e.Worktree != "" && e.Worktree != "main" {
		cwd = worktreePath(r.WorktreeBase, r.RepoName, e.Worktree)
	}
	group, groupCwd := repoGroup(r)
	_, err = spawnInNewWindow(cfg, r.Server, r.UseMosh,
		attachOrRespawn(r, tmuxName(r.RepoName, e.Worktree, e.Session), cwd),
		SpawnSpec{
			Server:     e.Server,
			Repo:       e.Repo,
			Worktree:   e.Worktree,
			Session:    e.Session,
			Cwd:        workspaceCwd(r, e.Worktree),
			WsTitle:    fmt.Sprintf("%s/%s", e.Repo, e.Worktree),
			TabTitle:   e.Session,
			GroupTitle: group,
			GroupCwd:   groupCwd,
		})
	return err
}

// ---- small helpers ---------------------------------------------------------

func splitWsTitle(name string) (repo, worktree string, ok bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func surfaceIDByTitle(surfaces []cmuxSurface, title string) (string, bool) {
	for _, s := range surfaces {
		if s.title == title {
			return s.id, true
		}
	}
	return "", false
}

func manifestKeySet() map[string]bool {
	set := map[string]bool{}
	for _, e := range loadManifestEntries() {
		set[e.key()] = true
	}
	return set
}

// manifestTitleSet keys desired tabs by cmux (workspace title, tab title) so
// orphan-closing can spare any tab cctl still wants, regardless of server.
func manifestTitleSet() map[string]bool {
	set := map[string]bool{}
	for _, e := range loadManifestEntries() {
		set[e.WsTitle+"\x00"+e.TabTitle] = true
	}
	return set
}

// ---- targeted close (used by delete) ---------------------------------------

// closeCmuxTabByTitle closes the one tab matching (wsTitle, tabTitle), and
// the workspace too if that was its last tab. Best-effort: deleting a
// session should also clear its cmux tab, but a missing tab is fine.
func closeCmuxTabByTitle(wsTitle, tabTitle string) {
	cli := cmuxCLIPath()
	if cli == "" || wsTitle == "" || tabTitle == "" {
		return
	}
	id, ok := findCmuxWorkspaceByName(cli, wsTitle)
	if !ok {
		return
	}
	surfaces := listCmuxSurfaces(cli, id)
	sid, ok := surfaceIDByTitle(surfaces, tabTitle)
	if !ok {
		return
	}
	if out, err := exec.Command(cli, "close-surface", "--workspace", id, "--surface", sid).CombinedOutput(); err != nil {
		log().Debug("cmux-close-surface-fail", "ws", wsTitle, "tab", tabTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		return
	}
	log().Info("cmux-close-tab", "ws", wsTitle, "tab", tabTitle)
	if len(surfaces) <= 1 {
		if out, err := exec.Command(cli, "close-workspace", "--workspace", id).CombinedOutput(); err != nil {
			log().Debug("cmux-close-workspace-fail", "ws", wsTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		}
	}
}

// closeCmuxWorkspaceByTitle closes an entire workspace (used when a whole
// worktree is removed).
func closeCmuxWorkspaceByTitle(wsTitle string) {
	cli := cmuxCLIPath()
	if cli == "" || wsTitle == "" {
		return
	}
	id, ok := findCmuxWorkspaceByName(cli, wsTitle)
	if !ok {
		return
	}
	if out, err := exec.Command(cli, "close-workspace", "--workspace", id).CombinedOutput(); err != nil {
		log().Debug("cmux-close-workspace-fail", "ws", wsTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		return
	}
	log().Info("cmux-close-workspace", "ws", wsTitle)
}
