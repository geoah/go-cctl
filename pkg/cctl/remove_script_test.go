package cctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoveWorktreeScript_OutsideBase pins the safety model after the
// "dd fails on worktrees living outside worktree_base" report: a path
// outside the base is removable when (and only when) git lists it as a
// registered worktree of the repo — via `git worktree remove`, never the
// rm -rf fallback, which stays anchored inside the base. Drives the real
// script through bash against a real repo.
func TestRemoveWorktreeScript_OutsideBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	base := filepath.Join(root, "base")
	outsideWT := filepath.Join(root, "outside-wt")
	insideWT := filepath.Join(base, "inside-wt")
	randomDir := filepath.Join(root, "random-dir")

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo,
			"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(randomDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "init")
	git("worktree", "add", "-q", outsideWT, "-b", "t1")
	git("worktree", "add", "-q", insideWT, "-b", "t2")

	run := func(wt string) (string, error) {
		script := removeWorktreeScript(repo, wt, base)
		out, err := exec.Command("bash", "-c", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// Registered worktree OUTSIDE the base: git removes it.
	if out, err := run(outsideWT); err != nil {
		t.Errorf("outside registered worktree should be removed, got err=%v out=%s", err, out)
	}
	if _, err := os.Stat(outsideWT); !os.IsNotExist(err) {
		t.Errorf("outside-wt should be gone")
	}

	// Unregistered dir outside the base: refused, dir untouched.
	if out, err := run(randomDir); err == nil || !strings.Contains(out, "not a registered worktree") {
		t.Errorf("unregistered outside dir must be refused, got err=%v out=%s", err, out)
	}
	if _, err := os.Stat(randomDir); err != nil {
		t.Errorf("random-dir must be untouched, stat err=%v", err)
	}

	// Inside the base: removed (rm fallback allowed).
	if out, err := run(insideWT); err != nil {
		t.Errorf("inside-base worktree should be removed, got err=%v out=%s", err, out)
	}
	if _, err := os.Stat(insideWT); !os.IsNotExist(err) {
		t.Errorf("inside-wt should be gone")
	}

	// The repo itself must always be refused.
	if out, err := run(repo); err == nil || !strings.Contains(out, "main checkout") {
		t.Errorf("repo path must be refused, got err=%v out=%s", err, out)
	}
}
