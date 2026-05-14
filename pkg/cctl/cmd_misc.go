package cctl

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newServersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "servers",
		Short: "List configured servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			names := cfg.serverNames()
			sort.Strings(names)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tHOST\tUSER\tMOSH")
			for _, n := range names {
				s := cfg.Servers[n]
				host := s.Host
				user := s.User
				mosh := "yes"
				if s.Local {
					host, user, mosh = "(local)", "-", "-"
				} else {
					if s.Mosh != nil && !*s.Mosh {
						mosh = "no"
					}
					if user == "" {
						user = "-"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, host, user, mosh)
			}
			return w.Flush()
		},
	}
}

func newReposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos <server>",
		Short: "List repos available on a server (explicit + discovered)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			srv, ok := cfg.Servers[args[0]]
			if !ok {
				return fmt.Errorf("unknown server %q", args[0])
			}
			repos, err := cfg.repos(args[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			}
			names := repoNames(repos)
			sort.Strings(names)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPATH\tDEFAULT_BRANCH\tSOURCE")
			for _, n := range names {
				r := repos[n]
				db := r.DefaultBranch
				if db == "" {
					db = "main"
				}
				source := "discovered"
				if _, explicit := srv.Repos[n]; explicit {
					source = "explicit"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, r.Path, db, source)
			}
			return w.Flush()
		},
	}
}

func newSSHCmd() *cobra.Command {
	var noMosh bool
	cmd := &cobra.Command{
		Use:   "ssh <server>",
		Short: "Connect to a server (mosh by default), no tmux",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			r, err := cfg.resolve(args[0], "")
			if err != nil {
				// resolve fails when there's >1 repo and none is specified — fine for plain ssh.
				srv, ok := cfg.Servers[args[0]]
				if !ok {
					return err
				}
				return execInteractive(srv, !noMosh && (srv.Mosh == nil || *srv.Mosh), "exec $SHELL -l")
			}
			useMosh := r.UseMosh && !noMosh
			return execInteractive(r.Server, useMosh, "exec $SHELL -l")
		},
	}
	cmd.Flags().BoolVar(&noMosh, "no-mosh", false, "use ssh -t instead of mosh")
	return cmd
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Config inspection"}
	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the resolved config path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveConfigPath()
			if err != nil {
				return err
			}
			fmt.Println(p)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "sample",
		Short: "Print a starter ~/.cctl.yaml to stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print(sampleConfig)
			return nil
		},
	})
	return cmd
}

const sampleConfig = `# ~/.cctl.yaml — managed sessions for mosh + tmux + claude
defaults:
  branch_prefix: me             # worktree branch is <branch_prefix>/<session>
  worktree_base: ~/worktrees    # remote path; per-repo override allowed
  claude_flags: ["--dangerously-skip-permissions"]
  mosh: true

servers:
  workspace:
    host: 10.0.0.10
    user: me
    ssh_key: ~/.ssh/id_ed25519
    ssh_opts:
      - "-o"
      - "IdentitiesOnly=yes"
      - "-o"
      - "StrictHostKeyChecking=accept-new"
    # repo_sources: cctl walks each path up to max_depth looking for .git dirs.
    # The repo name is the leaf directory (e.g. ~/src/github.com/my-org/my-app
    # becomes "my-app"). Collisions across sources get parent-prefixed.
    repo_sources:
      - path: ~/src/github.com
        max_depth: 3
        default_branch: main

  local:
    local: true
    repo_sources:
      - path: ~/src/github.com
        max_depth: 3
        default_branch: main
    # repos: optional explicit overrides — entries here win over discovery.
    # repos:
    #   custom:
    #     path: ~/some/other/place
    #     default_branch: develop
`
