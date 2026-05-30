//go:build !identity

package main

import (
	"fmt"
	"os"
)

// voiceAgentRunner runs the Go voice-agent media participant. It is nil on
// every binary except the voice node (set via init() in
// subcommand_voice_agent.go behind `//go:build voice`), so the `voice-agent`
// subcommand is only actually available on the voice binary. Keeping it as a
// hook avoids dragging the LiveKit CGO dependency into the dispatch table on
// CGO-free builds.
var voiceAgentRunner func(args []string) int

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
	case "voice-agent":
		if voiceAgentRunner == nil {
			fmt.Fprintln(os.Stderr, "voice-agent requires the voice node binary (build with -tags voice).")
			return true, 2
		}
		return true, voiceAgentRunner(args[1:])
	}
	return false, 0
}
