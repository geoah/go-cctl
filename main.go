// Command cctl is a TUI + CLI for managing mosh+tmux+claude sessions
// across local and remote git checkouts. All logic lives in
// github.com/geoah/go-cctl/pkg/cctl; this binary is just the entry point
// so the module can be `go install`'d directly.
package main

import "github.com/geoah/go-cctl/pkg/cctl"

func main() {
	cctl.Run()
}
