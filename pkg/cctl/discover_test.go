package cctl

import (
	"strings"
	"testing"
)

// TestDropNestedRepos pins the one-repo-one-group fix: a repo that contains
// nested .git dirs (submodules, vendored clones, nested projects) must not
// surface those inner repos as their own top-level repos — otherwise each
// gets its own cmux sidebar group.
func TestDropNestedRepos(t *testing.T) {
	found := []discoveredRepo{
		{parts: []string{"org", "app"}, srcPath: "/src"},                  // keep
		{parts: []string{"org", "app", "vendor", "lib"}, srcPath: "/src"}, // nested in org/app → drop
		{parts: []string{"org", "other"}, srcPath: "/src"},                // keep (sibling)
		{parts: []string{"sub"}, srcPath: "/src/org/app"},                 // /src/org/app/sub → drop
	}
	kept := dropNestedRepos(found)

	got := map[string]bool{}
	for _, d := range kept {
		got[strings.TrimRight(d.srcPath, "/")+"/"+strings.Join(d.parts, "/")] = true
	}
	if len(kept) != 2 {
		t.Fatalf("want 2 top-level repos kept, got %d: %v", len(kept), got)
	}
	if !got["/src/org/app"] || !got["/src/org/other"] {
		t.Errorf("expected /src/org/app and /src/org/other kept; got %v", got)
	}
	if got["/src/org/app/vendor/lib"] || got["/src/org/app/sub"] {
		t.Errorf("nested repos should have been dropped; got %v", got)
	}
}

// TestDropNestedRepos_NoFalsePositives: sibling repos that merely share a
// name prefix are not nested and must both survive.
func TestDropNestedRepos_NoFalsePositives(t *testing.T) {
	found := []discoveredRepo{
		{parts: []string{"app"}, srcPath: "/src"},
		{parts: []string{"app-utils"}, srcPath: "/src"}, // /src/app-utils is NOT under /src/app
	}
	if kept := dropNestedRepos(found); len(kept) != 2 {
		t.Fatalf("prefix-sharing siblings must both survive; got %d", len(kept))
	}
}
