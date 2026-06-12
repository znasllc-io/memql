//go:build voice

package agent

// avatar_room_voice.go is the voice-tagged LiveKit glue that binds the
// CGO-free avatar-vendor core (integrations/avatarvendor) to the live room
// media plane. It is the Go analog of the Python avatar.start(session, room)
// seam (avatar_drive.py): it runs AFTER the room join + local-track publish,
// mints a LiveKit join token for the avatar participant, tells the vendor
// engine to join (via avatarvendor.StartAvatarSession), and -- the load-bearing
// part -- reassigns the cascade's audio sink so the assistant's PCM is
// forwarded to the avatar over a LiveKit byte data-stream instead of going only
// to the local Opus track.
//
// Source-agnostic invariant (avatar_drive.py): the cascade (#455) and the
// realtime executor (#457) each write the assistant's PCM into the audioSink
// their room bridge constructed, and BOTH bridges wrap that sink through
// maybeStartAvatar (room_audio_voice.go for the cascade, room_realtime_voice.go
// for the realtime executor -- the latter was missing until #1428, which is
// why realtime sessions never drove the avatar). So whichever executor
// produced the frames, the avatar lip-syncs them with zero executor-aware
// branching here -- the Go expression of the Python `session.output.audio`
// reassignment. The producer's PCM rate rides along (pcmSampleRate) so the
// byte-stream header always matches the bytes.
//
// Everything that touches the LiveKit room/media types is here behind
// `//go:build voice`; the REST + dispatch logic it calls is pure Go (the
// avatarvendor package) and tested in the default lane.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/znasllc-io/memql/integrations/avatarvendor"
)

// avatarTokenTTL bounds the avatar participant's LiveKit join token. Generous
// for a long session; the session ends well before it expires.
const avatarTokenTTL = 24 * time.Hour

// avatarConfigFromConfig projects the process Config onto the narrow
// avatarvendor.AvatarConfig the dispatch layer consumes.
func avatarConfigFromConfig(cfg Config) avatarvendor.AvatarConfig {
	return avatarvendor.AvatarConfig{
		Vendor:                cfg.AvatarVendor,
		AnamAPIKey:            cfg.AnamAPIKey,
		SimliAPIKey:           cfg.SimliAPIKey,
		AnamDefaultPersonaID:  cfg.AnamDefaultPersonaID,
		AnamDefaultAvatarID:   cfg.AnamDefaultAvatarID,
		AnamDefaultPersonaNam: cfg.AnamDefaultPersonaNm,
	}
}

// maybeStartAvatar resolves the avatar plan for the session and, when an avatar
// should run, mints the avatar's LiveKit token, starts the vendor session, and
// returns an audioSink that forwards the assistant's PCM to the avatar. The
// returned sink WRAPS the underlying local-track sink so audio still reaches
// any non-avatar subscriber; callers pass the wrapped sink to the executor.
//
// pcmSampleRate is the PRODUCER's PCM16 rate -- the rate of the bytes the
// wrapped sink will actually receive (ttsPCMSampleRate for the cascade,
// audio.OpenAISampleRate for the realtime executor). It is stamped on the
// avatar byte-stream header so the vendor engine resamples correctly; stamping
// the vendor's preferred rate here instead was the #1428 format bug (24 kHz
// realtime PCM declared as 16 kHz).
//
// Behavior matching the Python avatar_drive.start_avatar:
//   - No avatar configured (plan nil) -> returns the underlying sink unchanged
//     (audio-only), no error.
//   - Vendor start fails -> logs and returns the underlying sink unchanged
//     (non-fatal: the voice path stays alive in audio-only mode).
//   - A hard config mismatch (selected vendor, no API key) from
//     ResolveAvatarPlan -> logged and treated as audio-only (non-fatal), so a
//     misconfiguration never wedges the whole voice session.
//
// It is invoked from newRoomAudioBridge and newRealtimeRoomBridge after the
// local track is published.
func maybeStartAvatar(ctx context.Context, cfg Config, req RoomRequest, room *lksdk.Room, underlying audioSink, pcmSampleRate int, logger *slog.Logger) audioSink {
	// Mint the avatar's LiveKit join token up front so the (CGO-free) start
	// core can hand it to the vendor engine. A mint failure degrades to
	// audio-only, like every other avatar failure.
	token, err := mintAvatarToken(cfg, req.RoomName, req.GaAgentID)
	if err != nil {
		if logger != nil {
			logger.Warn("voice-agent avatar disabled (token)", "err", err)
		}
		return underlying
	}

	startCtx, cancel := context.WithTimeout(ctx, avatarvendor.AvatarRequestTimeout*3)
	defer cancel()

	// Anam's engine dials INTO the LiveKit server from the cloud, so it needs a
	// publicly-reachable URL. LIVEKIT_PUBLIC_URL wins when set (the internal
	// ws://livekit:7880 the agent uses is unreachable from outside), matching
	// the Python AnamPersonaSession.start url resolution.
	res, started, err := avatarvendor.StartAvatarSession(startCtx, avatarConfigFromConfig(cfg), avatarvendor.PersonaInput{
		AvatarPersonaID: req.Persona.AvatarPersonaID,
		AvatarVendor:    req.Persona.AvatarVendor,
		VideoEnabled:    req.Persona.VideoEnabled(),
	}, avatarvendor.AvatarStartParams{
		RoomName:     req.RoomName,
		LiveKitURL:   cfg.LiveKitPublicURL(),
		LiveKitToken: token,
	}, nil)
	if err != nil {
		// Config mismatch or vendor start failure: log and ride audio-only.
		// Non-fatal -- the assistant keeps talking on its own audio track.
		if logger != nil {
			logger.Warn("voice-agent avatar disabled; audio-only", "err", err)
		}
		return underlying
	}
	if !started {
		if logger != nil {
			logger.Info("voice-agent avatar disabled (audio-only)",
				"video_enabled", req.Persona.VideoEnabled(),
				"runtime_vendor", cfg.AvatarVendor)
		}
		return underlying
	}

	if logger != nil {
		logger.Info("voice-agent avatar started; lip-syncing assistant audio (source-agnostic)",
			"avatar_identity", res.AvatarIdentity,
			"session_id", res.SessionID,
			"pcm_sample_rate", pcmSampleRate,
			"vendor_sample_rate", res.LiveKitSampleRate)
	}

	return &avatarForwardingSink{
		underlying:     underlying,
		room:           room,
		avatarIdentity: res.AvatarIdentity,
		sampleRate:     pcmSampleRate,
		logger:         logger,
	}
}

// mintAvatarToken builds the LiveKit join JWT the vendor engine uses to join
// the room as the avatar participant. It is kind=agent and carries the
// publish-on-behalf attribute pointing at the GA identity, so the avatar's
// republished tracks are attributed to the assistant -- mirroring the Python
// AnamPersonaSession token mint (ATTRIBUTE_PUBLISH_ON_BEHALF).
func mintAvatarToken(cfg Config, roomName, gaIdentity string) (string, error) {
	at := auth.NewAccessToken(cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)
	grant := &auth.VideoGrant{RoomJoin: true, Room: roomName}
	at.SetVideoGrant(grant).
		SetKind(livekit.ParticipantInfo_AGENT).
		SetIdentity(avatarvendor.AvatarParticipantIdentity).
		SetName(avatarvendor.AvatarParticipantName).
		SetAttributes(map[string]string{"lk.publish_on_behalf": gaIdentity}).
		SetValidFor(avatarTokenTTL)
	token, err := at.ToJWT()
	if err != nil {
		return "", fmt.Errorf("sign avatar LiveKit token: %w", err)
	}
	return token, nil
}

// avatarForwardingSink is the audioSink the cascade writes the assistant's PCM
// into when an avatar is active. It is the Go expression of the Python
// `output.audio` reassignment: every frame is forwarded to the avatar
// participant over a LiveKit byte data-stream (topic lk.audio_stream) so the
// avatar lip-syncs it, AND passed through to the underlying local-track sink so
// the audio is still published normally.
//
// It is executor-agnostic: it sees only PCM bytes via WriteFrame, exactly like
// the cascade's localTrackSink, so the realtime executor (#457) -- which writes
// into the same sink -- is lip-synced for free.
type avatarForwardingSink struct {
	underlying     audioSink
	room           *lksdk.Room
	avatarIdentity string
	sampleRate     int
	logger         *slog.Logger

	// mu guards the lazily-opened byte-stream writer and the per-segment
	// forwarding meter. A new writer is opened on the first frame after a Flush
	// (each utterance is its own stream segment; closing the writer signals
	// AudioSegmentEnd to the avatar).
	mu     sync.Mutex
	writer *lksdk.ByteStreamWriter
	// meter counts the frames/bytes forwarded in the current segment; its
	// snapshot is logged on segment close (Flush) -- the #1428
	// frames-forwarded-to-vendor observability, so a dead forwarding leg shows
	// up as a MISSING per-utterance log instead of silently-frozen lips.
	meter avatarvendor.ForwardMeter
}

// WriteFrame forwards one PCM16 frame to the avatar byte-stream and to the
// underlying sink. A forwarding failure is logged, never returned, so a
// transient avatar hiccup can't break the local audio publish.
func (s *avatarForwardingSink) WriteFrame(pcm []byte) error {
	if len(pcm) == 0 {
		return s.underlying.WriteFrame(pcm)
	}
	if err := s.forward(pcm); err != nil && s.logger != nil {
		s.logger.Debug("voice-agent avatar forward failed", "err", err)
	}
	return s.underlying.WriteFrame(pcm)
}

// forward writes the PCM to the current byte-stream writer, opening one if this
// is the first frame of a new segment.
func (s *avatarForwardingSink) forward(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		w := s.room.LocalParticipant.StreamBytes(lksdk.StreamBytesOptions{
			Topic:                 avatarvendor.AudioStreamTopic,
			MimeType:              "audio/x-raw",
			DestinationIdentities: []string{s.avatarIdentity},
			// The header declares the PRODUCER's PCM rate so the vendor engine
			// resamples correctly (24 kHz realtime / 16 kHz cascade) -- see
			// avatarvendor.PCMStreamAttributes (#1428).
			Attributes: avatarvendor.PCMStreamAttributes(s.sampleRate),
		})
		if w == nil {
			return fmt.Errorf("avatar: StreamBytes returned nil writer")
		}
		s.writer = w
	}
	s.writer.Write(pcm, nil)
	s.meter.Add(len(pcm))
	return nil
}

// Flush ends the current avatar audio segment (closing the byte-stream writer,
// which the avatar reads as AudioSegmentEnd) and tells the avatar to drop any
// queued-but-unplayed audio via the lk.clear_buffer RPC, so a barge-in cuts the
// avatar's speech immediately. It then flushes the underlying sink so the local
// track is cut too. Mirrors DataStreamAudioOutput.clear_buffer + flush.
func (s *avatarForwardingSink) Flush() {
	s.mu.Lock()
	w := s.writer
	s.writer = nil
	snap := s.meter.SnapshotAndReset()
	s.mu.Unlock()
	if w != nil {
		w.Close()
	}
	// Per-utterance forwarding receipt (#1428): one Info line per closed audio
	// segment proving the assistant's PCM reached the vendor. Its ABSENCE while
	// the assistant speaks is the dead-lips signal -- this failure mode must
	// never be silent again. Skipped for empty segments (a barge-in can flush
	// before any frame lands).
	if snap.Frames > 0 && s.logger != nil {
		s.logger.Info("voice-agent avatar audio forwarded to vendor",
			"avatar_identity", s.avatarIdentity,
			"frames", snap.Frames,
			"bytes", snap.Bytes,
			"pcm_ms", avatarvendor.PCMDurationMs(snap.Bytes, s.sampleRate),
			"wall_ms", snap.Wall.Milliseconds(),
			"sample_rate", s.sampleRate)
	}
	// Best-effort interruption signal, fired async so a barge-in's Flush never
	// blocks the audio loop on the RPC round-trip (PerformRpc waits for a reply
	// with a multi-second floor). Errors are ignored -- the avatar may not have
	// registered the RPC yet on a very early barge-in.
	if s.room != nil {
		go func() {
			_, _ = s.room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
				DestinationIdentity: s.avatarIdentity,
				Method:              avatarvendor.RPCClearBuffer,
				Payload:             "",
			})
		}()
	}
	s.underlying.Flush()
}
