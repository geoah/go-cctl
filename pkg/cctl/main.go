// Package cctl implements `cctl` — a TUI + CLI for managing
// mosh+tmux+claude sessions across local and remote git checkouts.
//
// Quick start:
//
//	cctl claude <server> <session> [--repo R] [--no-worktree] [--branch B] [-- <prompt...>]
//	cctl ls [server]
//	cctl rm <server> <session>
//	cctl rma <server>
//
// See the repository README for the full config schema.
package cctl

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Version is the semver from version.txt (bumped by release-please),
// injected at build time via -ldflags (see the cctl:install mise task).
// Defaults to "dev" for a plain `go build`; versionString() then falls back
// to the module build info.
var Version = "dev"

var configPath string

// restartAll / restartYes back the `cctl --restart-all [--yes]` startup flow:
// reload tmux config + destroy every tracked session, then let the TUI's normal
// startup reconcile revive them so new ~/.tmux.conf / mosh settings take hold.
var (
	restartAll bool
	restartYes bool
)

var rootCmd = &cobra.Command{
	Use:           "cctl",
	Short:         "Manage mosh+tmux+claude sessions across remote servers",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	Long: `cctl manages create/resume/list/teardown of tmux sessions running
Claude Code on remote servers, with optional git-worktree branches.

Running cctl with no arguments opens the interactive TUI. The subcommands
below are for scripting and direct CLI use.

Examples:
  cctl                                  # open the TUI
  cctl --restart-all                    # restart every tracked session (new tmux/mosh settings)
  cctl claude workspace audit -- investigate the auth bug
  cctl claude local/my-app hotfix --no-worktree
  cctl ls
  cctl rm local/my-app hotfix
  cctl rma workspace --yes
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if restartAll {
			if err := restartAllOnStartup(restartYes); err != nil {
				return err
			}
		}
		return runTUI()
	},
}

// restartAllOnStartup is the `--restart-all` entry point: reload every server's
// tmux config and kill all tracked sessions (after a confirmation, unless
// --yes), then return so the TUI's startup reconcile revives them. The revived
// sessions attach fresh, so the reloaded tmux Ms/scroll settings and a
// re-established mosh connection all take effect.
func restartAllOnStartup(yes bool) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	targets := restartTargets(cfg, loadManifestEntries())
	fmt.Fprintf(os.Stderr, "restart-all: reload tmux config on %d server(s) and restart %d tracked session(s).\n",
		len(cfg.Servers), len(targets))
	fmt.Fprintln(os.Stderr, "            running agents will be killed and resumed (claude --continue / codex resume --last).")
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "            (no tracked sessions — will just reload config.)")
	}
	if !yes {
		fmt.Fprint(os.Stderr, "Continue? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Fprintln(os.Stderr, "restart-all: aborted — opening the TUI without restarting.")
			return nil
		}
	}
	res := restartAllTrackedSessions(cfg)
	fmt.Fprintf(os.Stderr, "restart-all: reloaded config on %d/%d server(s); restarting %d session(s) — reconciling on startup...\n",
		res.reloaded, res.servers, res.killed)
	if res.unreachable > 0 {
		fmt.Fprintf(os.Stderr, "restart-all: %d server(s) unreachable, %d session(s) left tracked (will revive when reachable).\n",
			res.unreachable, res.skipped)
	}
	return nil
}

func init() {
	rootCmd.Version = versionString()
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "path to cctl config (default: $CCTL_CONFIG or ~/.cctl.yaml)")
	rootCmd.Flags().BoolVar(&restartAll, "restart-all", false, "on startup, reload tmux config and destroy+restore all tracked sessions so new ~/.tmux.conf / mosh settings take effect")
	rootCmd.Flags().BoolVarP(&restartYes, "yes", "y", false, "skip the --restart-all confirmation prompt")

	rootCmd.AddCommand(newClaudeCmd())
	rootCmd.AddCommand(newCodexCmd())
	rootCmd.AddCommand(newLsCmd())
	rootCmd.AddCommand(newRmCmd())
	rootCmd.AddCommand(newRmaCmd())
	rootCmd.AddCommand(newServersCmd())
	rootCmd.AddCommand(newReposCmd())
	rootCmd.AddCommand(newSSHCmd())
	rootCmd.AddCommand(newConfigCmd())
	rootCmd.AddCommand(newInitCmd())
	rootCmd.AddCommand(newReconcileCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newVersionCmd())
}

// Run is the package's CLI entry point. It wires up logging, executes the
// cobra root command, and exits the process with a non-zero status on
// failure. Intended to be called from a thin top-level main.go.
func Run() {
	initLogger()
	log().Debug("argv", "args", os.Args)
	if err := rootCmd.Execute(); err != nil {
		log().Error("execute-failed", "err", err.Error())
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
