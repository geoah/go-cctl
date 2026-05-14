package cctl

import (
	"fmt"
	"strings"
)

// defaultMaxDepth is used when a RepoSource omits max_depth. 4 covers
// org/repo, gh-style provider/org/repo, and one extra level of nesting.
const defaultMaxDepth = 4

// discoveredRepo is the intermediate form produced by walking repo_sources:
// each found .git directory becomes one entry, with its relative-to-source
// path components and the source-side defaults that apply to it.
type discoveredRepo struct {
	parts         []string // path components relative to source root
	srcPath       string   // the (possibly ~-rooted) source path
	defaultBranch string
}

// discoverRepos walks every RepoSource on a server (via its remote shell) and
// returns a map of repoName → Repo. The repo name is the basename of the
// repo's path (e.g. "my-app"). When two discovered repos share a basename,
// the colliding entries get parent-prefixed (e.g. "my-org-my-app" and
// "fork-my-app") until they are unique.
//
// Failures from individual sources are surfaced as an error; discovery for
// the other sources still runs (so a misconfigured directory doesn't kill the
// whole tree).
func discoverRepos(s Server) (map[string]Repo, error) {
	if len(s.RepoSources) == 0 {
		return nil, nil
	}
	var found []discoveredRepo
	var firstErr error
	for _, src := range s.RepoSources {
		maxDepth := src.MaxDepth
		if maxDepth <= 0 {
			maxDepth = defaultMaxDepth
		}
		defBranch := src.DefaultBranch
		if defBranch == "" {
			defBranch = "main"
		}
		// cd into the source so find paths come back as relative ./a/b/.git;
		// +1 on depth because .git is one level below the repo dir we care
		// about. -prune stops descent into nested .git contents.
		cmd := fmt.Sprintf(
			`cd %s 2>/dev/null && find . -maxdepth %d -type d -name .git -prune 2>/dev/null || true`,
			shellPath(src.Path), maxDepth+1,
		)
		raw, err := runRemote(s, cmd)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", src.Path, err)
			}
			continue
		}
		srcPath := strings.TrimRight(src.Path, "/")
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// expected form: ./my-org/my-app/.git ; sometimes ./.git for a
			// repo at the source root itself.
			rel := strings.TrimPrefix(line, "./")
			rel = strings.TrimSuffix(rel, "/.git")
			rel = strings.TrimSuffix(rel, ".git")
			if rel == "" || rel == "." {
				continue
			}
			parts := strings.Split(rel, "/")
			// Skip anything whose path contains a hidden directory or a
			// known noise dir. With path: ~/ this otherwise surfaces things
			// like .nvm / .cache / .t3 / node_modules / vendor as "repos".
			if hasNoisyComponent(parts) {
				continue
			}
			found = append(found, discoveredRepo{
				parts:         parts,
				srcPath:       srcPath,
				defaultBranch: defBranch,
			})
		}
	}
	return assignRepoNames(found), firstErr
}

// hasNoisyComponent returns true if any path component is hidden
// (starts with ".") or matches a well-known dependency-cache name. Those
// dirs hold tool internals or third-party code, not user projects, and
// surfacing them as discovered repos when path: ~/ is configured pollutes
// the TUI and `cctl repos` with entries the user can't act on
// meaningfully.
//
// Kept conservative: only filter on the EXACT name, not substring. So a
// repo legitimately named "node-modules" or "vendor-x" is still found.
func hasNoisyComponent(parts []string) bool {
	for _, p := range parts {
		if strings.HasPrefix(p, ".") {
			return true
		}
		switch p {
		case "node_modules", "vendor":
			return true
		}
	}
	return false
}

// assignRepoNames turns a list of discovered repos into a name→Repo map,
// using the leaf component as the name and falling back to parent-prefixed
// names ("parent-leaf") only for entries that would otherwise collide. The
// algorithm widens the prefix one component at a time until every entry has
// a unique name. Names never contain '/' so they can be used safely as one
// segment of a tmux session name.
func assignRepoNames(found []discoveredRepo) map[string]Repo {
	if len(found) == 0 {
		return map[string]Repo{}
	}
	widths := make([]int, len(found))
	for i := range widths {
		widths[i] = 1
	}
	nameFor := func(i int) string {
		w := widths[i]
		parts := found[i].parts
		if w > len(parts) {
			w = len(parts)
		}
		return strings.Join(parts[len(parts)-w:], "-")
	}
	// Widen colliding names one component at a time. Capped at 10 iterations
	// to defend against pathological inputs (deeply nested submodules etc.).
	for range 10 {
		groups := map[string][]int{}
		for i := range found {
			groups[nameFor(i)] = append(groups[nameFor(i)], i)
		}
		changed := false
		for _, idxs := range groups {
			if len(idxs) <= 1 {
				continue
			}
			for _, i := range idxs {
				if widths[i] < len(found[i].parts) {
					widths[i]++
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	out := map[string]Repo{}
	for i, d := range found {
		name := nameFor(i)
		rel := strings.Join(d.parts, "/")
		out[name] = Repo{
			Path:          d.srcPath + "/" + rel,
			DefaultBranch: d.defaultBranch,
		}
	}
	return out
}
