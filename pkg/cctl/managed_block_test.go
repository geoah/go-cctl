package cctl

import (
	"strings"
	"testing"
)

func TestMergeManagedBlock_AppendsToEmpty(t *testing.T) {
	got, changed := mergeManagedBlock("", "set -g mouse on\n")
	if !changed {
		t.Fatal("expected changed=true on empty input")
	}
	if !strings.Contains(got, managedBeginMarker) || !strings.Contains(got, managedEndMarker) {
		t.Fatalf("missing markers in output:\n%s", got)
	}
	if !strings.Contains(got, "set -g mouse on") {
		t.Fatalf("body missing:\n%s", got)
	}
}

func TestMergeManagedBlock_AppendsToExisting(t *testing.T) {
	existing := "# my pre-existing tmux.conf\nset -g status-keys vi\n"
	got, changed := mergeManagedBlock(existing, "set -g mouse on\n")
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !strings.HasPrefix(got, existing) && !strings.HasPrefix(got, "# my pre-existing tmux.conf\nset -g status-keys vi") {
		t.Fatalf("existing content was clobbered:\n%s", got)
	}
	if !strings.Contains(got, "set -g mouse on") {
		t.Fatalf("body missing")
	}
}

func TestMergeManagedBlock_ReplacesExistingBlock(t *testing.T) {
	existing := "# keep me\n" + managedBeginMarker + "\nold body\n" + managedEndMarker + "\n# also keep me\n"
	got, changed := mergeManagedBlock(existing, "new body line\n")
	if !changed {
		t.Fatal("expected changed=true")
	}
	if strings.Contains(got, "old body") {
		t.Fatalf("old body still present:\n%s", got)
	}
	if !strings.Contains(got, "new body line") {
		t.Fatalf("new body missing:\n%s", got)
	}
	if !strings.Contains(got, "# keep me") || !strings.Contains(got, "# also keep me") {
		t.Fatalf("surrounding lines lost:\n%s", got)
	}
}

func TestMergeManagedBlock_IdempotentWhenUnchanged(t *testing.T) {
	body := "set -g mouse on\n"
	first, _ := mergeManagedBlock("# header\n", body)
	second, changed := mergeManagedBlock(first, body)
	if changed {
		t.Fatalf("second pass should be no-op; got changed=true. before:\n%s\nafter:\n%s", first, second)
	}
}

func TestMergeManagedBlock_MismatchedMarkers_BailsOut(t *testing.T) {
	// only the begin marker present: refuse to clobber
	existing := "# stuff\n" + managedBeginMarker + "\nbody but no end\n"
	got, changed := mergeManagedBlock(existing, "new body\n")
	if changed {
		t.Fatalf("expected no change with mismatched markers; got:\n%s", got)
	}
}
