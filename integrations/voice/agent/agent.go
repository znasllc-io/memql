package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// agent.go is the top-level voice-agent entrypoint, the Go analog of
// voice-agent/voice_agent/main.py::run + entrypoint. It loads config, resolves
// the class="voice_agent" JWT, builds the gRPC client + room joiner, and runs
// one Session. It is CGO-free; the room joiner it constructs is build-tag-
// resolved (room_voice.go under `-tags voice`, room_default.go otherwise), so
// this file compiles in every lane.
//
// In the cluster topology the voice-agent runs as the Voice node
// (docs/voice/451-livekit-go-room-participation.md, Section 3c). The
// entrypoint is wired into the voice node binary as the `voice-agent`
// subcommand (subcommand_voice_agent.go, `//go:build voice`).

// RunOptions parameterizes Run for testability. All fields are optional.
type RunOptions struct {
	// Getenv overrides the environment accessor (defaults to os.Getenv).
	Getenv Getenv
	// RoomName is the LiveKit room to join. When empty it is read from the
	// MEMQL_VOICE_ROOM_NAME env var. The memQL convention is
	// "polyphon-<spaceId>".
	RoomName string
	// Dialer overrides the gRPC dialer (defaults to the insecure dialer).
	Dialer Dialer
	// Logger is the structured logger (defaults to a text handler at the
	// configured level on stderr).
	Logger *slog.Logger
}

// Run loads config, resolves the voice-agent token, and runs a single session
// to completion. It blocks until the session ends, ctx is cancelled, or an
// unrecoverable startup error occurs.
func Run(ctx context.Context, opts RunOptions) error {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg, err := LoadConfig(getenv)
	if err != nil {
		return fmt.Errorf("voice-agent: load config: %w", err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = newLogger(cfg.LogLevel)
	}

	token, err := ResolveVoiceAgentToken(ctx, getenv, nil, logger)
	if err != nil {
		return fmt.Errorf("voice-agent: resolve token: %w", err)
	}
	cfg.VoiceAgentToken = token

	roomName := strings.TrimSpace(opts.RoomName)
	if roomName == "" {
		roomName = strings.TrimSpace(getenv("MEMQL_VOICE_ROOM_NAME"))
	}
	if roomName == "" {
		return fmt.Errorf(
			"voice-agent: room name is required (set MEMQL_VOICE_ROOM_NAME or pass RoomName)")
	}

	logger.Info("voice-agent starting",
		"livekit", cfg.LiveKitURL, "memql", cfg.MemqlGRPCAddr,
		"avatar", cfg.AvatarVendor, "executor", cfg.VoiceExecutor, "room", roomName)

	client := NewClient(cfg.MemqlGRPCAddr, cfg.VoiceAgentToken, opts.Dialer, logger)
	joiner := NewRoomJoiner(cfg, client, logger)
	session := NewSession(cfg, client, roomName, joiner, logger)

	return session.Run(ctx)
}

// newLogger builds a stderr text logger at the configured level. Mirrors the
// Python agent's setup_logging.
func newLogger(level string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARN", "WARNING":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
