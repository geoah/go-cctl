package cctl

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// newDoctorCmd builds `cctl doctor [server...]`. It runs the same checks
// the TUI quietly performs and prints a per-server report so the user can
// see WHY a repo / session is misbehaving without having to drive cctl
// through a failing flow first.
//
// Checks per server:
//   - transport: can we run `true` over ssh / locally?
//   - tools: git / tmux / bash present + their versions
//   - configured repos: does the path exist? is it a git repo? if missing,
//     find directories named the same under $HOME so we can suggest fixes.
//   - active sessions: list cctl/* sessions and flag any whose worktree no
//     longer exists on disk (the kind of orphan that confused us today).
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [server...]",
		Short: "Diagnose transport, tool, repo, and session health per server",
		Long: `Run a battery of read-only checks against each configured server and
print a single report. Useful when a TUI row looks wrong, when "fix
repos.<name>.path" errors keep firing, or when a cctl session can no
longer find its worktree.

With no arguments, runs against every server in ~/.cctl.yaml. Pass one
or more server names to scope it down.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			targets := args
			if len(targets) == 0 {
				for name := range cfg.Servers {
					targets = append(targets, name)
				}
				sort.Strings(targets)
			}
			var firstErr error
			for i, name := range targets {
				if i > 0 {
					fmt.Println()
				}
				if err := runDoctor(cfg, name); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		},
	}
}

func runDoctor(cfg *Config, name string) error {
	srv, ok := cfg.Servers[name]
	if !ok {
		fmt.Printf("server %q: not in config\n", name)
		return fmt.Errorf("unknown server %q", name)
	}
	header := name
	switch {
	case srv.Local:
		header += "  (local)"
	case srv.User != "" && srv.Host != "":
		header += fmt.Sprintf("  (%s@%s)", srv.User, srv.Host)
	case srv.Host != "":
		header += "  (" + srv.Host + ")"
	}
	fmt.Println(header)
	fmt.Println(strings.Repeat("─", len(header)))

	// 1. transport
	if _, err := runRemote(srv, "true"); err != nil {
		fmt.Printf("  transport: ❌ %v\n", err)
		fmt.Println("  (skipping remaining checks)")
		return err
	}
	fmt.Println("  transport: ✓")

	// 2. tools — single round-trip
	probe := `
echo "GIT:$(command -v git 2>/dev/null) $(git --version 2>/dev/null | head -1)"
echo "TMUX:$(command -v tmux 2>/dev/null) $(tmux -V 2>/dev/null)"
echo "BASH:$(command -v bash 2>/dev/null) $(bash --version 2>/dev/null | head -1)"
`
	if out, err := runRemote(srv, probe); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			kv := strings.SplitN(line, ":", 2)
			if len(kv) != 2 {
				continue
			}
			val := strings.TrimSpace(kv[1])
			tool := strings.ToLower(kv[0])
			if val == "" || strings.HasPrefix(val, " ") {
				fmt.Printf("  %-9s ❌ not found\n", tool+":")
				continue
			}
			fmt.Printf("  %-9s ✓ %s\n", tool+":", val)
		}
	} else {
		fmt.Printf("  tools: ❌ %v\n", err)
	}

	// 3. repos — reuse the production helper so this report tracks reality.
	repos := mergeRepos(srv)
	discovered, _ := discoverRepos(srv)
	for k, v := range discovered {
		if _, taken := repos[k]; !taken {
			repos[k] = v
		}
	}
	if len(repos) == 0 {
		fmt.Println("  repos:     (none configured / discovered)")
		return nil
	}
	_, statuses, err := listAllWorktrees(srv, repos)
	if err != nil {
		fmt.Printf("  repos:     ❌ %v\n", err)
		return err
	}
	fmt.Println("  repos:")
	names := make([]string, 0, len(repos))
	for n := range repos {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := repos[n]
		status, ok := statuses[n]
		if !ok {
			status = RepoStatusOK
		}
		marker := "✓"
		switch status {
		case RepoStatusMissing:
			marker = "❌"
		case RepoStatusNoGit:
			marker = "⚠"
		}
		fmt.Printf("    %s %-22s %s  [%s]\n", marker, n, r.Path, status)
		if status == RepoStatusMissing {
			suggestMissingRepo(srv, n)
		}
	}

	// 4. sessions — flag ones whose worktree no longer exists on disk.
	sessions, err := listSessions(name, srv)
	if err != nil {
		fmt.Printf("  sessions:  ❌ %v\n", err)
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("  sessions:  (none)")
		return nil
	}
	fmt.Println("  sessions:")
	for _, s := range sessions {
		repo, repoOK := repos[s.Repo]
		marker := "✓"
		note := ""
		if !repoOK {
			marker = "⚠"
			note = "  (repo not in config)"
		} else {
			wt := worktreePath(stringOrDefault(cfg.Defaults.WorktreeBase, "~/worktrees"), s.Repo, s.Worktree)
			out, _ := runRemote(srv, fmt.Sprintf(`test -e %s && echo present || echo gone`, shellPath(wt)))
			if strings.TrimSpace(out) != "present" {
				marker = "⚠"
				note = "  (worktree dir gone: " + wt + ")"
			}
			// Also check parent repo path
			if statuses[s.Repo] == RepoStatusMissing {
				marker = "❌"
				note = "  (repo " + repo.Path + " gone — orphan session)"
			}
		}
		attached := "detached"
		if s.Attached {
			attached = "attached"
		}
		fmt.Printf("    %s %s  (repo=%s wt=%s)  %s%s\n",
			marker, s.Session, s.Repo, s.Worktree, attached, note)
	}
	return nil
}

// suggestMissingRepo searches the server's $HOME for dirs named after the
// repo so doctor can offer a "did you mean..." hint. Best-effort; silent if
// nothing useful is found.
func suggestMissingRepo(srv Server, name string) {
	cmd := fmt.Sprintf(`find "$HOME" -maxdepth 6 -type d -name %s 2>/dev/null | head -8`, shellQuote(name))
	out, err := runRemote(srv, cmd)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return
	}
	fmt.Println("      candidates on this host:")
	for _, line := range lines {
		fmt.Printf("        • %s\n", line)
	}
	fmt.Println("      → update servers.<server>.repos." + name + ".path or clone there")
}

func stringOrDefault(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}

// guard unused import if helpers shift around in tests
var _ = os.Stderr
