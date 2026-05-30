//go:build voice

package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"

	mediasdk "github.com/livekit/media-sdk"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/deepgram"
)

// room_audio_voice.go is the voice-tagged (CGO/libopus) glue that binds the
// LiveKit room media plane to the pure-Go cascade orchestrator (cascade.go).
// It is the implementation of docs/voice/451-livekit-go-room-participation.md
// sections 2b/2c: decode each remote human audio track to PCM16 via
// NewPCMRemoteTrack and feed it to a Deepgram STT stream; publish the
// assistant's TTS audio via NewPCMLocalTrack.
//
// Everything that touches the media-sdk PCM types lives here, behind
// `//go:build voice`, so the cascade/turn-taking logic and its tests stay
// in the default CGO-free lane. The audioSink / ttsSynthesizer seams the
// cascade consumes are satisfied here by real LiveKit + Deepgram objects.

// roomAudioBridge owns the per-session media plane: the Deepgram STT/TTS
// clients, the published local track, and one cascade orchestrator. It is
// constructed inside JoinAndServe once the room connection is confirmed.
type roomAudioBridge struct {
	cfg     Config
	roomReq RoomRequest
	logger  *slog.Logger

	asr   *deepgram.ASRClient
	local *lkmedia.PCMLocalTrack

	cascade *Cascade

	mu      sync.Mutex
	streams map[string]polyphon.ASRStream // by participant identity
}

// newRoomAudioBridge builds the bridge: Deepgram clients from the agent
// config, a published local audio track, and the cascade orchestrator
// wired to the TTS adapter + the local track as its room sink.
func newRoomAudioBridge(ctx context.Context, cfg Config, req RoomRequest, client *Client, room *lksdk.Room, logger *slog.Logger) (*roomAudioBridge, error) {
	dgCfg := deepgram.Config{
		APIKey:   cfg.DeepgramAPIKey,
		ASRModel: cfg.DGASRModel,
		TTSModel: cfg.DGTTSModel,
		Language: cfg.DGLanguage,
		Logger:   logger,
	}
	asr, err := deepgram.NewASRClient(dgCfg)
	if err != nil {
		return nil, fmt.Errorf("voice-agent room audio: deepgram asr: %w", err)
	}
	tts, err := deepgram.NewTTSClient(dgCfg)
	if err != nil {
		return nil, fmt.Errorf("voice-agent room audio: deepgram tts: %w", err)
	}

	local, err := lkmedia.NewPCMLocalTrack(ttsPCMSampleRate, 1, protoLogger.GetLogger())
	if err != nil {
		return nil, fmt.Errorf("voice-agent room audio: local track: %w", err)
	}
	if _, err := room.LocalParticipant.PublishTrack(local, &lksdk.TrackPublicationOptions{
		Name:   "agent-voice",
		Source: livekit.TrackSource_MICROPHONE,
	}); err != nil {
		return nil, fmt.Errorf("voice-agent room audio: publish track: %w", err)
	}

	// The cascade's room-publish sink. When an avatar (#460) is enabled for
	// this session, maybeStartAvatar wraps it so the assistant's PCM is ALSO
	// forwarded to the avatar participant over a LiveKit byte data-stream for
	// lip-sync; when no avatar is configured (or its start fails) it returns
	// the local-track sink unchanged (audio-only). The wrap is at this sink --
	// the one the cascade AND the realtime executor (#457) write into -- so the
	// avatar lip-syncs whichever executor produced the frames, source-agnostic.
	var sink audioSink = &localTrackSink{track: local}
	sink = maybeStartAvatar(ctx, cfg, req, room, sink, logger)
	// Consume the #456 persona resolver: ResolveTTSVoice maps the resolved
	// canonical voice to the active provider's voice id (the cascade's TTS
	// voice). This replaces the standalone canonical-voice default the
	// cascade ships for tests.
	cascade := NewCascade(ctx, CascadeConfig{
		SpaceID:   req.SpaceID,
		GaAgentID: req.GaAgentID,
		Thread:    threadContextFor(req),
	}, client, newDeepgramTTSAdapter(tts, logger), sink, ResolveTTSVoice(req.Persona), logger)
	cascade.Start()

	return &roomAudioBridge{
		cfg:     cfg,
		roomReq: req,
		logger:  logger,
		asr:     asr,
		local:   local,
		cascade: cascade,
		streams: make(map[string]polyphon.ASRStream),
	}, nil
}

// onTrackSubscribed opens a Deepgram STT stream for a newly-subscribed
// human audio track and wires NewPCMRemoteTrack to feed it PCM16 at the STT
// sample rate. The decoded PCM frames flow to the per-participant sttSink,
// whose result channel is consumed by the cascade.
func (b *roomAudioBridge) onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if pub.Kind() != lksdk.TrackKindAudio {
		return
	}
	identity := rp.Identity()
	if identity == b.roomReq.GaAgentID {
		// Never transcribe our own published track (feedback-loop guard).
		return
	}

	stream, err := b.asr.StartStream(context.Background(), asrConfigFor(b.cfg))
	if err != nil {
		if b.logger != nil {
			b.logger.Warn("voice-agent room audio: start STT stream failed",
				"identity", identity, "err", err)
		}
		return
	}
	b.mu.Lock()
	b.streams[identity] = stream
	b.mu.Unlock()

	// The cascade consumes the result channel: interim/final transcripts +
	// the surfaced SpeechStarted onset (polyphon.ASRKindSpeechStarted).
	go b.cascade.ConsumeASR(stream.Results())

	if _, err := lkmedia.NewPCMRemoteTrack(track, &sttSink{stream: stream, logger: b.logger},
		lkmedia.WithTargetSampleRate(sttSampleRate),
		lkmedia.WithTargetChannels(1),
		lkmedia.WithHandleJitter(true),
	); err != nil && b.logger != nil {
		b.logger.Warn("voice-agent room audio: NewPCMRemoteTrack failed",
			"identity", identity, "err", err)
	}
	if b.logger != nil {
		b.logger.Info("voice-agent room audio: STT wired for participant", "identity", identity)
	}
}

// onParticipantDisconnected tears down a departed participant's STT stream.
func (b *roomAudioBridge) onParticipantDisconnected(rp *lksdk.RemoteParticipant) {
	identity := rp.Identity()
	b.mu.Lock()
	stream := b.streams[identity]
	delete(b.streams, identity)
	b.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

// close tears down the bridge: cascade + every STT stream + the local track.
func (b *roomAudioBridge) close() {
	b.cascade.Close()
	b.mu.Lock()
	for _, s := range b.streams {
		_ = s.Close()
	}
	b.streams = make(map[string]polyphon.ASRStream)
	b.mu.Unlock()
	if b.local != nil {
		_ = b.local.Close()
	}
}

// threadContextFor selects the cognition thread context for a session's
// turn requests. v1 ships the 1:1 standard-space default (TEAM); the
// polyphon group-thread path (THREAD_CONTEXT_GROUP) is selected once the
// session carries the space architecture.
// TODO(#456): plumb the space architecture through the SessionAck so a
// polyphon space resolves to THREAD_CONTEXT_GROUP, matching the Python
// agent's thread_context resolution.
func threadContextFor(_ RoomRequest) memqlv1.VoiceAgentTurnRequest_ThreadContext {
	return memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM
}

// localTrackSink adapts a PCMLocalTrack onto the cascade audioSink seam:
// it packs PCM16 little-endian bytes into a media.PCM16Sample and writes it
// to the published track (the SDK encodes to Opus + upsamples on publish).
type localTrackSink struct {
	track *lkmedia.PCMLocalTrack
}

func (s *localTrackSink) WriteFrame(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	sample := bytesToPCM16(pcm)
	return s.track.WriteSample(sample)
}

// Flush drops frames already queued in the publish track so a barge-in
// cuts the assistant audio immediately (PCMLocalTrack.ClearQueue).
func (s *localTrackSink) Flush() { s.track.ClearQueue() }

// sttSink adapts NewPCMRemoteTrack's PCMRemoteTrackWriter onto the Deepgram
// STT stream: each decoded PCM16 frame is packed to little-endian bytes and
// sent via SendAudio.
type sttSink struct {
	stream polyphon.ASRStream
	logger *slog.Logger
}

func (s *sttSink) WriteSample(sample mediasdk.PCM16Sample) error {
	if len(sample) == 0 {
		return nil
	}
	if err := s.stream.SendAudio(pcm16ToBytes(sample)); err != nil {
		if s.logger != nil {
			s.logger.Debug("voice-agent room audio: STT send failed", "err", err)
		}
	}
	return nil
}

func (s *sttSink) Close() error { return nil }

// pcm16ToBytes packs a media.PCM16Sample ([]int16) into little-endian
// PCM16 bytes (the Deepgram linear16 wire shape).
func pcm16ToBytes(sample mediasdk.PCM16Sample) []byte {
	out := make([]byte, len(sample)*2)
	for i, v := range sample {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

// bytesToPCM16 unpacks little-endian PCM16 bytes into a media.PCM16Sample.
func bytesToPCM16(pcm []byte) mediasdk.PCM16Sample {
	n := len(pcm) / 2
	out := make(mediasdk.PCM16Sample, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}
