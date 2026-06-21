package cctl

import (
	"encoding/json"
	"fmt"
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
	restored int
	closed   int
	errs     int
}

func (r syncResult) touched() bool {
	return r.adopted+r.restored+r.closed+r.errs > 0
}

func (r syncResult) summary() string {
	return fmt.Sprintf("synced cmux — closed %d, adopted %d, restored %d",
		r.closed, r.adopted, r.restored)
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
	// Clean up legacy cctl resume-command bindings (the cmux "auto restore?"
	// prompt spam). No-op once gone, since cctl no longer creates them.
	pruneCctlResumeCommands(cli)
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

	// Re-read after adoption so close sees the full tracked set.
	entries := loadManifestEntries()

	// Close cctl tabs whose tmux session is dead — tracked dead sessions and
	// identifiable orphans. Sync NEVER revives and sets NO resume bindings:
	// that churn is what triggered cmux's "auto restore?" prompts. Bringing
	// sessions back is the explicit R key (restoreFromManifest).
	closeDead(cli, views, wsMetaByID, liveByServer, localName, entries, &res)
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

// closeDead closes cctl tabs whose tmux session is no longer alive — tracked
// sessions (in the manifest) and identifiable orphans alike — so cmux shows
// only what's actually running. It never closes a tab whose session is alive,
// never closes when a server's liveness couldn't be determined (unreachable),
// and never prunes the manifest, so the R key can still restore a session
// whose tab it closed. No reviving and no resume bindings here — that's what
// keeps cmux quiet.
func closeDead(cli string, views []cmuxWsView, wsMeta map[string]wsMeta, liveByServer map[string]map[string]bool, localName string, entries []wsEntry, res *syncResult) {
	desired := manifestTitleSet()
	// Tracked tabs know their real server from the manifest — use that for
	// liveness even when cmux's group metadata is missing.
	srvByTab := map[string]string{}
	for _, e := range entries {
		srvByTab[e.WsTitle+"\x00"+e.TabTitle] = e.Server
	}
	for _, w := range views {
		repo, wt, ok := splitWsTitle(w.name)
		if !ok {
			continue
		}
		groupServer := localName
		if m, ok := wsMeta[w.id]; ok && m.server != "" {
			groupServer = m.server
		}
		closedHere := 0
		for _, sf := range w.surfaces {
			if sf.title == "" {
				continue
			}
			key := w.name + "\x00" + sf.title
			server := groupServer
			if s, ok := srvByTab[key]; ok {
				server = s
			}
			live := liveByServer[server]
			if live == nil {
				continue // server unreachable — can't tell; leave it
			}
			if live[tmuxName(repo, wt, sf.title)] {
				continue // alive — keep
			}
			// Dead. Close only tabs we own: tracked (in the manifest) or
			// positively cctl-identified (local resume binding / remote group).
			owned := desired[key]
			if !owned {
				if server == localName {
					owned = isCctlLocalSurface(cli, w.id, sf.id)
				} else {
					owned = wsMeta[w.id].cctlRemote
				}
			}
			if !owned {
				continue
			}
			if out, err := exec.Command(cli, "close-surface", "--workspace", w.id, "--surface", sf.id).CombinedOutput(); err != nil {
				log().Debug("sync-close-surface-fail", "ws", w.name, "tab", sf.title, "err", err.Error(), "out", strings.TrimSpace(string(out)))
				continue
			}
			log().Info("sync-close-dead-tab", "ws", w.name, "tab", sf.title, "server", server)
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

// restoreFromManifest is the R key: bring back every tracked session that
// isn't currently up — (re)open its cmux tab running the durable wrapper,
// which resurrects+resumes it (tmux new-session -A → claude --continue).
// Explicit and separate from the automatic sync, so it never surprises the
// user; it sets no resume bindings, so it doesn't trip cmux's restore prompts.
func restoreFromManifest(cfg *Config) syncResult {
	var res syncResult
	cli := cmuxCLIPath()
	if cli == "" {
		return res
	}
	// Liveness per server, so already-up sessions are skipped.
	liveByServer := map[string]map[string]bool{}
	addLive := func(name string, srv Server) {
		if sessions, err := listSessions(name, srv); err == nil {
			set := make(map[string]bool, len(sessions))
			for _, ss := range sessions {
				set[ss.Name] = true
			}
			liveByServer[name] = set
		}
	}
	if cfg.syncAllServers() {
		for n, s := range cfg.Servers {
			addLive(n, s)
		}
	} else if name, srv, ok := findLocalServer(cfg); ok {
		addLive(name, srv)
	}

	byName := map[string]cmuxWsView{}
	for _, w := range listCmuxWorkspaces(cli) {
		byName[w.name] = cmuxWsView{id: w.id, name: w.name, surfaces: listCmuxSurfaces(cli, w.id)}
	}
	for _, e := range loadManifestEntries() {
		live := liveByServer[e.Server] != nil && liveByServer[e.Server][e.TmuxName]
		hasTab := false
		if w, ok := byName[e.WsTitle]; ok {
			_, hasTab = surfaceIDByTitle(w.surfaces, e.TabTitle)
		}
		if live && hasTab {
			continue // already up
		}
		if err := restoreSpawn(cfg, e); err != nil {
			log().Warn("restore-fail", "ws", e.WsTitle, "tab", e.TabTitle, "err", err.Error())
			res.errs++
			continue
		}
		res.restored++
	}
	return res
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
