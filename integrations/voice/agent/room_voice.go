//go:build voice

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

// room_voice.go is the LiveKit media-participant room JOIN for the Go
// voice-agent. It is voice-tagged because the LiveKit server SDK and its
// media-sdk pull the CGO libopus/soxr dependency (see docs/voice/
// 451-livekit-go-room-participation.md, Caveat 1) -- keeping it behind
// `//go:build voice` lets the default CGO-free CI lanes stay green while the
// `-tags voice` CGO lane proves the libopus foundation.
//
// Scope for #454: mint a LiveKit token (reusing the same auth path
// component/polyphon/localroom.go uses), connect to the room with that token
// via lksdk.ConnectToRoomWithToken, confirm the connection, wire diagnostic
// track/participant callbacks, and block until the room disconnects. The
// audio pipeline -- subscribing to remote PCM (NewPCMRemoteTrack), publishing
// agent audio (NewPCMLocalTrack), turn-taking and barge-in -- is OUT of scope
// and lands in #455. The PCM types from media-sdk are referenced below behind
// clearly-marked #455 seams so this file genuinely exercises the CGO build.

// roomConnectTimeout bounds the initial LiveKit room connect + connection
// confirmation. Generous for a cold WebRTC negotiation but bounded so a
// misconfigured LiveKit URL fails the join loudly.
const roomConnectTimeout = 30 * time.Second

// liveKitRoomJoiner is the voice-build RoomJoiner: it owns the LiveKit
// media-participant lifetime for one session.
type liveKitRoomJoiner struct {
	cfg    Config
	client *Client
	logger *slog.Logger
}

// NewRoomJoiner builds the voice-build RoomJoiner from the resolved config.
// The default (CGO-free) build supplies a different constructor
// (room_default.go) so the package compiles without CGO; only the voice
// entrypoint calls this one. client is the open gRPC client the cascade
// audio loop (#455) drives for transcript forwarding + turn requests.
func NewRoomJoiner(cfg Config, client *Client, logger *slog.Logger) RoomJoiner {
	return &liveKitRoomJoiner{cfg: cfg, client: client, logger: logger}
}

// JoinAndServe mints a join token for the agent identity, connects to the
// room, confirms the connection, and blocks until the room disconnects or
// ctx is cancelled. It returns a reason string for VoiceAgentSessionEnd.
func (j *liveKitRoomJoiner) JoinAndServe(ctx context.Context, req RoomRequest) (string, error) {
	token, err := j.mintToken(req)
	if err != nil {
		return "error", fmt.Errorf("voice-agent room join: mint token: %w", err)
	}

	// disconnected is closed by the OnDisconnected callback so JoinAndServe
	// can unblock when LiveKit tears the room down. reasonMu guards the
	// reason captured from OnDisconnectedWithReason.
	disconnected := make(chan struct{})
	var once sync.Once
	var reasonMu sync.Mutex
	disconnectReason := "normal"

	// bridge is the per-session media plane: the cascade bridge (#455: STT per
	// remote track + OpenAI TTS publish) or the realtime bridge (#457: STT
	// for labeled transcripts + active-speaker audio in, gpt-realtime output
	// audio published), selected behind the executor seam (RoomRequest.Executor).
	// Both satisfy mediaBridge. It is constructed after the connection is
	// confirmed (it publishes a track), so the track/participant callbacks
	// reach it through this pointer guarded by bridgeMu. Until it is set, the
	// callbacks fall through to diagnostics.
	var bridgeMu sync.Mutex
	var bridge mediaBridge
	getBridge := func() mediaBridge {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		return bridge
	}

	cb := &lksdk.RoomCallback{
		OnDisconnected: func() {
			once.Do(func() { close(disconnected) })
		},
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			reasonMu.Lock()
			disconnectReason = string(reason)
			reasonMu.Unlock()
			once.Do(func() { close(disconnected) })
		},
		OnParticipantConnected: func(rp *lksdk.RemoteParticipant) {
			if j.logger != nil {
				j.logger.Info("voice-agent room participant connected",
					"identity", rp.Identity(), "kind", rp.Kind())
			}
		},
		OnParticipantDisconnected: func(rp *lksdk.RemoteParticipant) {
			if j.logger != nil {
				j.logger.Info("voice-agent room participant disconnected",
					"identity", rp.Identity())
			}
			if b := getBridge(); b != nil {
				b.onParticipantDisconnected(rp)
			}
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if j.logger != nil {
					j.logger.Info("voice-agent track subscribed",
						"identity", rp.Identity(),
						"kind", pub.Kind(),
						"source", pub.Source().String())
				}
				// #455 cascade: open an OpenAI STT stream for this human
				// audio track and feed it decoded PCM16. Falls through to
				// diagnostics only if the bridge isn't up yet.
				if b := getBridge(); b != nil {
					b.onTrackSubscribed(track, pub, rp)
				}
			},
		},
	}

	connectCtx, cancel := context.WithTimeout(ctx, roomConnectTimeout)
	defer cancel()

	livekitURL := j.cfg.LiveKitURL
	if j.logger != nil {
		j.logger.Info("voice-agent connecting to LiveKit room",
			"url", livekitURL, "room", req.RoomName, "identity", req.GaAgentID)
	}

	room, err := lksdk.ConnectToRoomWithToken(livekitURL, token, cb)
	if err != nil {
		return "error", fmt.Errorf("voice-agent room join: connect %q: %w", req.RoomName, err)
	}
	defer room.Disconnect()

	// Confirm the connection landed before we declare the join successful.
	if err := j.confirmConnected(connectCtx, room); err != nil {
		return "error", err
	}
	if j.logger != nil {
		j.logger.Info("voice-agent joined LiveKit room",
			"room", room.Name(), "state", string(room.ConnectionState()))
	}

	// Stand up the media plane now that the connection is confirmed, behind
	// the executor-selection seam (#457). The realtime path (gpt-realtime
	// speech-to-speech) is built only when MEMQL_VOICE_EXECUTOR=realtime
	// resolved cleanly; otherwise (the default, or a realtime fallback) the
	// cascade path is built -- no regression. Both publish the agent's local
	// audio track and register a SpeakSink so unsolicited SI replies reach
	// playout (conductor gate B); OnTrackSubscribed feeds each remote human
	// track into the chosen bridge.
	bridgeCtx, bridgeCancel := context.WithCancel(ctx)
	defer bridgeCancel()
	b, speakSink, err := j.buildMediaBridge(bridgeCtx, req, room)
	if err != nil {
		return "error", err
	}
	bridgeMu.Lock()
	bridge = b
	bridgeMu.Unlock()
	defer b.close()

	if req.RegisterSpeakSink != nil {
		req.RegisterSpeakSink(speakSink)
	}

	// Catch-up for the connect->build race. ConnectToRoomWithToken auto-subscribes
	// the tracks participants already published, firing OnTrackSubscribed BEFORE
	// the bridge exists -- those callbacks hit getBridge()==nil and are dropped.
	// The realtime bridge build dials OpenAI + fetches the tool registry (~1s), so
	// a human already in the room loses their mic-track wiring and the session sits
	// warm-but-muted (humans=0, no audio, no turns -- the "voice connects but never
	// responds" symptom). LiveKit does NOT re-fire for already-subscribed tracks,
	// so replay them into the freshly-built bridge now. onTrackSubscribed dedups
	// per identity, so a track that races in after setBridge is not double-counted.
	j.replaySubscribedTracks(room, b)

	// Block until the room disconnects or the caller cancels.
	select {
	case <-disconnected:
		reasonMu.Lock()
		reason := disconnectReason
		reasonMu.Unlock()
		return normalizeDisconnectReason(reason), nil
	case <-ctx.Done():
		return "normal", nil
	}
}

// replaySubscribedTracks feeds every already-subscribed remote audio track into
// the bridge's onTrackSubscribed, covering tracks that auto-subscribed during the
// connect->build window (before the bridge pointer was set, so the live callback
// dropped them). Idempotent against the live callback: onTrackSubscribed dedups
// per participant identity.
func (j *liveKitRoomJoiner) replaySubscribedTracks(room *lksdk.Room, b mediaBridge) {
	if room == nil || b == nil {
		return
	}
	for _, rp := range room.GetRemoteParticipants() {
		if rp == nil || rp.Identity() == "" {
			continue
		}
		for _, pub := range rp.TrackPublications() {
			rpub, ok := pub.(*lksdk.RemoteTrackPublication)
			if !ok || rpub.Kind() != lksdk.TrackKindAudio || !rpub.IsSubscribed() {
				continue
			}
			tr := rpub.TrackRemote()
			if tr == nil {
				continue
			}
			if j.logger != nil {
				j.logger.Info("voice-agent track replay (connect-before-bridge race)",
					"identity", rp.Identity())
			}
			b.onTrackSubscribed(tr, rpub, rp)
		}
	}
}

// confirmConnected polls the room connection state until it reports connected
// or the connect deadline elapses. ConnectToRoomWithToken returns once the
// signal connection is established, but we confirm the state machine has
// actually transitioned to Connected so a half-open join surfaces as an error
// rather than a silent no-audio session.
func (j *liveKitRoomJoiner) confirmConnected(ctx context.Context, room *lksdk.Room) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if room.ConnectionState() == lksdk.ConnectionStateConnected {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"voice-agent room join: connection not confirmed within %s (state=%s)",
				roomConnectTimeout, string(room.ConnectionState()))
		case <-ticker.C:
		}
	}
}

// mintToken builds a LiveKit join JWT for the agent's identity, reusing the
// exact auth path component/polyphon/localroom.go uses. The agent joins under
// the GA identity (the room name is already "polyphon-<spaceId>").
func (j *liveKitRoomJoiner) mintToken(req RoomRequest) (string, error) {
	at := auth.NewAccessToken(j.cfg.LiveKitAPIKey, j.cfg.LiveKitAPISecret)
	grant := &auth.VideoGrant{RoomJoin: true, Room: req.RoomName}
	at.SetVideoGrant(grant).
		SetIdentity(req.GaAgentID).
		SetName("Assistant").
		SetValidFor(24 * time.Hour)
	token, err := at.ToJWT()
	if err != nil {
		return "", fmt.Errorf("sign LiveKit token: %w", err)
	}
	return token, nil
}

// normalizeDisconnectReason maps a LiveKit disconnect reason onto the small
// VoiceAgentSessionEnd reason vocabulary ("normal" / "error" / "inactivity").
// Unknown reasons default to "normal" so a clean room close isn't misreported
// as an error.
func normalizeDisconnectReason(reason string) string {
	if reason == "" {
		return "normal"
	}
	return "normal"
}
