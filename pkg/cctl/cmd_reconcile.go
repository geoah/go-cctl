package cctl

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// newReconcileCmd exposes the cmux↔cctl reconcile as a one-shot CLI command.
// The TUI runs the same pass on startup and on R/S; this lets you (or cctl's
// own tooling) run it headlessly — converge cmux to the manifest and report
// what changed — without opening the TUI. Run it from inside cmux so its CLI
// socket is reachable.
func newReconcileCmd() *cobra.Command {
	var relaunch bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Converge cmux workspaces to the cctl manifest (one sync pass)",
		Long: `Runs a single cmux↔cctl reconcile: adopt live tmux sessions, (re)spawn
tracked sessions that lost their workspace, prune stray tabs, bind resume
commands, and close dead cctl workspaces — the same pass the TUI runs on
startup and on the R/S keys.

With --relaunch, additionally force the agent (claude --continue / codex
resume) to relaunch in EVERY tracked session, even attached ones sitting at
a shell. Use it to get claude running everywhere after sessions have exited
to a prompt; it interrupts nothing that's idle but will restart a live agent.

Must run inside cmux (its control socket only answers attached processes).
Prints a summary of what changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			res := syncCmuxState(cfg)
			relaunched := 0
			if relaunch {
				for _, e := range loadManifestEntries() {
					if _, ok := cfg.Servers[e.Server]; !ok {
						continue
					}
					if isTerminalSession(e.Session) {
						continue // a `t` shell is MEANT to be a shell — never kill it to launch an agent
					}
					if err := respawnClaude(cfg, e); err != nil {
						log().Warn("reconcile-relaunch-fail", "session", e.TmuxName, "err", err.Error())
						continue
					}
					relaunched++
				}
			}
			fmt.Fprintf(os.Stderr,
				"reconcile: adopted=%d restored=%d pruned=%d bound=%d closed=%d relaunched=%d errs=%d\n",
				res.adopted, res.restored, res.pruned, res.bound, res.closed, relaunched, res.errs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&relaunch, "relaunch", false, "force the agent to relaunch in every tracked session (even attached ones at a shell)")
	return cmd
}
