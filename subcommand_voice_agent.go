//go:build voice

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
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

	// Subcommands dispatch in main() BEFORE the genesis auto-load, so load the
	// envelope here -- in a cluster deploy the voice-agent's required secrets
	// (OPENAI_API_KEY, MEMQL_NODE_BOOTSTRAP_TOKEN, ...)
	// are sealed in MEMQL_GENESIS_B64 and would otherwise be missing (#1049).
	if err := applySubcommandEnv("voice-agent"); err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resolve the room: --room, then MEMQL_VOICE_ROOM_NAME. With a room set we
	// run that single room (production / explicit dev). With no room we hand off
	// to the auto-join dispatcher, which watches LiveKit for active polyphon
	// rooms and joins one (dev convenience) or idles when auto-join is disabled.
	room := strings.TrimSpace(*roomName)
	if room == "" {
		room = strings.TrimSpace(os.Getenv("MEMQL_VOICE_ROOM_NAME"))
	}

	var err error
	if room != "" {
		err = agent.Run(ctx, agent.RunOptions{RoomName: room})
	} else {
		err = agent.RunDispatcher(ctx, agent.RunOptions{})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent: %v\n", err)
		return 1
	}
	return 0
}
