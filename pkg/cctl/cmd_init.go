package cctl

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var (
		// config bootstrap (repo scan → starter ~/.cctl.yaml)
		bootstrap  bool
		force      bool
		searchRoot string
		maxScan    int
		// tool/server selection (interactive by default; these drive it headless)
		tools      []string
		servers    []string
		allServers bool
		restart    bool
		yes        bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure tools (tmux/ghostty/mosh/claude/cmux) across your servers",
		Long: `init configures the tools cctl relies on — tmux, ghostty, mosh, claude,
and cmux — so scroll/select/copy/paste and agent orchestration work across
local and mosh-attached sessions.

Run with no flags for an interactive picker: choose which tools, which
servers (local + configured remotes), and whether to restart all tracked
sessions afterward. ghostty and cmux are always local (they're your
laptop's terminal + multiplexer); tmux/mosh/claude apply to the servers you
pick, skipping any that are unreachable.

Non-interactive (for scripts / no-TTY), drive it with flags:
  cctl init --yes                          # all tools, local only
  cctl init --tools tmux,mosh --all-servers
  cctl init --tools tmux --servers workspace --restart

init needs an existing ~/.cctl.yaml. To generate a starter by scanning your
repos, run: cctl init --bootstrap`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if bootstrap {
				return generateConfig(force, searchRoot, maxScan)
			}
			cfg, _, err := loadConfig()
			if err != nil {
				path, _ := resolveConfigPath()
				return fmt.Errorf("no usable config (%v).\n\n"+
					"cctl init configures tools for the servers in %s, so it needs that file first.\n"+
					"Generate a starter by scanning your repos:  cctl init --bootstrap",
					err, path)
			}

			// Interactive unless a selection flag is set or stdin isn't a TTY.
			flagDriven := yes || allServers || restart ||
				len(tools) > 0 || len(servers) > 0
			if !flagDriven && !stdinIsTTY() {
				// No TTY and no explicit selection: refuse rather than silently
				// reconfiguring tmux/ghostty/cmux with the all-tools default
				// (e.g. a piped or CI invocation).
				return fmt.Errorf("no TTY for the interactive picker — pass --yes (all tools, local) or --tools/--servers to run headless")
			}
			var sel initSelection
			if flagDriven || !stdinIsTTY() {
				sel, err = selectionFromFlags(cfg, tools, servers, allServers, restart)
			} else {
				sel, err = runInitWizard(cfg)
			}
			if err != nil {
				return err
			}
			if sel.canceled {
				fmt.Fprintln(os.Stderr, "init: canceled.")
				return nil
			}
			return runInit(cfg, sel)
		},
	}
	cmd.Flags().BoolVar(&bootstrap, "bootstrap", false, "scan for git repos and write a starter ~/.cctl.yaml, then exit")
	cmd.Flags().BoolVar(&force, "force", false, "with --bootstrap: overwrite an existing config file")
	cmd.Flags().StringVar(&searchRoot, "root", "", "with --bootstrap: directory to scan (default: $HOME)")
	cmd.Flags().IntVar(&maxScan, "max-depth", 5, "with --bootstrap: maximum directory depth to scan for .git")
	cmd.Flags().StringSliceVar(&tools, "tools", nil, "headless: tools to configure (tmux,ghostty,mosh,claude,cmux; default: all)")
	cmd.Flags().StringSliceVar(&servers, "servers", nil, "headless: server names to apply to (use 'local'; default: local)")
	cmd.Flags().BoolVar(&allServers, "all-servers", false, "headless: apply to local + every configured remote")
	cmd.Flags().BoolVar(&restart, "restart", false, "restart all tracked sessions after applying")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "headless: apply flag selections (or defaults) without prompting")
	cmd.MarkFlagsMutuallyExclusive("servers", "all-servers")
	return cmd
}

// generateConfig is the `cctl init --bootstrap` path: scan for git repos and
// write a starter ~/.cctl.yaml. Split out from the interactive flow so the two
// concerns don't tangle.
func generateConfig(force bool, searchRoot string, maxScan int) error {
	path, err := resolveConfigPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists — pass --force to overwrite", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve $HOME: %w", err)
	}
	root := searchRoot
	if root == "" {
		root = home
	}
	root = expandPath(root)

	fmt.Fprintf(os.Stderr, "scanning %s for git repos (maxdepth=%d)...\n", root, maxScan)
	repos, err := findGitRepos(root, maxScan)
	if err != nil {
		return fmt.Errorf("scan %s: %w", root, err)
	}
	if len(repos) == 0 {
		return fmt.Errorf("no git repos found under %s — pass --root to point elsewhere", root)
	}

	source, count, depth := pickRepoSource(home, repos)
	sourceForConfig := tildify(home, source)

	username := "me"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}

	fmt.Fprintf(os.Stderr, "found %d repo(s); using %s as repo_source (%d repos at depth %d)\n",
		len(repos), source, count, depth)

	content := renderInitConfig(username, sourceForConfig, depth)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)

	// Show a handful of discovered repos so the user can sanity-check.
	previewRepos(os.Stderr, source, repos, 8)
	return nil
}

// stdinIsTTY reports whether stdin is an interactive terminal. When it isn't
// (piped, cron, CI), the wizard can't run, so init falls back to flag/defaults.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

var allInitTools = []string{"tmux", "ghostty", "mosh", "claude", "cmux"}

func validInitTool(t string) bool {
	for _, v := range allInitTools {
		if v == t {
			return true
		}
	}
	return false
}

// selectionFromFlags builds an initSelection from the headless flags, applying
// defaults (all tools, local only) when a dimension is left unset.
func selectionFromFlags(cfg *Config, tools, servers []string, allServers, restart bool) (initSelection, error) {
	sel := initSelection{restart: restart}
	if len(tools) == 0 {
		sel.tools = append(sel.tools, allInitTools...)
	} else {
		for _, t := range tools {
			t = strings.ToLower(strings.TrimSpace(t))
			if !validInitTool(t) {
				return sel, fmt.Errorf("unknown tool %q (valid: %s)", t, strings.Join(allInitTools, ", "))
			}
			sel.tools = append(sel.tools, t)
		}
	}
	switch {
	case allServers:
		sel.localSelected = true
		sel.remotes = sortedRemoteNames(cfg)
	case len(servers) == 0:
		sel.localSelected = true // default: local only
	default:
		for _, s := range servers {
			s = strings.TrimSpace(s)
			if s == "" || s == "local" {
				sel.localSelected = true
				continue
			}
			srv, ok := cfg.Servers[s]
			if !ok {
				return sel, fmt.Errorf("unknown server %q", s)
			}
			if srv.Local {
				sel.localSelected = true
			} else {
				sel.remotes = append(sel.remotes, s)
			}
		}
	}
	return sel, nil
}

// runInit applies the selected tools to the selected servers, then optionally
// restarts all tracked sessions. Per-server tool failures (e.g. an unreachable
// remote) are logged and skipped so one dead host doesn't abort the rest.
func runInit(cfg *Config, sel initSelection) error {
	has := func(tool string) bool {
		for _, t := range sel.tools {
			if t == tool {
				return true
			}
		}
		return false
	}

	if has("tmux") {
		applyServerScopedTool(cfg, sel, "tmux",
			func() error { return applyManaged("tmux", "~/.tmux.conf", tmuxManagedBody, "") },
			func(name string, _ Server) error { return applyManaged("tmux", "~/.tmux.conf", tmuxManagedBody, name) })
	}
	if has("ghostty") {
		if err := applyManaged("ghostty", "~/.config/ghostty/config", ghostlyManagedBody, ""); err != nil {
			fmt.Fprintf(os.Stderr, "ghostty (local): %v\n", err)
		}
	}
	if has("mosh") {
		applyServerScopedTool(cfg, sel, "mosh",
			func() error { reportMoshLocal(); return nil },
			func(name string, srv Server) error { reportMoshRemote(name, srv); return nil })
	}
	if has("claude") {
		applyServerScopedTool(cfg, sel, "claude",
			func() error { reportClaudeLocal(); return nil },
			func(name string, srv Server) error { reportClaudeRemote(name, srv); return nil })
	}
	if has("cmux") {
		if err := runInitCmux(false, false, false, false); err != nil {
			fmt.Fprintf(os.Stderr, "cmux (local): %v\n", err)
		}
	}

	if sel.restart {
		fmt.Fprintln(os.Stderr, "\ninit: restarting all tracked sessions...")
		res := restartAllTrackedSessions(cfg)
		fmt.Fprintf(os.Stderr, "init: reloaded config on %d/%d server(s), killed %d session(s)",
			res.reloaded, res.servers, res.killed)
		if res.unreachable > 0 {
			fmt.Fprintf(os.Stderr, " (%d server(s) unreachable, %d session(s) left tracked)", res.unreachable, res.skipped)
		}
		fmt.Fprintln(os.Stderr, ".")
		if !stdinIsTTY() {
			fmt.Fprintln(os.Stderr, "init: no TTY — sessions killed but not revived. Run `cctl` to reconcile cmux ↔ tmux and revive them.")
			return nil
		}
		// Revive through the TUI's startup reconcile — NOT a bare
		// syncCmuxState(cfg) here. The correct cmux↔tmux sync must wait until
		// every server's real session list has loaded, and it also closes the
		// now-dead workspaces and regroups. That sequencing lives in the TUI
		// (maybeStartupSync); reconciling immediately after the kills — before
		// cmux has caught up — leaves stale/duplicate workspaces and empty
		// groups. This is exactly what `cctl --restart-all` does.
		fmt.Fprintln(os.Stderr, "init: opening cctl to reconcile cmux ↔ tmux and revive sessions...")
		return runTUI()
	}
	return nil
}

// applyServerScopedTool runs a tool's local step (if local is selected) and its
// remote step for each selected remote, logging and continuing past per-server
// failures.
func applyServerScopedTool(cfg *Config, sel initSelection, tool string, local func() error, remote func(name string, srv Server) error) {
	if sel.localSelected {
		if err := local(); err != nil {
			fmt.Fprintf(os.Stderr, "%s (local): %v\n", tool, err)
		}
	}
	for _, name := range sel.remotes {
		srv, ok := cfg.Servers[name]
		if !ok {
			continue
		}
		if err := remote(name, srv); err != nil {
			fmt.Fprintf(os.Stderr, "%s (%s): %v — skipped\n", tool, name, err)
		}
	}
}

// findGitRepos returns absolute paths of every git repo under root, up to
// maxDepth levels deep. .git directories are pruned so we don't descend into
// repo internals.
func findGitRepos(root string, maxDepth int) ([]string, error) {
	if _, err := exec.LookPath("find"); err == nil {
		// +1 because .git is one level below the repo dir.
		out, err := exec.Command("find", root, "-maxdepth", fmt.Sprint(maxDepth+1), "-type", "d", "-name", ".git", "-prune").Output()
		if err == nil {
			return parseFindOutput(string(out)), nil
		}
		// fall through to filepath.Walk on error
	}
	return walkGitRepos(root, maxDepth)
}

func parseFindOutput(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		repo := strings.TrimSuffix(line, "/.git")
		repo = strings.TrimSuffix(repo, ".git")
		if repo == "" {
			continue
		}
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// walkGitRepos is a pure-Go fallback for systems without find(1).
func walkGitRepos(root string, maxDepth int) ([]string, error) {
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if !d.IsDir() {
			return nil
		}
		depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
		if depth > maxDepth+1 {
			return filepath.SkipDir
		}
		base := d.Name()
		// Hidden dirs (except the user's home itself) are usually noise.
		if depth > 0 && base != ".git" && strings.HasPrefix(base, ".") {
			return filepath.SkipDir
		}
		if base == "node_modules" || base == "vendor" {
			return filepath.SkipDir
		}
		if base == ".git" {
			out = append(out, filepath.Dir(path))
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// pickRepoSource chooses the directory under home that best summarizes
// where the user keeps their repos. depth is measured in path components
// below home (0 = $HOME itself).
//
// Naive "highest count wins" picks $HOME for any layout that has one
// stray repo at the top (because $HOME covers literally every repo).
// The user almost always wants the deepest *cluster* — e.g. they have
// 7 repos under ~/src/github.com plus a stray ~/scratch, and the right
// answer is ~/src/github.com, not $HOME.
//
// We score each candidate ancestor as `count * (depth + 1)`. That keeps
// deeper ancestors competitive even with slightly lower counts, while
// still falling back to $HOME when no real cluster exists (single repo
// at $HOME/myrepo → only candidate is $HOME, score 1, picked).
// Tie-break: deeper wins, then lexicographic for stability.
func pickRepoSource(home string, repos []string) (path string, count, depth int) {
	type cand struct{ count, depth int }
	counts := map[string]cand{}
	for _, r := range repos {
		rel, err := filepath.Rel(home, r)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		// candidates are ancestors at depths 0..len(parts)-1 below home.
		for d := range parts {
			var anc string
			if d == 0 {
				anc = home
			} else {
				anc = filepath.Join(home, filepath.Join(parts[:d]...))
			}
			c := counts[anc]
			c.count++
			c.depth = d
			counts[anc] = c
		}
	}
	bestScore := 0
	for p, c := range counts {
		score := c.count * (c.depth + 1)
		better := score > bestScore ||
			(score == bestScore && c.depth > depth) ||
			(score == bestScore && c.depth == depth && p < path)
		if better {
			path, count, depth, bestScore = p, c.count, c.depth, score
		}
	}
	return path, count, depth
}

// tildify rewrites a home-relative absolute path back to ~-form for config
// readability. Paths outside $HOME are returned unchanged.
func tildify(home, p string) string {
	if p == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

func previewRepos(w *os.File, root string, repos []string, n int) {
	if len(repos) == 0 {
		return
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "discovered repos:")
	for i, r := range repos {
		if i >= n {
			fmt.Fprintf(w, "  ... and %d more\n", len(repos)-n)
			break
		}
		display := r
		if rel, err := filepath.Rel(root, r); err == nil && !strings.HasPrefix(rel, "..") {
			display = rel
		}
		fmt.Fprintf(w, "  • %s\n", display)
	}
}

// renderInitConfig is a tiny templater for the starter config so we don't drag
// in text/template for two substitutions.
func renderInitConfig(username, source string, depthFromHome int) string {
	// max_depth is how far below source we look for .git. We allow at least
	// two extra levels beyond what we already saw (provider/org/repo style).
	maxDepth := 3
	if depthFromHome <= 1 {
		maxDepth = 4
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# ~/.cctl.yaml — generated by `cctl init`\n")
	b.WriteString("defaults:\n")
	fmt.Fprintf(&b, "  branch_prefix: %s          # worktree branch is <branch_prefix>/<session>\n", username)
	b.WriteString("  worktree_base: ~/worktrees    # local-side path; per-repo override allowed\n")
	b.WriteString("  claude_flags: [\"--dangerously-skip-permissions\"]\n")
	b.WriteString("  # codex_flags: []            # flags for `cctl codex` (OpenAI Codex CLI)\n")
	b.WriteString("  # agent: codex                # which agent the TUI launches (claude|codex); override per server/repo\n")
	b.WriteString("  mosh: true                    # ignored for local servers\n\n")
	b.WriteString("servers:\n")
	b.WriteString("  local:\n")
	b.WriteString("    local: true\n")
	b.WriteString("    repo_sources:\n")
	fmt.Fprintf(&b, "      - path: %s\n", source)
	fmt.Fprintf(&b, "        max_depth: %d\n", maxDepth)
	b.WriteString("        default_branch: main\n\n")
	b.WriteString("    # repos: optional explicit overrides — entries here win over discovery.\n")
	b.WriteString("    # repos:\n")
	b.WriteString("    #   custom:\n")
	b.WriteString("    #     path: ~/some/other/place\n")
	b.WriteString("    #     default_branch: develop\n\n")
	b.WriteString("  # Add remote servers below. Example:\n")
	b.WriteString("  # workspace:\n")
	b.WriteString("  #   host: 34.172.11.54\n")
	b.WriteString("  #   user: " + username + "\n")
	b.WriteString("  #   ssh_key: ~/.ssh/id_ed25519\n")
	b.WriteString("  #   repo_sources:\n")
	b.WriteString("  #     - path: ~/src\n")
	b.WriteString("  #       max_depth: 3\n")
	return b.String()
}
