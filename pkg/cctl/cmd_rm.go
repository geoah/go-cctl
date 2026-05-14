package cctl

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newRmCmd() *cobra.Command {
	var keepWorktree bool
	cmd := &cobra.Command{
		Use:   "rm <server[/repo]> <session>",
		Short: "Kill a Claude session (and remove its worktree by default)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverName, repoName := parseTarget(args[0])
			sessionName := args[1]
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			srv, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("unknown server %q", serverName)
			}

			// Resolve (repo, worktree) for this session by listing tmux. The
			// CLI convention is worktree == session, but TUI-created sessions
			// may have a worktree distinct from the session name, so we must
			// look it up rather than assume.
			sessions, err := listSessions(serverName, srv)
			if err != nil {
				return err
			}
			var matches []SessionInfo
			for _, s := range sessions {
				if s.Session != sessionName {
					continue
				}
				if repoName != "" && s.Repo != repoName {
					continue
				}
				matches = append(matches, s)
			}
			worktreeName := sessionName // fall back to CLI convention
			switch len(matches) {
			case 0:
				if repoName == "" {
					return fmt.Errorf("no cctl session %q on %s", sessionName, serverName)
				}
				// keep CLI convention (worktree == session) so worktree
				// removal still runs against the likely path.
			case 1:
				repoName = matches[0].Repo
				worktreeName = matches[0].Worktree
			default:
				var names []string
				for _, m := range matches {
					names = append(names, m.Repo+"/"+m.Worktree)
				}
				return fmt.Errorf("ambiguous session %q on %s (in: %s) — use %s/<repo> %s", sessionName, serverName, strings.Join(names, ", "), serverName, sessionName)
			}

			r, err := cfg.resolve(serverName, repoName)
			if err != nil {
				return err
			}
			tname := tmuxName(r.RepoName, worktreeName, sessionName)

			exists, err := hasSession(r.Server, tname)
			if err != nil {
				return err
			}
			if exists {
				if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(tname))); err != nil {
					return fmt.Errorf("kill-session: %w", err)
				}
				fmt.Fprintf(os.Stderr, "killed tmux session %s on %s\n", tname, r.ServerName)
			} else {
				fmt.Fprintf(os.Stderr, "no tmux session %s on %s (continuing)\n", tname, r.ServerName)
			}

			if !keepWorktree {
				wt := worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
				script := removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)
				if out, err := runRemote(r.Server, script); err != nil {
					if out != "" {
						fmt.Fprintln(os.Stderr, strings.TrimSpace(out))
					}
					return fmt.Errorf("remove worktree: %w", err)
				}
				fmt.Fprintf(os.Stderr, "removed worktree %s (branch kept)\n", wt)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepWorktree, "keep-worktree", false, "do not remove the worktree directory")
	return cmd
}
