package cctl

import (
	"strings"
	"testing"
	"time"
)

func TestConnectTimeout(t *testing.T) {
	if d := (Server{}).connectTimeout(); d != 10*time.Second {
		t.Errorf("default = %s, want 10s", d)
	}
	if d := (Server{Timeout: "3s"}).connectTimeout(); d != 3*time.Second {
		t.Errorf("3s = %s", d)
	}
	if d := (Server{Timeout: "1m"}).connectTimeout(); d != time.Minute {
		t.Errorf("1m = %s", d)
	}
	for _, bad := range []string{"nonsense", "0s", "-5s", ""} {
		if d := (Server{Timeout: bad}).connectTimeout(); d != 10*time.Second {
			t.Errorf("Timeout=%q -> %s, want default 10s", bad, d)
		}
	}
}

func TestSshArgsConnectTimeout(t *testing.T) {
	if got := strings.Join(sshArgs(Server{Host: "h"}), " "); !strings.Contains(got, "ConnectTimeout=10") {
		t.Errorf("default args = %q, want ConnectTimeout=10", got)
	}
	// Sub-second rounds up to 1, never 0.
	if got := strings.Join(sshArgs(Server{Host: "h", Timeout: "500ms"}), " "); !strings.Contains(got, "ConnectTimeout=1") {
		t.Errorf("500ms args = %q, want ConnectTimeout=1", got)
	}
	if got := strings.Join(sshArgs(Server{Host: "h", Timeout: "12s"}), " "); !strings.Contains(got, "ConnectTimeout=12") {
		t.Errorf("12s args = %q, want ConnectTimeout=12", got)
	}
}

func TestShellPath(t *testing.T) {
	cases := map[string]string{
		"~":              `"$HOME"`,
		"~/my-app":       `"$HOME/my-app"`,
		"~/a b/c":        `"$HOME/a b/c"`,
		"/abs/path":      `"/abs/path"`,
		`/with"quote`:    `"/with\"quote"`,
		`/with$dollar`:   `"/with\$dollar"`,
		"/with`backtick": "\"/with\\`backtick\"",
	}
	for in, want := range cases {
		got := shellPath(in)
		if got != want {
			t.Errorf("shellPath(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":                  "''",
		"plain":             "plain",
		"with space":        "'with space'",
		"branch/name":       "branch/name", // / is safe
		"with'apostrophe":   `'with'\''apostrophe'`,
		"prompt; rm -rf /":  "'prompt; rm -rf /'",
		"investigate auth!": "'investigate auth!'",
	}
	for in, want := range cases {
		got := shellQuote(in)
		if got != want {
			t.Errorf("shellQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in           string
		server, repo string
	}{
		{"local", "local", ""},
		{"local/rxtx", "local", "rxtx"},
		{"workspace/my-app", "workspace", "my-app"},
		{"a/b/c", "a", "b/c"},
		{"", "", ""},
	}
	for _, c := range cases {
		s, r := parseTarget(c.in)
		if s != c.server || r != c.repo {
			t.Errorf("parseTarget(%q) = (%q, %q); want (%q, %q)", c.in, s, r, c.server, c.repo)
		}
	}
}

func TestParseTmuxName(t *testing.T) {
	cases := []struct {
		name           string
		repo, wt, sess string
		ok             bool
	}{
		// canonical 4-part format
		{"cctl/rxtx.dev/audit/audit", "rxtx.dev", "audit", "audit", true},
		{"cctl/rxtx.dev/main/coding", "rxtx.dev", "main", "coding", true},
		// shapes that are not 4-part are rejected (no legacy compat)
		{"cctl/rxtx/b300", "", "", "", false},
		{"cctl/repo/sess/with/extra/slashes", "", "", "", false},
		{"other-session", "", "", "", false},
		{"cctl/", "", "", "", false},
		{"cctl/foo", "", "", "", false},
	}
	for _, c := range cases {
		gotRepo, gotWT, gotSess, gotOK := parseTmuxName(c.name)
		if gotRepo != c.repo || gotWT != c.wt || gotSess != c.sess || gotOK != c.ok {
			t.Errorf("parseTmuxName(%q) = (%q, %q, %q, %v); want (%q, %q, %q, %v)",
				c.name, gotRepo, gotWT, gotSess, gotOK, c.repo, c.wt, c.sess, c.ok)
		}
	}
}
