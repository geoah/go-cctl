package cctl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Unit tests drive probedMsg against fake hosts; never let that spawn a
// real ssh tail. Integration tests don't exercise the bridge.
func init() {
	newNotifyWatcher = func(serverName string, srv Server, replayStatus bool) *notifyWatcher {
		return &notifyWatcher{server: serverName, dead: true}
	}
}

// ---- worktree porcelain parser --------------------------------------------

func TestParseWorktreePorcelain_MultipleWorktrees(t *testing.T) {
	in := strings.Join([]string{
		"worktree /Users/me/src/github.com/me/rxtx.dev",
		"HEAD abc123",
		"branch refs/heads/main",
		"",
		"worktree /Users/me/worktrees/rxtx.dev/audit",
		"HEAD def456",
		"branch refs/heads/me/audit",
		"",
		"worktree /Users/me/worktrees/rxtx.dev/detached",
		"HEAD 999fff",
		"detached",
		"",
	}, "\n")
	wts := parseWorktreePorcelain(in)
	if len(wts) != 3 {
		t.Fatalf("want 3 worktrees, got %d: %+v", len(wts), wts)
	}
	if wts[0].Branch != "main" || wts[1].Branch != "me/audit" || wts[2].Branch != "" {
		t.Errorf("branches wrong: %v / %v / %v", wts[0].Branch, wts[1].Branch, wts[2].Branch)
	}
	if wts[2].Path != "/Users/me/worktrees/rxtx.dev/detached" {
		t.Errorf("third worktree path wrong: %q", wts[2].Path)
	}
}

func TestParseWorktreePorcelain_Empty(t *testing.T) {
	if got := parseWorktreePorcelain(""); len(got) != 0 {
		t.Errorf("empty input should yield no worktrees, got %d", len(got))
	}
}

// ---- batched worktree list -------------------------------------------------

func TestParseAllWorktrees_GroupsBySentinel(t *testing.T) {
	repos := map[string]Repo{
		"rxtx.dev": {Path: "/Users/me/src/github.com/me/rxtx.dev"},
		"my-app":   {Path: "/Users/me/src/github.com/my-org/my-app"},
	}
	raw := strings.Join([]string{
		"@@CCTL_REPO=rxtx.dev",
		"worktree /Users/me/src/github.com/me/rxtx.dev",
		"HEAD a",
		"branch refs/heads/main",
		"",
		"worktree /Users/me/worktrees/rxtx.dev/audit",
		"HEAD b",
		"branch refs/heads/me/audit",
		"",
		"@@CCTL_REPO=my-app",
		"worktree /Users/me/src/github.com/my-org/my-app",
		"HEAD c",
		"branch refs/heads/main",
		"",
	}, "\n")
	got, statuses := parseAllWorktrees(raw, "@@CCTL_REPO=", "@@CCTL_STATUS=", repos)
	if len(got) != 2 {
		t.Fatalf("want 2 repos, got %d (%+v)", len(got), got)
	}
	// Absent status lines → default OK so the existing call paths stay healthy.
	for name, want := range map[string]RepoStatus{"rxtx.dev": RepoStatusOK, "my-app": RepoStatusOK} {
		if got := statuses[name]; got != want {
			t.Errorf("default status for %s: got %s want %s", name, got, want)
		}
	}
	if len(got["rxtx.dev"]) != 2 {
		t.Errorf("rxtx.dev: want 2 worktrees, got %d", len(got["rxtx.dev"]))
	}
	if len(got["my-app"]) != 1 {
		t.Errorf("my-app: want 1 worktree, got %d", len(got["my-app"]))
	}
	// main worktree should be tagged correctly
	for _, w := range got["rxtx.dev"] {
		if w.Path == "/Users/me/src/github.com/me/rxtx.dev" {
			if w.Name != "main" || !w.IsMain {
				t.Errorf("primary checkout not named 'main': %+v", w)
			}
		}
	}
}

// TestParseAllWorktrees_StatusEmittedPerRepo covers the regression that
// triggered the "ghost my-app" bug: a configured repo whose path was
// missing on the server used to be displayed as if it were healthy because
// the shell error from `git -C <missing> worktree list` was swallowed and
// no upstream signal differentiated it from an empty repo. Now the
// listing script prefixes each repo block with @@CCTL_STATUS=<state> and
// the parser propagates that through.
func TestParseAllWorktrees_StatusEmittedPerRepo(t *testing.T) {
	repos := map[string]Repo{
		"healthy": {Path: "/h"},
		"gone":    {Path: "/g"},
		"notgit":  {Path: "/n"},
	}
	raw := strings.Join([]string{
		"@@CCTL_REPO=healthy",
		"@@CCTL_STATUS=OK",
		"worktree /h",
		"HEAD a",
		"branch refs/heads/main",
		"",
		"@@CCTL_REPO=gone",
		"@@CCTL_STATUS=MISSING",
		"",
		"@@CCTL_REPO=notgit",
		"@@CCTL_STATUS=NO_GIT",
		"",
	}, "\n")
	wts, statuses := parseAllWorktrees(raw, "@@CCTL_REPO=", "@@CCTL_STATUS=", repos)
	if statuses["healthy"] != RepoStatusOK {
		t.Errorf("healthy status: got %s want OK", statuses["healthy"])
	}
	if statuses["gone"] != RepoStatusMissing {
		t.Errorf("gone status: got %s want MISSING", statuses["gone"])
	}
	if statuses["notgit"] != RepoStatusNoGit {
		t.Errorf("notgit status: got %s want NO_GIT", statuses["notgit"])
	}
	// healthy still has its worktree visible
	if len(wts["healthy"]) != 1 {
		t.Errorf("healthy worktrees: got %d want 1", len(wts["healthy"]))
	}
	// missing/notgit have empty worktree slices (no shell output past the marker)
	if len(wts["gone"]) != 0 {
		t.Errorf("gone worktrees: got %d want 0", len(wts["gone"]))
	}
	// And the status line itself didn't leak into the parsed buffer as junk.
	for _, w := range wts["healthy"] {
		if strings.Contains(w.Path, "@@CCTL_STATUS") {
			t.Errorf("status line leaked into worktree path: %+v", w)
		}
	}
}

func TestAnnotateWorktrees_BasenameCollisions(t *testing.T) {
	wts := []Worktree{
		{Path: "/repo"},
		{Path: "/wts/rxtx.dev/audit"},
		{Path: "/wts/another/audit"},
	}
	repo := Repo{Path: "/repo"}
	got := annotateWorktrees(wts, "rxtx.dev", repo)
	// main, audit, audit-1 (or similar — confirm uniqueness)
	names := map[string]int{}
	for _, w := range got {
		names[w.Name]++
	}
	for n, c := range names {
		if c > 1 {
			t.Errorf("name %q appeared %d times — annotate should disambiguate", n, c)
		}
	}
	if names["main"] != 1 {
		t.Errorf("expected one 'main' worktree, got %d", names["main"])
	}
}

// ---- repo name collision logic --------------------------------------------

func TestAssignRepoNames_LeafOnlyByDefault(t *testing.T) {
	found := []discoveredRepo{
		{parts: []string{"me", "rxtx.dev"}, srcPath: "/Users/me/src/github.com", defaultBranch: "main"},
		{parts: []string{"my-org", "my-app"}, srcPath: "/Users/me/src/github.com", defaultBranch: "main"},
	}
	got := assignRepoNames(found)
	if _, ok := got["rxtx.dev"]; !ok {
		t.Errorf("expected leaf-only name 'rxtx.dev'; got keys %v", keys(got))
	}
	if _, ok := got["my-app"]; !ok {
		t.Errorf("expected leaf-only name 'my-app'; got keys %v", keys(got))
	}
}

func TestAssignRepoNames_CollisionPrefixesParent(t *testing.T) {
	found := []discoveredRepo{
		{parts: []string{"foo", "shared"}, srcPath: "/src/github.com"},
		{parts: []string{"bar", "shared"}, srcPath: "/src/github.com"},
	}
	got := assignRepoNames(found)
	if _, ok := got["shared"]; ok {
		t.Errorf("collision should not yield bare 'shared'; got keys %v", keys(got))
	}
	if _, ok := got["foo-shared"]; !ok {
		t.Errorf("expected 'foo-shared' after parent-prefix; got %v", keys(got))
	}
	if _, ok := got["bar-shared"]; !ok {
		t.Errorf("expected 'bar-shared' after parent-prefix; got %v", keys(got))
	}
}

func TestAssignRepoNames_ThreeWayCollisionWidensFurther(t *testing.T) {
	// Triple collision at leaf AND at parent — should widen to grandparent.
	found := []discoveredRepo{
		{parts: []string{"a", "x", "shared"}, srcPath: "/s"},
		{parts: []string{"b", "x", "shared"}, srcPath: "/s"},
		{parts: []string{"c", "x", "shared"}, srcPath: "/s"},
	}
	got := assignRepoNames(found)
	// "x-shared" still collides 3 ways, so should widen to "a-x-shared" etc.
	want := []string{"a-x-shared", "b-x-shared", "c-x-shared"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %q; got %v", w, keys(got))
		}
	}
}

// ---- pickRepoSource (cctl init) -------------------------------------------

func TestPickRepoSource_PrefersDeeperMostPopulated(t *testing.T) {
	home := "/Users/me"
	repos := []string{
		home + "/src/github.com/me/rxtx.dev",
		home + "/src/github.com/my-org/my-app",
		home + "/src/github.com/beeper/ai-chats",
		home + "/work/single-repo",
	}
	path, count, depth := pickRepoSource(home, repos)
	if path != home+"/src/github.com" {
		t.Errorf("want path=%s; got %s (count=%d depth=%d)", home+"/src/github.com", path, count, depth)
	}
	if count != 3 {
		t.Errorf("want count=3 (the github.com-rooted repos); got %d", count)
	}
	if depth != 2 {
		t.Errorf("want depth=2 (src/github.com under home); got %d", depth)
	}
}

func TestPickRepoSource_SingleRepoUsesParent(t *testing.T) {
	home := "/h"
	got, _, _ := pickRepoSource(home, []string{home + "/myrepo"})
	if got != home {
		t.Errorf("single repo at $HOME/myrepo → source=$HOME; got %q", got)
	}
}

// ---- spawn detection -------------------------------------------------------

// TestDetectSpawner_ConfigPrefHonoursKnownNames verifies that a known
// provider name in defaults.spawn wins over env signals — kept as the
// extension point for when we add another terminal in the future.
// "inline" was removed from the registry, so any unknown name falls
// through to the auto path and we get cmux (the only remaining provider).
func TestDetectSpawner_ConfigPrefHonoursKnownNames(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("__CFBundleIdentifier", "com.mitchellh.ghostty")
	s, reason := detectSpawner("cmux")
	if s.Name() != "cmux" {
		t.Errorf("config cmux → cmux; got %s (%s)", s.Name(), reason)
	}
	if !strings.HasPrefix(reason, "config:") {
		t.Errorf("reason should start with config:; got %q", reason)
	}
}

func TestDetectSpawner_CmuxBundleSignal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("__CFBundleIdentifier", "com.cmuxterm.app")
	// Pretend no CMUX_* env (the test runner doesn't set any).
	s, reason := detectSpawner("")
	if s.Name() != "cmux" {
		t.Errorf("bundle id = cmux → cmux; got %s (%s)", s.Name(), reason)
	}
}

func TestDetectSpawner_CmuxEnvSignal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("__CFBundleIdentifier", "")
	t.Setenv("CMUX_WORKSPACE_ID", "ws-abc")
	s, _ := detectSpawner("")
	if s.Name() != "cmux" {
		t.Errorf("CMUX_WORKSPACE_ID set → cmux; got %s", s.Name())
	}
}

// TestDetectSpawner_AlwaysCmux locks in the post-simplification contract:
// detectSpawner never returns anything other than cmux, regardless of
// signals or config. The inline fallback was removed when cctl committed
// to "Enter always opens a new cmux tab" — keep this test as the canary
// for any future driver-by-config attempts.
func TestDetectSpawner_AlwaysCmux(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("__CFBundleIdentifier", "")
	for _, kv := range []string{"CMUX_WORKSPACE_ID", "CMUX_SURFACE_ID", "CMUX_TAB_ID", "CMUX_SOCKET_PATH", "CMUX_AGENT_LAUNCH_CWD"} {
		t.Setenv(kv, "")
	}
	for _, pref := range []string{"", "auto", "cmux", "garbage"} {
		s, _ := detectSpawner(pref)
		if s.Name() != "cmux" {
			t.Errorf("detectSpawner(%q): got %s, want cmux", pref, s.Name())
		}
	}
}

// ---- ensureWorktreeScript --------------------------------------------------

func TestEnsureWorktreeScript_RunsPostCreateOnExisting(t *testing.T) {
	script := ensureWorktreeScript("/repo", "/wt", "me/audit", "main", []string{"mise trust", "direnv allow"})
	// Hooks must run even when the worktree already exists — the trust
	// command is idempotent and the user expects it applied either way.
	// Verify by checking that the post-create block is NOT inside a
	// branch we'd skip on existing worktrees.
	if !strings.Contains(script, `mise trust`) {
		t.Fatal("script doesn't include the post-create cmd")
	}
	if !strings.Contains(script, `direnv allow`) {
		t.Fatal("script doesn't include the second post-create cmd")
	}
	// The script should NOT have an early-exit gate around the post-create:
	// look for the post-create line and ensure it's at top-level
	// (after `fi`, not before).
	idxExit := strings.Index(script, `git worktree add`)
	idxFi := strings.Index(script[idxExit:], "fi")
	if idxFi == -1 {
		t.Fatal("expected 'fi' closing the git-worktree-add block")
	}
	idxPost := strings.Index(script, `cd "$WT"`)
	if idxPost == -1 || idxPost < idxExit+idxFi {
		// post-create cd "$WT" should be AFTER the closing fi.
	} else if idxPost < idxExit {
		t.Errorf("post-create runs BEFORE the worktree-add block")
	}
}

func TestEnsureWorktreeScript_NoPostCreateWhenEmpty(t *testing.T) {
	script := ensureWorktreeScript("/repo", "/wt", "b", "main", nil)
	if strings.Contains(script, "post-create") {
		t.Errorf("empty hooks → no post-create scaffolding; got:\n%s", script)
	}
}

// TestEnsureWorktreeScript_PreflightsRepoExists guards the failure mode that
// bit us when ~/my-app was missing on a remote server: `cd "$REPO"` failed
// with a generic bash error, and the user couldn't tell the path was missing
// vs. some other git problem. The script must:
//
//  1. Detect the missing dir before cd and emit a message that names the
//     ORIGINAL config string (e.g. `~/my-app`) — not just the expanded
//     `$HOME`-prefixed form — so the user knows which config key to edit.
//  2. Detect that the dir exists but isn't a git repo, separately.
//  3. Exit non-zero before any `cd` so the bash error doesn't drown the hint.
func TestEnsureWorktreeScript_PreflightsRepoExists(t *testing.T) {
	script := ensureWorktreeScript("~/my-app", "~/worktrees/my-app/b300", "me/b300", "main", nil)
	mustContain := []string{
		`if [ ! -d "$REPO" ]; then`,
		`REPO_CFG='~/my-app'`,
		`does not exist on this host`,
		`~/.cctl.yaml`,
		`if [ ! -e "$REPO/.git" ]; then`,
		`not a git repository`,
		`exit 2`,
	}
	for _, want := range mustContain {
		if !strings.Contains(script, want) {
			t.Errorf("script missing diagnostic %q\nfull script:\n%s", want, script)
		}
	}
	idxCheck := strings.Index(script, `if [ ! -d "$REPO" ]; then`)
	idxCd := strings.Index(script, `cd "$REPO"`)
	if idxCheck == -1 || idxCd == -1 || idxCheck > idxCd {
		t.Errorf("preflight must precede `cd \"$REPO\"`; check=%d cd=%d", idxCheck, idxCd)
	}
}

// ---- removeWorktreeScript --------------------------------------------------

func TestRemoveWorktreeScript_HasFallbackRm(t *testing.T) {
	// WT must be INSIDE the worktree_base to clear the new safety check;
	// the bare /wt of the old test would now be refused.
	script := removeWorktreeScript("/repo", "/base/wt", "/base")
	if !strings.Contains(script, "git worktree remove") {
		t.Error("script should try git worktree remove first")
	}
	if !strings.Contains(script, "rm -rf -- \"$WT\"") {
		t.Error("script should still rm -rf the WT (now guarded by worktree_base check)")
	}
	if !strings.Contains(script, "still exists") {
		t.Error("script should verify removal at the end")
	}
}

// ---- claudeLaunchScript ----------------------------------------------------

func TestClaudeLaunchScript_PerSessionResumeOrFresh(t *testing.T) {
	got := claudeLaunchScript("/wt", []string{"--dangerously-skip-permissions"}, "", false, "sid-test")
	// Per-session id, not the worktree's most-recent conversation: resume this
	// session's own transcript if it exists, else start fresh pinned to the id.
	if !strings.Contains(got, "claude --resume sid-test") {
		t.Errorf("should resume by session id; got:\n%s", got)
	}
	if !strings.Contains(got, "claude --session-id sid-test") {
		t.Errorf("fresh launch should pin the session id; got:\n%s", got)
	}
	if strings.Contains(got, "claude --continue") {
		t.Errorf("must NOT use --continue (resumes the worktree's latest, not this session); got:\n%s", got)
	}
	if !strings.Contains(got, "$proj/$sid.jsonl") {
		t.Errorf("should branch on this session's transcript file; got:\n%s", got)
	}
	if !strings.Contains(got, "tr '/.' '--'") {
		t.Error("launch script should encode cwd via tr '/.' '--' (claude's project path scheme)")
	}
}

func TestClaudeSessionID_DeterministicAndDistinct(t *testing.T) {
	a := claudeSessionID("local", tmuxName("rxtx.dev", "mneme", "android"))
	if a != claudeSessionID("local", tmuxName("rxtx.dev", "mneme", "android")) {
		t.Error("session id must be deterministic for the same identity")
	}
	// Same worktree, different session → different id (the whole point).
	if a == claudeSessionID("local", tmuxName("rxtx.dev", "mneme", "default")) {
		t.Error("different sessions on one worktree must get different ids")
	}
	// Looks like a v5 UUID: 8-4-4-4-12, version nibble 5.
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Errorf("not a valid v5 UUID: %q", a)
	}
}

func TestClaudeLaunchScript_AppendsPromptWhenProvided(t *testing.T) {
	got := claudeLaunchScript("/wt", nil, "say hi", false, "sid-test")
	if !strings.Contains(got, "say hi") {
		t.Errorf("prompt should be shell-included in the launch script; got:\n%s", got)
	}
}

func TestClaudeLaunchScript_GuardsAgainstMissingCwd(t *testing.T) {
	got := claudeLaunchScript("/wt", nil, "", false, "sid-test")
	// If the worktree was deleted, the cd must abort the script (after a
	// readable pause) instead of letting claude launch in whatever
	// directory the shell happened to start in.
	if !strings.Contains(got, `cd "/wt" || {`) {
		t.Errorf("launch script must guard the cd; got:\n%s", got)
	}
}

// attachOrRespawn must emit `tmux new-session -A`, not a bare attach: the
// command is baked into a wrapper script that cmux persists in its
// workspace layout and re-runs on restore, possibly long after the tmux
// session died. A bare attach then fails with "can't find session";
// `new-session -A` resurrects it (claude --continue) in the worktree.
func TestAttachOrRespawn_SurvivesDeadSession(t *testing.T) {
	r := &Resolved{ClaudeFlags: []string{"--dangerously-skip-permissions"}}
	got := attachOrRespawn(r, "cctl/rxtx_dev/main/mneme", "/repos/rxtx.dev")
	if !strings.Contains(got, "tmux new-session -A -s cctl/rxtx_dev/main/mneme") {
		t.Errorf("want new-session -A with the session name; got:\n%s", got)
	}
	if strings.Contains(got, "tmux attach") {
		t.Errorf("bare tmux attach must not appear; got:\n%s", got)
	}
	if !strings.Contains(got, `cd "/repos/rxtx.dev"`) {
		t.Errorf("respawn launch must cd into the session's worktree; got:\n%s", got)
	}
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("respawn launch must keep the repo's claude flags; got:\n%s", got)
	}
}

func TestClaudeLaunchScript_PrependsCmuxBinWhenInsideCmux(t *testing.T) {
	got := claudeLaunchScript("/wt", nil, "", false, "sid-test")
	// The script must guard with CMUX_WORKSPACE_ID so it's a no-op when
	// not running inside cmux (e.g., remote target via mosh).
	if !strings.Contains(got, `CMUX_WORKSPACE_ID`) {
		t.Errorf("launch script must guard cmux PATH-prefix with CMUX_WORKSPACE_ID; got:\n%s", got)
	}
	// And must reference the wrapper path so the integration actually engages.
	if !strings.Contains(got, "/Applications/cmux.app/Contents/Resources/bin") {
		t.Errorf("launch script must prepend cmux's bin to PATH; got:\n%s", got)
	}
}

// ---- cmux layout / args ---------------------------------------------------

// TestBuildCmuxLayout_OneTerminalOnly is the regression test for the
// "two tabs" bug: cmux's default workspace template included a Files panel
// plus the terminal. Switching to `new-workspace --layout` with a single
// pane / single terminal surface should produce exactly one surface.
func TestBuildCmuxLayout_OneTerminalOnly(t *testing.T) {
	got, err := buildCmuxLayout("/tmp/cctl-foo.sh")
	if err != nil {
		t.Fatalf("buildCmuxLayout error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("layout is not valid JSON: %v\ngot: %s", err, got)
	}
	// Must be a single pane (no `direction`/`children` split).
	if _, ok := parsed["children"]; ok {
		t.Errorf("layout should be a single pane, not a split: %s", got)
	}
	pane, ok := parsed["pane"].(map[string]any)
	if !ok {
		t.Fatalf("layout missing top-level pane: %s", got)
	}
	surfaces, ok := pane["surfaces"].([]any)
	if !ok || len(surfaces) != 1 {
		t.Fatalf("expected exactly one surface, got: %v", pane["surfaces"])
	}
	surf, _ := surfaces[0].(map[string]any)
	if surf["type"] != "terminal" {
		t.Errorf("surface type should be terminal, got %v", surf["type"])
	}
	if surf["command"] != "/tmp/cctl-foo.sh" {
		t.Errorf("surface command should match script path, got %v", surf["command"])
	}
}

// TestCmuxNewWorkspaceArgs_StructureAndFlags asserts the argv shape:
//   - first arg is the subcommand `new-workspace`
//   - `--cwd`, `--layout`, `--focus true` are always present
//   - `--name <title>` is included only when a title is set
//   - the embedded layout is itself valid JSON (sanity on quoting)
func TestCmuxNewWorkspaceArgs_StructureAndFlags(t *testing.T) {
	t.Run("with title", func(t *testing.T) {
		args, err := cmuxNewWorkspaceArgs("/tmp/foo.sh", "/home/user/repo", "my-app/b300/test", false)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(args) == 0 || args[0] != "new-workspace" {
			t.Fatalf("first arg should be new-workspace: %v", args)
		}
		joined := strings.Join(args, " ")
		for _, want := range []string{"--name my-app/b300/test", "--cwd /home/user/repo", "--focus false", "--layout "} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv missing %q: %v", want, args)
			}
		}
		// The --layout value should be JSON-decodable.
		for i, a := range args {
			if a == "--layout" && i+1 < len(args) {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(args[i+1]), &parsed); err != nil {
					t.Errorf("--layout value isn't valid JSON: %v\nvalue: %s", err, args[i+1])
				}
			}
		}
	})
	t.Run("without title", func(t *testing.T) {
		args, err := cmuxNewWorkspaceArgs("/tmp/foo.sh", "/home/user/repo", "", false)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		for _, a := range args {
			if a == "--name" {
				t.Errorf("empty title shouldn't produce --name: %v", args)
			}
		}
	})
}

// ---- list-workspaces parser -----------------------------------------------

// TestParseCmuxWorkspaceList_SelectedRowMarkers is the regression test for the
// duplicate-workspace bug: cmux prefixes the SELECTED row with "*" and suffixes
// it with "[selected]". If those markers aren't stripped, the selected
// workspace parses with id "*" and a name that never matches, so
// findCmuxWorkspaceByName can't see it and callers create a duplicate.
func TestParseCmuxWorkspaceListJSON_KeysOnCustomTitle(t *testing.T) {
	// Captured shape of `cmux workspace list --json`: custom_title is the
	// durable cctl name; title is the volatile display that drifts to cwd.
	raw := []byte(`{
	  "workspaces": [
	    {"ref":"workspace:1","title":"~","custom_title":null,"has_custom_title":false,"description":"Claude Control Center"},
	    {"ref":"workspace:13","title":"…/remotes/workspace/olympus/gb300-vast","custom_title":"olympus/gb300-vast/poc","has_custom_title":true},
	    {"id":"9AED4D5F-F9F2-47CF-81E6-45D6868999AB","ref":"workspace:9","title":"…/geoah/rxtx.dev","custom_title":"olympus/gb300-k8s/docs","has_custom_title":true}
	  ]
	}`)
	got := parseCmuxWorkspaceListJSON(raw)
	if len(got) != 3 {
		t.Fatalf("want 3 workspaces, got %d: %+v", len(got), got)
	}
	// No custom title → fall back to the (volatile) title, id from ref.
	if got[0].name != "~" || got[0].id != "workspace:1" {
		t.Errorf("control ws = %+v, want name=~ id=workspace:1", got[0])
	}
	// A drifted title must NOT win — the durable custom_title is the identity.
	if got[1].name != "olympus/gb300-vast/poc" {
		t.Errorf("ws[1].name = %q, want the custom_title (not the cwd title)", got[1].name)
	}
	if got[1].id != "workspace:13" {
		t.Errorf("ws[1].id = %q, want ref fallback workspace:13", got[1].id)
	}
	// Prefer the UUID when present.
	if got[2].id != "9AED4D5F-F9F2-47CF-81E6-45D6868999AB" {
		t.Errorf("ws[2].id = %q, want the UUID", got[2].id)
	}
	if got[2].name != "olympus/gb300-k8s/docs" {
		t.Errorf("ws[2].name = %q, want custom_title olympus/gb300-k8s/docs", got[2].name)
	}
}

func TestParseCmuxWorkspaceList_SelectedRowMarkers(t *testing.T) {
	raw := strings.Join([]string{
		"  AA3721EF-C46A-4B08-AB99-902EE24E8872  dev/olympus",
		"* 668D35A2-4CF0-4D97-89F1-30186AF75AB4  olympus/main/lethe-docs-review  [selected]",
		"  06E02301-CB9C-434F-82DB-7E253AC2A8A0  olympus/wt/test1",
		"", // trailing blank line
	}, "\n")

	got := parseCmuxWorkspaceList(raw)
	if len(got) != 3 {
		t.Fatalf("want 3 workspaces, got %d: %+v", len(got), got)
	}

	// The selected row must parse with its real UUID and clean name.
	sel := got[1]
	if sel.id != "668D35A2-4CF0-4D97-89F1-30186AF75AB4" {
		t.Errorf("selected workspace id = %q, want the UUID (not the \"*\" marker)", sel.id)
	}
	if sel.name != "olympus/main/lethe-docs-review" {
		t.Errorf("selected workspace name = %q, want %q (no UUID/[selected] leakage)", sel.name, "olympus/main/lethe-docs-review")
	}

	// And it must be findable by name via the same path spawn/reconcile use.
	want := map[string]string{
		"dev/olympus":                    "AA3721EF-C46A-4B08-AB99-902EE24E8872",
		"olympus/main/lethe-docs-review": "668D35A2-4CF0-4D97-89F1-30186AF75AB4",
		"olympus/wt/test1":               "06E02301-CB9C-434F-82DB-7E253AC2A8A0",
	}
	byName := map[string]string{}
	for _, w := range got {
		byName[w.name] = w.id
	}
	for name, id := range want {
		if byName[name] != id {
			t.Errorf("workspace %q: id = %q, want %q", name, byName[name], id)
		}
	}
}

// ---- connection state machine ---------------------------------------------

// TestProbeRetryPolicy pins the single-retry policy: the auto-retry delay is
// short and there's exactly one retry (initial probe + 1) before a host is
// left disconnected until a manual refresh.
func TestProbeRetryPolicy(t *testing.T) {
	if probeRetryDelay != 2*time.Second {
		t.Errorf("probeRetryDelay = %s, want 2s", probeRetryDelay)
	}
	if maxProbeAttempts != 2 {
		t.Errorf("maxProbeAttempts = %d, want 2 (initial + one retry)", maxProbeAttempts)
	}
}

// TestConnState_ProbedMsgSuccess drives a real Update tick: a successful
// probe must move the state from connConnecting to connConnected, clear
// the error, and reset attempts so the next failure starts backoff fresh.
func TestConnState_ProbedMsgSuccess(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"w": {Host: "1.2.3.4", User: "u"}}},
		serverNames: []string{"w"},
		state: map[string]*serverState{
			"w": {conn: connConnecting, connAttempts: 3, connErr: &stringError{"old"}},
		},
	}
	_, _ = m.Update(probedMsg{server: "w"})
	st := m.state["w"]
	if st.conn != connConnected {
		t.Errorf("conn after success: got %v, want connConnected", st.conn)
	}
	if st.connErr != nil {
		t.Errorf("connErr should be cleared on success, got %v", st.connErr)
	}
	if st.connAttempts != 0 {
		t.Errorf("connAttempts should reset to 0 on success, got %d", st.connAttempts)
	}
}

// TestConnState_ProbedMsgFailure mirrors the success test: a failed probe
// must flip to connDisconnected, capture the error, and (in the wider
// model) schedule a retry. We can't easily inspect the returned tea.Cmd
// payload without running it, so we only assert state here.
func TestConnState_ProbedMsgFailure(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"w": {Host: "1.2.3.4", User: "u"}}},
		serverNames: []string{"w"},
		state: map[string]*serverState{
			"w": {conn: connConnecting, connAttempts: 1},
		},
	}
	wantErr := &stringError{"ssh exit 255: Connection refused"}
	_, cmd := m.Update(probedMsg{server: "w", err: wantErr})
	st := m.state["w"]
	if st.conn != connDisconnected {
		t.Errorf("conn after failure: got %v, want connDisconnected", st.conn)
	}
	if st.connErr == nil || st.connErr.Error() != wantErr.Error() {
		t.Errorf("connErr after failure: got %v want %v", st.connErr, wantErr)
	}
	if cmd == nil {
		t.Errorf("expected a retry tea.Cmd (Tick) after failure, got nil")
	}
}

// TestConnState_DisconnectedHidesChildren verifies the UX contract: when a
// server is connDisconnected, rebuildRows must not append any child rows
// underneath it. This is the regression test for the "ghost tree" fix
// applied at the server level (parallel to the repo-level fix).
func TestConnState_DisconnectedHidesChildren(t *testing.T) {
	m := &tuiModel{
		cfg: &Config{Servers: map[string]Server{
			"w": {Host: "1.2.3.4", User: "u"},
		}},
		serverNames: []string{"w"},
		state: map[string]*serverState{
			"w": {
				conn:            connDisconnected,
				connErr:         &stringError{"ssh exit 255"},
				connAttempts:    2,
				repos:           map[string]Repo{"r1": {Path: "/r1"}},
				worktrees:       map[string][]Worktree{"r1": {{Name: "main", Path: "/r1"}}},
				sessions:        []SessionInfo{{Server: "w", Repo: "r1", Worktree: "main", Session: "s1"}},
				reposLoaded:     true,
				sessionsLoaded:  true,
				worktreesLoaded: true,
			},
		},
		expanded: map[string]bool{},
	}
	// Force-expand the server to make sure expansion alone isn't what
	// suppresses children — disconnection is.
	m.expanded["server:w"] = true
	m.rebuildRows()
	for _, r := range m.rows {
		if r.kind != rowServer {
			t.Errorf("disconnected server must yield no child rows; got %+v", r)
		}
	}
}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

// ---- samePath / annotateWorktrees — main-checkout safety -------------------

// TestSamePath_TildeMatchesExpandedHome is the regression test for the
// 2026-05-13 incident. samePath was returning false for
// samePath("/home/me/my-app", "~/my-app"), so annotateWorktrees
// labeled the main checkout as a normal worktree, dd's "main" guard
// missed, and rm -rf deleted the user's repo. THIS TEST MUST KEEP
// PASSING. If it fails, dd is one keystroke away from data loss again.
func TestSamePath_TildeMatchesExpandedHome(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		note string
	}{
		{"/home/me/my-app", "~/my-app", true, "exact bug repro"},
		{"~/my-app", "/home/me/my-app", true, "same, swapped sides"},
		{"/Users/me/repo", "~/repo", true, "macOS home form"},
		{"/home/me/my-app/", "~/my-app", true, "trailing slash on absolute"},
		{"~/", "/home/me", false, "bare ~/ shouldn't match arbitrary home"},
		{"~/foo", "/home/x/bar/foo", true, "matches /<anything>/foo"},
		{"~/foo", "/home/x/foobar", false, "boundary required — no partial-name match"},
		{"~/a/b", "/home/x/a/b", true, "multi-segment tilde path"},
		{"~/a/b", "/home/x/c/a/b", true, "tilde tail at a path boundary further in"},
		{"~/a/b", "/home/x/a-b", false, "tail without boundary must NOT match"},
	}
	for _, c := range cases {
		if got := samePath(c.a, c.b); got != c.want {
			t.Errorf("samePath(%q, %q) = %v, want %v (%s)", c.a, c.b, got, c.want, c.note)
		}
	}
}

// TestAnnotateWorktrees_MainCheckoutLabelledMainViaTilde is the parser-level
// regression. With repo.Path="~/my-app" and git emitting absolute paths,
// the main checkout must end up Name="main" IsMain=true so dd's name-based
// guard kicks in. Failing this test means the user's repo is one keystroke
// from rm -rf again.
func TestAnnotateWorktrees_MainCheckoutLabelledMainViaTilde(t *testing.T) {
	wts := []Worktree{
		{Path: "/home/me/my-app"},                // main, from git
		{Path: "/home/me/worktrees/my-app/b300"}, // real worktree
	}
	repo := Repo{Path: "~/my-app"} // config form — ~-rooted on purpose
	got := annotateWorktrees(wts, "my-app", repo)
	if got[0].Name != "main" || !got[0].IsMain {
		t.Fatalf("main checkout mislabeled — name=%q IsMain=%v\n"+
			"This is the exact regression that deleted ~/my-app on 2026-05-13.",
			got[0].Name, got[0].IsMain)
	}
	if got[1].Name == "main" || got[1].IsMain {
		t.Errorf("real worktree mislabeled as main: %+v", got[1])
	}
}

// TestRemoveWorktreeScript_RefusesMainCheckout is the script-level guard.
// Even if the caller is wrong about which path is the main checkout, the
// script itself MUST refuse to rm-rf when $WT == $REPO. This is the third
// layer of defense and the only one that runs server-side, so it's the
// last line before user data is gone.
func TestRemoveWorktreeScript_RefusesMainCheckout(t *testing.T) {
	script := removeWorktreeScript("~/my-app", "~/my-app", "~/worktrees")
	// The refusal must come BEFORE the rm -rf. Easiest assertion: the
	// refusal text precedes "rm -rf" in the rendered script.
	idxGuard := strings.Index(script, "main checkout")
	idxRm := strings.Index(script, "rm -rf")
	if idxGuard == -1 {
		t.Fatalf("script missing main-checkout refusal text:\n%s", script)
	}
	if idxRm == -1 {
		t.Fatalf("script missing rm -rf at all — unexpected")
	}
	if idxGuard > idxRm {
		t.Errorf("refusal must precede rm -rf; guard=%d rm=%d\n%s", idxGuard, idxRm, script)
	}
	if !strings.Contains(script, "exit 3") {
		t.Errorf("script should `exit 3` on refusal so cctl can distinguish this from other failures")
	}
	if !strings.Contains(script, `pwd -P`) {
		t.Errorf("script should resolve real paths (pwd -P) to defeat symlinks; got:\n%s", script)
	}
}

// TestRemoveWorktreeScript_HasAllSafetyGuards locks in the post-incident
// invariants so we never lose them in a refactor. Each guard's
// presence + content is asserted independently; if you delete one of
// these, the test tells you which.
func TestRemoveWorktreeScript_HasAllSafetyGuards(t *testing.T) {
	script := removeWorktreeScript("/repo", "/base/wt", "/base")
	musts := []struct {
		needle string
		why    string
	}{
		{`WT_REAL="$(cd "$WT" 2>/dev/null && pwd -P || true)"`, "must pwd -P the WT to defeat symlinks"},
		{`REPO_REAL="$(cd "$REPO" 2>/dev/null && pwd -P || true)"`, "must pwd -P the REPO to defeat symlinks"},
		{`is the repo's main checkout`, "must explicitly call out the WT==REPO refusal cause"},
		{`contains the repo`, "must refuse when WT is an ancestor of REPO"},
		{`looks like home or root`, "must refuse $WT=$HOME or '/'"},
		{`outside worktree_base`, "must refuse paths outside the configured worktree_base"},
		{`check_under_base`, "must contain the containment helper"},
		{`git worktree remove`, "must try git first before any rm"},
		{`rm -rf -- "$WT"`, "must rm only the WT (never the parent)"},
	}
	for _, m := range musts {
		if !strings.Contains(script, m.needle) {
			t.Errorf("missing safety check %q (%s)", m.needle, m.why)
		}
	}
}

// ---- discover noise filter ------------------------------------------------

// TestHasNoisyComponent_FiltersHiddenAndDependencyDirs pins the discovery
// filter so `path: ~/` doesn't bury the TUI in entries for .nvm, .cache,
// node_modules, etc. The point of the filter is to keep this scope-creep
// from drowning real repos like ~/my-app.
func TestHasNoisyComponent_FiltersHiddenAndDependencyDirs(t *testing.T) {
	cases := []struct {
		parts []string
		noisy bool
		why   string
	}{
		{[]string{"my-app"}, false, "plain repo at home"},
		{[]string{"src", "github.com", "me", "rxtx.dev"}, false, "github layout"},
		{[]string{".nvm"}, true, "dot dir at top level"},
		{[]string{".cache", "mise", "python", "pyenv"}, true, "nested under dot dir"},
		{[]string{".t3", "worktrees", "my-app"}, true, "cursor-style hidden cache"},
		{[]string{"foo", "node_modules", "bar"}, true, "node_modules anywhere in path"},
		{[]string{"vendor"}, true, "vendor at top level"},
		{[]string{"node-modules"}, false, "hyphenated isn't node_modules"},
		{[]string{"vendor-x"}, false, "prefix match isn't an exact match"},
	}
	for _, c := range cases {
		if got := hasNoisyComponent(c.parts); got != c.noisy {
			t.Errorf("hasNoisyComponent(%v) = %v want %v (%s)", c.parts, got, c.noisy, c.why)
		}
	}
}

// ---- background-task registry -----------------------------------------------

// TestBgTasks_LifecycleAndAutoClear drives the task helpers used from the
// Update loop: start → running, finish → done with a clearAt (failures
// stick around longer than successes), tick prunes expired entries.
func TestBgTasks_LifecycleAndAutoClear(t *testing.T) {
	m := &tuiModel{state: map[string]*serverState{}}
	id, cmd := m.startTask("sess:w/r/wt/x", "deleting…")
	if cmd == nil {
		t.Errorf("startTask should return a non-nil tick cmd")
	}
	if m.runningTaskFor("sess:w/r/wt/x") == nil {
		t.Fatalf("task should be running for its own key")
	}
	m.finishTask(id, "killed", nil)
	if m.runningTaskFor("sess:w/r/wt/x") != nil {
		t.Errorf("finished task must not count as running")
	}
	okClear := m.tasks[0].clearAt
	if m.tasks[0].label != "killed" || m.tasks[0].failed || time.Now().After(okClear) {
		t.Errorf("after success finish: %+v", m.tasks[0])
	}
	// Failure: detail recorded, longer clearAt than success.
	id2, _ := m.startTask("wt:w/r/wt", "removing…")
	m.finishTask(id2, "remove failed", fmt.Errorf("stderr noise"))
	failed := m.tasks[1]
	if !failed.failed || failed.detail != "stderr noise" {
		t.Errorf("after failure finish: %+v", failed)
	}
	if !failed.clearAt.After(okClear) {
		t.Errorf("failure clearAt (%v) should outlast success clearAt (%v) so the user can read the error",
			failed.clearAt, okClear)
	}
	// Drive the tickMsg path: forcing clearAt into the past must prune.
	for _, bt := range m.tasks {
		bt.clearAt = time.Now().Add(-time.Second)
	}
	m.Update(tickMsg{})
	if len(m.tasks) != 0 {
		t.Errorf("expired tasks should be pruned on tick; got %d left", len(m.tasks))
	}
}

// TestTaskKeysConflict pins the subtree-overlap rule that decides which
// concurrent actions are allowed: same row, ancestor, and descendant
// conflict; siblings and unrelated rows don't; anonymous keys never do.
func TestTaskKeysConflict(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		why  string
	}{
		{"sess:w/r/wt/x", "sess:w/r/wt/x", true, "same session"},
		{"wt:w/r/wt", "sess:w/r/wt/x", true, "worktree vs session under it"},
		{"sess:w/r/wt/x", "wt:w/r/wt", true, "session vs its worktree"},
		{"sess:w/r/wt/x", "sess:w/r/wt/y", false, "sibling sessions"},
		{"wt:w/r/a", "wt:w/r/b", false, "sibling worktrees"},
		{"wt:w/r/wt", "wt:w/r/wt2", false, "prefix-similar names are not ancestors"},
		{"", "wt:w/r/wt", false, "anonymous never conflicts"},
	}
	for _, c := range cases {
		if got := taskKeysConflict(c.a, c.b); got != c.want {
			t.Errorf("taskKeysConflict(%q, %q) = %v want %v (%s)", c.a, c.b, got, c.want, c.why)
		}
	}
}

// TestNeedTick covers the "should we keep ticking" predicate. The tick
// is what drives spinner animation + auto-clear, so this is the
// load-bearing logic that decides whether the UI feels stuck or smooth.
func TestNeedTick(t *testing.T) {
	m := &tuiModel{state: map[string]*serverState{}}
	if m.needTick() {
		t.Errorf("idle model should not need ticking")
	}
	id, _ := m.startTask("sess:w/r/wt/x", "working…")
	if !m.needTick() {
		t.Errorf("running task must keep ticking for spinner animation")
	}
	m.finishTask(id, "done", nil)
	if !m.needTick() {
		t.Errorf("finished task with pending clearAt should keep ticking until expired")
	}
	m.tasks[0].clearAt = time.Now().Add(-time.Second)
	if m.needTick() {
		t.Errorf("task past clearAt should stop ticking")
	}
	// Server-side animation: connecting and post-connect loading both
	// keep the tick alive so server-row spinners advance.
	m.tasks = nil
	m.state["x"] = &serverState{conn: connConnecting}
	if !m.needTick() {
		t.Errorf("connecting server should require ticking")
	}
	m.state["x"] = &serverState{conn: connConnected, sessionsLoaded: false}
	if !m.needTick() {
		t.Errorf("connected-but-loading server should require ticking")
	}
	m.state["x"] = &serverState{conn: connConnected, sessionsLoaded: true, reposLoaded: true, worktreesLoaded: true}
	if m.needTick() {
		t.Errorf("fully-loaded server with no active tasks should stop ticking")
	}
}

// ---- cmux focus-existing --------------------------------------------------

// TestFindCmuxWorkspaceByName_Parsing exercises the lookup helper with
// representative cmux list-workspaces output shapes. We can't call the
// real CLI from a unit test (cmux's socket rejects out-of-bundle clients),
// so this verifies the line parser against synthetic input — which is
// the only thing that ever needs to change if cmux's output format
// drifts.
//
// We isolate the parser by re-implementing the loop body here over a
// hardcoded string. Keep this in sync with the spawn.go logic.
func TestCmuxWorkspaceListParser(t *testing.T) {
	cases := []struct {
		raw, want, wantID string
	}{
		{"abc123-uuid workspace/my-app/b300/test", "workspace/my-app/b300/test", "abc123-uuid"},
		{"def456 ws name with spaces", "ws name with spaces", "def456"},
		{"ghi789  trim  multiple spaces", "trim multiple spaces", "ghi789"}, // Fields collapses runs
		{"only-uuid-no-name", "anything", ""},                               // no name → no match
	}
	for _, c := range cases {
		gotID := ""
		for _, line := range strings.Split(c.raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			id := fields[0]
			cand := strings.TrimSpace(strings.Join(fields[1:], " "))
			if cand == c.want {
				gotID = id
			}
		}
		if gotID != c.wantID {
			t.Errorf("for raw=%q want=%q: got id=%q want=%q", c.raw, c.want, gotID, c.wantID)
		}
	}
}

// TestParseCmuxGroupList pins the `workspace-group list --json` payload
// shape ({"groups":[{"id","ref","name",…}]} — cmux's
// v2WorkspaceGroupPayload) and the id-then-ref fallback.
func TestParseCmuxGroupList(t *testing.T) {
	raw := `{"groups":[
		{"id":"AAA-1","ref":"workspace_group:1","name":"rxtx.dev","member_count":2},
		{"id":"","ref":"workspace_group:2","name":"workspace/olympus","member_count":5}
	]}`
	if id, ok := parseCmuxGroupList(raw, "rxtx.dev"); !ok || id != "AAA-1" {
		t.Errorf("want id AAA-1, got %q ok=%v", id, ok)
	}
	if id, ok := parseCmuxGroupList(raw, "workspace/olympus"); !ok || id != "workspace_group:2" {
		t.Errorf("empty id should fall back to ref, got %q ok=%v", id, ok)
	}
	if _, ok := parseCmuxGroupList(raw, "nope"); ok {
		t.Errorf("unknown group name must not match")
	}
	if _, ok := parseCmuxGroupList("not json", "x"); ok {
		t.Errorf("malformed payload must not match")
	}
}

// TestRepoGroup pins the sidebar-group naming: bare repo name locally,
// server-qualified for remotes (so same-named repos on different hosts
// don't merge), with the anchor cwd only set for local repos.
func TestRepoGroup(t *testing.T) {
	local := &Resolved{ServerName: "local", Server: Server{Local: true},
		RepoName: "rxtx.dev", Repo: Repo{Path: "/Users/me/src/rxtx.dev"}}
	if name, cwd := repoGroup(local); name != "rxtx.dev" || cwd != "/Users/me/src/rxtx.dev" {
		t.Errorf("local repoGroup = %q,%q", name, cwd)
	}
	remote := &Resolved{ServerName: "workspace", Server: Server{Host: "h"},
		RepoName: "olympus", Repo: Repo{Path: "~/olympus"}}
	name, cwd := repoGroup(remote)
	if name != "workspace/olympus" {
		t.Errorf("remote repoGroup name = %q", name)
	}
	// Remote repos anchor at the local stand-in project dir, which must
	// carry a .git marker so cmux's project-root walk resolves it.
	if !strings.HasSuffix(cwd, filepath.Join(".cctl", "remotes", "workspace", "olympus")) {
		t.Errorf("remote repoGroup cwd = %q, want the stand-in dir", cwd)
	}
	if fi, err := os.Stat(filepath.Join(cwd, ".git")); err != nil || !fi.IsDir() {
		t.Errorf("stand-in dir must contain a .git marker, stat err=%v", err)
	}
	// Workspace cwd for a remote worktree is a subdir of the stand-in,
	// so the .git walk lands on the repo dir.
	if got := workspaceCwd(remote, "b300"); got != filepath.Join(cwd, "b300") {
		t.Errorf("workspaceCwd(remote) = %q, want stand-in subdir", got)
	}
}

// TestParseCmuxSurfaceID accepts both id shapes the cmux CLI can print —
// full UUIDs and "surface:N" short refs — anywhere in the output text,
// since the exact phrasing around them isn't part of the CLI contract.
func TestParseCmuxSurfaceID(t *testing.T) {
	cases := []struct{ out, want string }{
		{"surface:7", "surface:7"},
		{"Created surface surface:12 in pane:1", "surface:12"},
		{"3FA85F64-5717-4562-B3FC-2C963F66AFA6", "3FA85F64-5717-4562-B3FC-2C963F66AFA6"},
		{"created 3fa85f64-5717-4562-b3fc-2c963f66afa6 (surface)", "3fa85f64-5717-4562-b3fc-2c963f66afa6"},
		{"ok", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseCmuxSurfaceID(c.out); got != c.want {
			t.Errorf("parseCmuxSurfaceID(%q) = %q want %q", c.out, got, c.want)
		}
	}
}

// TestLastCmuxListedID picks the id column of the newest (last) entry —
// the fallback used when new-surface's output carries no recognizable id.
func TestLastCmuxListedID(t *testing.T) {
	out := "uuid-1 first tab\nuuid-2 second tab\n\nuuid-3 newest tab\n\n"
	if got := lastCmuxListedID(out); got != "uuid-3" {
		t.Errorf("lastCmuxListedID = %q want uuid-3", got)
	}
	if got := lastCmuxListedID("\n \n"); got != "" {
		t.Errorf("blank list should yield empty id, got %q", got)
	}
}

// ---- dd refusal on repo/server rows ---------------------------------------

// lastTask returns the most recently added task (test helper).
func lastTask(m *tuiModel) *bgTask {
	if len(m.tasks) == 0 {
		return nil
	}
	return m.tasks[len(m.tasks)-1]
}

// TestExecuteDelete_RepoRowRefusedWithExplicitMessage pins the post-incident
// rule "cctl can only delete worktrees and sessions, never the repo
// itself." Pressing dd on a (r) row must not silently noop and must
// surface a visible red refusal in the footer task list.
func TestExecuteDelete_RepoRowRefusedWithExplicitMessage(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"w": {}}},
		serverNames: []string{"w"},
		state:       map[string]*serverState{"w": {}},
		rows:        []treeRow{{kind: rowRepo, server: "w", repo: "r1"}},
		cursor:      0,
	}
	_, _ = m.executeDelete()
	last := lastTask(m)
	if last == nil || !last.failed {
		t.Fatalf("dd on repo: want a failed task entry, got %+v", last)
	}
	if !strings.Contains(last.label, "can't delete a repo") {
		t.Errorf("dd on repo: label = %q, want it to mention can't delete", last.label)
	}
}

// TestExecuteDelete_ServerRowRefusedWithExplicitMessage is the parallel
// to the repo case: servers are config entries, not deletable from cctl.
func TestExecuteDelete_ServerRowRefusedWithExplicitMessage(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"w": {}}},
		serverNames: []string{"w"},
		state:       map[string]*serverState{"w": {}},
		rows:        []treeRow{{kind: rowServer, server: "w"}},
		cursor:      0,
	}
	_, _ = m.executeDelete()
	last := lastTask(m)
	if last == nil || !last.failed {
		t.Fatalf("dd on server: want a failed task entry, got %+v", last)
	}
	if !strings.Contains(last.label, "can't delete a server") {
		t.Errorf("dd on server: label = %q, want it to mention can't delete", last.label)
	}
}

// TestExecuteDelete_BlocksConflictingTargetsOnly is the regression test for
// the "dd is flaky when you do several things at once" complaint: a second
// dd on the SAME row (or its parent worktree) while a delete is running
// must be refused, but actions on unrelated rows must go through.
func TestExecuteDelete_BlocksConflictingTargetsOnly(t *testing.T) {
	sessA := &SessionInfo{Repo: "r1", Worktree: "wt", Session: "a", Name: "cctl/r1/wt/a"}
	sessB := &SessionInfo{Repo: "r1", Worktree: "other", Session: "b", Name: "cctl/r1/other/b"}
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"w": {Local: true, Repos: map[string]Repo{"r1": {Path: "/tmp/r1"}}}}},
		serverNames: []string{"w"},
		state:       map[string]*serverState{"w": {}},
		rows: []treeRow{
			{kind: rowSession, server: "w", repo: "r1", worktree: "wt", session: sessA},
			{kind: rowWorktree, server: "w", repo: "r1", worktree: "wt"},
			{kind: rowSession, server: "w", repo: "r1", worktree: "other", session: sessB},
		},
		cursor: 0,
	}
	// First dd on session A: starts a running task.
	_, cmd := m.executeDelete()
	if cmd == nil {
		t.Fatalf("first delete should produce a cmd")
	}
	if m.runningTaskFor("sess:w/r1/wt/a") == nil {
		t.Fatalf("first delete should register a running task")
	}
	before := len(m.tasks)
	// Second dd on the same row: refused with a failed notice, no new
	// running task.
	_, _ = m.executeDelete()
	last := lastTask(m)
	if last == nil || !last.failed || !strings.Contains(last.label, "busy") {
		t.Errorf("dd on busy row should refuse with a 'busy' notice; got %+v", last)
	}
	if got := len(m.tasks); got != before+1 {
		t.Errorf("refusal should add exactly one notice; tasks %d → %d", before, got)
	}
	// dd on the parent worktree: also refused (ancestor conflict).
	m.cursor = 1
	_, _ = m.executeDelete()
	if last := lastTask(m); last == nil || !last.failed || !strings.Contains(last.label, "busy") {
		t.Errorf("dd on parent worktree of a busy session should refuse; got %+v", last)
	}
	// dd on an unrelated session: allowed (new running task).
	m.cursor = 2
	_, cmd = m.executeDelete()
	if cmd == nil {
		t.Fatalf("delete on unrelated row should produce a cmd")
	}
	if m.runningTaskFor("sess:w/r1/other/b") == nil {
		t.Errorf("unrelated delete should run concurrently, not be blocked")
	}
}

// TestPreparingPrefix_TaskTargetsSpin covers the per-row spinner: the row
// targeted by a running task spins, and so does its parent worktree
// (subtree overlap) — but unrelated rows don't, and finished tasks stop
// the spinner.
func TestPreparingPrefix_TaskTargetsSpin(t *testing.T) {
	m := &tuiModel{spinnerFrame: 3}
	id, _ := m.startTask("sess:w/r1/wt/a", "deleting…")
	target := treeRow{kind: rowSession, server: "w", repo: "r1",
		session: &SessionInfo{Repo: "r1", Worktree: "wt", Session: "a"}}
	parent := treeRow{kind: rowWorktree, server: "w", repo: "r1", worktree: "wt"}
	other := treeRow{kind: rowRepo, server: "w", repo: "r2"}
	if got := m.preparingPrefix(target); got == "" {
		t.Errorf("targeted row should get a spinner prefix, got empty")
	}
	if got := m.preparingPrefix(parent); got == "" {
		t.Errorf("parent worktree of a busy session should spin too, got empty")
	}
	if got := m.preparingPrefix(other); got != "" {
		t.Errorf("unrelated row should NOT get a spinner prefix, got %q", got)
	}
	m.finishTask(id, "done", nil)
	if got := m.preparingPrefix(target); got != "" {
		t.Errorf("finished task: no row should spin, got %q", got)
	}
}

// ---- tmux name sanitization --------------------------------------------------

// TestTmuxName_SanitizesLikeTmux pins the rxtx.dev fix: tmux silently
// replaces '.' and ':' in session names, so cctl must build names that
// already match what tmux will store — otherwise has-session/attach
// target a name that doesn't exist.
func TestTmuxName_SanitizesLikeTmux(t *testing.T) {
	if got := tmuxName("rxtx.dev", "main", "mneme"); got != "cctl/rxtx_dev/main/mneme" {
		t.Errorf("tmuxName = %q, want dots replaced like tmux does", got)
	}
	if got := tmuxName("plain", "wt", "s"); got != "cctl/plain/wt/s" {
		t.Errorf("tmuxName = %q, names without reserved chars must pass through", got)
	}
}

// TestNormalizeSessionNames maps parsed-back sanitized components to the
// real repo/worktree names so sessions group under the right rows instead
// of a ghost "rxtx_dev" repo. Exact matches must win over remaps.
func TestNormalizeSessionNames(t *testing.T) {
	st := &serverState{
		repos: map[string]Repo{
			"rxtx.dev":  {Path: "/r/rxtx.dev"},
			"exact_one": {Path: "/r/exact_one"},
		},
		worktrees: map[string][]Worktree{
			"rxtx.dev": {{Name: "v1.2"}, {Name: "main"}},
		},
		sessions: []SessionInfo{
			{Repo: "rxtx_dev", Worktree: "v1_2", Session: "a"},
			{Repo: "rxtx_dev", Worktree: "main", Session: "b"},
			{Repo: "exact_one", Worktree: "wt", Session: "c"},
			{Repo: "unknown_repo", Worktree: "wt", Session: "d"},
		},
	}
	st.normalizeSessionNames()
	if st.sessions[0].Repo != "rxtx.dev" || st.sessions[0].Worktree != "v1.2" {
		t.Errorf("session a: got %s/%s, want rxtx.dev/v1.2", st.sessions[0].Repo, st.sessions[0].Worktree)
	}
	if st.sessions[1].Repo != "rxtx.dev" || st.sessions[1].Worktree != "main" {
		t.Errorf("session b: got %s/%s, want rxtx.dev/main", st.sessions[1].Repo, st.sessions[1].Worktree)
	}
	if st.sessions[2].Repo != "exact_one" {
		t.Errorf("session c: exact repo name must be kept, got %s", st.sessions[2].Repo)
	}
	if st.sessions[3].Repo != "unknown_repo" {
		t.Errorf("session d: unmatched repo must stay as parsed, got %s", st.sessions[3].Repo)
	}
}

// ---- plain-terminal sessions (`t` key) --------------------------------------

// TestTerminalSessions covers the t-key plumbing: name recognition (so
// dead terminal sessions resurrect as shells, not claude), the idempotent
// new-session -A -c command, and per-worktree name auto-increment.
func TestTerminalSessions(t *testing.T) {
	for name, want := range map[string]bool{
		"term": true, "term2": true, "term17": true,
		"terminal": false, "xterm": false, "poc": false, "": false,
	} {
		if got := isTerminalSession(name); got != want {
			t.Errorf("isTerminalSession(%q) = %v want %v", name, got, want)
		}
	}

	cmd := terminalCmd("cctl/r/wt/term", "/wt/path")
	if !strings.Contains(cmd, "tmux new-session -A -s cctl/r/wt/term") || !strings.Contains(cmd, `-c "/wt/path"`) {
		t.Errorf("terminalCmd = %q, want new-session -A with -c cwd", cmd)
	}
	if strings.Contains(cmd, "claude") {
		t.Errorf("terminalCmd must not involve claude: %q", cmd)
	}

	// attachOrRespawn routes terminal-named sessions to the shell command.
	r := &Resolved{}
	if got := attachOrRespawn(r, "cctl/r/wt/term2", "/wt"); strings.Contains(got, "claude") {
		t.Errorf("attachOrRespawn on a term session must not resurrect claude: %q", got)
	}
	if got := attachOrRespawn(r, "cctl/r/wt/poc", "/wt"); !strings.Contains(got, "claude") {
		t.Errorf("attachOrRespawn on a normal session must keep the claude launch: %q", got)
	}

	m := &tuiModel{state: map[string]*serverState{"w": {sessions: []SessionInfo{
		{Repo: "r", Worktree: "wt", Session: "term"},
		{Repo: "r", Worktree: "wt", Session: "term2"},
		{Repo: "r", Worktree: "other", Session: "term"},
	}}}}
	if got := m.freeTerminalName("w", "r", "wt"); got != "term3" {
		t.Errorf("freeTerminalName(wt) = %q want term3", got)
	}
	if got := m.freeTerminalName("w", "r", "fresh"); got != "term" {
		t.Errorf("freeTerminalName(fresh wt) = %q want term", got)
	}
}

// ---- cursor identity across rebuilds ----------------------------------------

// TestRestoreCursor_FollowsRowIdentity is the regression test for "dd
// moves my cursor to the wrong place": the cursor must track the row it
// was on (by identity, not index) when the tree changes shape, and fall
// back to the nearest surviving ancestor when that row was deleted.
func TestRestoreCursor_FollowsRowIdentity(t *testing.T) {
	sessA := &SessionInfo{Repo: "r1", Worktree: "wt", Session: "a"}
	sessB := &SessionInfo{Repo: "r1", Worktree: "wt", Session: "b"}
	srv := treeRow{kind: rowServer, server: "w"}
	repo := treeRow{kind: rowRepo, server: "w", repo: "r1"}
	wt := treeRow{kind: rowWorktree, server: "w", repo: "r1", worktree: "wt"}
	rowA := treeRow{kind: rowSession, server: "w", repo: "r1", worktree: "wt", session: sessA}
	rowB := treeRow{kind: rowSession, server: "w", repo: "r1", worktree: "wt", session: sessB}

	// Cursor on session b; a delete of session a (above it) lands and the
	// refresh removes that row. The highlight must stay on b, not on
	// whatever slid into index 4.
	m := &tuiModel{rows: []treeRow{srv, repo, wt, rowA, rowB}, cursor: 4}
	prev := m.rows[m.cursor]
	m.rows = []treeRow{srv, repo, wt, rowB}
	m.restoreCursor(&prev)
	if got := m.rows[m.cursor]; got.kind != rowSession || got.session.Session != "b" {
		t.Errorf("cursor should follow session b, landed on %+v", got)
	}

	// Cursor on the row that was itself deleted: fall back to its
	// worktree row.
	m = &tuiModel{rows: []treeRow{srv, repo, wt, rowA, rowB}, cursor: 3}
	prev = m.rows[m.cursor]
	m.rows = []treeRow{srv, repo, wt, rowB}
	m.restoreCursor(&prev)
	if got := m.rows[m.cursor]; got.kind != rowWorktree || got.worktree != "wt" {
		t.Errorf("deleted row's cursor should land on its worktree, landed on %+v", got)
	}

	// Whole worktree gone: fall back to the repo row.
	m = &tuiModel{rows: []treeRow{srv, repo, wt, rowA, rowB}, cursor: 2}
	prev = m.rows[m.cursor]
	m.rows = []treeRow{srv, repo}
	m.restoreCursor(&prev)
	if got := m.rows[m.cursor]; got.kind != rowRepo || got.repo != "r1" {
		t.Errorf("deleted worktree's cursor should land on its repo, landed on %+v", got)
	}
}

// ---- sync (R/S) and upgrade (UU) --------------------------------------------

// TestSyncCmux_RegistersRunningTask pins the R/S entrypoint: it always kicks
// off the reconcile as a tracked background task (the actual adopt/heal/
// restore/close work runs off-thread in syncCmuxState, which talks to live
// cmux+tmux and is covered by the pure-helper tests). The old detached-only
// selection rule moved into syncCmuxState.
func TestSyncCmux_RegistersRunningTask(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"up": {Local: true}}},
		serverNames: []string{"up"},
		state:       map[string]*serverState{"up": {conn: connConnected, sessionsLoaded: true}},
	}
	_, cmd := m.syncCmux()
	if cmd == nil {
		t.Fatalf("syncCmux should produce a cmd")
	}
	last := lastTask(m)
	if last == nil || last.done {
		t.Fatalf("syncCmux should register a running task, got %+v", last)
	}
	if !strings.Contains(last.label, "reconcil") {
		t.Errorf("task label should mention reconcile; got %q", last.label)
	}
}

// TestUpgradeTarget_ExcludesTerminalSessions: UU restarts claude
// sessions only — plain term* shells don't run claude and must be left
// alone.
func TestUpgradeTarget_ExcludesTerminalSessions(t *testing.T) {
	m := &tuiModel{
		serverNames: []string{"w"},
		state: map[string]*serverState{"w": {sessions: []SessionInfo{
			{Repo: "r", Worktree: "wt", Session: "poc", Name: "cctl/r/wt/poc"},
			{Repo: "r", Worktree: "wt", Session: "term", Name: "cctl/r/wt/term"},
			{Repo: "r", Worktree: "wt", Session: "term2", Name: "cctl/r/wt/term2"},
		}}},
		rows:   []treeRow{{kind: rowServer, server: "w"}},
		cursor: 0,
	}
	server, names := m.upgradeTarget()
	if server != "w" {
		t.Errorf("upgradeTarget server = %q want w", server)
	}
	if len(names) != 1 || names[0] != "cctl/r/wt/poc" {
		t.Errorf("upgradeTarget names = %v, want only the claude session", names)
	}
}

// TestClaudeUpdateScript_RunsInLoginShellWithInstallDirs pins the fix for
// "UU fails on remote: claude: command not found" — bare ssh gets the
// minimal PATH, so the update must run through a login shell with the
// well-known install dirs prepended.
func TestClaudeUpdateScript_RunsInLoginShellWithInstallDirs(t *testing.T) {
	got := claudeUpdateScript("claude update")
	for _, want := range []string{
		"bash -lc 'claude update'",
		"$HOME/.local/bin",
		"$HOME/.local/share/mise/shims",
		`export PATH=`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("claudeUpdateScript missing %q; got:\n%s", want, got)
		}
	}
}

// TestUpgradeClaude_BlocksWholeServerWhileRunning: the upgrade task is
// keyed on the server row, so any action under that server conflicts
// until it finishes.
func TestUpgradeClaude_BlocksWholeServerWhileRunning(t *testing.T) {
	m := &tuiModel{
		cfg:         &Config{Servers: map[string]Server{"w": {Local: true}}},
		serverNames: []string{"w"},
		state:       map[string]*serverState{"w": {}},
		rows:        []treeRow{{kind: rowServer, server: "w"}},
		cursor:      0,
	}
	_, cmd := m.upgradeClaude()
	if cmd == nil {
		t.Fatalf("upgradeClaude should produce a cmd")
	}
	if m.runningTaskFor("sess:w/r/wt/x") == nil {
		t.Errorf("a session row under the upgrading server should read as busy")
	}
	// Second UU while running: refused.
	_, _ = m.upgradeClaude()
	if last := lastTask(m); last == nil || !last.failed || !strings.Contains(last.label, "busy") {
		t.Errorf("upgrade while busy should refuse, got %+v", last)
	}
}

// ---- helpers ---------------------------------------------------------------

func keys(m map[string]Repo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
