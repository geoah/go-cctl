package cctl

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sessionPrefix is the namespace prefix used for tmux session names so that
// cctl only ever touches sessions it owns.
const sessionPrefix = "cctl/"

// tmuxName returns the canonical tmux session name. With the worktree level
// added, every name has four "/"-separated parts:
//
//	cctl/<repo>/<worktree>/<session>
//
// Repo names are leaf-only (no '/'), worktree names are basenames, so the
// split is unambiguous.
func tmuxName(repo, worktree, session string) string {
	return sessionPrefix + repo + "/" + worktree + "/" + session
}

// parseTmuxName splits a tmux session name into (repo, worktree, session, ok).
// Only the canonical 4-part form is accepted; tmux sessions with any other
// shape are silently ignored (filtered out by the listSessions caller).
func parseTmuxName(name string) (repo, worktree, session string, ok bool) {
	if !strings.HasPrefix(name, sessionPrefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(name, sessionPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// hasSession returns true if the tmux session exists on the remote.
func hasSession(s Server, name string) (bool, error) {
	code, _, err := runRemoteCode(s, fmt.Sprintf("tmux has-session -t %s 2>/dev/null", shellQuote(name)))
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// SessionInfo is one entry from `tmux ls` on a remote.
type SessionInfo struct {
	Server   string
	Name     string // full tmux name, e.g. cctl/rxtx.dev/audit/quickcheck
	Repo     string
	Worktree string
	Session  string
	Attached bool
	Created  time.Time
	Age      time.Duration
}

// listSessions returns every cctl-managed tmux session on the given server.
// Returns an empty slice (not an error) if tmux isn't running.
func listSessions(serverName string, s Server) ([]SessionInfo, error) {
	// #{session_created} is a Unix timestamp; #{session_attached} is 0 or 1.
	cmd := `tmux list-sessions -F '#{session_name}|#{session_attached}|#{session_created}' 2>/dev/null || true`
	out, err := runRemote(s, cmd)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var results []SessionInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		repo, worktree, session, ok := parseTmuxName(parts[0])
		if !ok {
			continue
		}
		attached := parts[1] != "0"
		var created time.Time
		if ts, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
			created = time.Unix(ts, 0)
		}
		results = append(results, SessionInfo{
			Server:   serverName,
			Name:     parts[0],
			Repo:     repo,
			Worktree: worktree,
			Session:  session,
			Attached: attached,
			Created:  created,
			Age:      now.Sub(created),
		})
	}
	return results, nil
}

func humanAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
