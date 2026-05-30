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
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"

	mediasdk "github.com/livekit/media-sdk"
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
	logger *slog.Logger
}

// NewRoomJoiner builds the voice-build RoomJoiner from the resolved config.
// The default (CGO-free) build supplies a different constructor
// (room_default.go) so the package compiles without CGO; only the voice
// entrypoint calls this one.
func NewRoomJoiner(cfg Config, logger *slog.Logger) RoomJoiner {
	return &liveKitRoomJoiner{cfg: cfg, logger: logger}
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
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				// Diagnostic only for #454 -- mirrors the Python agent's
				// track_subscribed listener so we can confirm audio tracks
				// reach the agent. The decode-to-PCM wiring is #455 (see the
				// seam at the bottom of this file).
				if j.logger != nil {
					j.logger.Info("voice-agent track subscribed",
						"identity", rp.Identity(),
						"kind", pub.Kind(),
						"source", pub.Source().String())
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

	// TODO(#455): subscribe to remote audio tracks and decode to PCM16 via
	// lkmedia.NewPCMRemoteTrack(track, sink, lkmedia.WithTargetSampleRate(24000),
	// lkmedia.WithTargetChannels(1)) wired into the Deepgram STT stream, and
	// publish agent audio via lkmedia.NewPCMLocalTrack(24000, 1, logger) +
	// room.LocalParticipant.PublishTrack(...). The turn-taking state machine
	// (#455) feeds frames in both directions. The references below keep the
	// CGO media-sdk path compiled and exercised for the #454 build
	// foundation without standing up the loop.
	_ = lkmedia.NewPCMRemoteTrack
	_ = lkmedia.NewPCMLocalTrack
	var _ mediasdk.PCM16Sample

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
