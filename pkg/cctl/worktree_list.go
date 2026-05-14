package cctl

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Worktree is one entry from `git worktree list --porcelain` on the server,
// enriched with a cctl-friendly Name we can use as a tmux session-name
// segment.
type Worktree struct {
	Repo   string // the cctl repo name this worktree belongs to
	Name   string // identifier safe for tmux session names; "main" for the primary checkout
	Path   string // remote absolute path (with $HOME expanded by the remote shell when applicable)
	Branch string // current branch, or empty for detached HEAD
	IsMain bool   // true for the original clone (i.e. the repo.Path itself)
}

// RepoStatus is the on-server health of a configured repo path.
type RepoStatus string

const (
	RepoStatusOK      RepoStatus = "OK"      // path exists and is a git repo
	RepoStatusMissing RepoStatus = "MISSING" // path does not exist
	RepoStatusNoGit   RepoStatus = "NO_GIT"  // path exists but has no .git
)

// String renders the status for the TUI / CLI.
func (r RepoStatus) String() string { return string(r) }

// listAllWorktrees runs `git worktree list --porcelain` for every repo on a
// server in a single shell invocation, separating output blocks with a
// sentinel marker. One SSH round-trip for N repos.
//
// Returns:
//   - worktrees per repo
//   - status per repo (so the TUI can surface MISSING / NO_GIT without
//     each call site having to re-probe)
//
// The combined script emits, per repo:
//
//	@@CCTL_REPO=<name>
//	@@CCTL_STATUS=<OK|MISSING|NO_GIT>
//	<git worktree list --porcelain output, only when status is OK>
//
// Previously a missing path was silently absorbed by `git ... 2>/dev/null
// || true`, so the TUI happily showed configured-but-absent repos as
// healthy. That's how a 10-hour-old tmux session on a wiped repo could
// haunt the tree.
func listAllWorktrees(s Server, repos map[string]Repo) (map[string][]Worktree, map[string]RepoStatus, error) {
	if len(repos) == 0 {
		return nil, nil, nil
	}
	const sentinel = "@@CCTL_REPO="
	const statusKey = "@@CCTL_STATUS="
	names := make([]string, 0, len(repos))
	for n := range repos {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		r := repos[name]
		fmt.Fprintf(&b, "echo %s%s\n", sentinel, name)
		fmt.Fprintf(&b, `if [ ! -d %[1]s ]; then echo %[2]sMISSING; elif [ ! -e %[1]s/.git ]; then echo %[2]sNO_GIT; else echo %[2]sOK; git -C %[1]s worktree list --porcelain 2>/dev/null || true; fi`+"\n",
			shellPath(r.Path), statusKey)
	}
	raw, err := runRemote(s, b.String())
	if err != nil {
		return nil, nil, err
	}
	wts, statuses := parseAllWorktrees(raw, sentinel, statusKey, repos)
	return wts, statuses, nil
}

// parseAllWorktrees splits the combined output by the sentinel marker and
// hands each block to parseWorktreePorcelain. Status lines (@@CCTL_STATUS=)
// are stripped from the per-repo buffer and collected into a second map so
// the caller can tell why a repo has zero worktrees (MISSING vs NO_GIT vs
// just-an-empty-repo-which-shouldn't-happen).
//
// Backward-tolerant: if the input doesn't carry @@CCTL_STATUS lines (older
// scripts, or stub data in tests), repos default to RepoStatusOK so the
// existing behavior is preserved.
func parseAllWorktrees(raw, sentinel, statusKey string, repos map[string]Repo) (map[string][]Worktree, map[string]RepoStatus) {
	out := map[string][]Worktree{}
	statuses := map[string]RepoStatus{}
	var current string
	var buf strings.Builder
	flush := func() {
		if current == "" {
			return
		}
		repo, ok := repos[current]
		if !ok {
			return
		}
		if _, set := statuses[current]; !set {
			statuses[current] = RepoStatusOK
		}
		out[current] = annotateWorktrees(parseWorktreePorcelain(buf.String()), current, repo)
		buf.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, sentinel) {
			flush()
			current = strings.TrimPrefix(line, sentinel)
			continue
		}
		if statusKey != "" && strings.HasPrefix(line, statusKey) && current != "" {
			statuses[current] = RepoStatus(strings.TrimPrefix(line, statusKey))
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return out, statuses
}

// annotateWorktrees normalises Name + Repo + IsMain for a slice of parsed
// worktrees belonging to one repo.
func annotateWorktrees(wts []Worktree, repoName string, repo Repo) []Worktree {
	mainPath := strings.TrimRight(repo.Path, "/")
	used := map[string]int{}
	for i := range wts {
		wts[i].Repo = repoName
		isMain := samePath(wts[i].Path, mainPath)
		wts[i].IsMain = isMain
		name := "main"
		if !isMain {
			name = path.Base(wts[i].Path)
			if name == "" || name == "." || name == "/" {
				name = fmt.Sprintf("wt%d", i)
			}
		}
		used[name]++
		if used[name] > 1 {
			name = fmt.Sprintf("%s-%d", name, used[name]-1)
		}
		wts[i].Name = name
	}
	return wts
}

// parseWorktreePorcelain parses the output of `git worktree list --porcelain`.
// Each worktree is a block of `worktree <path>`, optional `HEAD <sha>`,
// and either `branch <ref>` (attached) or `detached`. Blocks are separated by
// blank lines.
func parseWorktreePorcelain(raw string) []Worktree {
	var out []Worktree
	var cur Worktree
	flush := func() {
		if cur.Path != "" {
			out = append(out, cur)
		}
		cur = Worktree{}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "worktree":
			flush()
			cur.Path = v
		case "branch":
			cur.Branch = strings.TrimPrefix(v, "refs/heads/")
		case "detached":
			cur.Branch = ""
		}
	}
	flush()
	return out
}

// samePath does a tolerant comparison: trims trailing slashes and matches on
// path-boundary suffixes (git emits absolute paths, cctl stores ~-rooted).
//
// **Critical**: the ~-rooted form must match its expanded form. This is
// what annotateWorktrees uses to decide which worktree is the main checkout,
// and the "main" label is the only thing standing between a `dd` keystroke
// and `rm -rf` of the actual repo. A previous version did literal-suffix
// matching only — so `repo.Path="~/myrepo"` vs `git emits="/home/me/myrepo"`
// returned false, the main checkout was labeled with the repo name instead
// of "main", the dd "is this main?" guard didn't fire, and a real user's
// repo was deleted. Don't simplify without keeping the ~/ → /home/<user>/
// semantics.
func samePath(a, b string) bool {
	a = strings.TrimRight(a, "/")
	b = strings.TrimRight(b, "/")
	if a == b {
		return true
	}
	// Tilde-vs-absolute: one side is "~/foo", the other is anything ending
	// in "/foo" at a path boundary. We don't know the remote $HOME here,
	// but git always emits absolute paths that end in the same basename
	// chain as the config form, so suffix-match the tail after "~/".
	if rest, ok := stripTildeSlash(b); ok && pathBoundaryHasSuffix(a, rest) {
		return true
	}
	if rest, ok := stripTildeSlash(a); ok && pathBoundaryHasSuffix(b, rest) {
		return true
	}
	// Generic suffix-at-boundary for the rare case where one side is a
	// relative-looking absolute path. Kept for parity with the old logic.
	if strings.HasSuffix(a, b) || strings.HasSuffix(b, a) {
		shorter, longer := a, b
		if len(b) < len(a) {
			shorter, longer = b, a
		}
		if longer == shorter {
			return true
		}
		idx := len(longer) - len(shorter)
		if idx > 0 && longer[idx-1] == '/' {
			return true
		}
	}
	return false
}

func stripTildeSlash(p string) (string, bool) {
	if strings.HasPrefix(p, "~/") {
		return p[2:], true
	}
	if p == "~" {
		return "", true
	}
	return "", false
}

// pathBoundaryHasSuffix is `strings.HasSuffix` that also requires the
// preceding character to be '/' so "/a/foobar" doesn't match suffix "bar".
func pathBoundaryHasSuffix(s, suffix string) bool {
	if suffix == "" {
		// "~" matches any absolute path that points at a home (we can't
		// know which without expansion); reject to be safe.
		return false
	}
	if !strings.HasSuffix(s, suffix) {
		return false
	}
	if len(s) == len(suffix) {
		return true
	}
	return s[len(s)-len(suffix)-1] == '/'
}
