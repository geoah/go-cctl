package cctl

import "testing"

func TestShouldRespawnSession(t *testing.T) {
	cases := []struct {
		name                              string
		live, agentUp, attached, terminal bool
		want                              bool
	}{
		{"detached idle shell -> revive", true, false, false, false, true},
		// The fix: an attached session the user is sitting in must NOT be
		// respawn-killed by a reconcile triggered from some other action.
		{"attached idle shell -> leave alone", true, false, true, false, false},
		{"agent already running -> skip", true, true, false, false, false},
		{"terminal tab -> skip", true, false, false, true, false},
		{"dead/missing -> skip (spawn pass handles)", false, false, false, false, false},
		{"attached + agent up -> skip", true, true, true, false, false},
	}
	for _, tc := range cases {
		if got := shouldRespawnSession(tc.live, tc.agentUp, tc.attached, tc.terminal); got != tc.want {
			t.Errorf("%s: shouldRespawnSession(live=%v, up=%v, attached=%v, term=%v) = %v, want %v",
				tc.name, tc.live, tc.agentUp, tc.attached, tc.terminal, got, tc.want)
		}
	}
}
