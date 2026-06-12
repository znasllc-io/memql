//go:build voice

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// dispatcher_voice.go is the dev auto-join dispatcher (voice-tagged because it
// pulls the LiveKit server SDK). The voice-agent is a per-room participant, but
// there is no per-session launcher in dev -- so a long-running `voice-agent`
// service with no --room used to sit idle (or, before #511, crash-loop). This
// makes it useful instead: it watches LiveKit for active polyphon-<spaceId>
// rooms and joins one, so opening a space brings the GA into voice without any
// manual room wiring.
//
// Scope: dev convenience, one room at a time. Production launches the agent
// per-room (--room / MEMQL_VOICE_ROOM_NAME), which takes agent.Run directly and
// never reaches here. Concurrent multi-room dispatch is a follow-up.

const (
	// dispatcherPollInterval is how often we re-list LiveKit rooms while idle.
	dispatcherPollInterval = 3 * time.Second
)

// RunDispatcher runs the auto-join loop: discover an active polyphon room and
// run a session for it (blocking until that session ends), then re-discover.
// Honours MEMQL_VOICE_AUTOJOIN=false by idling instead. Returns nil on ctx
// cancellation (the subcommand's SIGINT/SIGTERM context).
func RunDispatcher(ctx context.Context, opts RunOptions) error {
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

	if !cfg.VoiceAutoJoin {
		// Auto-join disabled -- idle (the #511 behaviour) until signalled.
		logger.Info("voice-agent: auto-join disabled -- idling " +
			"(set --room / MEMQL_VOICE_ROOM_NAME to join a room, or MEMQL_VOICE_AUTOJOIN=true)")
		<-ctx.Done()
		return nil
	}

	token, err := ResolveVoiceAgentToken(ctx, getenv, nil, logger)
	if err != nil {
		return fmt.Errorf("voice-agent: resolve token: %w", err)
	}
	cfg.VoiceAgentToken = token

	client := NewClient(cfg.MemqlGRPCAddr, cfg.VoiceAgentToken, opts.Dialer, logger)
	joiner := NewRoomJoiner(cfg, client, logger)
	roomClient := lksdk.NewRoomServiceClient(httpLiveKitURL(cfg.LiveKitURL), cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)

	logger.Info("voice-agent: auto-join mode -- watching LiveKit for active polyphon rooms",
		"livekit", cfg.LiveKitURL, "memql", cfg.MemqlGRPCAddr, "executor", cfg.VoiceExecutor)

	for {
		if ctx.Err() != nil {
			return nil
		}

		room := discoverActiveRoom(ctx, roomClient, logger)
		if room == "" {
			// Nothing active -- wait and re-list (this is the idle state).
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(dispatcherPollInterval):
			}
			continue
		}

		logger.Info("voice-agent: joining active room", "room", room)
		session := NewSession(cfg, client, room, joiner, logger)
		if err := session.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("voice-agent: session ended with error", "room", room, "err", err)
			// Brief backoff so a persistently-failing room can't hot-loop.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(dispatcherPollInterval):
			}
		}
		// Session ended (room emptied / cost-guardrail teardown / disconnect);
		// loop back to discover the next active room.
	}
}

// discoverActiveRoom lists LiveKit rooms and returns the name of the first
// polyphon-<spaceId> room with at least one participant present, or "" when
// none is active. Best-effort: a list error logs at debug and returns "".
func discoverActiveRoom(ctx context.Context, roomClient *lksdk.RoomServiceClient, logger *slog.Logger) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := roomClient.ListRooms(cctx, &livekit.ListRoomsRequest{})
	if err != nil {
		if logger != nil {
			logger.Debug("voice-agent: list rooms failed", "err", err)
		}
		return ""
	}
	for _, r := range resp.GetRooms() {
		name := r.GetName()
		if !strings.HasPrefix(name, "polyphon-") {
			continue
		}
		if r.GetNumParticipants() == 0 {
			continue
		}
		// A raw participant count is not enough: during a rollout the
		// predecessor pod's ghost participant lingers in its old room for
		// ~15-30s, and an avatar vendor participant can outlive its session.
		// Adopting such a room wedges the single-room dispatcher on a space
		// no human is in (#1378) -- only a HUMAN participant marks a room
		// active.
		if !roomHasHumanParticipant(cctx, roomClient, name, logger) {
			continue
		}
		return name
	}
	return ""
}

// roomHasHumanParticipant lists a room's participants and reports whether at
// least one is a human (per isHumanParticipantIdentity). Best-effort: a list
// error logs at debug and counts as no-humans, so a transient API failure
// skips the room for one poll cycle instead of risking a wedge on machinery.
func roomHasHumanParticipant(ctx context.Context, roomClient *lksdk.RoomServiceClient, room string, logger *slog.Logger) bool {
	resp, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		if logger != nil {
			logger.Debug("voice-agent: list participants failed", "room", room, "err", err)
		}
		return false
	}
	for _, p := range resp.GetParticipants() {
		if isHumanParticipantIdentity(p.GetIdentity()) {
			return true
		}
	}
	return false
}

// httpLiveKitURL converts the agent's ws(s):// LiveKit URL to the http(s)://
// form the RoomService (Twirp/HTTP) client expects.
func httpLiveKitURL(u string) string {
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	}
	return u
}
