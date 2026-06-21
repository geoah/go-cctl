package cctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// This file is the one reconcile pass that keeps cmux and cctl in lockstep.
// It backs the R/S keys AND runs on startup (after every server has settled,
// not on a fixed timer), so "restore", "sync", and "make-them-match" are all
// the same operation:
//
//   1. Adopt  — record LIVE local tmux sessions into the manifest so they
//               survive the next reboot. Dead cmux tabs are deliberately not
//               adopted — they're what Close removes.
//   2. Heal   — pin every cctl tab's cmux resume binding to its durable
//               wrapper (fixes stale `tmux attach` / vanished $TMPDIR paths).
//   3. Restore— for manifest sessions whose tab is missing, or whose local
//               tmux session has died, (re)open/revive the tab — resuming
//               claude via the wrapper's `new-session -A` + `--continue`.
//   4. Close  — close cctl tabs no longer backed by a session, so cmux only
//               shows what's actually running/tracked. Runs every sync.
//
// Safety rails on Close: never touch a tab whose tmux session is still
// alive, and only act on tabs we can positively identify as cctl-local
// (resume source=cctl, or a cctl/ command that isn't ssh/mosh). Because
// Adopt+Restore populate the manifest first, "not in the manifest" by the
// time Close runs means "not a live or tracked session" — i.e. junk.

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
//
// Liveness + adopt + close span every server by default (defaults.
// sync_all_servers, default true); set false to limit them to the local
// server. Remote liveness costs one `tmux list-sessions` ssh round-trip per
// server, but sync is occasional (startup + R/S), so that's acceptable.
func syncCmuxState(cfg *Config) syncResult {
	var res syncResult
	cli := cmuxCLIPath()
	if cli == "" {
		return res
	}
	localName, _, hasLocal := findLocalServer(cfg)

	// Pick the servers to reconcile against.
	targets := map[string]Server{}
	if cfg.syncAllServers() {
		for n, s := range cfg.Servers {
			targets[n] = s
		}
	} else if hasLocal {
		targets[localName] = cfg.Servers[localName]
	}

	// Per-server tmux liveness (sanitized full names) + raw sessions for adopt.
	liveByServer := map[string]map[string]bool{}
	sessionsByServer := map[string][]SessionInfo{}
	for name, s := range targets {
		sessions, err := listSessions(name, s)
		if err != nil {
			continue
		}
		set := make(map[string]bool, len(sessions))
		for _, ss := range sessions {
			set[ss.Name] = true
		}
		liveByServer[name] = set
		sessionsByServer[name] = sessions
	}

	// Snapshot cmux, and map each workspace to its server via the sidebar
	// groups cctl creates ("server/repo" remote, bare "repo" local).
	var views []cmuxWsView
	for _, w := range listCmuxWorkspaces(cli) {
		views = append(views, cmuxWsView{id: w.id, name: w.name, surfaces: listCmuxSurfaces(cli, w.id)})
	}
	wsMetaByID := mapWorkspaceMeta(cli, cfg, localName)

	// Adopt only LIVE sessions into the manifest (never dead cmux tabs —
	// those are what Close removes; adopting them would protect junk
	// forever). Reboot-revival relies on the persisted manifest, not on
	// re-adopting dead tabs.
	for name, s := range targets {
		res.adopted += adoptFromTmux(name, s, sessionsByServer[name])
	}

	// Re-read after adoption so heal/restore see the full desired set.
	entries := loadManifestEntries()
	healRestore(cfg, cli, views, liveByServer, &res, entries)

	// Close cctl tabs no longer backed by a session. Safety rails live in
	// closeOrphans (cctl-owned + never-close-alive), so it's safe even on
	// the first, manifest-less run.
	closeOrphans(cli, views, wsMetaByID, liveByServer, localName, &res)
	return res
}

// adoptFromTmux records live tmux sessions on one server that are missing
// from the manifest. Repo names are de-sanitized via the server's repo list
// so "rxtx_dev" → "rxtx.dev". Remote entries are flagged so heal/close treat
// them as cmux-ssh-managed.
func adoptFromTmux(serverName string, srv Server, sessions []SessionInfo) int {
	if len(sessions) == 0 {
		return 0
	}
	repos := mergeRepos(srv)
	// discoverRepos shells out; only worth it locally (fast). Remote dot-repos
	// fall back to the sanitized name, which is harmless for adoption.
	if srv.Local {
		if discovered, err := discoverRepos(srv); err == nil {
			for k, v := range discovered {
				repos[k] = v
			}
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
		key := manifestKey(serverName, repo, s.Worktree, s.Session)
		if have[key] {
			continue
		}
		manifestUpsertEntry(wsEntry{
			Server: serverName, Repo: repo, Worktree: s.Worktree, Session: s.Session,
			TmuxName: s.Name,
			WsTitle:  fmt.Sprintf("%s/%s", repo, s.Worktree), TabTitle: s.Session, Remote: !srv.Local,
		})
		have[key] = true
		n++
	}
	return n
}

// healRestore pins resume bindings on existing tabs (reviving dead local
// ones), and (re)opens tabs that are missing entirely. `liveByServer` maps a
// server name to the set of its live tmux session names.
func healRestore(cfg *Config, cli string, views []cmuxWsView, liveByServer map[string]map[string]bool, res *syncResult, entries []wsEntry) {
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
		// Remote tabs have no local resume binding (cmux-ssh manages their
		// restore); a present one needs nothing here.
		if e.Remote {
			continue
		}
		// Tab exists: rebind it to the durable wrapper (the core fix), and if
		// the session's tmux has died, revive the pane now.
		script, updated, err := ensureDurableScript(cfg, e)
		if err != nil || script == "" {
			if err != nil {
				log().Debug("sync-script-ensure-fail", "ws", e.WsTitle, "tab", e.TabTitle, "err", err.Error())
			}
			continue
		}
		setCmuxResumeBinding(cli, w.id, sid, updated.Cwd, e.TabTitle, script)
		res.healed++
		alive := liveByServer[e.Server] != nil && liveByServer[e.Server][e.TmuxName]
		if !alive {
			if out, err := exec.Command(cli, "respawn-pane", "--workspace", w.id, "--surface", sid, "--command", script).CombinedOutput(); err != nil {
				log().Debug("sync-revive-fail", "ws", e.WsTitle, "tab", e.TabTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			} else {
				res.restored++
			}
		}
	}
}

// closeOrphans closes cctl tabs that should no longer exist: not in the
// manifest and not currently alive in tmux, on any server. Workspaces
// emptied as a result are closed too.
func closeOrphans(cli string, views []cmuxWsView, wsMeta map[string]wsMeta, liveByServer map[string]map[string]bool, localName string, res *syncResult) {
	desired := manifestTitleSet()
	for _, w := range views {
		repo, wt, ok := splitWsTitle(w.name)
		if !ok {
			continue
		}
		server := localName
		if m, ok := wsMeta[w.id]; ok && m.server != "" {
			server = m.server
		}
		live := liveByServer[server]
		closedHere := 0
		for _, sf := range w.surfaces {
			if sf.title == "" {
				continue
			}
			if desired[w.name+"\x00"+sf.title] {
				continue
			}
			if live != nil && live[tmuxName(repo, wt, sf.title)] {
				continue // never close a live session's tab
			}
			// Only close tabs we can positively identify as cctl's: local
			// ones via their resume binding, remote ones via membership of a
			// verified cctl "server/repo" group.
			owned := false
			if server == localName {
				owned = isCctlLocalSurface(cli, w.id, sf.id)
			} else {
				owned = wsMeta[w.id].cctlRemote
			}
			if !owned {
				continue
			}
			if out, err := exec.Command(cli, "close-surface", "--workspace", w.id, "--surface", sf.id).CombinedOutput(); err != nil {
				log().Debug("sync-close-surface-fail", "ws", w.name, "tab", sf.title, "err", err.Error(), "out", strings.TrimSpace(string(out)))
				continue
			}
			log().Info("sync-close-orphan-tab", "ws", w.name, "tab", sf.title, "server", server)
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

// wsMeta is what sync infers about a cmux workspace from the sidebar groups.
type wsMeta struct {
	server     string // server the workspace belongs to (localName if ungrouped)
	cctlRemote bool   // member of a verified cctl "<remoteServer>/<repo>" group
}

// mapWorkspaceMeta derives, per cmux workspace UUID, which server it belongs
// to. cctl files each workspace under a sidebar group named "server/repo"
// for remotes and bare "repo" locally; a group whose prefix matches a
// configured remote server marks its members as that server's (and as
// cctl-owned remote workspaces, safe to close when dead).
func mapWorkspaceMeta(cli string, cfg *Config, localName string) map[string]wsMeta {
	out := map[string]wsMeta{}
	for _, g := range listCmuxGroups(cli) {
		meta := wsMeta{server: localName}
		if prefix, _, found := strings.Cut(g.name, "/"); found {
			if srv, ok := cfg.Servers[prefix]; ok && !srv.Local {
				meta = wsMeta{server: prefix, cctlRemote: true}
			}
		}
		for _, wsID := range g.members {
			out[wsID] = meta
		}
	}
	return out
}

// cmuxGroup is a sidebar group's name + member workspace UUIDs.
type cmuxGroup struct {
	name    string
	members []string
}

// listCmuxGroups parses `workspace-group list --json` (uuid id-format) into
// name + member ids. Empty on any failure.
func listCmuxGroups(cli string) []cmuxGroup {
	out, err := cmuxCmd(cli, "--id-format", "uuids", "workspace-group", "list", "--json").Output()
	if err != nil {
		return nil
	}
	return parseCmuxGroups(out)
}

// parseCmuxGroups extracts (name, member ids) from `workspace-group list
// --json`. Split out for unit testing without a live cmux.
func parseCmuxGroups(raw []byte) []cmuxGroup {
	var payload struct {
		Groups []struct {
			Name    string   `json:"name"`
			Members []string `json:"member_workspace_ids"`
		} `json:"groups"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	var res []cmuxGroup
	for _, g := range payload.Groups {
		res = append(res, cmuxGroup{name: g.Name, members: g.Members})
	}
	return res
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
