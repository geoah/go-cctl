package cctl

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
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
	migrated int
	errs     int
}

func (r syncResult) touched() bool {
	return r.adopted+r.restored+r.closed+r.migrated+r.errs > 0
}

func (r syncResult) summary() string {
	return fmt.Sprintf("synced cmux — closed %d, renamed %d, adopted %d, restored %d",
		r.closed, r.migrated, r.adopted, r.restored)
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

	// Snapshot cmux.
	views := snapshotCmuxViews(cli)

	// Migrate any legacy two-part "repo/worktree" workspaces to the per-
	// session "repo/worktree/session" naming. This de-duplicates same-named
	// workspaces (the reported bug) by giving each session a unique name.
	// Re-snapshot afterward so the rest of the pass sees the new names.
	if n := migrateCmuxWorkspaceNames(cli, views, loadManifestEntries(), liveByServer); n > 0 {
		res.migrated = n
		views = snapshotCmuxViews(cli)
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

	// Close cctl workspaces whose tmux session is dead. Sync NEVER revives and
	// sets NO resume bindings: that churn is what triggered cmux's "auto
	// restore?" prompts. Bringing sessions back is the explicit R key.
	closeDead(cli, views, wsMetaByID, liveByServer, localName, entries, &res)
	return res
}

// snapshotCmuxViews lists every cmux workspace with its surfaces.
func snapshotCmuxViews(cli string) []cmuxWsView {
	var views []cmuxWsView
	for _, w := range listCmuxWorkspaces(cli) {
		views = append(views, cmuxWsView{id: w.id, name: w.name, surfaces: listCmuxSurfaces(cli, w.id)})
	}
	return views
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
			WsTitle:  cmuxWsTitle(repo, s.Worktree, s.Session), TabTitle: s.Session, Remote: !srv.Local,
		})
		have[key] = true
		n++
	}
	return n
}

// closeDead closes cctl workspaces whose tmux session is no longer alive, so
// cmux shows only what's actually running. With one workspace per session
// (named repo/worktree/session) the match is by the stable WORKSPACE NAME —
// not the tab title, which terminals overwrite (that mismatch was creating
// duplicate same-named workspaces). Never closes a live session's workspace,
// never closes when a server's liveness is unknown (unreachable), and never
// prunes the manifest (so the R key can still restore it).
func closeDead(cli string, views []cmuxWsView, wsMeta map[string]wsMeta, liveByServer map[string]map[string]bool, localName string, entries []wsEntry, res *syncResult) {
	desired := manifestWsSet()
	srvByWs := map[string]string{}
	for _, e := range entries {
		srvByWs[cmuxWsTitle(e.Repo, e.Worktree, e.Session)] = e.Server
	}
	for _, w := range views {
		meta := wsMeta[w.id]
		server := meta.server
		if server == "" {
			server = localName
		}
		if s, ok := srvByWs[w.name]; ok {
			server = s
		}
		live := liveByServer[server]
		if live == nil {
			continue // server unreachable — can't tell; leave it
		}
		// inCctlRemoteGroup: cmux would never name a group "<remoteServer>/repo"
		// itself, so a member whose name sits under that repo is unambiguously
		// a cctl workspace — safe to close even legacy two-part ones.
		inCctlRemoteGroup := meta.remote && meta.repoRoot != "" && strings.HasPrefix(w.name, meta.repoRoot+"/")

		parts := strings.Split(w.name, "/")
		switch len(parts) {
		case 3:
			repo, wt, sess := parts[0], parts[1], parts[2]
			if live[tmuxName(repo, wt, sess)] {
				continue // alive — keep
			}
			if !desired[w.name] && !inCctlRemoteGroup {
				continue // dead but not provably ours
			}
		case 2:
			// Legacy name (no session). Only act inside a verified cctl remote
			// group; close when no session on that worktree is alive. Local
			// two-part groups can be cmux's own project grouping — leave them.
			if !inCctlRemoteGroup {
				continue
			}
			if anyLiveForWorktree(live, parts[0], parts[1]) {
				continue // a session there is alive — migration will rename it
			}
		default:
			continue // "cctl" control workspace, user workspaces, etc.
		}
		if out, err := exec.Command(cli, "close-workspace", "--workspace", w.id).CombinedOutput(); err != nil {
			log().Debug("sync-close-workspace-fail", "ws", w.name, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			continue
		}
		log().Info("sync-close-dead-workspace", "ws", w.name, "server", server)
		res.closed++
	}
}

// migrateCmuxWorkspaceNames renames legacy two-part "repo/worktree" cctl
// workspaces to the per-session "repo/worktree/session" scheme. That's what
// de-duplicates same-named workspaces: each session ends up uniquely named.
// The session is derived from the surface (the tmux name in its title, a clean
// tab title matching a tracked/live session, or the sole manifest session for
// the worktree). Workspaces it can't confidently map are left untouched.
// Returns how many were renamed.
func migrateCmuxWorkspaceNames(cli string, views []cmuxWsView, entries []wsEntry, liveByServer map[string]map[string]bool) int {
	byWt := map[string][]string{}
	trackedTmux := map[string]bool{}
	for _, e := range entries {
		byWt[e.Repo+"/"+e.Worktree] = append(byWt[e.Repo+"/"+e.Worktree], e.Session)
		trackedTmux[e.TmuxName] = true
	}
	renamed := 0
	for _, w := range views {
		parts := strings.Split(w.name, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue // only legacy two-part names
		}
		repo, wt := parts[0], parts[1]
		sess := sessionForWorkspace(w, repo, wt, byWt[repo+"/"+wt], trackedTmux, liveByServer)
		if sess == "" {
			continue
		}
		newName := cmuxWsTitle(repo, wt, sess)
		if newName == w.name {
			continue
		}
		if out, err := cmuxCmd(cli, "rename-workspace", "--workspace", w.id, newName).CombinedOutput(); err != nil {
			log().Debug("sync-rename-workspace-fail", "ws", w.name, "to", newName, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			continue
		}
		log().Info("sync-migrate-workspace", "from", w.name, "to", newName)
		renamed++
	}
	return renamed
}

// cctlSessionInTitleRe pulls the session (4th path component) out of a tmux
// session name embedded in a surface title, e.g.
// "[mosh] cctl/olympus/gb300-k8s/nfs-temp:0:bash - …" → "nfs-temp".
var cctlSessionInTitleRe = regexp.MustCompile(`cctl/[^/ ]+/[^/ ]+/([A-Za-z0-9._-]+)`)

// sessionForWorkspace best-effort derives the cctl session a legacy two-part
// workspace holds, for the migration rename. Empty when it can't be sure.
func sessionForWorkspace(w cmuxWsView, repo, wt string, manifestSessions []string, trackedTmux map[string]bool, liveByServer map[string]map[string]bool) string {
	for _, sf := range w.surfaces {
		if m := cctlSessionInTitleRe.FindStringSubmatch(sf.title); m != nil {
			return m[1]
		}
	}
	for _, sf := range w.surfaces {
		cand := strings.TrimSpace(sf.title)
		if cand == "" || strings.ContainsAny(cand, " /") {
			continue
		}
		tn := tmuxName(repo, wt, cand)
		if trackedTmux[tn] || anyLive(liveByServer, tn) {
			return cand
		}
	}
	if len(manifestSessions) == 1 {
		return manifestSessions[0]
	}
	return ""
}

func anyLive(liveByServer map[string]map[string]bool, tmux string) bool {
	for _, set := range liveByServer {
		if set[tmux] {
			return true
		}
	}
	return false
}

// restoreFromManifest is the R key: bring back every tracked session that
// isn't currently up — (re)open its per-session workspace running the durable
// wrapper, which resurrects+resumes it (tmux new-session -A → claude
// --continue). Explicit and separate from the automatic sync, so it never
// surprises the user; it sets no resume bindings, so it doesn't trip cmux's
// restore prompts.
func restoreFromManifest(cfg *Config) syncResult {
	var res syncResult
	cli := cmuxCLIPath()
	if cli == "" {
		return res
	}
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

	open := map[string]bool{}
	for _, w := range listCmuxWorkspaces(cli) {
		open[w.name] = true
	}
	for _, e := range loadManifestEntries() {
		live := liveByServer[e.Server] != nil && liveByServer[e.Server][e.TmuxName]
		hasWs := open[cmuxWsTitle(e.Repo, e.Worktree, e.Session)]
		if live && hasWs {
			continue // already up
		}
		if err := restoreSpawn(cfg, e); err != nil {
			log().Warn("restore-fail", "ws", cmuxWsTitle(e.Repo, e.Worktree, e.Session), "err", err.Error())
			res.errs++
			continue
		}
		res.restored++
	}
	return res
}

// wsMeta is what sync infers about a cmux workspace from the sidebar groups.
type wsMeta struct {
	server   string // server the workspace belongs to (localName if ungrouped)
	remote   bool   // member of a verified cctl "<remoteServer>/<repo>" group
	repoRoot string // the repo component of that group, e.g. "olympus"
}

// mapWorkspaceMeta derives, per cmux workspace UUID, which server it belongs
// to and the repo of its cctl sidebar group. cctl files each workspace under a
// group named "server/repo" for remotes and bare "repo" locally; a group
// whose prefix matches a configured remote server marks its members as that
// server's (and, since cmux would never name a group "<remoteServer>/<repo>"
// on its own, as unambiguously cctl-owned).
func mapWorkspaceMeta(cli string, cfg *Config, localName string) map[string]wsMeta {
	out := map[string]wsMeta{}
	for _, g := range listCmuxGroups(cli) {
		meta := deriveWsMeta(g.name, cfg, localName)
		for _, wsID := range g.members {
			out[wsID] = meta
		}
	}
	return out
}

// deriveWsMeta classifies a cctl sidebar group name. Split out for testing.
func deriveWsMeta(groupName string, cfg *Config, localName string) wsMeta {
	if prefix, rest, found := strings.Cut(groupName, "/"); found {
		if srv, ok := cfg.Servers[prefix]; ok && !srv.Local {
			return wsMeta{server: prefix, remote: true, repoRoot: rest}
		}
	}
	return wsMeta{server: localName, repoRoot: groupName}
}

// anyLiveForWorktree reports whether any cctl session on a worktree is alive,
// used to decide a legacy two-part workspace (no session in its name) is dead.
func anyLiveForWorktree(live map[string]bool, repo, worktree string) bool {
	prefix := sessionPrefix + tmuxSafeName(repo) + "/" + tmuxSafeName(worktree) + "/"
	for k := range live {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
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
			WsTitle:    cmuxWsTitle(e.Repo, e.Worktree, e.Session),
			TabTitle:   e.Session,
			GroupTitle: group,
			GroupCwd:   groupCwd,
		})
	return err
}

// ---- small helpers ---------------------------------------------------------

// cmuxWsTitle is the cmux workspace name for a session: repo/worktree/session.
// Each session gets its own uniquely-named workspace so cctl can match it by
// the stable workspace NAME (tab titles are overwritten by the terminal).
func cmuxWsTitle(repo, worktree, session string) string {
	return repo + "/" + worktree + "/" + session
}

// parseWsTitle splits a "repo/worktree/session" workspace name. ok=false for
// any other shape (the cctl control workspace, the user's own workspaces, or
// legacy two-part names awaiting migration).
func parseWsTitle(name string) (repo, worktree, session string, ok bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func manifestKeySet() map[string]bool {
	set := map[string]bool{}
	for _, e := range loadManifestEntries() {
		set[e.key()] = true
	}
	return set
}

// manifestWsSet is the set of cmux workspace names cctl wants to exist (one
// per tracked session), so closeDead spares any workspace still tracked.
func manifestWsSet() map[string]bool {
	set := map[string]bool{}
	for _, e := range loadManifestEntries() {
		set[cmuxWsTitle(e.Repo, e.Worktree, e.Session)] = true
	}
	return set
}

// ---- targeted close (used by delete) ---------------------------------------

// closeCmuxWorkspaceByTitle closes the workspace with the given exact name
// (one session = one workspace, so deleting a session closes its workspace).
// Best-effort: a missing workspace is fine.
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

// closeCmuxWorkspacesByPrefix closes every workspace whose name starts with
// prefix — used when a whole worktree is removed (prefix "repo/worktree/"),
// since each of its sessions is its own workspace.
func closeCmuxWorkspacesByPrefix(prefix string) {
	cli := cmuxCLIPath()
	if cli == "" || prefix == "" {
		return
	}
	for _, w := range listCmuxWorkspaces(cli) {
		if !strings.HasPrefix(w.name, prefix) {
			continue
		}
		if out, err := exec.Command(cli, "close-workspace", "--workspace", w.id).CombinedOutput(); err != nil {
			log().Debug("cmux-close-workspace-fail", "ws", w.name, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			continue
		}
		log().Info("cmux-close-workspace", "ws", w.name)
	}
}
