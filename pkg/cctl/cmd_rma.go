package cctl

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newRmaCmd() *cobra.Command {
	var (
		yes          bool
		keepWorktree bool
	)
	cmd := &cobra.Command{
		Use:   "rma <server[/repo]>",
		Short: "Kill every cctl session on a server (optionally only one repo)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverName, repoFilter := parseTarget(args[0])
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			srv, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("unknown server %q", serverName)
			}
			all, err := listSessions(serverName, srv)
			if err != nil {
				return err
			}
			sessions := all
			if repoFilter != "" {
				sessions = sessions[:0]
				for _, s := range all {
					if s.Repo == repoFilter {
						sessions = append(sessions, s)
					}
				}
			}
			if len(sessions) == 0 {
				fmt.Fprintln(os.Stderr, "no cctl sessions to remove")
				return nil
			}
			target := serverName
			if repoFilter != "" {
				target = serverName + "/" + repoFilter
			}
			fmt.Fprintf(os.Stderr, "About to remove %d session(s) on %s:\n", len(sessions), target)
			for _, s := range sessions {
				fmt.Fprintf(os.Stderr, "  %s (repo=%s, age=%s)\n", s.Session, s.Repo, humanAge(s.Age))
			}
			if !yes {
				fmt.Fprint(os.Stderr, "Proceed? [y/N] ")
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
					fmt.Fprintln(os.Stderr, "aborted")
					return nil
				}
			}

			var firstErr error
			for _, s := range sessions {
				r, err := cfg.resolve(serverName, s.Repo)
				if err != nil {
					fmt.Fprintf(os.Stderr, "skip %s: %v\n", s.Session, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(s.Name))); err != nil {
					fmt.Fprintf(os.Stderr, "kill %s: %v\n", s.Name, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				fmt.Fprintf(os.Stderr, "killed %s\n", s.Name)
				if !keepWorktree {
					// Use the worktree name from the parsed tmux name, not
					// s.Session — they only coincide for CLI-created
					// sessions; TUI sessions may have differing names.
					wt := worktreePath(r.WorktreeBase, r.RepoName, s.Worktree)
					if out, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
						if out != "" {
							fmt.Fprintln(os.Stderr, strings.TrimSpace(out))
						}
						fmt.Fprintf(os.Stderr, "remove worktree %s: %v\n", wt, err)
						if firstErr == nil {
							firstErr = err
						}
					} else {
						fmt.Fprintf(os.Stderr, "removed worktree %s\n", wt)
					}
				}
			}
			return firstErr
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVar(&keepWorktree, "keep-worktree", false, "do not remove worktree directories")
	return cmd
}
