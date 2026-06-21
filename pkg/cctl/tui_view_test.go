package cctl

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestStartupQueue pins the startup queue: one connect task per server plus a
// pending sync step; each connect finishes when its server settles, and the
// sync flips from pending to running once all have.
func TestStartupQueue(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"local": {Local: true}, "ws": {Host: "h"}}},
		serverNames: []string{"local", "ws"},
		state: map[string]*serverState{
			"local": {conn: connConnecting},
			"ws":    {conn: connConnecting},
		},
		startupConnectTasks: map[string]int{},
	}
	// Seed the queue the way Init does.
	for _, n := range m.serverNames {
		id, _ := m.startTask("startup:connect:"+n, n+": connecting…")
		m.startupConnectTasks[n] = id
	}
	m.startupSyncTaskID = m.startPendingTask("startup:sync", "sync cmux ↔ cctl")

	if got := len(m.startupConnectTasks); got != 2 {
		t.Fatalf("want 2 connect tasks, got %d", got)
	}
	syncTask := func() *bgTask {
		for _, tk := range m.tasks {
			if tk.id == m.startupSyncTaskID {
				return tk
			}
		}
		return nil
	}
	if !syncTask().pending {
		t.Fatal("sync task should start pending")
	}

	// local settles (connected + loaded) → its connect task finishes.
	st := m.state["local"]
	st.conn = connConnected
	st.sessionsLoaded, st.reposLoaded, st.worktreesLoaded = true, true, true
	m.settleServerTask("local")
	if _, still := m.startupConnectTasks["local"]; still {
		t.Error("local connect task should be removed after settle")
	}
	// Not all settled yet → sync still pending.
	if c := m.maybeStartupSync(); c != nil || !syncTask().pending {
		t.Error("sync must stay pending until every server settles")
	}

	// ws settles → all settled → sync flips to running.
	ws := m.state["ws"]
	ws.conn = connConnected
	ws.sessionsLoaded, ws.reposLoaded, ws.worktreesLoaded = true, true, true
	m.settleServerTask("ws")
	if c := m.maybeStartupSync(); c == nil {
		t.Fatal("maybeStartupSync should fire once all settle")
	}
	if syncTask().pending {
		t.Error("sync task should be running after all servers settle")
	}
	// Idempotent: a second call does nothing.
	if c := m.maybeStartupSync(); c != nil {
		t.Error("maybeStartupSync should fire exactly once")
	}
}

// TestReconnectTriggersSync pins the "refresh a remote → sync runs" fix: once
// startup is done, a server that (re)connects and finishes loading requests a
// (debounced) sync.
func TestReconnectTriggersSync(t *testing.T) {
	loaded := func() *serverState {
		return &serverState{conn: connConnected, sessionsLoaded: true, reposLoaded: true, worktreesLoaded: true}
	}
	m := &tuiModel{
		cfg:                 &Config{Servers: map[string]Server{"ws": {Host: "h"}}},
		serverNames:         []string{"ws"},
		state:               map[string]*serverState{"ws": loaded()},
		startupConnectTasks: map[string]int{},
		startupSynced:       true, // past the startup one-shot
	}
	m.state["ws"].pendingSync = true // just reconnected

	cmd := m.startupProgress("ws")
	if cmd == nil {
		t.Fatal("reconnect of a fully-loaded server should request a sync")
	}
	if !m.syncPending {
		t.Error("syncPending should be set after a reconnect")
	}
	if m.state["ws"].pendingSync {
		t.Error("pendingSync should be cleared once the sync is requested")
	}
	// Debounce: another reconnect while a sync is already queued is a no-op.
	if c := m.requestSync(); c != nil {
		t.Error("requestSync must not double-schedule while one is pending")
	}

	// Without pendingSync (a plain re-render), no sync is requested.
	m.syncPending = false
	if c := m.startupProgress("ws"); c != nil {
		t.Error("a loaded server with no pendingSync should not request a sync")
	}
}

// TestMaybeStartupSyncClearsPending: the one-shot startup sync clears every
// server's pendingSync so the reconnect path doesn't immediately re-fire.
func TestMaybeStartupSyncClearsPending(t *testing.T) {
	m := &tuiModel{
		cfg:               &Config{Servers: map[string]Server{"ws": {Host: "h"}}},
		serverNames:       []string{"ws"},
		state:             map[string]*serverState{"ws": {conn: connConnected, sessionsLoaded: true, reposLoaded: true, worktreesLoaded: true, pendingSync: true}},
		startupSyncTaskID: 1,
		tasks:             []*bgTask{{id: 1, label: "sync", pending: true}},
	}
	if cmd := m.maybeStartupSync(); cmd == nil {
		t.Fatal("startup sync should fire once all settle")
	}
	if !m.startupSynced {
		t.Error("startupSynced should be set")
	}
	if m.state["ws"].pendingSync {
		t.Error("startup sync must clear pendingSync to avoid an immediate re-sync")
	}
}

func TestEnsureCursorVisible(t *testing.T) {
	m := &tuiModel{rows: make([]treeRow, 20)}

	m.cursor, m.scrollOffset = 0, 0
	m.ensureCursorVisible(5)
	if m.scrollOffset != 0 {
		t.Fatalf("top: offset=%d want 0", m.scrollOffset)
	}
	m.cursor = 10
	m.ensureCursorVisible(5)
	if m.scrollOffset != 6 { // 10 - 5 + 1
		t.Fatalf("scroll down: offset=%d want 6", m.scrollOffset)
	}
	m.cursor = 3
	m.ensureCursorVisible(5)
	if m.scrollOffset != 3 {
		t.Fatalf("scroll up: offset=%d want 3", m.scrollOffset)
	}
	m.cursor = 19
	m.ensureCursorVisible(5)
	if m.scrollOffset != 15 { // clamp to len-visible
		t.Fatalf("bottom: offset=%d want 15", m.scrollOffset)
	}
	m.cursor = 19
	m.ensureCursorVisible(50) // viewport bigger than list
	if m.scrollOffset != 0 {
		t.Fatalf("tall viewport: offset=%d want 0", m.scrollOffset)
	}
}

// TestBrowseViewLayout pins the panel geometry: the rendered frame fills
// exactly the terminal height and never exceeds its width, and the cursor
// scrolls into view when it's past the fold.
func TestBrowseViewLayout(t *testing.T) {
	mk := func(w, h, nrows int) *tuiModel {
		m := &tuiModel{
			cfg:      &Config{Servers: map[string]Server{}},
			width:    w,
			height:   h,
			expanded: map[string]bool{},
			state:    map[string]*serverState{},
		}
		for i := 0; i < nrows; i++ {
			name := fmt.Sprintf("srv%02d", i)
			m.serverNames = append(m.serverNames, name)
			m.state[name] = &serverState{conn: connConnected, sessionsLoaded: true, reposLoaded: true, worktreesLoaded: true}
			m.rows = append(m.rows, treeRow{kind: rowServer, server: name})
		}
		return m
	}

	// Wide terminal → paneled layout.
	m := mk(120, 30, 50)
	m.cursor = 49
	out := m.browseView()
	if got := lipgloss.Height(out); got != 30 {
		t.Errorf("paneled height = %d, want 30", got)
	}
	if got := lipgloss.Width(out); got > 120 {
		t.Errorf("paneled width = %d, want <= 120", got)
	}
	if m.scrollOffset == 0 {
		t.Errorf("cursor at bottom should have scrolled the panel (offset=%d)", m.scrollOffset)
	}

	// Empty tree still fills the frame.
	if got := lipgloss.Height(mk(120, 30, 0).browseView()); got != 30 {
		t.Errorf("empty paneled height = %d, want 30", got)
	}

	// Narrow terminal → linear fallback, must not panic.
	_ = mk(45, 20, 10).browseView()
}
