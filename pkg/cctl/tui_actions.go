package cctl

// The TUI's background-action layer: every function here runs OFF the
// bubbletea Update loop (inside tea.Cmd goroutines) and reports back via a
// message — prepare/spawn/attach/kill workers, the reconcile command, and
// the cmux-close-event handler. Split from tui.go, which keeps the model,
// the Update loop, and the synchronous mode/key handlers.

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// spawnInNewWindow launches an interactive cmd in a new tab (or window,
// depending on the terminal) using the configured/auto-detected Spawner.
// Fire-and-forget — TUI stays alive. Returns the provider name + an error
// if the spawn failed. The previous inline-fallback path was removed when
// cctl committed to "Enter always opens in a new tab".
//
// We always write a tiny wrapper script first so each Spawner only ever
// has to hand its terminal one filesystem path; this dodges all the
// arg-quoting/splitting pitfalls.
func spawnInNewWindow(cfg *Config, s Server, useMosh bool, cmdStr string, spec SpawnSpec) (string, error) {
	inner, err := interactiveCmd(s, useMosh, cmdStr)
	if err != nil {
		return "", fmt.Errorf("build interactive cmd: %w", err)
	}
	script, err := writeSpawnScript(inner, spec)
	if err != nil {
		return "", err
	}
	spec.Script = script
	// transport: cmux-ssh — request a remote-SSH workspace running the
	// raw remote command; the wrapper script above stays as the fallback
	// if the cmux ssh path fails.
	if s.useCmuxSSH() {
		spec.Remote = &RemoteSpawn{
			Destination: sshTarget(s),
			Port:        s.Port,
			Identity:    expandPath(s.SSHKey),
			SSHOptions:  sshOptionValues(s.SSHOpts),
			Command:     cmdStr,
		}
	}
	pref := ""
	if cfg != nil {
		pref = cfg.Defaults.Spawn
	}
	sp, reason := detectSpawner(pref)
	log().Debug("tui-spawn", "provider", sp.Name(), "reason", reason, "script", script,
		"cwd", spec.Cwd, "ws", spec.WsTitle, "tab", spec.TabTitle,
		"group", spec.GroupTitle, "focusExisting", spec.FocusExisting)
	if err := sp.Spawn(spec); err != nil {
		// Only clean up disposable ($TMPDIR) wrappers. A durable
		// ~/.cctl/spawn script may already back an existing cmux tab; a
		// transient spawn failure here mustn't yank it out from under a
		// later restore.
		if !spec.hasIdentity() {
			os.Remove(script)
		}
		return sp.Name(), err
	}
	// Record the session in the restore manifest so a reboot (which wipes
	// tmux, cctl's only other memory) can bring it back. Disposable spawns
	// without identity aren't tracked.
	if spec.hasIdentity() {
		manifestUpsert(spec)
		// Record the cmux workspace UUID immediately (reconcile also
		// backfills, but capturing it here makes the very first dd/match
		// rename-proof). Retry-lookup: new-workspace can return before the
		// workspace is queryable.
		if sp.Name() == "cmux" && spec.WsTitle != "" {
			if cli := cmuxCLIPath(); cli != "" {
				if id, ok := findCmuxWorkspaceByNameRetry(cli, spec.WsTitle); ok {
					manifestSetWsID(spec.Server, spec.Repo, spec.Worktree, spec.Session, id)
				}
			}
		}
	}
	return sp.Name(), nil
}

func (m *tuiModel) terminalPrepareCmd(serverName, repoName, worktree, name string, taskID int) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		// A terminal session is a plain shell; reconcile's spawn recognizes the
		// term/term2/… name and uses terminalCmd, so it converges like any other
		// session through the single path.
		e := wsEntry{
			Server:   serverName,
			Repo:     repoName,
			Worktree: worktree,
			Session:  name,
			TmuxName: tmuxName(r.RepoName, worktree, name),
			WsTitle:  cmuxWsTitle(repoName, worktree, name),
			TabTitle: name,
			Cwd:      workspaceCwd(r, worktree),
		}
		return m.openSessionViaReconcile(e,
			fmt.Sprintf("%s/%s/%s/%s", serverName, repoName, worktree, name), serverName, taskID)
	}
}

// syncCmd runs the reconcile off the UI goroutine. `quiet` suppresses the
// "already in sync" note (used by the automatic startup pass, which
// shouldn't chatter when there's nothing to do).
// cmuxWorkspaceClosedCmd verifies a cmux-side workspace close and, when it
// holds up, deletes the session the way dd does (kill tmux + forget in the
// manifest). Guards, in order: cmux must still answer (a dying cmux mustn't
// mass-delete), the workspace must actually be gone, and the session must be
// tracked. cctl-initiated closes never get here as tracked — every internal
// path removes the manifest entry BEFORE closing the workspace.
func (m *tuiModel) cmuxWorkspaceClosedCmd(msg cmuxWorkspaceClosedMsg) tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		time.Sleep(1500 * time.Millisecond)
		cli := cmuxCLIPath()
		if cli == "" {
			return nil
		}
		wss := listCmuxWorkspaces(cli)
		if len(wss) == 0 {
			return nil // CLI failed / cmux shutting down — not user intent
		}
		for _, w := range wss {
			if msg.wsID != "" && w.id == msg.wsID {
				return nil // still exists — spurious
			}
		}
		var entry *wsEntry
		for _, e := range loadManifestEntries() {
			if (msg.wsID != "" && e.WsID == msg.wsID) ||
				(msg.title != "" && cmuxWsTitle(e.Repo, e.Worktree, e.Session) == msg.title) {
				ec := e
				entry = &ec
				break
			}
		}
		if entry == nil {
			return nil // untracked (or cctl removed it first) — nothing to do
		}
		srv, ok := cfg.Servers[entry.Server]
		if !ok {
			return nil
		}
		log().Info("cmux-close-dd", "ws", msg.title, "session", entry.TmuxName, "server", entry.Server)
		if _, err := runRemote(srv, fmt.Sprintf("tmux kill-session -t %s", shellQuote(entry.TmuxName))); err != nil {
			log().Debug("cmux-close-dd-kill-fail", "session", entry.TmuxName, "err", err.Error())
		}
		manifestRemove(entry.Server, entry.Repo, entry.Worktree, entry.Session)
		return actionDoneMsg{
			msg:     fmt.Sprintf("closed %s — session removed", cmuxWsTitle(entry.Repo, entry.Worktree, entry.Session)),
			refresh: entry.Server,
			taskID:  -1,
		}
	}
}

func (m *tuiModel) syncCmd(taskID int, quiet bool) tea.Cmd {
	return func() tea.Msg {
		res := syncCmuxState(m.cfg)
		log().Info("tui-sync", "restored", res.restored, "closed", res.closed,
			"renamed", res.migrated, "grouped", res.grouped, "adopted", res.adopted,
			"errs", res.errs)
		msg := res.summary()
		if !res.touched() {
			if quiet {
				msg = ""
			} else {
				msg = "cmux already matches cctl"
			}
		}
		// Refresh CONNECTED servers (fetch-only, no probe) so the tree shows
		// the sessions reconcile just revived/attached — without re-probing
		// (which would time out on unreachable hosts and loop into another sync).
		return actionDoneMsg{msg: msg, taskID: taskID, refreshConnected: true}
	}
}

func (m *tuiModel) upgradeClaudeCmd(serverName string, srv Server, sessions []string, taskID int) tea.Cmd {
	return func() tea.Msg {
		// Map sanitized tmux repo names back to real ones so resolve() finds
		// the worktree (e.g. "rxtx_dev" → "rxtx.dev").
		bySafe := map[string]string{}
		if allRepos, rerr := m.cfg.repos(serverName); rerr == nil {
			for n := range allRepos {
				bySafe[tmuxSafeName(n)] = n
			}
		}
		// Sessions on one server can resolve to different agents (different
		// repos), so the self-update is keyed on the resolved agent and run at
		// most once per agent. If an agent's update fails we skip respawning
		// that agent's sessions — same conservative intent as the original
		// "update failed → don't bounce sessions", scoped per agent.
		updated := map[string]bool{}
		updateErrs := map[string]string{}
		ensureUpdate := func(agent string) bool {
			if updated[agent] {
				return updateErrs[agent] == ""
			}
			updated[agent] = true
			cmd := m.cfg.agentUpdateCmd(agent)
			if out, err := runRemote(srv, agentUpdateScript(cmd)); err != nil {
				updateErrs[agent] = fmt.Sprintf("%s: %v — %s", cmd, err, abbrev(strings.TrimSpace(out), 200))
				log().Warn("tui-agent-update-fail", "server", serverName, "agent", agent, "err", err.Error())
				return false
			}
			return true
		}

		restarted, failed := 0, 0
		for _, name := range sessions {
			// Respawn with the CURRENT launch script (not the window's stale
			// original), so the restart picks up the new agent binary AND the
			// current flags/hooks. claude --continue / codex resume --last
			// resumes the conversation, so the session survives. Falls back to
			// re-running the original command when we can't resolve the
			// session's worktree.
			cmd := fmt.Sprintf("tmux respawn-window -k -t %s", shellQuote(name))
			if repo, wt, _, ok := parseTmuxName(name); ok {
				if real, found := bySafe[repo]; found {
					repo = real
				}
				if r, rerr := m.cfg.resolve(serverName, repo); rerr == nil {
					if !ensureUpdate(r.Agent) {
						// Update for this agent failed; leave the session as-is.
						failed++
						continue
					}
					cwd := r.Repo.Path
					if wt != "" && wt != "main" {
						cwd = worktreePath(r.WorktreeBase, r.RepoName, wt)
					}
					launch := agentLaunchScript(r, cwd, "", name)
					cmd = fmt.Sprintf("tmux respawn-window -k -t %s %s", shellQuote(name), shellQuote(launch))
				}
			}
			if _, err := runRemote(srv, cmd); err != nil {
				log().Warn("tui-respawn-fail", "server", serverName, "session", name, "err", err.Error())
				failed++
				continue
			}
			restarted++
		}
		msg := fmt.Sprintf("agents upgraded on %s; relaunched %d session(s)", serverName, restarted)
		if len(updateErrs) > 0 {
			parts := make([]string, 0, len(updateErrs))
			for agent, e := range updateErrs {
				parts = append(parts, fmt.Sprintf("%s (%s)", agent, e))
			}
			return actionDoneMsg{
				msg:     msg,
				err:     fmt.Errorf("agent update failed: %s", strings.Join(parts, "; ")),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		if failed > 0 {
			return actionDoneMsg{
				msg:     msg,
				err:     fmt.Errorf("%d session(s) failed to restart — see ~/.cctl.log", failed),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		return actionDoneMsg{msg: msg, refresh: serverName, taskID: taskID}
	}
}

func (m *tuiModel) attachCmd(serverName, repoName string, sess SessionInfo, taskID int) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		// Preserve the session's recorded agent (so re-recording the manifest
		// doesn't overwrite a per-session override with the config default);
		// fall back to the resolved agent for untracked sessions.
		if a := manifestAgent(serverName, repoName, sess.Worktree, sess.Session); a != "" {
			r.Agent = a
		}
		// Attaching = "make sure this session's workspace exists + focus it",
		// which is exactly what the reconcile converges to: a live session is
		// left alone (just focused), a detached one is revived. One path.
		e := wsEntry{
			Server:   serverName,
			Repo:     repoName,
			Worktree: sess.Worktree,
			Session:  sess.Session,
			TmuxName: sess.Name,
			WsTitle:  cmuxWsTitle(repoName, sess.Worktree, sess.Session),
			TabTitle: sess.Session,
			Cwd:      workspaceCwd(r, sess.Worktree),
			Agent:    r.Agent,
		}
		return m.openSessionViaReconcile(e,
			fmt.Sprintf("%s/%s/%s", serverName, repoName, sess.Session), serverName, taskID)
	}
}

func (m *tuiModel) newSessionPrepareCmd(serverName, repoName, worktree, sessionName, prompt, agent string, taskID int) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		log().Info("tui-new-session-prepare-start",
			"server", serverName, "repo", repoName, "wt", worktree, "session", sessionName, "agent", agent)
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			log().Error("tui-new-session-resolve-fail",
				"server", serverName, "repo", repoName, "err", err.Error())
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		// The form's agent choice overrides the config-resolved default for
		// this session; everything downstream (launch + manifest) uses it.
		if agent != "" {
			r.Agent = agent
		}
		// A branch-style worktree name ("feat/core") would break the slash-
		// delimited identity scheme (invisible session + eternal respawn-kill
		// loop); use the slug as the identity and the original as the branch.
		worktree, wtBranch := normalizeWorktree(worktree)
		// prepareAgent does the SETUP (create the worktree, resolve the branch).
		// Its returned launch command is intentionally ignored: the single
		// reconcile path rebuilds the launch from the manifest entry, so cmux
		// mutation happens in exactly one place (syncCmuxState). The initial
		// prompt is carried on the manifest entry and consumed by the first
		// reconcile spawn.
		if _, err := prepareAgent(r, worktree, sessionName, wtBranch, prompt, false, false); err != nil {
			log().Info("tui-new-session-prepare-done", "server", serverName, "session", sessionName,
				"dur", time.Since(start).String(), "err", errString(err))
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		// Record desired state, then converge cmux through the one reconcile
		// path (runs here, off the UI goroutine) and focus the new workspace.
		e := wsEntry{
			Server:   serverName,
			Repo:     repoName,
			Worktree: worktree,
			Session:  sessionName,
			TmuxName: tmuxName(repoName, worktree, sessionName),
			WsTitle:  cmuxWsTitle(repoName, worktree, sessionName),
			TabTitle: sessionName,
			Cwd:      workspaceCwd(r, worktree),
			Agent:    r.Agent,
			Prompt:   prompt,
		}
		log().Info("tui-new-session-prepare-done", "server", serverName, "session", sessionName,
			"dur", time.Since(start).String())
		return m.openSessionViaReconcile(e,
			fmt.Sprintf("%s/%s/%s/%s", serverName, repoName, worktree, sessionName), serverName, taskID)
	}
}

// openSessionViaReconcile is the ONE interactive "open a session" path: it
// records the session in the manifest (desired state), converges cmux through
// the single reconcile (syncCmuxState) — which spawns/heals the workspace,
// prunes stray tabs, and first-launches with any pending prompt — then focuses
// it. new-session, terminal, and attach all funnel through here, so no
// interactive action mutates cmux directly. Runs inside a tea.Cmd (off the UI
// goroutine), so the reconcile's ssh work doesn't block rendering.
func (m *tuiModel) openSessionViaReconcile(e wsEntry, label, refresh string, taskID int) prepareDoneMsg {
	// Converge ONLY this session, not the whole multi-server reconcile — a full
	// syncCmuxState here would make every Enter block on unreachable remotes
	// (10s/ssh). If the session is already live with a workspace, just focus it;
	// otherwise spawn it via the shared restoreSpawn primitive (idempotent, and
	// it consumes any first-launch prompt on the entry). Only the session's own
	// server is touched.
	up := sessionIsUp(m.cfg, e)
	if up {
		// Nothing will consume a first-launch prompt on an already-up session;
		// persisting it would fire a stale prompt when the session later dies
		// and reconcile revives it.
		e.Prompt = ""
	}
	manifestUpsertEntry(e)
	if !up {
		if err := restoreSpawn(m.cfg, e, true); err != nil {
			log().Warn("tui-open-spawn-fail", "ws", e.WsTitle, "err", err.Error())
			return prepareDoneMsg{err: err, label: label, refresh: refresh, taskID: taskID}
		}
		// A fresh workspace lands at the END of cmux's list, splitting its
		// repo's cluster in the sidebar until the next reconcile. Slot it into
		// name order now (cheap, local; no-op when already ordered).
		if m.cfg.orderWorkspaces() {
			orderCmuxWorkspacesByName(cmuxCLIPath())
		}
	}
	focusCmuxWorkspace(e.WsTitle)
	return prepareDoneMsg{label: label, refresh: refresh, taskID: taskID}
}

func (m *tuiModel) killCmd(serverName, repoName, tmuxFullName, worktreeName, session string, removeWorktree bool, taskID int) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return actionDoneMsg{err: err, taskID: taskID}
		}
		if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(tmuxFullName))); err != nil {
			return actionDoneMsg{err: fmt.Errorf("kill %s: %w", tmuxFullName, err), taskID: taskID}
		}
		// dd removes the session everywhere: forget it in the restore
		// manifest (so sync won't bring it back) and close its cmux workspace
		// so the window matches cctl. Best-effort — a missing one is fine.
		wsID := ""
		for _, e := range loadManifestEntries() {
			if e.Server == serverName && e.Repo == repoName && e.Worktree == worktreeName && e.Session == session {
				wsID = e.WsID
				break
			}
		}
		manifestRemove(serverName, repoName, worktreeName, session)
		closeCmuxWorkspaceByIDOrTitle(wsID, cmuxWsTitle(repoName, worktreeName, session))
		// "main" worktree is the original checkout — never delete it.
		if removeWorktree && worktreeName != "main" {
			wt := worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
			if _, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
				return actionDoneMsg{
					msg:     fmt.Sprintf("killed %s (worktree removal failed)", tmuxFullName),
					err:     fmt.Errorf("remove worktree %s: %w", wt, err),
					refresh: serverName,
					taskID:  taskID,
				}
			}
			return actionDoneMsg{
				msg:     fmt.Sprintf("killed %s + removed worktree", tmuxFullName),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		return actionDoneMsg{
			msg:     fmt.Sprintf("killed %s (worktree kept)", tmuxFullName),
			refresh: serverName,
			taskID:  taskID,
		}
	}
}

// killWorktreeCmd removes a whole worktree: kills every cctl-managed tmux
// session running on it, then runs `git worktree remove` (with --force +
// rm -rf fallbacks). The branch is intentionally kept (matching `cctl rm`
// behavior) so unpushed work isn't lost. Refuses to touch "main".
//
// `wtPath` is the actual on-disk path from `git worktree list`, when
// known (TUI flow). If empty we fall back to the cctl-convention path
// `<worktree_base>/<repo>/<worktree_name>` — the legacy behavior, which
// silently no-ops for worktrees that live outside that convention.
//
// `victims` is the list of tmux session names to kill first, snapshotted
// by the caller on the UI goroutine — this closure runs concurrently with
// the model and must not read m.state.
func (m *tuiModel) killWorktreeCmd(serverName, repoName, worktreeName, wtPath string, victims []string, taskID int) tea.Cmd {
	return func() tea.Msg {
		if worktreeName == "main" {
			return actionDoneMsg{err: fmt.Errorf("cannot remove the main worktree"), taskID: taskID}
		}
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return actionDoneMsg{err: err, taskID: taskID}
		}
		// Belt-and-braces: even if the row was misclassified by a
		// samePath regression, refuse to act when the on-disk path
		// equals the configured repo path. removeWorktreeScript also
		// guards this — three layers because the impact of getting
		// it wrong is "user loses their repo".
		if wtPath != "" && samePath(wtPath, r.Repo.Path) {
			log().Error("tui-kill-worktree-refused-main",
				"server", serverName, "repo", repoName, "wt", worktreeName,
				"path", wtPath, "repo_path", r.Repo.Path)
			return actionDoneMsg{err: fmt.Errorf(
				"refused: wtPath %s is the main checkout (repo.Path=%s) — the row was likely mis-classified",
				wtPath, r.Repo.Path,
			), taskID: taskID}
		}
		wt := wtPath
		if wt == "" {
			wt = worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
		}
		log().Info("tui-kill-worktree-start",
			"server", serverName, "repo", repoName, "wt", worktreeName,
			"path", wt, "sessions", len(victims))
		for _, name := range victims {
			if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(name))); err != nil {
				log().Warn("tui-kill-worktree-session-fail", "name", name, "err", err.Error())
			}
		}
		// The worktree's sessions are dead now, so their per-session cmux
		// workspaces are stale shells: forget the worktree in the manifest and
		// close every "repo/worktree/*" workspace so cmux matches cctl (done
		// before the git removal so it happens even if that fails).
		manifestRemoveWorktree(serverName, repoName, worktreeName)
		closeCmuxWorkspacesByPrefix(repoName + "/" + worktreeName + "/")
		if _, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
			return actionDoneMsg{
				msg:     fmt.Sprintf("killed %d session(s); worktree removal failed", len(victims)),
				err:     fmt.Errorf("remove worktree %s: %w", wt, err),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		log().Info("tui-kill-worktree-ok", "wt", wt, "sessions_killed", len(victims))
		return actionDoneMsg{
			msg:     fmt.Sprintf("removed %s (path=%s, killed %d session(s); branch kept)", worktreeName, wt, len(victims)),
			refresh: serverName,
			taskID:  taskID,
		}
	}
}

// ---- tree building ---------------------------------------------------------
