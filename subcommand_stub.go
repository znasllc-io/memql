//go:build !identity

package main

import (
	"fmt"
	"os"
)

// dispatchSubcommand returns (true, exitCode) only for subcommands
// implemented on this binary. Default builds carry no operator
// subcommands -- the identity binary owns voice-agent-token, the
// other node binaries route through their normal server bootstrap.
func dispatchSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "voice-agent-token":
		fmt.Fprintln(os.Stderr, "voice-agent-token requires the identity binary (build with -tags identity).")
		return true, 2
	}
	return false, 0
}
