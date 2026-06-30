//go:build integration

// Integration tests for cctl. These exercise real ssh/bash, real git, real
// tmux — they require a working local environment plus optionally a
// reachable workspace server.
//
// Run with: go test -tags=integration ./...
//
// Coverage:
//   - worktree create + git-visibility + remove (both servers)
//   - tmux new-session + listSessions + hasSession + kill (both servers)
//   - cleanupSession (the unified kill+rm-worktree path used by rm/rma/TUI)
//     when worktree name differs from session name (TUI flow)
//   - cleanupSession when worktree name == session name (CLI flow)
//
// Each scenario sets up its own throwaway repo and worktree base in /tmp,
// so the user's real config and worktrees are never touched.

package cctl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const workspaceHost = "100.125.230.87"

// loadIntegrationServers returns (local, workspace, hasWorkspace).
// workspace is loaded from the user's config so we honor whatever ssh
// options/key/host they actually use, with a short connect timeout so
// tests fail fast when the box is unreachable.
func loadIntegrationServers(t *testing.T) (Server, Server, bool) {
	t.Helper()
	local := Server{Local: true}
	cfg, _, err := loadConfig()
	if err != nil {
		t.Logf("integration: loadConfig failed (%v) — workspace will be skipped", err)
		return local, Server{}, false
	}
	srv, ok := cfg.Servers["workspace"]
	if !ok {
		t.Logf("integration: no \"workspace\" server in config — skipping remote tests")
		return local, Server{}, false
	}
	// Reachability probe: short tcp ping via ssh BatchMode so we don't hang.
	probe := Server{
		Host: srv.Host, User: srv.User, Port: srv.Port, SSHKey: srv.SSHKey,
		SSHOpts: append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5"}, srv.SSHOpts...),
	}
	if _, err := runRemote(probe, "true"); err != nil {
		t.Logf("integration: workspace unreachable (%v) — skipping remote tests", err)
		return local, Server{}, false
	}
	return local, srv, true
}

// remoteTempDir creates a mktemp -d on the target server and registers cleanup.
func remoteTempDir(t *testing.T, s Server, prefix string) string {
	t.Helper()
	out, err := runRemote(s, fmt.Sprintf("mktemp -d /tmp/%s.XXXXXX", prefix))
	if err != nil {
		t.Fatalf("mktemp on %s: %v", transportLabel(s), err)
	}
	path := strings.TrimSpace(out)
	t.Cleanup(func() {
		_, _ = runRemote(s, fmt.Sprintf("rm -rf %s", shellQuote(path)))
	})
	return path
}

// initBareRepo creates a git repo with one empty commit at path and returns
// the resulting branch name (whatever the user's git defaults to — `main`
// on modern git, `master` on older).
func initBareRepo(t *testing.T, s Server, path string) string {
	t.Helper()
	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf("cd %s", shellQuote(path)),
		"git init -q",
		"git config user.email cctl-integration@local",
		"git config user.name 'cctl integration'",
		// Globally-configured signing/hooks must not affect the test repo.
		"git -c commit.gpgsign=false -c gpg.format=openpgp commit -q --allow-empty -m init",
		"git rev-parse --abbrev-ref HEAD",
	}, " && ")
	out, err := runRemote(s, script)
	if err != nil {
		t.Fatalf("init repo at %s on %s: %v", path, transportLabel(s), err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		t.Fatalf("init repo at %s: empty branch name (out=%q)", path, out)
	}
	return branch
}

// ensureRemovedOnRemote returns nil if path no longer exists on the server.
func pathExistsRemote(t *testing.T, s Server, path string) bool {
	t.Helper()
	out, err := runRemote(s, fmt.Sprintf(`test -e %s && echo present || echo gone`, shellQuote(path)))
	if err != nil {
		t.Fatalf("test -e %s: %v", path, err)
	}
	return strings.TrimSpace(out) == "present"
}

// killAndRemove is the test harness's equivalent of cleanupSession — same
// shape as cmd_rm does it (tmux kill-session + removeWorktreeScript).
// wtBase is the configured worktree_base (must contain wtPath for the
// removal to be allowed by the safety guards).
func killAndRemove(t *testing.T, s Server, repoPath, wtPath, wtBase, tname string) {
	t.Helper()
	// kill-session is non-fatal if it's already dead.
	_, _ = runRemote(s, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null || true", shellQuote(tname)))
	if ok, _ := hasSession(s, tname); ok {
		t.Errorf("kill-session left tmux session %s alive on %s", tname, transportLabel(s))
	}
	if _, err := runRemote(s, removeWorktreeScript(repoPath, wtPath, wtBase)); err != nil {
		t.Fatalf("removeWorktreeScript on %s: %v", transportLabel(s), err)
	}
	if pathExistsRemote(t, s, wtPath) {
		t.Errorf("worktree dir still present at %s on %s", wtPath, transportLabel(s))
	}
}

// ---- worktree round-trip ---------------------------------------------------

func TestIntegration_LocalWorktreeRoundTrip(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	worktreeRoundTrip(t, local, "local")
}

func TestIntegration_WorkspaceWorktreeRoundTrip(t *testing.T) {
	_, ws, ok := loadIntegrationServers(t)
	if !ok {
		t.Skip("workspace not available")
	}
	worktreeRoundTrip(t, ws, "workspace")
}

func worktreeRoundTrip(t *testing.T, srv Server, label string) {
	t.Helper()
	repo := remoteTempDir(t, srv, "cctl-repo")
	base := initBareRepo(t, srv, repo)
	wtBase := remoteTempDir(t, srv, "cctl-wt")
	wtPath := filepath.Join(wtBase, "feature-x")

	// 1. Create worktree from the ensure script (the same code path the
	//    CLI/TUI use).
	script := ensureWorktreeScript(repo, wtPath, "integration/feature-x", base, []string{"echo HOOK_RAN > " + shellQuote(filepath.Join(wtPath, ".hook"))})
	if out, err := runRemote(srv, script); err != nil {
		t.Fatalf("[%s] ensureWorktreeScript: %v\nout: %s", label, err, out)
	}
	if !pathExistsRemote(t, srv, wtPath) {
		t.Fatalf("[%s] worktree dir not created at %s", label, wtPath)
	}

	// 2. Git agrees this worktree belongs to the repo.
	out, err := runRemote(srv, fmt.Sprintf("git -C %s worktree list --porcelain", shellQuote(repo)))
	if err != nil {
		t.Fatalf("[%s] worktree list: %v", label, err)
	}
	if !strings.Contains(out, wtPath) {
		t.Errorf("[%s] git worktree list missing %s; got:\n%s", label, wtPath, out)
	}

	// 3. post-create hook actually ran inside the worktree.
	hookOut, err := runRemote(srv, fmt.Sprintf("cat %s 2>/dev/null || echo MISSING", shellQuote(filepath.Join(wtPath, ".hook"))))
	if err != nil {
		t.Fatalf("[%s] read hook marker: %v", label, err)
	}
	if !strings.Contains(hookOut, "HOOK_RAN") {
		t.Errorf("[%s] post-create hook didn't run; marker file says %q", label, strings.TrimSpace(hookOut))
	}

	// 4. Re-running the same script must be a no-op (worktree exists), and
	//    must still re-run the post-create hook — that's the contract.
	if _, err := runRemote(srv, fmt.Sprintf("rm -f %s", shellQuote(filepath.Join(wtPath, ".hook")))); err != nil {
		t.Fatalf("[%s] clear hook marker: %v", label, err)
	}
	if _, err := runRemote(srv, script); err != nil {
		t.Fatalf("[%s] ensureWorktreeScript (rerun): %v", label, err)
	}
	hookOut, _ = runRemote(srv, fmt.Sprintf("cat %s 2>/dev/null || echo MISSING", shellQuote(filepath.Join(wtPath, ".hook"))))
	if !strings.Contains(hookOut, "HOOK_RAN") {
		t.Errorf("[%s] post-create hook didn't run on rerun (regression of post_create-existing fix); marker=%q", label, strings.TrimSpace(hookOut))
	}

	// 5. Remove worktree and verify cleanup.
	if _, err := runRemote(srv, removeWorktreeScript(repo, wtPath, wtBase)); err != nil {
		t.Fatalf("[%s] removeWorktreeScript: %v", label, err)
	}
	if pathExistsRemote(t, srv, wtPath) {
		t.Errorf("[%s] worktree dir still present after remove: %s", label, wtPath)
	}
}

// TestIntegration_LocalEnsureWorktree_MissingRepoErrorIsActionable covers the
// recent bug: when the configured repo path doesn't exist on the server, the
// error must name the original config form (e.g. ~/foo) so the user can find
// the right config key to edit. Run only locally so we control the input.
func TestIntegration_LocalEnsureWorktree_MissingRepoErrorIsActionable(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	bogus := "/tmp/cctl-definitely-not-a-repo-" + fmt.Sprint(time.Now().UnixNano())
	wt := filepath.Join(remoteTempDir(t, local, "cctl-wt"), "feat")
	script := ensureWorktreeScript(bogus, wt, "x", "main", nil)
	_, err := runRemote(local, script)
	if err == nil {
		t.Fatalf("expected error for missing repo path")
	}
	want := []string{"does not exist on this host", "~/.cctl.yaml"}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error message must contain %q; got: %v", w, err)
		}
	}
}

// ---- tmux session round-trip -----------------------------------------------

func TestIntegration_LocalSessionRoundTrip(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	sessionRoundTrip(t, local, "local")
}

func TestIntegration_WorkspaceSessionRoundTrip(t *testing.T) {
	_, ws, ok := loadIntegrationServers(t)
	if !ok {
		t.Skip("workspace not available")
	}
	sessionRoundTrip(t, ws, "workspace")
}

func sessionRoundTrip(t *testing.T, srv Server, label string) {
	t.Helper()
	if _, err := runRemote(srv, "command -v tmux"); err != nil {
		t.Skipf("[%s] tmux not installed on remote: %v", label, err)
	}
	repo := remoteTempDir(t, srv, "cctl-repo")
	base := initBareRepo(t, srv, repo)
	wtBase := remoteTempDir(t, srv, "cctl-wt")
	wtName := "wtA"
	sessName := "sessA"
	wtPath := filepath.Join(wtBase, wtName)

	if _, err := runRemote(srv, ensureWorktreeScript(repo, wtPath, "integration/"+wtName, base, nil)); err != nil {
		t.Fatalf("[%s] create worktree: %v", label, err)
	}
	// Repo name in tmux is whatever cctl would assign; use a unique sentinel
	// per test run to avoid collisions with leftover sessions.
	repoLabel := fmt.Sprintf("itrepo%d", time.Now().UnixNano())
	tname := tmuxName(repoLabel, wtName, sessName)
	defer killAndRemove(t, srv, repo, wtPath, wtBase, tname)

	// Start a detached tmux session running a long sleep (proxy for claude).
	launch := fmt.Sprintf("cd %s && sleep 120", shellQuote(wtPath))
	if _, err := runRemote(srv, fmt.Sprintf("tmux new-session -d -s %s %s", shellQuote(tname), shellQuote(launch))); err != nil {
		t.Fatalf("[%s] tmux new-session: %v", label, err)
	}

	// hasSession === true
	ok, err := hasSession(srv, tname)
	if err != nil || !ok {
		t.Errorf("[%s] hasSession(%s)=%v err=%v", label, tname, ok, err)
	}

	// listSessions sees our session with parsed parts intact.
	sessions, err := listSessions(label, srv)
	if err != nil {
		t.Fatalf("[%s] listSessions: %v", label, err)
	}
	var found *SessionInfo
	for i := range sessions {
		if sessions[i].Name == tname {
			found = &sessions[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("[%s] %s not in listSessions; got %d sessions", label, tname, len(sessions))
	}
	if found.Repo != repoLabel || found.Worktree != wtName || found.Session != sessName {
		t.Errorf("[%s] listSessions parsed wrong parts: %+v", label, found)
	}

	// Idempotent path: prepareClaude detects an existing session via
	// hasSession() and produces a `tmux new-session -A` command (attach
	// if alive, resurrect if not) rather than a fresh launch. We don't
	// actually attach here (no tty), but we can verify the precondition
	// that drives that branch: hasSession is true *and* a duplicate
	// new-session (without -A) would fail. The actual attach path is
	// exercised end-to-end via `cctl claude` itself in interactive use.
	if _, err := runRemote(srv, fmt.Sprintf("tmux new-session -d -s %s true 2>&1", shellQuote(tname))); err == nil {
		t.Errorf("[%s] expected duplicate new-session to fail (proves attach branch is needed)", label)
	}

	// Kill — hasSession should report false afterwards.
	if _, err := runRemote(srv, fmt.Sprintf("tmux kill-session -t %s", shellQuote(tname))); err != nil {
		t.Fatalf("[%s] kill-session: %v", label, err)
	}
	if ok, _ := hasSession(srv, tname); ok {
		t.Errorf("[%s] session still alive after kill-session", label)
	}
}

// ---- worktree-name != session-name (TUI flow) ------------------------------

// TestIntegration_LocalCleanup_WorktreeNameDiffersFromSession is the
// regression test for cmd_rm.go/cmd_rma.go using sessionName as the
// worktree component. With wt="featureWT" and session="quicktest", removing
// must target the correct paths.
func TestIntegration_LocalCleanup_WorktreeNameDiffersFromSession(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	if _, err := runRemote(local, "command -v tmux"); err != nil {
		t.Skipf("tmux not installed: %v", err)
	}
	repo := remoteTempDir(t, local, "cctl-repo")
	base := initBareRepo(t, local, repo)
	wtBase := remoteTempDir(t, local, "cctl-wt")
	wtName := "featureWT"
	sessName := "quicktest"
	wtPath := filepath.Join(wtBase, wtName)
	if _, err := runRemote(local, ensureWorktreeScript(repo, wtPath, "integration/"+wtName, base, nil)); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	repoLabel := fmt.Sprintf("itrepo%d", time.Now().UnixNano())
	tname := tmuxName(repoLabel, wtName, sessName)
	launch := fmt.Sprintf("cd %s && sleep 120", shellQuote(wtPath))
	if _, err := runRemote(local, fmt.Sprintf("tmux new-session -d -s %s %s", shellQuote(tname), shellQuote(launch))); err != nil {
		t.Fatalf("tmux new-session: %v", err)
	}
	// Sanity precondition.
	if ok, _ := hasSession(local, tname); !ok {
		t.Fatalf("session %s didn't start", tname)
	}

	// Now drive the cleanup the way cmd_rm does it: look up SessionInfo,
	// then use s.Worktree (NOT s.Session) for the worktree path.
	sessions, _ := listSessions("local", local)
	var match SessionInfo
	for _, s := range sessions {
		if s.Name == tname {
			match = s
		}
	}
	if match.Name == "" {
		t.Fatalf("listSessions didn't return %s", tname)
	}
	if match.Worktree != wtName {
		t.Fatalf("listSessions parsed worktree=%q want %q", match.Worktree, wtName)
	}
	// The bug-prone line: previously rm used sessionName for both slots.
	// Verify that worktreePath built from match.Worktree resolves correctly.
	gotWtPath := worktreePath(wtBase, repoLabel, match.Worktree)
	wantWtPath := filepath.Join(wtBase, repoLabel, match.Worktree)
	if gotWtPath != wantWtPath {
		t.Errorf("worktreePath built %q, want %q", gotWtPath, wantWtPath)
	}
	// And that the inverse (bug) would have aimed at the wrong dir.
	bugged := worktreePath(wtBase, repoLabel, match.Session)
	if bugged == gotWtPath {
		t.Errorf("bugged and fixed worktreePath() coincide — test can't detect regression")
	}
	killAndRemove(t, local, repo, wtPath, wtBase, tname)
}

// TestIntegration_DDOnMainCheckoutMustNotDeleteRepo is the e2e regression
// test for the 2026-05-13 incident where pressing `dd` on what looked like
// the main worktree row deleted the entire repo from disk (the samePath
// bug + missing script-level guard).
//
// Sets up a real git repo, runs removeWorktreeScript with WT==REPO (the
// exact shape that would fire if the caller misclassifies), and asserts
// the repo still exists afterward. The script MUST refuse with exit 3.
func TestIntegration_DDOnMainCheckoutMustNotDeleteRepo(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	repo := remoteTempDir(t, local, "cctl-mainsafe")
	_ = initBareRepo(t, local, repo)
	// Sanity precondition.
	if !pathExistsRemote(t, local, repo) {
		t.Fatalf("setup: repo path missing at %s before test", repo)
	}
	// Drive the exact failure path: WT == REPO (the configured main
	// checkout). The script's guard must refuse.
	script := removeWorktreeScript(repo, repo, filepath.Dir(repo))
	_, err := runRemote(local, script)
	if err == nil {
		t.Fatalf("removeWorktreeScript ran SUCCESSFULLY against the main checkout — that's the data-loss bug")
	}
	if !strings.Contains(err.Error(), "main checkout") {
		t.Errorf("error should call out the refusal cause; got: %v", err)
	}
	// And — crucially — the repo must still be there.
	if !pathExistsRemote(t, local, repo) {
		t.Fatalf("repo at %s was deleted despite the guard — the bug is back", repo)
	}
}

// TestIntegration_DDOnMainCheckout_TildePath layers the tilde regression on
// top of the script guard test: builds the script the way it would actually
// be built when the user's config has `path: ~/something`. Even with
// shellPath rewriting `~/x` → `"$HOME/x"`, the runtime comparison must
// still flag the main checkout.
func TestIntegration_DDOnMainCheckout_TildePath(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	// Build a fake $HOME inside a tempdir so we can use ~-rooted paths
	// against it without touching the real $HOME.
	fakeHome := remoteTempDir(t, local, "cctl-fakehome")
	repoSubdir := "my-app-int"
	repoAbs := filepath.Join(fakeHome, repoSubdir)
	if _, err := runRemote(local, "mkdir -p "+shellQuote(repoAbs)); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	_ = initBareRepo(t, local, repoAbs)
	// Now run the script with REPO and WT both set to `~/my-app-int`
	// in config form, with HOME overridden so $HOME/x resolves to repoAbs.
	repoCfg := "~/" + repoSubdir
	script := "HOME=" + shellQuote(fakeHome) + "\n" + removeWorktreeScript(repoCfg, repoCfg, "~")
	_, err := runRemote(local, script)
	if err == nil {
		t.Fatalf("tilde-path WT==REPO ran SUCCESSFULLY — the script didn't refuse")
	}
	if !pathExistsRemote(t, local, repoAbs) {
		t.Fatalf("repo at %s was deleted via ~-rooted WT==REPO — the regression is back", repoAbs)
	}
}

// TestIntegration_RemoveWorktree_RefusesOutsideWorktreeBase drives the new
// safety guard: even when WT != REPO and WT isn't a parent of REPO, the
// script must refuse if WT lives OUTSIDE the configured worktree_base.
// This is the load-bearing safety net: if every other check were broken,
// rm -rf can still only ever target a path the user designated for
// worktrees.
func TestIntegration_RemoveWorktree_RefusesOutsideWorktreeBase(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	// Build: repo at /tmp/<a>/repo, wt-base at /tmp/<b>/base, and a
	// "victim" dir at /tmp/<c>/something — totally outside both base
	// and repo. Asking removeWorktreeScript to remove the victim with
	// worktreeBase set to /tmp/<b>/base MUST be refused.
	repo := remoteTempDir(t, local, "cctl-r")
	_ = initBareRepo(t, local, repo)
	base := remoteTempDir(t, local, "cctl-b")
	victim := remoteTempDir(t, local, "cctl-victim")
	// Drop a sentinel inside victim so we can prove it survived.
	if _, err := runRemote(local, "echo SURVIVED > "+shellQuote(filepath.Join(victim, "marker"))); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	script := removeWorktreeScript(repo, victim, base)
	_, err := runRemote(local, script)
	if err == nil {
		t.Fatalf("removeWorktreeScript ran SUCCESSFULLY against %s (outside %s) — the worktree_base guard didn't fire", victim, base)
	}
	if !strings.Contains(err.Error(), "outside worktree_base") {
		t.Errorf("error should call out worktree_base; got: %v", err)
	}
	// The victim and its marker must still exist.
	if !pathExistsRemote(t, local, victim) {
		t.Fatalf("victim dir at %s was deleted despite being outside worktree_base", victim)
	}
	out, _ := runRemote(local, "cat "+shellQuote(filepath.Join(victim, "marker")))
	if !strings.Contains(out, "SURVIVED") {
		t.Errorf("victim marker missing — file system damage occurred: %q", out)
	}
}

// TestIntegration_RemoveWorktree_RefusesAncestorOfRepo covers the
// ancestor case: if WT is a parent dir of REPO (e.g. WT=/home/user and
// REPO=/home/user/myrepo), removing WT would take the repo with it. The
// script must refuse.
func TestIntegration_RemoveWorktree_RefusesAncestorOfRepo(t *testing.T) {
	local, _, _ := loadIntegrationServers(t)
	parent := remoteTempDir(t, local, "cctl-p")
	repo := filepath.Join(parent, "inner-repo")
	if _, err := runRemote(local, "mkdir -p "+shellQuote(repo)); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	_ = initBareRepo(t, local, repo)
	// Even though `parent` is itself inside the worktree_base we pass,
	// the ancestor-of-repo check should refuse before the base check
	// runs (and the test asserts the repo isn't removed regardless of
	// which guard fires).
	script := removeWorktreeScript(repo, parent, parent)
	_, err := runRemote(local, script)
	if err == nil {
		t.Fatalf("removeWorktreeScript ran SUCCESSFULLY with WT=%s as a parent of REPO=%s", parent, repo)
	}
	if !pathExistsRemote(t, local, repo) {
		t.Fatalf("repo at %s was deleted — ancestor-of-repo guard didn't fire", repo)
	}
}

// TestIntegration_LoadConfig_ExpansionOnLocal is a sanity check that the
// config-resolution path (which feeds prepareClaude) actually works with
// the user's installed config — protects against schema/key renames.
func TestIntegration_LoadConfig_ExpansionOnLocal(t *testing.T) {
	cfg, path, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !strings.HasSuffix(path, ".cctl.yaml") && !strings.HasSuffix(path, ".cctl.yml") {
		t.Errorf("config path doesn't look like .cctl.yaml: %s", path)
	}
	if _, ok := cfg.Servers["local"]; !ok {
		t.Errorf("no local server in config — most of cctl is broken without one")
	}
	// resolve("local", "") should pick a repo for the user (or error
	// cleanly if local has zero repos). The point here is the call doesn't
	// panic and returns a usable Resolved or a real error.
	r, err := cfg.resolve("local", "")
	if err != nil {
		// If local has multiple repos, bare resolve returns an error —
		// that's expected and not a failure of this test.
		t.Logf("resolve(local, \"\"): %v (acceptable when local has !=1 repos)", err)
		return
	}
	if r.WorktreeBase == "" {
		t.Errorf("resolved worktree_base is empty — defaults missing?")
	}
	if r.Server.Local != true {
		t.Errorf("resolved local server's Local field is false: %+v", r.Server)
	}
}

// TestInitTmux_ConfigParsesInRealTmux feeds the generated tmux managed block to
// a real tmux server and asserts it both parses cleanly and registers the
// copy/clipboard directives. The unit test only checks the lines are present as
// strings; this is the guard that catches tmux *rejecting* them — e.g. the
// "invalid octal escape" bug where the OSC 52 Ms override needed doubled
// backslashes (\\E / \\007) to survive tmux's config-string lexer.
func TestInitTmux_ConfigParsesInRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "tmux.conf")
	merged, _ := mergeManagedBlock("", tmuxManagedBody) // exactly what applyManaged writes
	if err := os.WriteFile(conf, []byte(merged), 0o644); err != nil {
		t.Fatalf("write temp tmux.conf: %v", err)
	}

	sock := fmt.Sprintf("cctl-it-%d", os.Getpid())
	run := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", sock}, args...)...).CombinedOutput()
		return string(out), err
	}
	// Start an isolated server (-f /dev/null) so the user's real ~/.tmux.conf
	// doesn't interfere, then source ours explicitly to surface parse errors.
	if out, err := exec.Command("tmux", "-L", sock, "-f", "/dev/null", "new-session", "-d", "-s", "probe").CombinedOutput(); err != nil {
		t.Fatalf("start tmux server: %v\n%s", err, out)
	}
	defer run("kill-server")

	if out, err := run("source-file", conf); err != nil ||
		strings.Contains(strings.ToLower(out), "invalid") ||
		strings.Contains(strings.ToLower(out), "error") {
		t.Fatalf("tmux rejected the generated config: err=%v out=%q", err, out)
	}

	checks := []struct {
		what  string
		args  []string
		needs string
	}{
		{"Ms OSC 52 override", []string{"show-options", "-g", "terminal-overrides"}, "Ms="},
		{"set-clipboard", []string{"show-options", "-s", "set-clipboard"}, "on"},
		{"allow-passthrough", []string{"show-options", "-g", "allow-passthrough"}, "on"},
		{"focus-events", []string{"show-options", "-g", "focus-events"}, "on"},
		{"aggressive-resize", []string{"show-options", "-gw", "aggressive-resize"}, "on"},
	}
	for _, c := range checks {
		out, err := run(c.args...)
		if err != nil {
			t.Errorf("%s: query failed: %v", c.what, err)
			continue
		}
		if !strings.Contains(out, c.needs) {
			t.Errorf("%s: expected %q in tmux output, got %q", c.what, c.needs, strings.TrimSpace(out))
		}
	}
}
