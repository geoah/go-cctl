package cctl

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
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
	grouped  int
	errs     int
}

func (r syncResult) touched() bool {
	return r.adopted+r.restored+r.closed+r.migrated+r.grouped+r.errs > 0
}

func (r syncResult) summary() string {
	return fmt.Sprintf("reconciled cmux — restored %d, closed %d, renamed %d, grouped %d, adopted %d",
		r.restored, r.closed, r.migrated, r.grouped, r.adopted)
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

	// Per-server tmux liveness (sanitized full names) + raw sessions for adopt,
	// plus which sessions actually have claude running (vs. sitting at a shell
	// — claude exited, or the session came back as a plain shell after a
	// reboot). The tmux session existing isn't enough: we want claude up.
	liveByServer := map[string]map[string]bool{}
	sessionsByServer := map[string][]SessionInfo{}
	claudeUpByServer := map[string]map[string]bool{}
	attachedByServer := map[string]map[string]bool{}
	for name, s := range targets {
		sessions, err := listSessions(name, s)
		if err != nil {
			continue
		}
		set := make(map[string]bool, len(sessions))
		attached := make(map[string]bool, len(sessions))
		for _, ss := range sessions {
			set[ss.Name] = true
			if ss.Attached {
				attached[ss.Name] = true
			}
		}
		liveByServer[name] = set
		sessionsByServer[name] = sessions
		attachedByServer[name] = attached
		claudeUpByServer[name] = claudeRunningSessions(s)
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

	// Adopt only LIVE sessions into the manifest (never dead cmux tabs —
	// those are what Close removes; adopting them would protect junk
	// forever). Reboot-revival relies on the persisted manifest, not on
	// re-adopting dead tabs.
	for name, s := range targets {
		res.adopted += adoptFromTmux(name, s, sessionsByServer[name])
	}

	// Collapse any duplicate rows that describe one session under both its real
	// and tmux-sanitized worktree name (a legacy adopt bug). Left in place they
	// resurrect a duplicate workspace on every pass. Runs after adopt so a
	// freshly-adopted real-name entry can absorb its sanitized twin.
	manifestDedupByTmux()

	// Re-read after adoption so open/group/close see the full tracked set.
	entries := loadManifestEntries()

	// Converge to the manifest (desired state): (re)spawn every tracked
	// session that has no workspace OR no live tmux session. This revives
	// dead-but-tracked sessions (reboot/kill → `claude --continue`) and opens
	// tabs for live ones that lost theirs — local and remote alike. dd is the
	// only way to drop a session from the manifest, so nothing here resurrects
	// something you deleted. Do this BEFORE grouping so the grouping pass
	// files the freshly-opened workspaces too.
	for _, e := range entriesToSpawn(views, liveByServer, entries) {
		if err := restoreSpawn(cfg, e); err != nil {
			log().Warn("sync-spawn-fail", "ws", cmuxWsTitle(e.Repo, e.Worktree, e.Session), "err", err.Error())
			res.errs++
			continue
		}
		log().Info("sync-spawn", "ws", cmuxWsTitle(e.Repo, e.Worktree, e.Session), "server", e.Server)
		res.restored++
	}

	// Restart the agent in tracked sessions whose tmux is alive but sitting at
	// a shell — the agent exited, or (common after a reboot) the session came
	// back as a plain shell so cctl's `new-session -A` just attached it without
	// launching the agent. respawn-window re-runs the launch in place (claude
	// --continue / codex resume --last resumes). See shouldRespawnSession for
	// the exact gating (notably: never respawn an attached session).
	for _, e := range entries {
		if liveByServer[e.Server] == nil {
			continue // server unreachable — handled by the spawn pass above
		}
		if claudeUpByServer[e.Server] == nil {
			continue // agent state unknown (probe failed) — never risk killing a live session
		}
		live := liveByServer[e.Server][e.TmuxName]
		agentUp := claudeUpByServer[e.Server][e.TmuxName]
		attached := attachedByServer[e.Server][e.TmuxName]
		if !shouldRespawnSession(live, agentUp, attached, isTerminalSession(e.Session)) {
			continue
		}
		if err := respawnClaude(cfg, e); err != nil {
			log().Warn("sync-respawn-fail", "session", e.TmuxName, "err", err.Error())
			res.errs++
			continue
		}
		log().Info("sync-respawn-agent", "session", e.TmuxName, "server", e.Server)
		res.restored++
	}

	// Re-snapshot so grouping + close see the workspaces just opened, then
	// file every cctl workspace (local and remote) under one sidebar group
	// per repo. Grouping here (with the workspace id straight from the
	// snapshot) is the reliable path — no dependence on a freshly-created
	// workspace being listed yet.
	views = snapshotCmuxViews(cli)
	wsMetaByID := mapWorkspaceMeta(cli, cfg, localName)
	res.grouped = ensureRepoGrouping(cli, cfg, localName, views, wsMetaByID, entries)

	// Close cctl workspaces whose tmux session is dead. Sync NEVER revives and
	// sets NO resume bindings: that churn is what triggered cmux's "auto
	// restore?" prompts. Bringing dead sessions back is the explicit R key.
	closeDead(cli, views, wsMetaByID, liveByServer, localName, entries, cfg.syncCloseUnmatched(), &res)
	return res
}

// entriesToSpawn returns the tracked sessions that aren't fully up and should
// be (re)spawned to converge cmux+tmux to the manifest: a session is "up" only
// when its tmux session is live AND it has a cmux workspace. Anything else —
// dead (reboot/kill), or live-but-no-tab — is spawned via the durable wrapper
// (`tmux new-session -A` revives a dead one with `claude --continue`; opens a
// tab for a live one). Servers we couldn't reach are skipped (can't tell, and
// the spawn would fail). Pure, so it's unit-testable.
func entriesToSpawn(views []cmuxWsView, liveByServer map[string]map[string]bool, entries []wsEntry) []wsEntry {
	existing := map[string]bool{}
	for _, w := range views {
		existing[w.name] = true
	}
	var out []wsEntry
	for _, e := range entries {
		srvLive, reachable := liveByServer[e.Server]
		if !reachable {
			continue // server unreachable — leave it alone
		}
		name := cmuxWsTitle(e.Repo, e.Worktree, e.Session)
		if srvLive[e.TmuxName] && existing[name] {
			continue // already up (live + has a tab)
		}
		existing[name] = true // de-dup if two entries map to the same name
		out = append(out, e)
	}
	return out
}

// psSplitMarker separates the two payloads claudeRunningSessions fetches in a
// single ssh round-trip: tmux's pane list and a process snapshot.
const psSplitMarker = "---PSSPLIT---"

// claudeRunningSessions returns the set of tmux sessions on a server that have
// claude (or any non-shell program) running in one of their panes. It does NOT
// trust tmux's pane_current_command: claude (node) spawns its own process
// group, so on Linux tmux reports the pane's *shell* (zsh/bash) as the current
// command even while claude runs underneath — which made every live session
// look idle and got it killed+relaunched. Instead it walks each pane's process
// subtree (ppid links survive the process-group split). Returns nil on error
// (treated as "unknown" by the caller → never respawn, so a probe blip can't
// kill a live session).
func claudeRunningSessions(srv Server) map[string]bool {
	out, err := runRemote(srv, `tmux list-panes -a -F '#{session_name} #{pane_pid}' 2>/dev/null; echo `+psSplitMarker+`; ps -eo pid=,ppid=,comm= 2>/dev/null`)
	if err != nil {
		return nil
	}
	panes, procs, ok := strings.Cut(out, psSplitMarker)
	if !ok {
		return nil
	}
	return parseClaudeRunningTree(panes, procs)
}

// parseClaudeRunningTree decides, per tmux session, whether a non-shell program
// (claude) runs in any of its panes by walking the process subtree rooted at
// each pane's pid. `panes` is `session_name pane_pid` lines; `procs` is
// `pid ppid comm` lines (a full `ps` snapshot). A pane's root pid is its shell,
// so the session counts as running only when a non-shell descendant exists.
// Split out so it's unit-testable without a live box.
func parseClaudeRunningTree(panes, procs string) map[string]bool {
	children := map[int][]int{}
	comm := map[int]string{}
	for _, line := range strings.Split(procs, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		comm[pid] = strings.Join(f[2:], " ")
		children[ppid] = append(children[ppid], pid)
	}
	up := map[string]bool{}
	for _, line := range strings.Split(panes, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[len(f)-1])
		if err != nil {
			continue
		}
		if subtreeHasNonShell(pid, children, comm) {
			up[f[0]] = true
		}
	}
	return up
}

// subtreeHasNonShell reports whether the process tree rooted at pid contains a
// process whose command is not a bare shell — i.e. claude (or a tool it ran).
// The root is the pane's own shell, so only a non-shell descendant trips it.
func subtreeHasNonShell(root int, children map[int][]int, comm map[int]string) bool {
	seen := map[int]bool{}
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue // guard against a malformed (cyclic) snapshot
		}
		seen[pid] = true
		if c, ok := comm[pid]; ok && c != "" && !isShellCommand(c) {
			return true
		}
		stack = append(stack, children[pid]...)
	}
	return false
}

// isShellCommand reports whether a tmux pane_current_command is a bare login/
// interactive shell (so claude is NOT running there). Leading "-" marks a
// login shell.
func isShellCommand(cmd string) bool {
	switch strings.TrimPrefix(cmd, "-") {
	case "sh", "bash", "zsh", "fish", "dash", "ksh", "tcsh", "csh":
		return true
	}
	return false
}

// respawnClaude restarts claude in an existing tmux session that's sitting at a
// shell, by re-running the current launch script in place (respawn-window -k →
// claude --continue resumes). The session survives; only the idle shell pane
// is replaced.
// shouldRespawnSession decides whether the reconcile revive pass relaunches the
// agent in a tracked session. It revives a session only when it is:
//   - live in tmux (exists; a wiped/dead one is handled by the spawn pass), and
//   - not already running the agent (a non-shell foreground = agent up), and
//   - NOT attached, and
//   - not a plain terminal tab (the `t` key).
//
// The attached guard is the important one: reconcile runs after every action
// (e.g. a `dd` on another row), and respawn-window -k on a session the user is
// sitting in would kill their work mid-look — which reads as "deleting one
// session interrupted my other session". An attached shell is left alone; the
// user can relaunch the agent themselves. Only detached shells get revived.
func shouldRespawnSession(live, agentUp, attached, terminal bool) bool {
	return live && !agentUp && !attached && !terminal
}

func respawnClaude(cfg *Config, e wsEntry) error {
	r, err := cfg.resolve(e.Server, e.Repo)
	if err != nil {
		return err
	}
	cwd := r.Repo.Path
	if e.Worktree != "" && e.Worktree != "main" {
		cwd = worktreePath(r.WorktreeBase, r.RepoName, e.Worktree)
	}
	// Relaunch the agent this session was created with (persisted in the
	// manifest); fall back to the config-resolved agent for older/adopted
	// entries that predate per-session tracking.
	if e.Agent != "" {
		r.Agent = e.Agent
	}
	launch := agentLaunchScript(r, cwd, "", e.TmuxName)
	_, err = runRemote(r.Server, fmt.Sprintf("tmux respawn-window -k -t %s %s", shellQuote(e.TmuxName), shellQuote(launch)))
	return err
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
// from the manifest. Repo AND worktree names are de-sanitized back to their
// real forms ("rxtx_dev" → "rxtx.dev", "ecr_b1" → "ecr.b1"): a tmux session
// name is sanitized ('.'/':' → '_'), so parseTmuxName hands us the sanitized
// components, but spawns record the manifest under the REAL names. Adopting
// under the sanitized name creates a mismatched duplicate entry (and a
// duplicate cmux workspace) that a later dd — which targets the real name —
// can't remove, so the reconcile keeps reviving it. Remote entries are flagged
// so heal/close treat them as cmux-ssh-managed.
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
	// Per-repo map from sanitized worktree name → real name, from
	// `git worktree list` (one round-trip). Mirrors the repo de-sanitization
	// above and normalizeSessionNames in the TUI. Best-effort: on error we
	// fall back to the sanitized name (harmless when the worktree has no
	// reserved chars).
	wtBySafe := map[string]map[string]string{}
	if wts, _, err := listAllWorktrees(srv, repos); err == nil {
		for repo, list := range wts {
			m := map[string]string{}
			for _, wt := range list {
				m[tmuxSafeName(wt.Name)] = wt.Name
			}
			wtBySafe[repo] = m
		}
	}
	have := manifestKeySet()
	n := 0
	for _, s := range sessions {
		repo := s.Repo
		if real, ok := bySafe[s.Repo]; ok {
			repo = real
		}
		worktree := s.Worktree
		if real, ok := wtBySafe[repo][s.Worktree]; ok {
			worktree = real
		}
		key := manifestKey(serverName, repo, worktree, s.Session)
		if have[key] {
			continue
		}
		manifestUpsertEntry(wsEntry{
			Server: serverName, Repo: repo, Worktree: worktree, Session: s.Session,
			TmuxName: s.Name,
			WsTitle:  cmuxWsTitle(repo, worktree, s.Session), TabTitle: s.Session, Remote: !srv.Local,
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
func closeDead(cli string, views []cmuxWsView, wsMeta map[string]wsMeta, liveByServer map[string]map[string]bool, localName string, entries []wsEntry, closeUnmatched bool, res *syncResult) {
	desired := manifestWsSet()
	// Canonical tmux identity of every tracked workspace. A workspace whose name
	// sanitizes to the same session as a tracked one — but that isn't itself
	// tracked — is a legacy sanitized-vs-real twin (e.g. "olympus/ecr_b1/1" left
	// over after the manifest healed to the real "olympus/ecr.b1/1"). Its shared
	// tmux session is live, so the plain liveness check would keep it forever;
	// this set lets closeDead collapse the duplicate onto the canonical tab.
	desiredTmux := map[string]bool{}
	for name := range desired {
		if r, w, s, ok := parseWsTitle(name); ok {
			desiredTmux[tmuxName(r, w, s)] = true
		}
	}
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
		if !shouldCloseDeadWorkspace(w.name, meta, live, desired, desiredTmux, closeUnmatched) {
			continue
		}
		if out, err := exec.Command(cli, "close-workspace", "--workspace", w.id).CombinedOutput(); err != nil {
			log().Debug("sync-close-workspace-fail", "ws", w.name, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			continue
		}
		log().Info("sync-close-dead-workspace", "ws", w.name, "server", server)
		res.closed++
	}
}

// shouldCloseDeadWorkspace is the pure decision behind closeDead. `live` is
// the workspace's server's live tmux-session set (caller guarantees non-nil —
// an unreachable server is never pruned). Returns true only for a workspace
// whose session is dead AND that cctl may close:
//
//   - 3-part "repo/worktree/session": close when tracked in the manifest, in a
//     verified cctl remote group, or — only with closeUnmatched — any dead
//     cctl-shaped name (the opt-in that prunes tabs cctl can't otherwise prove
//     it owns). Also closes a live one when its tmux identity matches a tracked
//     canonical workspace under a different name (a stale sanitized twin whose
//     real-name tab already covers the session — desiredTmux carries those).
//   - 2-part legacy "repo/worktree": close only inside a verified cctl remote
//     group (cmux would never name such a group itself). Never via
//     closeUnmatched — a bare two-part name is exactly what a manual tab or
//     cmux's own project grouping looks like.
//   - anything else (the "cctl" control workspace, single-name or custom
//     workspaces): never closed.
func shouldCloseDeadWorkspace(name string, meta wsMeta, live, desired, desiredTmux map[string]bool, closeUnmatched bool) bool {
	inCctlRemoteGroup := meta.remote && meta.repoRoot != "" && strings.HasPrefix(name, meta.repoRoot+"/")
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 3:
		if desired[name] {
			return false // tracked — the reconcile revives/keeps it, never closes
		}
		t := tmuxName(parts[0], parts[1], parts[2])
		if desiredTmux[t] {
			return true // stale sanitized twin of a tracked canonical workspace —
			// its real-name tab already covers this (live) session, so drop the dup
		}
		if live[t] {
			return false // live (and would've been adopted) — keep
		}
		// Orphan: a cctl-shaped tab not in the manifest (dd'd or junk). Close
		// when we can confirm it's ours (remote group) or the user opted into
		// pruning unmatched tabs.
		return inCctlRemoteGroup || closeUnmatched
	case 2:
		if !inCctlRemoteGroup {
			return false
		}
		return !anyLiveForWorktree(live, parts[0], parts[1])
	default:
		return false
	}
}

// ensureRepoGrouping files every cctl workspace under one sidebar group per
// repo — "repo" for local, "server/repo" for remote (repoGroup decides which,
// the same way spawn does). cmux otherwise groups workspaces by each one's own
// cwd directory, which splits a repo's worktrees into separate folders. This
// is the heal for workspaces that predate spawn-time grouping (or lost it).
//
// Local and remote are handled the same way; the only wrinkle is that a
// workspace's name ("repo/worktree/session") doesn't encode its server, so we
// take the server from the manifest (which also disambiguates a repo that
// exists both locally and on a remote). A workspace that's neither tracked nor
// already in a remote group can only be placed when its first path component
// is a known local repo — a remote server can't be inferred from the name
// alone. Returns how many it (re)grouped.
func ensureRepoGrouping(cli string, cfg *Config, localName string, views []cmuxWsView, wsMeta map[string]wsMeta, entries []wsEntry) int {
	srvByWs := map[string]string{}
	for _, e := range entries {
		srvByWs[cmuxWsTitle(e.Repo, e.Worktree, e.Session)] = e.Server
	}
	localRepos := map[string]Repo{}
	if localName != "" {
		localRepos = mergeRepos(cfg.Servers[localName])
		if d, err := discoverRepos(cfg.Servers[localName]); err == nil {
			for k, v := range d {
				localRepos[k] = v
			}
		}
	}
	grouped := 0
	for _, w := range views {
		parts := strings.Split(w.name, "/")
		if len(parts) < 2 || parts[0] == "" {
			continue // "cctl" control / single-name / user workspace
		}
		repo := parts[0]
		if wsMeta[w.id].repoRoot == repo {
			continue // already in this repo's group (local or remote)
		}

		// Which server does this workspace belong to? Manifest first (it
		// disambiguates same-named local/remote repos), then an existing
		// remote group, else fall back to local.
		var group, groupCwd string
		if server := srvByWs[w.name]; server != "" {
			r, err := cfg.resolve(server, repo)
			if err != nil {
				continue
			}
			group, groupCwd = repoGroup(r)
		} else if m := wsMeta[w.id]; m.remote && m.server != "" {
			r, err := cfg.resolve(m.server, repo)
			if err != nil {
				continue
			}
			group, groupCwd = repoGroup(r)
		} else if r, ok := localRepos[repo]; ok {
			group, groupCwd = repo, expandPath(r.Path)
		} else {
			log().Debug("sync-group-skip", "ws", w.name, "reason", "repo not a known local repo and not tracked/remote")
			continue // can't place it safely
		}
		if err := ensureCmuxGroupMembership(cli, group, groupCwd, w.id); err != nil {
			continue // ensureCmuxGroupMembership already logged the failure
		}
		log().Info("sync-group", "ws", w.name, "group", group)
		grouped++
	}
	return grouped
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
	// Honor the session's recorded agent (fallback: config-resolved).
	if e.Agent != "" {
		r.Agent = e.Agent
	}
	group, groupCwd := repoGroup(r)
	_, err = spawnInNewWindow(cfg, r.Server, r.UseMosh,
		agentAttachOrRespawn(r, tmuxName(r.RepoName, e.Worktree, e.Session), cwd),
		SpawnSpec{
			Server:     e.Server,
			Repo:       e.Repo,
			Worktree:   e.Worktree,
			Session:    e.Session,
			Agent:      r.Agent,
			Cwd:        workspaceCwd(r, e.Worktree),
			WsTitle:    cmuxWsTitle(e.Repo, e.Worktree, e.Session),
			TabTitle:   e.Session,
			GroupTitle: group,
			GroupCwd:   groupCwd,
			// Sync/reconcile discovered this session; the user didn't ask to
			// jump to it right now. Open it in the background so a batch
			// (e.g. review-prs) doesn't yank the viewport once per session.
			Background: true,
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

// ---- restart-all (the `cctl --restart-all` destroy pass) -------------------

// tmuxReloadCmd re-reads ~/.tmux.conf into the running tmux server so
// `cctl init tmux` edits take effect there. Non-fatal: `|| true` swallows the
// "no server running" case (no sessions to restart anyway) and a missing file.
const tmuxReloadCmd = "tmux source-file ~/.tmux.conf 2>/dev/null || true"

// restartResult summarizes a --restart-all destroy pass.
type restartResult struct {
	servers  int // servers in the config
	reloaded int // servers whose tmux config we reloaded
	killed   int // tracked sessions killed (to be revived by the reconcile)
}

// restartTargets returns the tracked sessions --restart-all will kill: every
// manifest entry whose server still exists in the config. An entry for a server
// that's been removed can't be restored, so it's skipped rather than killed
// with no way back. Pure, so it's unit-testable.
func restartTargets(cfg *Config, entries []wsEntry) []wsEntry {
	var out []wsEntry
	for _, e := range entries {
		if _, ok := cfg.Servers[e.Server]; ok {
			out = append(out, e)
		}
	}
	return out
}

// restartAllTrackedSessions is the destroy half of `cctl --restart-all`: reload
// each server's tmux config (so ~/.tmux.conf / mosh edits take effect), then
// kill every tracked cctl session. The manifest is left intact, so the startup
// reconcile that runs right after revives each one fresh (`tmux new-session -A`
// + `claude --continue` / `codex resume`), and the fresh client attach picks up
// the reloaded Ms/scroll settings. Live sessions are adopted first so nothing is
// killed without a restore path. Per-server/-session errors are logged, not
// fatal — one unreachable host shouldn't block the rest.
func restartAllTrackedSessions(cfg *Config) restartResult {
	res := restartResult{servers: len(cfg.Servers)}

	// 1. Adopt live sessions so the manifest is the complete tracked set before
	//    we start killing (otherwise a live-but-unadopted session would be
	//    killed with nothing in the manifest to restore it).
	for name, s := range cfg.Servers {
		sessions, err := listSessions(name, s)
		if err != nil {
			log().Warn("restart-all-list-fail", "server", name, "err", err.Error())
			continue
		}
		adoptFromTmux(name, s, sessions)
	}

	// 2. Reload tmux config per server so the revived sessions get new settings.
	for name, s := range cfg.Servers {
		if _, err := runRemote(s, tmuxReloadCmd); err != nil {
			log().Warn("restart-all-reload-fail", "server", name, "err", err.Error())
			continue
		}
		res.reloaded++
	}

	// 3. Kill each tracked session. Manifest kept → the reconcile restores it.
	for _, e := range restartTargets(cfg, loadManifestEntries()) {
		s := cfg.Servers[e.Server]
		if _, err := runRemote(s, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null || true", shellQuote(e.TmuxName))); err != nil {
			log().Warn("restart-all-kill-fail", "session", e.TmuxName, "err", err.Error())
			continue
		}
		res.killed++
	}
	return res
}
