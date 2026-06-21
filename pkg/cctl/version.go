package cctl

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Commit is the short git SHA the binary was built from, set at build time
// via -ldflags (see the cctl:install / cctl:build mise tasks). Empty on a
// plain `go build`. Version comes from version.txt (managed by release-please)
// injected the same way; `go install …@vX` falls back to the module version
// embedded in the build info.
var Commit = ""

// versionString renders the semver plus the build commit when known, e.g.
// "0.3.1 (a1b2c3d)". When the binary was built without the ldflags (a bare
// `go install …@version`), it recovers the version from the module build info.
func versionString() string {
	v := Version
	if v == "" || v == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
				v = mv
			}
		}
	}
	if Commit != "" {
		return v + " (" + Commit + ")"
	}
	return v
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
