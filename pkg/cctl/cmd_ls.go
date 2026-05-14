package cctl

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls [server[/repo]]",
		Short: "List cctl-managed Claude sessions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			var serverNames []string
			var repoFilter string
			if len(args) == 1 {
				server, repo := parseTarget(args[0])
				if _, ok := cfg.Servers[server]; !ok {
					return fmt.Errorf("unknown server %q", server)
				}
				serverNames = []string{server}
				repoFilter = repo
			} else {
				for n := range cfg.Servers {
					serverNames = append(serverNames, n)
				}
				sort.Strings(serverNames)
			}

			type result struct {
				server   string
				sessions []SessionInfo
				err      error
			}

			results := make([]result, len(serverNames))
			var wg sync.WaitGroup
			for i, n := range serverNames {
				wg.Add(1)
				go func(i int, n string) {
					defer wg.Done()
					sess, err := listSessions(n, cfg.Servers[n])
					results[i] = result{server: n, sessions: sess, err: err}
				}(i, n)
			}
			wg.Wait()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVER\tSESSION\tREPO\tATTACHED\tAGE")
			any := false
			for _, r := range results {
				if r.err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", r.server, r.err)
					continue
				}
				for _, s := range r.sessions {
					if repoFilter != "" && s.Repo != repoFilter {
						continue
					}
					any = true
					att := "no"
					if s.Attached {
						att = "yes"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Server, s.Session, s.Repo, att, humanAge(s.Age))
				}
			}
			if !any {
				fmt.Fprintln(w, "(no sessions)")
			}
			return w.Flush()
		},
	}
}
