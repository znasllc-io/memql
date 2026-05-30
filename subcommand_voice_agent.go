//go:build voice

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/znasllc-io/memql/integrations/voice/agent"
)

// subcommand_voice_agent.go folds the Go voice-agent media participant into
// the voice node binary as the `voice-agent` subcommand (memql-voice
// voice-agent ...). It is voice-tagged because the agent's room-join path
// pulls the LiveKit CGO libopus/soxr dependency; the dispatch hook
// (voiceAgentRunner in subcommand_stub.go) is only set on this build, so the
// subcommand surfaces a clear "build with -tags voice" message everywhere
// else.
//
// Entrypoint decision (#454): rather than a standalone cmd/voice-agent with
// its own build + Docker wiring, the agent rides the existing voice node
// binary -- the Voice node is exactly where docs/voice/
// 451-livekit-go-room-participation.md Section 3c places the RoomAgent, and
// `make voice` / BUILD_TAGS=voice already produce + containerize this binary.
// Folding in as a subcommand reuses that toolchain end to end.

func init() {
	voiceAgentRunner = runVoiceAgentSubcommand
}

// runVoiceAgentSubcommand parses the voice-agent flags, installs a
// signal-cancelled context, and runs one agent session to completion.
func runVoiceAgentSubcommand(args []string) int {
	fs := flag.NewFlagSet("voice-agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	roomName := fs.String("room", "",
		"LiveKit room name to join (the memQL convention is polyphon-<spaceId>). "+
			"Falls back to MEMQL_VOICE_ROOM_NAME when unset.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, agent.RunOptions{RoomName: *roomName}); err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent: %v\n", err)
		return 1
	}
	return 0
}
