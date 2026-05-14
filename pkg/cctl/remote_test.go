package cctl

import "testing"

func TestShellPath(t *testing.T) {
	cases := map[string]string{
		"~":              `"$HOME"`,
		"~/my-app":      `"$HOME/my-app"`,
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
