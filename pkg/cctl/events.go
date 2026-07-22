package cctl

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
)

// cmuxWorkspaceClosedMsg is sent into the TUI loop when cmux reports that a
// workspace was closed (sidebar X, the custom sidebar's Close item, Cmd-W).
// The Update handler verifies against live state before treating it as a dd.
type cmuxWorkspaceClosedMsg struct {
	wsID  string
	title string
}

// watchCmuxWorkspaceCloses tails `cmux events` for workspace.closed frames
// and forwards them to send. Blocks until ctx is canceled or the stream ends;
// --reconnect lets the CLI ride out transient socket drops. Only event NAMES
// are trusted here — the TUI re-verifies (workspace really gone, cmux still
// answering, burst circuit-breaker) before acting, so an event replay or a
// shutdown burst can never mass-delete sessions.
func watchCmuxWorkspaceCloses(ctx context.Context, send func(cmuxWorkspaceClosedMsg)) {
	cli := cmuxCLIPath()
	if cli == "" {
		return
	}
	cmd := exec.CommandContext(ctx, cli, "events",
		"--category", "workspace", "--name", "workspace.closed",
		"--no-ack", "--no-heartbeat", "--reconnect")
	cmd.Env = append(os.Environ(), "CMUX_QUIET=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		log().Debug("cmux-events-start-fail", "err", err.Error())
		return
	}
	defer func() { _ = cmd.Wait() }()
	log().Info("cmux-events-watch-start")
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if wsID, title, ok := parseCmuxWorkspaceClosed(sc.Bytes()); ok {
			send(cmuxWorkspaceClosedMsg{wsID: wsID, title: title})
		}
	}
	log().Debug("cmux-events-watch-end")
}

// parseCmuxWorkspaceClosed extracts (workspace_id, custom_title||title) from
// one event frame. ok=false for any other frame shape or name. Pure, so it's
// unit-testable against captured event JSON.
func parseCmuxWorkspaceClosed(line []byte) (wsID, title string, ok bool) {
	var frame struct {
		Name    string `json:"name"`
		Payload struct {
			WorkspaceID string `json:"workspace_id"`
			CustomTitle string `json:"custom_title"`
			Title       string `json:"title"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		return "", "", false
	}
	if frame.Name != "workspace.closed" {
		return "", "", false
	}
	title = frame.Payload.CustomTitle
	if title == "" {
		title = frame.Payload.Title
	}
	if frame.Payload.WorkspaceID == "" && title == "" {
		return "", "", false
	}
	return frame.Payload.WorkspaceID, title, true
}
