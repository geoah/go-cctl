package cctl

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runTUI loads the config and runs the interactive TUI. Returns when the user
// quits or an unrecoverable error occurs.
func runTUI() error {
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	log().Info("tui-start", "config", path, "servers", len(cfg.Servers))
	p := tea.NewProgram(newTUIModel(cfg), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		log().Error("tui-error", "err", err.Error())
		return err
	}
	log().Info("tui-stop")
	if fm, ok := final.(*tuiModel); ok && fm.exitErr != nil {
		return fm.exitErr
	}
	return nil
}

// ---- model -----------------------------------------------------------------

type rowKind int

const (
	rowServer rowKind = iota
	rowRepo
	rowWorktree
	rowSession
	rowEmpty
	rowLoading
)

// treeRow is one logical row of the tree. The interpretation of fields varies
// by kind: server-rows have only `server`, repo-rows add `repo`,
// worktree-rows add `worktree`, session-rows fill all of `server/repo/
// worktree/session`. Empty/loading rows carry a `note` for display.
type treeRow struct {
	kind     rowKind
	server   string
	repo     string
	worktree string
	session  *SessionInfo
	wt       *Worktree // populated for rowWorktree (and inherited where useful)
	note     string
	depth    int
}

type tuiMode int

const (
	modeBrowse tuiMode = iota
	modeNewForm
	modeConfirm
	modeFilter
)

// serverState bundles all data fetched for one server. Worktrees and sessions
// load in parallel; rendering tolerates partial state ("loading...").
type serverState struct {
	repos           map[string]Repo
	worktrees       map[string][]Worktree // repoName -> worktrees
	repoStatus      map[string]RepoStatus // repoName -> OK/MISSING/NO_GIT (from listAllWorktrees)
	sessions        []SessionInfo
	sessionErr      error
	repoErr         error
	wtErr           error
	sessionsLoaded  bool
	reposLoaded     bool
	worktreesLoaded bool

	// Connection lifecycle for remote servers. Local servers skip the
	// probe and stay in connConnected from start.
	conn         connState
	connAttempts int   // number of probes issued (including current/last)
	connErr      error // last probe failure
}

// connState is the reachability of a server's transport (ssh/mosh). It's
// independent of repo/session health — those can fail (missing repo,
// permission, etc.) on a server that's perfectly reachable; we don't want
// to flip "disconnected" because of that.
type connState int

const (
	connConnecting connState = iota
	connConnected
	connDisconnected
)

// statusKind is the phase of the status bar's state machine: idle (hidden),
// progress (spinner + label), success (✓ + label, auto-clears), failure
// (✗ + label + error, auto-clears slower so the user can read it).
type statusKind int

const (
	statusIdle statusKind = iota
	statusProgress
	statusSuccess
	statusFailure
)

// statusBar tracks the inline action indicator shown above the legend.
// One status at a time — newer events overwrite older ones (the typical
// flow is progress → success/failure → idle).
type statusBar struct {
	kind    statusKind
	label   string    // primary message ("spawning workspace/<repo>/<wt>")
	detail  string    // sub-line, usually the error text on failure
	clearAt time.Time // when to auto-revert to idle (success/failure only)
}

// spinnerFrames is a 10-step Braille spinner (industry standard, available
// in any monospace font). Frame index wraps mod len.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// tickMsg is the periodic beat that advances spinner animations and
// clears auto-expiring status states.
type tickMsg struct{}

// tickInterval is the spinner refresh rate. 100ms ≈ 10fps, smooth without
// burning CPU on terminals that re-render the whole frame.
const tickInterval = 100 * time.Millisecond

// needTick reports whether anything in the model is animating right now.
// Used to decide whether to schedule another tick after the current one.
func (m *tuiModel) needTick() bool {
	if m.status.kind == statusProgress {
		return true
	}
	if m.status.kind == statusSuccess || m.status.kind == statusFailure {
		if !m.status.clearAt.IsZero() && time.Now().Before(m.status.clearAt) {
			return true
		}
	}
	if m.preparingKey != "" {
		return true
	}
	for _, st := range m.state {
		if st == nil {
			continue
		}
		if st.conn == connConnecting {
			return true
		}
		if st.conn == connConnected && (!st.sessionsLoaded || !st.reposLoaded || !st.worktreesLoaded) {
			return true
		}
	}
	return false
}

// scheduleTick returns a tea.Cmd that ticks after tickInterval IF the
// model needs animation. Idempotent: if a tick is already pending the
// returned cmd is nil so we don't accumulate timers.
func (m *tuiModel) scheduleTick() tea.Cmd {
	if !m.needTick() {
		return nil
	}
	if m.tickPending {
		return nil
	}
	m.tickPending = true
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// setStatusProgress moves the status bar into progress mode with the
// given label and returns a tick cmd to start animating.
func (m *tuiModel) setStatusProgress(label string) tea.Cmd {
	m.status = statusBar{kind: statusProgress, label: label}
	return m.scheduleTick()
}

// setStatusSuccess flashes a green check + label and schedules auto-clear.
func (m *tuiModel) setStatusSuccess(label string) tea.Cmd {
	m.status = statusBar{kind: statusSuccess, label: label, clearAt: time.Now().Add(3 * time.Second)}
	return m.scheduleTick()
}

// setStatusFailure shows a red cross + label + detail and schedules
// auto-clear after a longer interval so the user can read the error.
func (m *tuiModel) setStatusFailure(label, detail string) tea.Cmd {
	m.status = statusBar{kind: statusFailure, label: label, detail: detail, clearAt: time.Now().Add(6 * time.Second)}
	return m.scheduleTick()
}

type tuiModel struct {
	cfg         *Config
	serverNames []string
	state       map[string]*serverState

	// expanded[key]=true means the user explicitly expanded it.
	// expandedDefault returns the default for a kind when no entry is present.
	expanded map[string]bool
	rows     []treeRow
	cursor   int

	width, height int

	mode      tuiMode
	statusMsg string
	statusErr string

	// status is the animated action-status state machine. Drives the line
	// above the legend: spinner while in progress, ✓/✗ with auto-clear
	// when done. See statusBar comments for transitions.
	status statusBar
	// spinnerFrame ticks while anything in the model is animating
	// (status.kind == statusProgress, status auto-clear pending, or any
	// server is connecting / still loading). Shared so every spinner in
	// the UI advances in lockstep.
	spinnerFrame int
	tickPending  bool
	// preparingKey is the rowKey of the row whose Enter action triggered
	// the in-flight prepare. While set, that row renders with a spinner
	// glyph on its left so the user can see exactly what's loading.
	// Cleared on prepareDoneMsg/actionDoneMsg.
	preparingKey string

	// new-session form
	formServer   string
	formRepo     string
	formWorktree string // if non-empty, worktree pre-filled (worktree-row flow)
	wtInput      textinput.Model
	nameInput    textinput.Model
	promptInput  textinput.Model
	formFocus    int // index into formFields (which depends on formWorktree presence)

	// confirm overlay
	confirmRow treeRow

	// k9s-style filter — when non-empty, only rows matching this substring
	// (or whose descendants match) are rendered.
	filter string

	// pendingD is true after the user pressed `d` once; the next key has
	// to also be `d` to confirm the delete (vim's dd flow). Any other key
	// cancels.
	pendingD bool

	exitErr error
}

func newTUIModel(cfg *Config) *tuiModel {
	names := cfg.serverNames()
	sort.Strings(names)
	state := map[string]*serverState{}
	for _, n := range names {
		state[n] = &serverState{}
	}
	mkInput := func(placeholder string, charLimit int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = charLimit
		ti.Prompt = ""
		return ti
	}
	m := &tuiModel{
		cfg:         cfg,
		serverNames: names,
		state:       state,
		expanded:    map[string]bool{},
		wtInput:     mkInput("worktree-name (e.g. audit)", 64),
		nameInput:   mkInput("session-name (e.g. quickcheck)", 64),
		promptInput: mkInput("initial prompt (optional)", 800),
	}
	// Initial expansion state is derived from the data — see isExpanded.
	// Anything with a running session expands automatically once data
	// arrives; users can override with ←/→.
	m.rebuildRows()
	return m
}

// isExpanded looks up an expansion key with kind-specific defaults. The
// guiding principle: if the subtree under a container has at least one
// session, the container expands by default — the user sees every running
// session at startup without manually drilling in. Explicit user toggles
// (left/right) override these defaults.
//
//   - filter active     → everything reads as expanded so the row-filter
//     pass can find matches; the visible tree is then
//     pruned to matches+ancestors.
//   - server            → expanded by default (top-level scaffolding).
//   - repo              → expanded if any session in any of its worktrees.
//   - worktree          → expanded if any session is running on it.
//
// Servers/repos/worktrees with no sessions stay collapsed (▶ or • glyph),
// matching the "show what matters first" k9s-ish vibe.
func (m *tuiModel) isExpanded(row treeRow) bool {
	if m.filter != "" {
		return true
	}
	if v, ok := m.expanded[keyForRow(row)]; ok {
		return v
	}
	switch row.kind {
	case rowServer:
		return true
	case rowRepo:
		st := m.state[row.server]
		if st == nil {
			return false
		}
		for _, sess := range st.sessions {
			if sess.Repo == row.repo {
				return true
			}
		}
		return false
	case rowWorktree:
		st := m.state[row.server]
		if st == nil {
			return false
		}
		for _, sess := range st.sessions {
			if sess.Repo == row.repo && sess.Worktree == row.worktree {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// keyForRow returns the canonical expansion key for a row.
func keyForRow(r treeRow) string {
	switch r.kind {
	case rowServer:
		return "server:" + r.server
	case rowRepo:
		return "repo:" + r.server + "/" + r.repo
	case rowWorktree:
		return "wt:" + r.server + "/" + r.repo + "/" + r.worktree
	}
	return ""
}

// rowKey is keyForRow extended to include sessions, used by the
// per-row preparing-spinner. (keyForRow stops at rowWorktree because
// it's used by m.expanded which only matters for container rows.)
func rowKey(r treeRow) string {
	if r.kind == rowSession {
		return "sess:" + r.server + "/" + r.repo + "/" + r.session.Worktree + "/" + r.session.Session
	}
	return keyForRow(r)
}

// ---- messages --------------------------------------------------------------

type loadedSessionsMsg struct {
	server   string
	sessions []SessionInfo
	err      error
}

type loadedReposMsg struct {
	server string
	repos  map[string]Repo
	err    error
}

type loadedWorktreesMsg struct {
	server    string
	worktrees map[string][]Worktree
	statuses  map[string]RepoStatus
	err       error
}

// probedMsg is the result of a reachability probe (cheap `true` over the
// transport). On success the model fans out the heavier sessions/repos/
// worktrees fetches. On failure the model marks the server disconnected
// and schedules a retry.
type probedMsg struct {
	server string
	err    error
}

// retryProbeMsg fires when the backoff timer elapses for a disconnected
// server. The handler re-issues loadServerCmd for that server only.
type retryProbeMsg struct {
	server string
}

type prepareDoneMsg struct {
	server  Server
	useMosh bool
	cmdStr  string
	cwd     string // suggested workspace cwd (used by cmuxSpawner)
	title   string // human label for the new tab/workspace (cmux rename-workspace)
	label   string
	refresh string
	err     error
}

type actionDoneMsg struct {
	msg     string
	refresh string
	err     error
}

// refreshServerMsg triggers a one-server reload, used as a follow-up after
// spawning a session in a new terminal window (so the tree picks it up).
type refreshServerMsg struct {
	server string
}

// spawnInNewWindow launches an interactive cmd in a new tab (or window,
// depending on the terminal) using the configured/auto-detected Spawner.
// Fire-and-forget — TUI stays alive. Returns the provider name + an error
// if the spawn failed. The previous inline-fallback path was removed when
// cctl committed to "Enter always opens in a new tab".
//
// We always write a tiny wrapper script first so each Spawner only ever
// has to hand its terminal one filesystem path; this dodges all the
// arg-quoting/splitting pitfalls.
func spawnInNewWindow(cfg *Config, s Server, useMosh bool, cmdStr, cwd, title string) (string, error) {
	inner, err := interactiveCmd(s, useMosh, cmdStr)
	if err != nil {
		return "", fmt.Errorf("build interactive cmd: %w", err)
	}
	script, err := writeSpawnScript(inner)
	if err != nil {
		return "", err
	}
	pref := ""
	if cfg != nil {
		pref = cfg.Defaults.Spawn
	}
	sp, reason := detectSpawner(pref)
	log().Debug("tui-spawn", "provider", sp.Name(), "reason", reason, "script", script, "cwd", cwd, "title", title)
	if err := sp.Spawn(script, cwd, title); err != nil {
		os.Remove(script)
		return sp.Name(), err
	}
	return sp.Name(), nil
}

// writeSpawnScript materializes a small shell script that exec's the
// interactive command. ghostty (or any other spawner) just sees a single
// path and there's no risk of arg-splitting going wrong. Old scripts are
// not auto-deleted — they're tiny and live in $TMPDIR, which macOS rotates.
func writeSpawnScript(inner *exec.Cmd) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# cctl-spawn — autogenerated wrapper for a single cctl session.\n")
	b.WriteString("exec")
	b.WriteString(" " + shellQuote(inner.Path))
	for _, a := range inner.Args[1:] {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")

	f, err := os.CreateTemp("", "cctl-spawn-*.sh")
	if err != nil {
		return "", fmt.Errorf("create wrapper: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write wrapper: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close wrapper: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("chmod wrapper: %w", err)
	}
	return f.Name(), nil
}

// ---- bubbletea lifecycle ---------------------------------------------------

func (m *tuiModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, n := range m.serverNames {
		cmds = append(cmds, m.loadServerCmd(n)...)
	}
	// Servers start in connConnecting so the model is animating from
	// turn 0; schedule the first tick to drive their spinners.
	if c := m.scheduleTick(); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

// loadServerCmd issues a reachability probe first, then (once connected)
// fans out the three heavy fetches: sessions, repos+discovery, and
// worktrees-for-all-repos. Local servers skip the probe because there's
// no transport that can fail.
//
// The model handles `probedMsg`: on success it triggers the fan-out; on
// failure it marks the server disconnected and schedules a retry.
func (m *tuiModel) loadServerCmd(serverName string) []tea.Cmd {
	srv := m.cfg.Servers[serverName]
	st := m.state[serverName]
	if st != nil {
		wasLoaded := st.conn == connConnected && st.sessionsLoaded && st.reposLoaded && st.worktreesLoaded
		st.conn = connConnecting
		st.connAttempts++
		// On a fresh or post-disconnect load we want rebuildRows to
		// show a "connecting…" placeholder instead of stale data, so
		// clear the loaded flags. But on a REFRESH of an already-
		// connected server, keep the data + flags so the user sees
		// the existing tree while we update; rows just appear dimmed
		// via the refreshing-render path until new data arrives.
		if !wasLoaded {
			st.sessionsLoaded = false
			st.reposLoaded = false
			st.worktreesLoaded = false
		}
	}
	if srv.Local {
		// Local servers can't transport-fail; emit synthetic success
		// + the heavy fetches directly.
		return append([]tea.Cmd{
			func() tea.Msg { return probedMsg{server: serverName, err: nil} },
		}, m.fetchServerCmd(serverName)...)
	}
	return []tea.Cmd{
		func() tea.Msg {
			start := time.Now()
			// Cheap reachability check. Don't reuse runRemote here so
			// stderr never echoes to the terminal — just want a yes/no.
			_, err := runRemote(srv, "true")
			log().Debug("tui-probe",
				"server", serverName, "dur", time.Since(start).String(),
				"err", errString(err))
			return probedMsg{server: serverName, err: err}
		},
	}
}

// fetchServerCmd performs the three heavy loads. Split out from
// loadServerCmd so probedMsg can trigger it once we know the box is up.
func (m *tuiModel) fetchServerCmd(serverName string) []tea.Cmd {
	srv := m.cfg.Servers[serverName]
	return []tea.Cmd{
		func() tea.Msg {
			start := time.Now()
			sessions, err := listSessions(serverName, srv)
			log().Debug("tui-load-sessions",
				"server", serverName, "count", len(sessions),
				"dur", time.Since(start).String(), "err", errString(err))
			return loadedSessionsMsg{server: serverName, sessions: sessions, err: err}
		},
		func() tea.Msg {
			start := time.Now()
			merged := mergeRepos(srv)
			discovered, derr := discoverRepos(srv)
			for k, v := range discovered {
				merged[k] = v
			}
			log().Debug("tui-load-repos",
				"server", serverName, "discovered", len(discovered),
				"explicit", len(srv.Repos), "total", len(merged),
				"dur", time.Since(start).String(), "err", errString(derr))
			return loadedReposMsg{server: serverName, repos: merged, err: derr}
		},
		func() tea.Msg {
			start := time.Now()
			// Re-merge here so worktrees pick up the same set as the repos
			// fetch; sequencing it via loadedReposMsg would force us to issue
			// two SSH round-trips in series.
			merged := mergeRepos(srv)
			discovered, _ := discoverRepos(srv)
			for k, v := range discovered {
				merged[k] = v
			}
			wts, statuses, err := listAllWorktrees(srv, merged)
			log().Debug("tui-load-worktrees",
				"server", serverName, "repos", len(merged),
				"dur", time.Since(start).String(), "err", errString(err),
				"statuses", statuses)
			return loadedWorktreesMsg{server: serverName, worktrees: wts, statuses: statuses, err: err}
		},
	}
}

// mergeRepos returns the explicit repos map (copy) so callers can layer
// discovery on top without mutating the original.
func mergeRepos(srv Server) map[string]Repo {
	out := map[string]Repo{}
	for k, v := range srv.Repos {
		out[k] = v
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// preparingPrefix returns an amber-bold spinner glyph + space when the
// given row matches m.preparingKey (i.e. its Enter triggered an in-flight
// prepare). Returns "" otherwise so the column doesn't shift.
func (m *tuiModel) preparingPrefix(r treeRow) string {
	if m.preparingKey == "" {
		return ""
	}
	if rowKey(r) != m.preparingKey {
		return ""
	}
	return warnStyle.Bold(true).Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " "
}

// canDelete reports whether dd is a valid action on the given row. Server
// and repo rows are never deletable (cctl's job is sessions + worktrees);
// the synthetic "main" worktree is also off-limits (it's the repo's
// primary checkout). Used by handleBrowseKey's "d" arm to skip the
// "press d again…" prompt when the second `d` would just refuse.
func canDelete(r treeRow) bool {
	switch r.kind {
	case rowSession:
		return true
	case rowWorktree:
		return r.worktree != "main"
	}
	return false
}

// probeRetryDelay returns how long to wait before the next reachability
// probe, given how many attempts have already been issued. Backoff caps
// quickly so a flapping host shows up-to-date status within ~a minute
// without hammering ssh.
//
// Sequence (attempts → delay):
//
//	1 → 2s
//	2 → 5s
//	3 → 10s
//	4 → 30s
//	5+ → 60s
func probeRetryDelay(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 2 * time.Second
	case attempts == 2:
		return 5 * time.Second
	case attempts == 3:
		return 10 * time.Second
	case attempts == 4:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}

func (m *tuiModel) refreshAllCmd() tea.Cmd {
	var cmds []tea.Cmd
	for _, n := range m.serverNames {
		cmds = append(cmds, m.loadServerCmd(n)...)
	}
	return tea.Batch(cmds...)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case loadedSessionsMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		st.sessions = msg.sessions
		st.sessionErr = msg.err
		st.sessionsLoaded = true
		m.rebuildRows()
		return m, nil

	case loadedReposMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		st.repos = msg.repos
		st.repoErr = msg.err
		st.reposLoaded = true
		m.rebuildRows()
		return m, nil

	case loadedWorktreesMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		st.worktrees = msg.worktrees
		st.repoStatus = msg.statuses
		st.wtErr = msg.err
		st.worktreesLoaded = true
		m.rebuildRows()
		return m, nil

	case probedMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		if msg.err == nil {
			st.conn = connConnected
			st.connErr = nil
			// Reset attempts on success so the next failure starts
			// backoff from scratch.
			st.connAttempts = 0
			m.rebuildRows()
			return m, tea.Batch(m.fetchServerCmd(msg.server)...)
		}
		st.conn = connDisconnected
		st.connErr = msg.err
		log().Warn("tui-probe-fail",
			"server", msg.server, "attempt", st.connAttempts,
			"err", msg.err.Error())
		m.rebuildRows()
		// Schedule a retry. The backoff caps quickly so a flapping
		// host doesn't end up in a once-an-hour state.
		server := msg.server
		delay := probeRetryDelay(st.connAttempts)
		return m, tea.Tick(delay, func(time.Time) tea.Msg {
			return retryProbeMsg{server: server}
		})

	case retryProbeMsg:
		st := m.state[msg.server]
		if st == nil || st.conn == connConnected {
			return m, nil
		}
		cmds := m.loadServerCmd(msg.server)
		if c := m.scheduleTick(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case tickMsg:
		m.tickPending = false
		// Advance the shared spinner frame so every spinner in the UI
		// (status bar, server rows) draws the same braille glyph.
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		// Auto-clear success/failure once their clearAt has passed.
		if m.status.kind == statusSuccess || m.status.kind == statusFailure {
			if !m.status.clearAt.IsZero() && time.Now().After(m.status.clearAt) {
				m.status = statusBar{}
			}
		}
		return m, m.scheduleTick()

	case prepareDoneMsg:
		// Prepare has finished. Clear the per-row spinner now; the
		// status bar carries success/failure for the spawn step.
		m.preparingKey = ""
		if msg.err != nil {
			return m, m.setStatusFailure(msg.label+": prepare failed", msg.err.Error())
		}
		progress := m.setStatusProgress("opening " + msg.label + " (cmux tab)…")
		provider, err := spawnInNewWindow(m.cfg, msg.server, msg.useMosh, msg.cmdStr, msg.cwd, msg.title)
		if err != nil {
			log().Warn("tui-spawn-fail", "provider", provider, "err", err.Error())
			return m, m.setStatusFailure(provider+" spawn failed", err.Error())
		}
		log().Info("tui-spawn-ok", "label", msg.label, "provider", provider)
		success := m.setStatusSuccess(fmt.Sprintf("opened %s (%s tab)", msg.label, provider))
		refresh := msg.refresh
		_ = progress
		return m, tea.Batch(success,
			tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
				return refreshServerMsg{server: refresh}
			}),
		)

	case refreshServerMsg:
		var cmds []tea.Cmd
		if msg.server == "" {
			cmds = append(cmds, m.refreshAllCmd())
		} else {
			cmds = append(cmds, m.loadServerCmd(msg.server)...)
		}
		if c := m.scheduleTick(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case actionDoneMsg:
		m.mode = modeBrowse
		var statusCmd tea.Cmd
		if msg.err != nil {
			label := msg.msg
			if label == "" {
				label = "action failed"
			}
			statusCmd = m.setStatusFailure(label, msg.err.Error())
		} else if msg.msg != "" {
			statusCmd = m.setStatusSuccess(msg.msg)
		}
		if msg.refresh != "" {
			cmds := m.loadServerCmd(msg.refresh)
			if statusCmd != nil {
				cmds = append(cmds, statusCmd)
			}
			return m, tea.Batch(cmds...)
		}
		return m, statusCmd

	case tea.KeyMsg:
		switch m.mode {
		case modeBrowse:
			return m.handleBrowseKey(msg)
		case modeNewForm:
			return m.handleFormKey(msg)
		case modeConfirm:
			return m.handleConfirmKey(msg)
		case modeFilter:
			return m.handleFilterKey(msg)
		}
	}
	return m, nil
}

// ---- key handling ----------------------------------------------------------

func (m *tuiModel) handleBrowseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
		}
	case "h", "left":
		m.clearPendingD()
		m.collapseCurrent()
	case "l", "right":
		m.clearPendingD()
		m.expandCurrent()
	case "enter", " ", "shift+enter", "ctrl+enter", "alt+enter":
		m.clearPendingD()
		// Enter (and any modifier variant) opens the row in a new cmux
		// tab. The inline / in-place mode was removed — cctl is built
		// around cmux's workspace UI and a separate path was just an
		// extra config knob people had to learn.
		return m.activateRow()
	case "n":
		m.clearPendingD()
		return m.startNewForm()
	case "d":
		// vim-style dd: first d arms (red footer prompt is the
		// confirmation). second d deletes outright — no extra modal.
		//
		// On a (s) session we keep the worktree (safer; the branch may
		// hold unpushed work). On a (w) worktree row we kill any
		// sessions on it and remove the worktree, same as before.
		if m.pendingD {
			m.pendingD = false
			m.statusErr = ""
			m.statusMsg = ""
			return m.executeDelete()
		}
		// If the row can't be deleted (server / repo / "main" worktree),
		// don't even arm the prompt — show the refusal immediately so the
		// user isn't led to believe a second `d` will do something.
		if m.cursor < len(m.rows) && !canDelete(m.rows[m.cursor]) {
			return m.executeDelete()
		}
		m.pendingD = true
		m.statusErr = "" // hide any earlier error so the red prompt below stands alone
		m.statusMsg = ""
		return m, nil
	case "r":
		m.clearPendingD()
		m.statusErr = ""
		log().Info("tui-refresh-all")
		return m, m.refreshAllCmd()
	case "/":
		m.clearPendingD()
		m.mode = modeFilter
		m.statusErr = ""
		log().Debug("tui-filter-start")
		return m, nil
	case "esc":
		// in browse mode, esc clears any active filter or pending dd
		if m.pendingD {
			m.pendingD = false
		}
		if m.filter != "" {
			log().Debug("tui-filter-clear")
			m.filter = ""
			m.rebuildRows()
		}
	case "?":
		m.statusMsg = "↑↓ move · ←→ collapse/expand · ⏎ attach · n new · dd delete · / filter · r refresh · q quit"
	default:
		// any other key cancels a pending dd so the next d starts fresh
		m.clearPendingD()
	}
	return m, nil
}

// clearPendingD resets the dd arming state. Safe to call unconditionally.
func (m *tuiModel) clearPendingD() {
	if m.pendingD {
		m.pendingD = false
	}
}

func (m *tuiModel) formFields() []*textinput.Model {
	if m.formWorktree == "" {
		// flow: pick worktree (or "main" / new), then session, then prompt
		return []*textinput.Model{&m.wtInput, &m.nameInput, &m.promptInput}
	}
	// flow: worktree pre-selected, only session + prompt
	return []*textinput.Model{&m.nameInput, &m.promptInput}
}

func (m *tuiModel) handleFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	fields := m.formFields()
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = modeBrowse
		for _, f := range fields {
			f.Blur()
		}
		return m, nil
	case "tab":
		m.formFocus = (m.formFocus + 1) % len(fields)
		for i, f := range fields {
			if i == m.formFocus {
				f.Focus()
			} else {
				f.Blur()
			}
		}
		return m, nil
	case "shift+tab":
		m.formFocus = (m.formFocus - 1 + len(fields)) % len(fields)
		for i, f := range fields {
			if i == m.formFocus {
				f.Focus()
			} else {
				f.Blur()
			}
		}
		return m, nil
	case "enter", "shift+enter", "ctrl+enter", "alt+enter":
		wt := m.formWorktree
		if wt == "" {
			wt = strings.TrimSpace(m.wtInput.Value())
		}
		name := strings.TrimSpace(m.nameInput.Value())
		if wt == "" {
			log().Info("tui-form-submit-empty-worktree")
			m.statusErr = "worktree is required (use 'main' for the primary checkout)"
			return m, nil
		}
		if name == "" {
			log().Info("tui-form-submit-empty-name")
			m.statusErr = "session name is required"
			return m, nil
		}
		prompt := strings.TrimSpace(m.promptInput.Value())
		server, repo := m.formServer, m.formRepo
		log().Info("tui-form-submit",
			"server", server, "repo", repo, "wt", wt, "session", name,
			"prompt_len", len(prompt))
		m.mode = modeBrowse
		for _, f := range fields {
			f.Blur()
		}
		m.statusErr = ""
		// Mark the source row (the row the user pressed Enter on to open
		// the form) so its render adds a spinner on the left. The form
		// captured the row's identity into formServer/formRepo/formWorktree
		// on open — if formWorktree was empty the source was a repo row,
		// otherwise a worktree row.
		if m.formWorktree == "" {
			m.preparingKey = "repo:" + server + "/" + repo
		} else {
			m.preparingKey = "wt:" + server + "/" + repo + "/" + m.formWorktree
		}
		progress := m.setStatusProgress(fmt.Sprintf("preparing %s/%s/%s/%s…", server, repo, wt, name))
		return m, tea.Batch(progress, m.newSessionPrepareCmd(server, repo, wt, name, prompt))
	}
	var cmd tea.Cmd
	*fields[m.formFocus], cmd = fields[m.formFocus].Update(msg)
	return m, cmd
}

func (m *tuiModel) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "n", "ctrl+c":
		m.mode = modeBrowse
		return m, nil
	case "y", "enter":
		row := m.confirmRow
		m.mode = modeBrowse
		switch row.kind {
		case rowSession:
			removeWT := row.note == "with-worktree"
			return m, m.killCmd(row.server, row.repo, row.session.Session, row.session.Name, row.session.Worktree, removeWT)
		case rowWorktree:
			var wtPath string
			if row.wt != nil {
				wtPath = row.wt.Path
			}
			return m, m.killWorktreeCmd(row.server, row.repo, row.worktree, wtPath)
		}
	}
	return m, nil
}

// handleFilterKey edits m.filter live (each keystroke triggers rebuildRows so
// the tree updates as you type). Enter commits the filter (returns to browse
// with it still applied); Esc clears + returns; Ctrl-u also clears.
func (m *tuiModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filter = ""
		m.mode = modeBrowse
		m.rebuildRows()
		return m, nil
	case "enter":
		m.mode = modeBrowse
		return m, nil
	case "ctrl+u":
		m.filter = ""
		m.rebuildRows()
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "backspace":
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
			m.rebuildRows()
		}
		return m, nil
	}
	// Treat any printable runes as filter input.
	if len(msg.Runes) > 0 {
		m.filter += string(msg.Runes)
		m.rebuildRows()
	}
	return m, nil
}

// ---- actions ---------------------------------------------------------------

// activateRow handles Enter (and modifier variants — all behave the same
// now). For repo/worktree rows it opens the new-session form. For session
// rows it goes straight to attach. Every path spawns in a new cmux tab.
func (m *tuiModel) activateRow() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	switch row.kind {
	case rowServer:
		m.toggleRow(row)
	case rowRepo, rowWorktree:
		return m.startNewForm()
	case rowSession:
		m.preparingKey = rowKey(row)
		progress := m.setStatusProgress(fmt.Sprintf("preparing %s/%s/%s/%s…",
			row.server, row.repo, row.session.Worktree, row.session.Session))
		return m, tea.Batch(progress, m.attachCmd(row.server, row.repo, row.session.Session, row.session.Name, row.session.Worktree))
	}
	return m, nil
}

func (m *tuiModel) toggleRow(row treeRow) {
	key := keyForRow(row)
	if key == "" {
		return
	}
	// Flip relative to the row's current effective expansion state; this
	// preserves the smart-default for worktrees (if it was implicitly
	// expanded, the first toggle collapses it explicitly, and vice versa).
	m.expanded[key] = !m.isExpanded(row)
	m.rebuildRows()
}

// collapseCurrent implements the "climbing" left-arrow behavior:
//
//   - on an expanded container row → collapse it in place;
//   - on a collapsed container OR a leaf row → climb to the parent and
//     collapse that, moving the cursor to the parent so further ← keeps
//     climbing.
//
// This matches the user's mental model of "press left until you reach
// something useful": one ← from a session takes you to its worktree (now
// collapsed), the next takes you to the repo, etc.
func (m *tuiModel) collapseCurrent() {
	if m.cursor >= len(m.rows) {
		return
	}
	row := m.rows[m.cursor]
	if isContainerKind(row.kind) && m.isExpanded(row) {
		m.expanded[keyForRow(row)] = false
		m.rebuildRows()
		return
	}
	parent, ok := m.parentRow(row)
	if !ok {
		return
	}
	if isContainerKind(parent.kind) {
		m.expanded[keyForRow(parent)] = false
	}
	m.rebuildRows()
	m.moveCursorTo(parent)
}

// expandCurrent expands the current row if it's a container with children.
// Empty containers (the `•` glyph) are no-ops since there's nothing to show.
func (m *tuiModel) expandCurrent() {
	if m.cursor >= len(m.rows) {
		return
	}
	row := m.rows[m.cursor]
	if !isContainerKind(row.kind) {
		return
	}
	if !m.hasChildren(row) {
		return
	}
	m.expanded[keyForRow(row)] = true
	m.rebuildRows()
}

func isContainerKind(k rowKind) bool {
	return k == rowServer || k == rowRepo || k == rowWorktree
}

// hasChildren reports whether expanding a container would surface any rows.
// A repo with no worktrees still has the synthetic "main" entry, so repos
// always claim to have children; worktrees claim children only when at
// least one session exists under them; servers when at least one repo is
// known.
func (m *tuiModel) hasChildren(r treeRow) bool {
	st := m.state[r.server]
	if st == nil {
		return false
	}
	switch r.kind {
	case rowServer:
		// Disconnected servers have nothing to show beneath them.
		if st.conn == connDisconnected {
			return false
		}
		// While we haven't heard back from repo/session discovery yet,
		// assume there's something to expand so the user can drill in
		// without the glyph briefly flickering to •.
		if !st.reposLoaded && !st.sessionsLoaded {
			return true
		}
		if len(st.repos) > 0 {
			return true
		}
		for _, sess := range st.sessions {
			if sess.Repo != "" {
				return true
			}
		}
		return false
	case rowRepo:
		return true
	case rowWorktree:
		for _, sess := range st.sessions {
			if sess.Repo == r.repo && sess.Worktree == r.worktree {
				return true
			}
		}
		return false
	}
	return false
}

// parentRow returns the ancestor of `r` (one level up). For sessions it's
// the worktree, for worktrees the repo, for repos the server. Returns
// ok=false when there's nothing above (a server row, or a row with empty
// identifiers).
func (m *tuiModel) parentRow(r treeRow) (treeRow, bool) {
	switch r.kind {
	case rowSession, rowEmpty, rowLoading:
		if r.worktree != "" {
			return treeRow{kind: rowWorktree, server: r.server, repo: r.repo, worktree: r.worktree}, true
		}
		if r.repo != "" {
			return treeRow{kind: rowRepo, server: r.server, repo: r.repo}, true
		}
		if r.server != "" {
			return treeRow{kind: rowServer, server: r.server}, true
		}
	case rowWorktree:
		return treeRow{kind: rowRepo, server: r.server, repo: r.repo}, true
	case rowRepo:
		return treeRow{kind: rowServer, server: r.server}, true
	}
	return treeRow{}, false
}

// moveCursorTo points the cursor at the row matching target's identifiers
// (kind + server/repo/worktree). No-op if not found.
func (m *tuiModel) moveCursorTo(target treeRow) {
	for i, r := range m.rows {
		if r.kind == target.kind &&
			r.server == target.server &&
			r.repo == target.repo &&
			r.worktree == target.worktree {
			m.cursor = i
			return
		}
	}
}

func (m *tuiModel) startNewForm() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	server, repo, worktree := row.server, row.repo, row.worktree
	if server == "" {
		m.statusErr = "select a server, repo, worktree, or session row"
		return m, nil
	}
	if repo == "" {
		st := m.state[server]
		if st != nil && len(st.repos) == 1 {
			for n := range st.repos {
				repo = n
			}
		}
		if repo == "" {
			m.statusErr = "expand a repo (and a worktree if you have one) and try again"
			return m, nil
		}
	}
	// rowSession's worktree is set; rowEmpty/loading under a worktree have it
	// too. rowRepo doesn't — leave worktree empty so the form asks for one.
	m.formServer = server
	m.formRepo = repo
	m.formWorktree = worktree
	m.wtInput.SetValue("")
	m.nameInput.SetValue("")
	m.promptInput.SetValue("")
	m.formFocus = 0
	fields := m.formFields()
	for i, f := range fields {
		if i == 0 {
			f.Focus()
		} else {
			f.Blur()
		}
	}
	m.mode = modeNewForm
	m.statusErr = ""
	return m, nil
}

// executeDelete is the vim-dd action: fires the kill straight away with no
// extra modal — the red "press d again" prompt was the confirmation. On a
// session row we keep the worktree (the branch is the user's work); on a
// worktree row we kill any sessions on it and `git worktree remove`. The
// synthetic "main" worktree is refused.
func (m *tuiModel) executeDelete() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	switch row.kind {
	case rowSession:
		log().Info("tui-dd-session",
			"server", row.server, "repo", row.repo,
			"wt", row.session.Worktree, "session", row.session.Session)
		return m, m.killCmd(row.server, row.repo, row.session.Session, row.session.Name, row.session.Worktree, false)
	case rowRepo:
		// Repos are deliberately not deletable from cctl. The post-incident
		// rule is: cctl can only delete things INSIDE the worktree_base.
		// Repos live outside it (they're the user's source of truth). Show
		// an explicit, visible refusal instead of silently doing nothing.
		log().Info("tui-dd-repo-refused", "server", row.server, "repo", row.repo)
		return m, m.setStatusFailure("can't delete a repo from cctl",
			"cctl only deletes worktrees and sessions; the repo dir is yours to manage (rm or git clone manually)")
	case rowServer:
		log().Info("tui-dd-server-refused", "server", row.server)
		return m, m.setStatusFailure("can't delete a server",
			"servers are config entries; edit ~/.cctl.yaml to remove one")
	case rowWorktree:
		if row.worktree == "main" {
			return m, m.setStatusFailure("can't delete the main worktree",
				"that's the repo's primary checkout; cctl will never remove it")
		}
		// Pass the *actual* on-disk path from `git worktree list` (not the
		// cctl-convention path) so we delete the worktree the user can see
		// in the DETAIL column, even if it lives outside ~/worktrees/<repo>/.
		var wtPath string
		if row.wt != nil {
			wtPath = row.wt.Path
		}
		log().Info("tui-dd-worktree",
			"server", row.server, "repo", row.repo, "wt", row.worktree, "path", wtPath)
		return m, m.killWorktreeCmd(row.server, row.repo, row.worktree, wtPath)
	default:
		return m, m.setStatusFailure("nothing to delete on this row", "select a session or worktree")
	}
}

// startKillConfirm is the older modal-based delete flow (kept around for
// completeness but no longer triggered by any keybinding — dd routes
// straight through executeDelete instead). Left in case we want a
// confirmation prompt later.
func (m *tuiModel) startKillConfirm(withWorktree bool) (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	switch row.kind {
	case rowSession:
		m.confirmRow = row
		if withWorktree {
			m.confirmRow.note = "with-worktree"
		} else {
			m.confirmRow.note = "keep-worktree"
		}
		m.mode = modeConfirm
	case rowWorktree:
		if row.worktree == "main" {
			m.statusErr = "cannot remove the main worktree (it's the repo's primary checkout)"
			return m, nil
		}
		m.confirmRow = row
		m.confirmRow.note = "wt-remove"
		m.mode = modeConfirm
	default:
		m.statusErr = "select a session or worktree to delete"
	}
	return m, nil
}

func (m *tuiModel) attachCmd(serverName, repoName, sessionName, tmuxFullName, worktreeName string) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return prepareDoneMsg{err: err}
		}
		// attachOrRespawn (not bare `tmux attach`): cmux re-runs the wrapper
		// script when restoring the workspace, possibly long after the
		// session died — see the helper's doc comment.
		cwd := r.Repo.Path
		if worktreeName != "" && worktreeName != "main" {
			cwd = worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
		}
		return prepareDoneMsg{
			server:  r.Server,
			useMosh: r.UseMosh,
			cmdStr:  attachOrRespawn(r, tmuxFullName, cwd),
			cwd:     workspaceCwd(r, worktreeName),
			title:   fmt.Sprintf("%s/%s/%s", repoName, worktreeName, sessionName),
			label:   fmt.Sprintf("%s/%s/%s", serverName, repoName, sessionName),
			refresh: serverName,
		}
	}
}

func (m *tuiModel) newSessionPrepareCmd(serverName, repoName, worktree, sessionName, prompt string) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		log().Info("tui-new-session-prepare-start",
			"server", serverName, "repo", repoName, "wt", worktree, "session", sessionName)
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			log().Error("tui-new-session-resolve-fail",
				"server", serverName, "repo", repoName, "err", err.Error())
			return prepareDoneMsg{err: err}
		}
		cmdStr, err := prepareClaude(r, worktree, sessionName, "", prompt, false, false)
		log().Info("tui-new-session-prepare-done",
			"server", serverName, "session", sessionName,
			"dur", time.Since(start).String(), "err", errString(err))
		if err != nil {
			return prepareDoneMsg{err: err}
		}
		return prepareDoneMsg{
			server:  r.Server,
			useMosh: r.UseMosh,
			cmdStr:  cmdStr,
			cwd:     workspaceCwd(r, worktree),
			title:   fmt.Sprintf("%s/%s/%s", repoName, worktree, sessionName),
			label:   fmt.Sprintf("%s/%s/%s/%s", serverName, repoName, worktree, sessionName),
			refresh: serverName,
		}
	}
}

// workspaceCwd is the directory hint we pass to Spawner.Spawn — for cmux
// this becomes the new workspace's working directory (and the sidebar
// label). For other spawners it's purely cosmetic since the wrapper
// script handles its own cd. "main" worktree maps to the repo's path,
// any other to <worktree_base>/<repo>/<worktree>.
//
// Note: paths returned here can still start with "~" because they're
// derived from the user's config; cmuxCLIPath spawns the CLI locally so
// only the local shell will tilde-expand. If it doesn't, cmux will
// usually fall back to $HOME — not the end of the world.
func workspaceCwd(r *Resolved, worktreeName string) string {
	// For remote servers, the worktree path only exists on the remote
	// host. cmux/wezterm/etc. spawn locally and would error with "Path
	// does not exist". Fall back to $HOME so the spawn opens cleanly;
	// the wrapper script's mosh/ssh handles the remote cd.
	if !r.Server.Local {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return ""
	}
	if worktreeName == "" || worktreeName == "main" {
		return expandPath(r.Repo.Path)
	}
	return expandPath(worktreePath(r.WorktreeBase, r.RepoName, worktreeName))
}

func (m *tuiModel) killCmd(serverName, repoName, sessionName, tmuxFullName, worktreeName string, removeWorktree bool) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return actionDoneMsg{err: err}
		}
		if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(tmuxFullName))); err != nil {
			return actionDoneMsg{err: fmt.Errorf("kill %s: %w", tmuxFullName, err)}
		}
		// "main" worktree is the original checkout — never delete it.
		if removeWorktree && worktreeName != "main" {
			wt := worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
			if _, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
				return actionDoneMsg{
					msg:     fmt.Sprintf("killed %s (worktree removal failed)", tmuxFullName),
					err:     fmt.Errorf("remove worktree %s: %w", wt, err),
					refresh: serverName,
				}
			}
			return actionDoneMsg{
				msg:     fmt.Sprintf("killed %s + removed worktree", tmuxFullName),
				refresh: serverName,
			}
		}
		return actionDoneMsg{
			msg:     fmt.Sprintf("killed %s (worktree kept)", tmuxFullName),
			refresh: serverName,
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
func (m *tuiModel) killWorktreeCmd(serverName, repoName, worktreeName, wtPath string) tea.Cmd {
	return func() tea.Msg {
		if worktreeName == "main" {
			return actionDoneMsg{err: fmt.Errorf("cannot remove the main worktree")}
		}
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return actionDoneMsg{err: err}
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
			)}
		}
		// Snapshot the sessions running on this worktree before we start
		// killing — we read from the model's last-known state (no SSH
		// round-trip needed) so multiple kills don't fight each other.
		st := m.state[serverName]
		var victims []string
		if st != nil {
			for _, s := range st.sessions {
				if s.Repo == repoName && s.Worktree == worktreeName {
					victims = append(victims, s.Name)
				}
			}
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
		if _, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
			return actionDoneMsg{
				msg:     fmt.Sprintf("killed %d session(s); worktree removal failed", len(victims)),
				err:     fmt.Errorf("remove worktree %s: %w", wt, err),
				refresh: serverName,
			}
		}
		log().Info("tui-kill-worktree-ok", "wt", wt, "sessions_killed", len(victims))
		return actionDoneMsg{
			msg:     fmt.Sprintf("removed %s (path=%s, killed %d session(s); branch kept)", worktreeName, wt, len(victims)),
			refresh: serverName,
		}
	}
}

// ---- tree building ---------------------------------------------------------

func (m *tuiModel) rebuildRows() {
	var rows []treeRow
	for _, server := range m.serverNames {
		serverRow := treeRow{kind: rowServer, server: server, depth: 0}
		rows = append(rows, serverRow)
		st := m.state[server]
		// Disconnected: don't list any children — the state under a
		// down server is stale and would mislead the user (the very
		// kind of "ghost tree" we just fixed for missing repos).
		if st != nil && st.conn == connDisconnected {
			continue
		}
		// Still connecting on first probe → show a placeholder, no
		// (stale) children.
		if st != nil && st.conn == connConnecting && !st.sessionsLoaded && !st.reposLoaded {
			if m.isExpanded(serverRow) {
				note := "connecting…"
				if st.connAttempts > 1 {
					note = fmt.Sprintf("reconnecting… (attempt %d)", st.connAttempts)
				}
				rows = append(rows, treeRow{kind: rowLoading, server: server, depth: 1, note: note})
			}
			continue
		}
		if !m.isExpanded(serverRow) {
			continue
		}
		// Collect repo names from both discovered/explicit map and observed
		// sessions (in case a session is on a repo not surfaced by discovery).
		repoSet := map[string]struct{}{}
		for r := range st.repos {
			repoSet[r] = struct{}{}
		}
		for _, sess := range st.sessions {
			repoSet[sess.Repo] = struct{}{}
		}
		// Loading state: nothing loaded yet → show placeholder.
		if !st.sessionsLoaded && !st.reposLoaded && len(repoSet) == 0 {
			rows = append(rows, treeRow{kind: rowLoading, server: server, depth: 1, note: "loading..."})
			continue
		}
		if len(repoSet) == 0 {
			rows = append(rows, treeRow{kind: rowEmpty, server: server, depth: 1, note: "no repos"})
			continue
		}
		repos := make([]string, 0, len(repoSet))
		for r := range repoSet {
			repos = append(repos, r)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			repoRow := treeRow{kind: rowRepo, server: server, repo: repo, depth: 1}
			rows = append(rows, repoRow)
			if !m.isExpanded(repoRow) {
				continue
			}
			// Collect worktree names: from listAllWorktrees + from sessions.
			wts := st.worktrees[repo]
			wtSet := map[string]struct{}{}
			wtByName := map[string]*Worktree{}
			for i := range wts {
				wtSet[wts[i].Name] = struct{}{}
				wtByName[wts[i].Name] = &wts[i]
			}
			for _, sess := range st.sessions {
				if sess.Repo == repo {
					wtSet[sess.Worktree] = struct{}{}
				}
			}
			// Always show "main" so users can create a session against it
			// even when no worktrees yet exist (or worktrees haven't loaded).
			wtSet["main"] = struct{}{}
			if !st.worktreesLoaded && len(wts) == 0 {
				rows = append(rows, treeRow{kind: rowLoading, server: server, repo: repo, depth: 2, note: "loading worktrees..."})
				continue
			}
			wtNames := make([]string, 0, len(wtSet))
			for n := range wtSet {
				wtNames = append(wtNames, n)
			}
			sort.Slice(wtNames, func(i, j int) bool {
				// "main" sorts first.
				if wtNames[i] == "main" {
					return true
				}
				if wtNames[j] == "main" {
					return false
				}
				return wtNames[i] < wtNames[j]
			})
			for _, wtName := range wtNames {
				wtRow := treeRow{
					kind:     rowWorktree,
					server:   server,
					repo:     repo,
					worktree: wtName,
					wt:       wtByName[wtName],
					depth:    2,
				}
				rows = append(rows, wtRow)
				if !m.isExpanded(wtRow) {
					continue
				}
				var sessions []SessionInfo
				for _, sess := range st.sessions {
					if sess.Repo == repo && sess.Worktree == wtName {
						sessions = append(sessions, sess)
					}
				}
				sort.Slice(sessions, func(i, j int) bool { return sessions[i].Session < sessions[j].Session })
				if len(sessions) == 0 {
					rows = append(rows, treeRow{
						kind: rowEmpty, server: server, repo: repo, worktree: wtName,
						depth: 3, note: "no sessions — press n to create",
					})
					continue
				}
				for i := range sessions {
					rows = append(rows, treeRow{
						kind: rowSession, server: server, repo: repo, worktree: wtName,
						session: &sessions[i], depth: 3,
					})
				}
			}
		}
	}
	if m.filter != "" {
		rows = applyFilter(rows, m.filter)
	}
	m.rows = rows
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// applyFilter keeps every row that matches the filter on its OWN primary
// identifier (server/repo/worktree/session name), plus every ancestor of
// such a match so the user still sees the tree position. Descendants of a
// matching repo are NOT pulled in unless they themselves match — searching
// a repo name must not surface unrelated sessions just because they
// happen to live under that repo.
//
// Placeholder rows (rowEmpty, rowLoading) are always dropped when filter is
// active — they're noise, not data.
func applyFilter(rows []treeRow, filter string) []treeRow {
	if filter == "" {
		return rows
	}
	needle := strings.ToLower(filter)

	// First pass: mark every row index that matches in its own right, and
	// record the ancestor identifiers we need to keep so the matches stay
	// in their tree positions.
	matchIdx := make(map[int]bool)
	keepAncestor := make(map[string]bool)
	for i, r := range rows {
		if r.kind == rowEmpty || r.kind == rowLoading {
			continue
		}
		if !rowMatches(r, needle) {
			continue
		}
		matchIdx[i] = true
		if r.server != "" {
			keepAncestor["server:"+r.server] = true
		}
		if r.repo != "" {
			keepAncestor["repo:"+r.server+"/"+r.repo] = true
		}
		if r.worktree != "" {
			keepAncestor["wt:"+r.server+"/"+r.repo+"/"+r.worktree] = true
		}
	}

	// Second pass: emit matches + ancestors (only if at least one descendant
	// matched) preserving original depths so the tree is intact.
	var out []treeRow
	for i, r := range rows {
		if matchIdx[i] {
			out = append(out, r)
			continue
		}
		key := keyForRow(r)
		if key != "" && keepAncestor[key] {
			out = append(out, r)
		}
	}
	return out
}

// rowMatches checks ONLY the row's own primary identifier — not the names
// of its ancestors. A session named X does not match Y just because it
// sits inside a repo named Y; the user wants matches, not lineage.
func rowMatches(r treeRow, needle string) bool {
	check := func(s string) bool {
		return s != "" && strings.Contains(strings.ToLower(s), needle)
	}
	switch r.kind {
	case rowServer:
		return check(r.server)
	case rowRepo:
		return check(r.repo)
	case rowWorktree:
		branch := ""
		if r.wt != nil {
			branch = r.wt.Branch
		}
		return check(r.worktree) || check(branch)
	case rowSession:
		return check(r.session.Session)
	}
	return false
}

// ---- view ------------------------------------------------------------------

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dimStyle     = lipgloss.NewStyle().Faint(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("7"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // amber/yellow for "connecting"
	darkRedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("88")) // ANSI 256 dark red — for (detached) etc.
)

// Column header labels — also used as minimum widths.
const (
	hdrName = "NAME"
	hdrInfo = "DETAIL"
	hdrAge  = "AGE"
)

func (m *tuiModel) View() string {
	switch m.mode {
	case modeNewForm:
		return m.formView()
	case modeConfirm:
		return m.confirmView()
	default:
		return m.browseView()
	}
}

func (m *tuiModel) browseView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cctl"))
	b.WriteString(dimStyle.Render("  — claude sessions"))
	// Surface the detected spawn provider in the header so the user can
	// verify cctl is launching new sessions where they expect (esp.
	// important because cmux inherits TERM_PROGRAM=ghostty from
	// libghostty, so what cctl picks isn't always obvious).
	pref := ""
	if m.cfg != nil {
		pref = m.cfg.Defaults.Spawn
	}
	sp, reason := detectSpawner(pref)
	b.WriteString(dimStyle.Render(fmt.Sprintf("   [spawn=%s · %s]", sp.Name(), reason)))
	b.WriteString("\n\n")

	// Filter bar (k9s-style): always visible above the table when active.
	if m.mode == modeFilter || m.filter != "" {
		prompt := dimStyle.Render("/")
		// Use a contrasting style while the user is editing the filter.
		fstyle := lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
		if m.mode != modeFilter {
			fstyle = dimStyle
		}
		b.WriteString(prompt + fstyle.Render(m.filter))
		if m.mode == modeFilter {
			b.WriteString(cursorStyle.Render("▎"))
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("   (%d rows)", len(m.rows))))
		b.WriteString("\n\n")
	}

	// Measure column widths from actual cell contents.
	nameW, infoW, ageW := m.measureColumns()

	b.WriteString(m.renderHeader(nameW, infoW, ageW))
	b.WriteString("\n")

	if len(m.rows) == 0 {
		hint := "(no servers configured — run `cctl init`)"
		if m.filter != "" {
			hint = "(no rows match filter — esc to clear)"
		}
		b.WriteString(dimStyle.Render(hint) + "\n")
	}
	for i, r := range m.rows {
		b.WriteString(m.renderRow(r, i == m.cursor, nameW, infoW, ageW))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

// measureColumns returns the natural width of each column based on actual row
// contents (and the header labels as minimums). If the sum overflows the
// terminal, the NAME column absorbs the squeeze.
func (m *tuiModel) measureColumns() (nameW, infoW, ageW int) {
	nameW = lipgloss.Width(hdrName)
	infoW = lipgloss.Width(hdrInfo)
	ageW = lipgloss.Width(hdrAge)
	for _, r := range m.rows {
		n, i, a := m.rowCells(r, false)
		if w := lipgloss.Width(n); w > nameW {
			nameW = w
		}
		if w := lipgloss.Width(i); w > infoW {
			infoW = w
		}
		if w := lipgloss.Width(a); w > ageW {
			ageW = w
		}
	}
	// 2 leading mark + 2 single-space gaps between columns = 4 cells.
	if m.width > 0 {
		total := nameW + infoW + ageW + 4
		if total > m.width {
			shrink := total - m.width
			if nameW-shrink >= lipgloss.Width(hdrName) {
				nameW -= shrink
			} else {
				nameW = lipgloss.Width(hdrName)
			}
		}
	}
	return nameW, infoW, ageW
}

func (m *tuiModel) renderHeader(nameW, infoW, ageW int) string {
	name := padOrTrunc(hdrName, nameW)
	info := padOrTrunc(hdrInfo, infoW)
	age := padOrTrunc(hdrAge, ageW)
	return "  " + headerStyle.Render(name) + " " + headerStyle.Render(info) + " " + headerStyle.Render(age)
}

// rowCells produces the three column strings for a row — separated from
// rendering so we can measure widths in a pre-pass.
//
// Conventions for the leading glyph:
//
//	▼  expanded (has children, currently showing them)
//	▶  collapsed (has children, hidden)
//	•  no children to show (an empty worktree, etc.)
//	●  session — attached
//	○  session — detached
//
// Each non-server row carries a small "(r)"/"(w)"/"(s)" tag so the row kind
// is unambiguous regardless of indent.
func (m *tuiModel) rowCells(r treeRow, selected bool) (name, info, age string) {
	indent := strings.Repeat("  ", r.depth)
	tagStyle := dimStyle
	switch r.kind {
	case rowServer:
		st := m.state[r.server]
		glyph := containerGlyph(m.isExpanded(r), m.hasChildren(r))
		nm := highlightMatch(r.server, m.filter)
		nameStyle := lipgloss.NewStyle().Bold(true)
		if selected {
			nameStyle = cursorStyle
		}
		styled := nm
		if m.filter == "" {
			styled = nameStyle.Render(r.server)
		}
		// Spinner sits to the LEFT of the name (between the tree
		// glyph and the server label) when probing or still loading
		// post-probe. Empty when fully loaded so the column doesn't
		// jitter as servers come online.
		spinPrefix := ""
		if st != nil {
			switch {
			case st.conn == connConnecting:
				spinPrefix = warnStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " "
			case st.conn == connConnected && (!st.sessionsLoaded || !st.reposLoaded || !st.worktreesLoaded):
				spinPrefix = dimStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " "
			}
		}
		name = indent + glyph + " " + spinPrefix + styled + childCountSuffix(len(st.sessions))
		srv := m.cfg.Servers[r.server]
		// Connection state takes precedence: an unreachable host has no
		// meaningful session/repo state to show.
		switch {
		case st.conn == connDisconnected:
			hint := "disconnected"
			if st.connErr != nil {
				hint = "disconnected: " + abbrev(st.connErr.Error(), 70)
			}
			if st.connAttempts > 1 {
				hint = fmt.Sprintf("%s (retry in %s, attempt %d)", hint, probeRetryDelay(st.connAttempts).String(), st.connAttempts)
			}
			info = errorStyle.Render(hint)
		case st.conn == connConnecting && st.sessionsLoaded && st.reposLoaded:
			// Refresh of already-loaded data: keep the host string
			// visible so the user can see what's being refreshed, and
			// append a dim "refreshing…" cue (the spinner prefix on
			// the name already shows motion).
			base := srv.Host
			if srv.Local {
				base = "(local)"
			}
			info = dimStyle.Render(base+" · refreshing…")
		case st.conn == connConnecting:
			if st.connAttempts > 1 {
				info = warnStyle.Render(fmt.Sprintf("reconnecting… (attempt %d)", st.connAttempts))
			} else {
				info = warnStyle.Render("connecting…")
			}
		case !st.sessionsLoaded && !st.reposLoaded:
			info = dimStyle.Render("loading…")
		case st.sessionErr != nil:
			info = errorStyle.Render("error: " + abbrev(st.sessionErr.Error(), 50))
		case srv.Local:
			info = dimStyle.Render("(local)")
		case srv.Host != "":
			info = dimStyle.Render(srv.Host)
		}

	case rowRepo:
		st := m.state[r.server]
		glyph := containerGlyph(m.isExpanded(r), m.hasChildren(r))
		nm := highlightMatch(r.repo, m.filter)
		if m.filter == "" && selected {
			nm = cursorStyle.Render(r.repo)
		}
		count := 0
		if st.worktreesLoaded {
			count = len(st.worktrees[r.repo])
		}
		name = indent + glyph + " " + tagStyle.Render("(r)") + " " + m.preparingPrefix(r) + nm + childCountSuffix(count)
		status := RepoStatusOK
		if st.repoStatus != nil {
			if s, ok := st.repoStatus[r.repo]; ok {
				status = s
			}
		}
		if repo, ok := st.repos[r.repo]; ok {
			switch status {
			case RepoStatusMissing:
				info = errorStyle.Render(repo.Path + " — MISSING on this host")
			case RepoStatusNoGit:
				info = errorStyle.Render(repo.Path + " — NOT a git repo (no .git)")
			default:
				info = dimStyle.Render(repo.Path)
			}
		}

	case rowWorktree:
		st := m.state[r.server]
		var sessCount int
		for _, s := range st.sessions {
			if s.Repo == r.repo && s.Worktree == r.worktree {
				sessCount++
			}
		}
		glyph := containerGlyph(m.isExpanded(r), m.hasChildren(r))
		nm := highlightMatch(r.worktree, m.filter)
		if m.filter == "" && selected {
			nm = cursorStyle.Render(r.worktree)
		}
		name = indent + glyph + " " + tagStyle.Render("(w)") + " " + m.preparingPrefix(r) + nm + childCountSuffix(sessCount)
		parentStatus := RepoStatusOK
		if st.repoStatus != nil {
			if s, ok := st.repoStatus[r.repo]; ok {
				parentStatus = s
			}
		}
		switch {
		case r.wt != nil && r.wt.Branch != "":
			info = dimStyle.Render(r.wt.Branch)
		case r.wt != nil:
			// Detached HEAD = worktree exists but not on a branch. Worth
			// calling out in dark red so the user notices their changes
			// are at risk if they switch worktrees.
			info = darkRedStyle.Render("(detached)")
		case r.worktree == "main" && parentStatus == RepoStatusOK:
			info = dimStyle.Render("(primary checkout)")
		case parentStatus == RepoStatusMissing:
			// Session-only ghost row hanging off a repo that's gone from
			// disk — tell the truth instead of "(no worktree on disk)".
			info = errorStyle.Render("(repo gone — session is orphaned)")
		case parentStatus == RepoStatusNoGit:
			info = errorStyle.Render("(repo path exists but has no .git)")
		case sessCount > 0:
			// The row exists only because of a tmux session — there's no
			// matching worktree dir on disk. dd is safe (it kills the
			// orphan session and the worktree-remove step is a no-op),
			// so call the state out specifically rather than the generic
			// "no worktree on disk" that made users worry about data
			// loss after the past delete-repo incident.
			info = warnStyle.Render("(orphan session — worktree dir gone; dd is safe)")
		default:
			info = dimStyle.Render("(no worktree on disk)")
		}

	case rowSession:
		sess := r.session
		marker := "○"
		mStyle := dimStyle
		if sess.Attached {
			marker = "●"
			mStyle = okStyle
		}
		nm := highlightMatch(sess.Session, m.filter)
		if m.filter == "" && selected {
			nm = cursorStyle.Render(sess.Session)
		}
		name = indent + mStyle.Render(marker) + " " + tagStyle.Render("(s)") + " " + m.preparingPrefix(r) + nm
		if sess.Attached {
			info = okStyle.Render("attached")
		} else {
			info = dimStyle.Render("detached")
		}
		age = dimStyle.Render(humanAge(sess.Age))

	case rowEmpty, rowLoading:
		name = indent + "  " + dimStyle.Italic(true).Render(r.note)
	}
	return name, info, age
}

func (m *tuiModel) renderRow(r treeRow, selected bool, nameW, infoW, ageW int) string {
	mark := "  "
	if selected {
		mark = cursorStyle.Render("▸ ")
	}
	name, info, age := m.rowCells(r, selected)
	return mark +
		padOrTruncStyled(name, nameW) + " " +
		padOrTruncStyled(info, infoW) + " " +
		padOrTruncStyled(age, ageW)
}

// childCountSuffix returns a "  (N)" gray suffix for non-zero counts, or
// empty when the count is 0 — keeps the tree visually tight by hiding zeros.
func childCountSuffix(n int) string {
	if n <= 0 {
		return ""
	}
	return " " + dimStyle.Render(fmt.Sprintf("(%d)", n))
}

// containerGlyph returns the leading glyph for a container row (server,
// repo, worktree). When the row has children we show an expand/collapse
// indicator; when it doesn't, a small bullet so the user knows nothing
// will appear if they try to expand.
func containerGlyph(expanded, hasChildren bool) string {
	if !hasChildren {
		return "•"
	}
	if expanded {
		return "▼"
	}
	return "▶"
}

// highlightMatch wraps every (case-insensitive) occurrence of needle inside
// name with a bold yellow style, so the matched letters pop in a filtered
// list — k9s/fzf style. Returns name unchanged when needle is empty or
// absent.
func highlightMatch(name, needle string) string {
	if needle == "" {
		return name
	}
	lower := strings.ToLower(name)
	low := strings.ToLower(needle)
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	var b strings.Builder
	for {
		idx := strings.Index(lower, low)
		if idx < 0 {
			b.WriteString(name)
			return b.String()
		}
		b.WriteString(name[:idx])
		b.WriteString(yellow.Render(name[idx : idx+len(needle)]))
		name = name[idx+len(needle):]
		lower = lower[idx+len(needle):]
	}
}

func (m *tuiModel) footer() string {
	var b strings.Builder
	// Status bar lives on its own line above the legend, separated by a
	// blank line so the moving spinner / colored message reads as a
	// distinct UI element instead of running into the keybinding list.
	statusLine := ""
	switch {
	case m.pendingD:
		statusLine = errorStyle.Bold(true).Render("press d again to delete — any other key cancels")
	case m.statusErr != "":
		statusLine = errorStyle.Width(m.errorWidth()).Render("error: " + m.statusErr)
	default:
		if line := m.renderStatusBar(); line != "" {
			statusLine = line
		} else if m.statusMsg != "" {
			statusLine = dimStyle.Render(m.statusMsg)
		}
	}
	if statusLine != "" {
		b.WriteString(statusLine + "\n\n")
	}
	// Legend: one shortcut per line. Was a 130-column single line that
	// truncated awkwardly on narrow terminals.
	for _, line := range legendLines {
		b.WriteString(dimStyle.Render(line) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderStatusBar draws the action status indicator (spinner / ✓ / ✗ with
// label). Returns "" when status is idle so the caller can fall through to
// other lines.
func (m *tuiModel) renderStatusBar() string {
	switch m.status.kind {
	case statusProgress:
		spin := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		// Use the warn (amber) hue + bold so it pops vs the dim legend.
		return warnStyle.Bold(true).Render(spin+" "+m.status.label) + dimStyle.Render(" — working…")
	case statusSuccess:
		return okStyle.Bold(true).Render("✓ " + m.status.label)
	case statusFailure:
		head := errorStyle.Bold(true).Render("✗ " + m.status.label)
		if m.status.detail == "" {
			return head
		}
		return head + dimStyle.Render(" — "+abbrev(m.status.detail, 200))
	}
	return ""
}

// legendLines is the keybinding reference shown at the bottom of the TUI.
// One shortcut per line so the footer doesn't depend on terminal width.
// Aligned with a soft tab so the keys line up visually.
var legendLines = []string{
	"↑↓        move",
	"←→        collapse / expand",
	"⏎         open in new cmux tab (r/w → form, s → attach)",
	"n         new session on selected row",
	"dd        delete (worktree or session — never the repo)",
	"/         filter (k9s-style)",
	"r         refresh",
	"q         quit",
}

// errorWidth returns the column width to use for wrapping error/status text.
// Falls back to 100 when the terminal hasn't reported its size yet.
func (m *tuiModel) errorWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 100
}

func (m *tuiModel) formView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("new session"))
	target := fmt.Sprintf("  — %s / %s", m.formServer, m.formRepo)
	if m.formWorktree != "" {
		target += " / " + m.formWorktree
	}
	b.WriteString(dimStyle.Render(target))
	b.WriteString("\n\n")

	type field struct {
		label string
		input *textinput.Model
	}
	var fields []field
	if m.formWorktree == "" {
		fields = []field{
			{"worktree:", &m.wtInput},
			{"session: ", &m.nameInput},
			{"prompt:  ", &m.promptInput},
		}
	} else {
		fields = []field{
			{"session: ", &m.nameInput},
			{"prompt:  ", &m.promptInput},
		}
	}
	for i, f := range fields {
		marker := "  "
		if i == m.formFocus {
			marker = cursorStyle.Render("▸ ")
		}
		b.WriteString(marker + f.label + " " + f.input.View() + "\n")
	}
	b.WriteString("\n")
	if m.statusErr != "" {
		b.WriteString(errorStyle.Width(m.errorWidth()).Render("error: "+m.statusErr) + "\n\n")
	}
	if m.formWorktree == "" {
		b.WriteString(dimStyle.Render("worktree = `main` reuses the primary checkout; any other name creates a new worktree on branch <prefix>/<name>.") + "\n")
	}
	b.WriteString(dimStyle.Render("⏎ submit · tab switch · esc cancel"))
	return b.String()
}

func (m *tuiModel) confirmView() string {
	row := m.confirmRow
	var b strings.Builder
	switch row.kind {
	case rowSession:
		tmuxFullName := row.session.Name
		withWT := row.note == "with-worktree"
		b.WriteString(titleStyle.Render("kill session?"))
		b.WriteString("\n\n")
		b.WriteString("  " + tmuxFullName + dimStyle.Render(fmt.Sprintf("  on %s", row.server)) + "\n\n")
		b.WriteString("  • kill tmux session\n")
		switch {
		case withWT && row.session.Worktree == "main":
			b.WriteString("  • " + dimStyle.Render("worktree=main is the primary checkout — never removed") + "\n")
		case withWT:
			b.WriteString("  • remove worktree (branch kept)\n")
		default:
			b.WriteString("  • " + dimStyle.Render("keep worktree") + "\n")
		}
	case rowWorktree:
		var victims int
		st := m.state[row.server]
		if st != nil {
			for _, s := range st.sessions {
				if s.Repo == row.repo && s.Worktree == row.worktree {
					victims++
				}
			}
		}
		b.WriteString(titleStyle.Render("remove worktree?"))
		b.WriteString("\n\n")
		b.WriteString("  " + row.repo + "/" + row.worktree + dimStyle.Render(fmt.Sprintf("  on %s", row.server)) + "\n\n")
		b.WriteString(fmt.Sprintf("  • kill %d running session(s) under this worktree\n", victims))
		b.WriteString("  • git worktree remove (branch kept)\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("y / ⏎ confirm · n / esc cancel"))
	return b.String()
}

// padOrTrunc pads or trims an unstyled string to exactly w printable columns.
func padOrTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

// padOrTruncStyled pads/trims a string that may contain ANSI escapes to w
// printable columns. Truncation only happens if the *visible* width exceeds
// w; otherwise we right-pad with spaces.
func padOrTruncStyled(s string, w int) string {
	if w <= 0 {
		return ""
	}
	vis := lipgloss.Width(s)
	switch {
	case vis == w:
		return s
	case vis < w:
		return s + strings.Repeat(" ", w-vis)
	default:
		// Naive truncation: cut bytes while watching visible width. This loses
		// styling on the cut tail; acceptable since we mostly avoid styles in
		// the same string as the truncate boundary.
		return truncStyled(s, w)
	}
}

func truncStyled(s string, w int) string {
	if w <= 1 {
		return s[:0]
	}
	// Build prefix one rune at a time until we'd exceed w-1 cells, then append "…".
	out := make([]rune, 0, len(s))
	inEsc := false
	used := 0
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			out = append(out, r)
			continue
		}
		if inEsc {
			out = append(out, r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if used >= w-1 {
			out = append(out, '…')
			break
		}
		out = append(out, r)
		used++
	}
	return string(out)
}
