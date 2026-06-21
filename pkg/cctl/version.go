package cctl

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Commit is the short git SHA the binary was built from, set at build time
// via -ldflags (see the cctl:install / cctl:build mise tasks). Empty on a
// plain `go build`. Version itself comes from the repo's VERSION file, which
// the post-commit hook bumps per Conventional Commit.
var Commit = ""

// versionString renders the semver plus the build commit when known, e.g.
// "0.3.1 (a1b2c3d)".
func versionString() string {
	if Commit != "" {
		return Version + " (" + Commit + ")"
	}
	return Version
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cctl version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(versionString())
		},
	}
}
