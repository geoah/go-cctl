package cctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level on-disk schema for ~/.cctl.yaml.
type Config struct {
	Defaults Defaults          `yaml:"defaults"`
	Servers  map[string]Server `yaml:"servers"`

	// repoCache memoises discoverRepos() per server for the lifetime of the
	// process so the TUI / repeated resolve() calls don't re-shell each time.
	repoCache map[string]map[string]Repo
}

type Defaults struct {
	BranchPrefix string   `yaml:"branch_prefix"`
	WorktreeBase string   `yaml:"worktree_base"`
	ClaudeFlags  []string `yaml:"claude_flags"`
	Mosh         *bool    `yaml:"mosh"`
	Shell        string   `yaml:"shell"`
	LogLevel     string   `yaml:"log_level"` // debug, info (default), warn, error
	// Spawn chooses the terminal-spawn provider when launching sessions
	// from the TUI: "auto" (default — detect from $TERM_PROGRAM), or one
	// of "ghostty", "cmux", "wezterm", "kitty", "iterm2", "inline".
	Spawn string `yaml:"spawn"`
	// WorktreePostCreate is a list of shell commands to run inside a
	// freshly-created worktree directory, right after `git worktree add`
	// succeeds and before the tmux session launches. Useful for things
	// like `mise trust`, `direnv allow`, `pre-commit install`, etc.
	// Each command is run via `sh -c "<cmd>"` and a failure is logged
	// but does NOT abort the session — these are conveniences, not
	// gating checks.
	WorktreePostCreate []string `yaml:"worktree_post_create"`
}

type Server struct {
	Host        string          `yaml:"host"`
	User        string          `yaml:"user"`
	Port        int             `yaml:"port"`
	SSHKey      string          `yaml:"ssh_key"`
	SSHOpts     []string        `yaml:"ssh_opts"`
	Mosh        *bool           `yaml:"mosh"`
	Local       bool            `yaml:"local"` // run commands locally instead of via ssh/mosh
	RepoSources []RepoSource    `yaml:"repo_sources"`
	Repos       map[string]Repo `yaml:"repos"`
}

// RepoSource is a search root: cctl walks it up to MaxDepth looking for git
// repos and surfaces each as a Repo named by its path relative to the source.
type RepoSource struct {
	Path          string `yaml:"path"`
	MaxDepth      int    `yaml:"max_depth"`
	DefaultBranch string `yaml:"default_branch"`
}

type Repo struct {
	Path          string `yaml:"path"`
	DefaultBranch string `yaml:"default_branch"`
	WorktreeBase  string `yaml:"worktree_base"`
}

// Resolved is a server+repo pair with defaults already merged in.
type Resolved struct {
	ServerName         string
	Server             Server
	RepoName           string
	Repo               Repo
	BranchPrefix       string
	WorktreeBase       string
	ClaudeFlags        []string
	UseMosh            bool
	DefaultBranch      string
	WorktreePostCreate []string
}

func loadConfig() (*Config, string, error) {
	path, err := resolveConfigPath()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, path, fmt.Errorf("config not found at %s — create one (run `cctl init` to auto-generate or `cctl config sample` for an example)", path)
		}
		return nil, path, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Servers) == 0 {
		return nil, path, fmt.Errorf("%s: no servers defined", path)
	}
	setLogLevel(c.Defaults.LogLevel)
	log().Info("config-loaded", "path", path, "servers", len(c.Servers))
	return &c, path, nil
}

func resolveConfigPath() (string, error) {
	if configPath != "" {
		return expandPath(configPath), nil
	}
	if env := os.Getenv("CCTL_CONFIG"); env != "" {
		return expandPath(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cctl.yaml"), nil
}

// expandPath expands a leading ~ to the user's home directory.
// Remote paths (those starting with ~ that we send over SSH) are left alone —
// the remote shell expands them. Only call this for *local* paths.
func expandPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolve picks a server and repo. repoName may be empty when the server has
// exactly one repo (discovered + explicit combined); otherwise it must be
// passed in via the <server>/<repo> CLI form or the TUI.
func (c *Config) resolve(serverName, repoName string) (*Resolved, error) {
	srv, ok := c.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("unknown server %q (configured: %s)", serverName, strings.Join(c.serverNames(), ", "))
	}
	allRepos, err := c.repos(serverName)
	if err != nil {
		return nil, err
	}
	if repoName == "" && len(allRepos) == 1 {
		for n := range allRepos {
			repoName = n
		}
	}
	repo, ok := allRepos[repoName]
	if !ok {
		if repoName == "" {
			return nil, fmt.Errorf("server %q has %d repos; specify one with %s/<repo> (available: %s)", serverName, len(allRepos), serverName, strings.Join(repoNames(allRepos), ", "))
		}
		return nil, fmt.Errorf("server %q has no repo %q (available: %s)", serverName, repoName, strings.Join(repoNames(allRepos), ", "))
	}

	useMosh := true
	if c.Defaults.Mosh != nil {
		useMosh = *c.Defaults.Mosh
	}
	if srv.Mosh != nil {
		useMosh = *srv.Mosh
	}
	if srv.Local {
		useMosh = false
	}

	wtBase := repo.WorktreeBase
	if wtBase == "" {
		wtBase = c.Defaults.WorktreeBase
	}
	if wtBase == "" {
		wtBase = "~/worktrees"
	}

	branchPrefix := c.Defaults.BranchPrefix
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	return &Resolved{
		ServerName:         serverName,
		Server:             srv,
		RepoName:           repoName,
		Repo:               repo,
		BranchPrefix:       branchPrefix,
		WorktreeBase:       wtBase,
		ClaudeFlags:        c.Defaults.ClaudeFlags,
		UseMosh:            useMosh,
		DefaultBranch:      defaultBranch,
		WorktreePostCreate: append([]string(nil), c.Defaults.WorktreePostCreate...),
	}, nil
}

func (c *Config) serverNames() []string {
	out := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		out = append(out, n)
	}
	return out
}

// repos returns the merged map of repos for a server: discovered (via
// repo_sources) plus explicit (via repos:), with explicit entries winning on
// name collision. Discovery is cached for the lifetime of the *Config.
func (c *Config) repos(serverName string) (map[string]Repo, error) {
	srv, ok := c.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("unknown server %q", serverName)
	}
	out := map[string]Repo{}
	if len(srv.RepoSources) > 0 {
		if c.repoCache == nil {
			c.repoCache = map[string]map[string]Repo{}
		}
		cached, ok := c.repoCache[serverName]
		if !ok {
			d, err := discoverRepos(srv)
			if err != nil {
				return nil, fmt.Errorf("discover repos on %s: %w", serverName, err)
			}
			c.repoCache[serverName] = d
			cached = d
		}
		for k, v := range cached {
			out[k] = v
		}
	}
	for k, v := range srv.Repos {
		out[k] = v
	}
	return out, nil
}

// invalidateRepoCache forces the next repos() call to re-run discovery.
//
//nolint:unused // wired in by the next TUI refresh task
func (c *Config) invalidateRepoCache(serverName string) {
	if c.repoCache == nil {
		return
	}
	delete(c.repoCache, serverName)
}

func repoNames(m map[string]Repo) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	return out
}

// parseTarget splits a "server" or "server/repo" CLI argument.
func parseTarget(s string) (server, repo string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
