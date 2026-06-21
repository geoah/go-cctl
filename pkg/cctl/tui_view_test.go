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
