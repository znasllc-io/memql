//go:build voice

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// dispatcher_voice.go is the auto-join dispatcher (voice-tagged because it
// pulls the LiveKit server SDK). The voice-agent is a per-room participant, but
// there is no per-session launcher in dev -- so a long-running `voice-agent`
// service with no --room used to sit idle (or, before #511, crash-loop). This
// makes it useful instead: it watches LiveKit for active polyphon-<spaceId>
// rooms and serves EACH of them concurrently, so opening a space brings the GA
// into voice without any manual room wiring, and two humans in two different
// spaces both get the GA at the same time (#1395) rather than the second
// waiting for the first room to idle.
//
// Scope: dev convenience. Production launches the agent per-room (--room /
// MEMQL_VOICE_ROOM_NAME), which takes agent.Run directly and never reaches
// here.

const (
	// dispatcherPollInterval is how often we re-list LiveKit rooms to pick up
	// newly-active spaces.
	dispatcherPollInterval = 3 * time.Second
)

// RunDispatcher runs the auto-join supervisor: discover every active polyphon
// room with a human present and no voice-agent already serving it, then run a
// session per room concurrently (each with its own gRPC client, LiveKit join,
// and idle-teardown lifecycle), re-discovering on every poll. Honours
// MEMQL_VOICE_AUTOJOIN=false by idling instead. Returns nil on ctx
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

	roomClient := lksdk.NewRoomServiceClient(httpLiveKitURL(cfg.LiveKitURL), cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)

	logger.Info("voice-agent: auto-join mode -- watching LiveKit for active polyphon rooms (concurrent multi-room)",
		"livekit", cfg.LiveKitURL, "memql", cfg.MemqlGRPCAddr, "executor", cfg.VoiceExecutor,
		"max_rooms", cfg.VoiceMaxRooms)

	discover := func(ctx context.Context) []string {
		return discoverActiveRooms(ctx, roomClient, logger)
	}
	return dispatchLoop(ctx, cfg, opts.Dialer, logger, discover, NewRoomJoiner)
}

// dispatchLoop is RunDispatcher's discover/serve supervisor, parameterized so
// tests can drive it without LiveKit: discover yields the set of active rooms
// eligible to serve (those with a human and no voice-agent already present);
// newJoiner builds the per-session room joiner (NewRoomJoiner in production).
//
// Each discovered room is served in its own goroutine with its OWN gRPC client
// and joiner -- a session's lifecycle (idle teardown, disconnect, cost
// guardrail) is fully isolated from every other room's, with no shared state
// or cross-talk. When a session ends, its room is removed from the active set
// so a later poll re-serves it if a human rejoins. The pool is bounded by
// cfg.VoiceMaxRooms.
func dispatchLoop(ctx context.Context, cfg Config, dial Dialer, logger *slog.Logger,
	discover func(context.Context) []string,
	newJoiner func(Config, *Client, *slog.Logger) RoomJoiner) error {

	maxRooms := cfg.VoiceMaxRooms
	if maxRooms <= 0 {
		maxRooms = defaultVoiceMaxRooms
	}

	var (
		mu     sync.Mutex
		active = make(map[string]struct{}) // rooms this replica is currently serving
		wg     sync.WaitGroup
	)

	// serve launches an isolated session goroutine for one room. The room is
	// recorded in `active` before launch and removed when the session ends, so
	// the same room is never double-served by THIS replica and is re-eligible
	// once it idles out.
	serve := func(room string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(active, room)
				mu.Unlock()
			}()

			logger.Info("voice-agent: serving active room", "room", room)
			// Each session gets its OWN client + joiner: Session.Run closes its
			// client on teardown and a closed Client cannot reconnect, so sharing
			// one would poison every later session with ErrClientClosed (#1409).
			// Independent clients also keep concurrent sessions from sharing a
			// single multiplexed stream (no cross-talk between rooms).
			client := NewClient(cfg.MemqlGRPCAddr, cfg.VoiceAgentToken, dial, logger)
			session := NewSession(cfg, client, room, newJoiner(cfg, client, logger), logger)
			if err := session.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("voice-agent: session ended with error", "room", room, "err", err)
			}
			// Session ended (room emptied / idle teardown / disconnect / error);
			// the deferred delete re-opens the room for a future poll.
		}()
	}

	for {
		if ctx.Err() != nil {
			break
		}

		for _, room := range discover(ctx) {
			mu.Lock()
			_, serving := active[room]
			atCap := len(active) >= maxRooms
			if !serving && !atCap {
				active[room] = struct{}{}
			}
			mu.Unlock()

			if serving {
				continue
			}
			if atCap {
				logger.Warn("voice-agent: at room-serving capacity -- deferring room",
					"room", room, "max_rooms", maxRooms)
				continue
			}
			serve(room)
		}

		select {
		case <-ctx.Done():
			// Stop discovering; sessions observe the same ctx and tear down.
			wg.Wait()
			return nil
		case <-time.After(dispatcherPollInterval):
		}
	}

	wg.Wait()
	return nil
}

// discoverActiveRooms lists LiveKit rooms and returns the names of every
// polyphon-<spaceId> room eligible to serve: at least one HUMAN participant
// present (so the agent never wedges on a room with only its own ghost or an
// avatar vendor participant, #1378) AND no voice-agent (`-ga`) participant
// already present (so a second replica does not double-serve a room another
// replica already owns, #1395). Best-effort: a list error logs at debug and
// returns nil.
func discoverActiveRooms(ctx context.Context, roomClient *lksdk.RoomServiceClient, logger *slog.Logger) []string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := roomClient.ListRooms(cctx, &livekit.ListRoomsRequest{})
	if err != nil {
		if logger != nil {
			logger.Debug("voice-agent: list rooms failed", "err", err)
		}
		return nil
	}
	var rooms []string
	for _, r := range resp.GetRooms() {
		name := r.GetName()
		// Serve product voice rooms (polyphon-<spaceId>) AND telephony rooms
		// (tel-<partitionId>-<auto>, Epic 4 / memql#1911): a PSTN caller bridged
		// in by livekit/sip is a human room participant, so the existing realtime
		// agent answers the phone with no further change. Telephony is core and
		// partition-scoped -- never keyed on a CoPresent space (Amendment A).
		if !strings.HasPrefix(name, "polyphon-") && !strings.HasPrefix(name, "tel-") {
			continue
		}
		if r.GetNumParticipants() == 0 {
			continue
		}
		// A raw participant count is not enough: during a rollout the
		// predecessor pod's ghost participant lingers in its old room for
		// ~15-30s, and an avatar vendor participant can outlive its session.
		// Inspect the participant list once and decide on its contents:
		//   - no human present        -> skip (avoids the agent-only wedge, #1378)
		//   - a voice-agent present    -> skip (another replica/session owns it, #1395)
		human, agent := classifyRoomParticipants(cctx, roomClient, name, logger)
		if !human {
			continue
		}
		if agent {
			// Already being served (by this replica's own session or by a peer
			// replica). Re-serving would put a second GA in the room.
			continue
		}
		rooms = append(rooms, name)
	}
	return rooms
}

// classifyRoomParticipants lists a room's participants once and reports whether
// it contains at least one human (per isHumanParticipantIdentity) and whether
// it contains a voice-agent participant (the `<spaceId>-ga` / agent-row
// identity the joiner mints). Best-effort: a list error logs at debug and
// reports (false, false), so a transient API failure skips the room for one
// poll cycle instead of risking a wedge on machinery or a double-serve.
func classifyRoomParticipants(ctx context.Context, roomClient *lksdk.RoomServiceClient, room string, logger *slog.Logger) (human, agent bool) {
	resp, err := roomClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		if logger != nil {
			logger.Debug("voice-agent: list participants failed", "room", room, "err", err)
		}
		return false, false
	}
	for _, p := range resp.GetParticipants() {
		if isHumanParticipantIdentity(p.GetIdentity()) {
			human = true
		} else if isVoiceAgentParticipantIdentity(p.GetIdentity()) {
			agent = true
		}
	}
	return human, agent
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
