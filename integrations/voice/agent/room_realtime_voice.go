//go:build voice

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"

	mediasdk "github.com/livekit/media-sdk"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/audio"
	"github.com/znasllc-io/memql/integrations/deepgram"
	"github.com/znasllc-io/memql/integrations/openai"
)

// room_realtime_voice.go is the voice-tagged (CGO/libopus) glue binding the
// LiveKit room media plane to the pure-Go realtime executor
// (realtime_executor.go), the realtime analog of room_audio_voice.go for the
// cascade. It is selected behind the executor-selection seam (room_voice.go
// branches on RoomRequest.Executor.Kind).
//
// Two channels feed the realtime session (docs/voice/433-multiparty-audio-
// routing.md): per-human Deepgram STT for labeled transcripts (the read side,
// attribution) and the active-speaker decoded PCM for prosody + barge-in (the
// hear side, streamed via the executor's StreamAudio). The model's output
// audio is published into the same local track sink the avatar (#460)
// subscribes to, exactly as the cascade publishes its TTS.
//
// Everything that touches the media-sdk PCM types lives here, behind
// `//go:build voice`. The conductor gate + multi-party labeled-item injection
// in realtime_executor.go stay CGO-free and unit-tested in the default lane.

// mediaBridge is the common surface the room-join callbacks drive, satisfied
// by both the cascade bridge (roomAudioBridge) and the realtime bridge
// (realtimeRoomBridge). It lets room_voice.go branch on the executor once and
// then treat the chosen bridge uniformly.
type mediaBridge interface {
	onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant)
	onParticipantDisconnected(rp *lksdk.RemoteParticipant)
	close()
}

// buildMediaBridge constructs the media plane for the resolved executor and
// returns it alongside the SpeakSink to register for unsolicited VoiceAgentSpeak
// pushes. Realtime is built only when the executor selection resolved to
// realtime (executor_select.go already validated the preconditions); otherwise
// the cascade is built -- the default and the clean fallback. A realtime build
// failure here (the live dial can still fail post-validation) is NOT swallowed
// into a cascade fallback at this layer: selection owns fallback; a hard
// failure after a clean selection surfaces as a join error so it is visible.
func (j *liveKitRoomJoiner) buildMediaBridge(ctx context.Context, req RoomRequest, room *lksdk.Room) (mediaBridge, SpeakSink, error) {
	if req.Executor.IsRealtime() {
		b, err := newRealtimeRoomBridge(ctx, j.cfg, req, j.client, room, j.logger)
		if err != nil {
			return nil, nil, err
		}
		return b, b.executor, nil
	}
	b, err := newRoomAudioBridge(ctx, j.cfg, req, j.client, room, j.logger)
	if err != nil {
		return nil, nil, err
	}
	return b, b.cascade, nil
}

// realtimeRoomBridge owns the per-session media plane for the realtime path:
// the Deepgram STT clients (read side), the realtime websocket session, the
// published local track (the model's output audio), and one realtime executor.
type realtimeRoomBridge struct {
	cfg     Config
	roomReq RoomRequest
	logger  *slog.Logger

	asr       *deepgram.ASRClient
	session   *openai.RealtimeSession
	local     *lkmedia.PCMLocalTrack
	executor  *RealtimeExecutor
	lifecycle *RealtimeSessionLifecycle // cost guardrails (#459)

	mu      sync.Mutex
	streams map[string]polyphon.ASRStream // by participant identity

	// NOTE(#433): v1 forwards every subscribed human track's audio into the
	// realtime input buffer and lets the model's prosody read the floor-holder.
	// A real active-speaker dwell/debounce off active_speakers_changed
	// (selecting a single identity to forward) is a tuning follow-up.
}

// newRealtimeRoomBridge builds the realtime media plane: a Deepgram STT client
// (read side), the gpt-realtime websocket session configured with the resolved
// session persona (turn_detection:null), a published local audio track for the
// model's output, and the realtime executor wired to the local track as its
// room sink. Returns an error so the caller can fall back -- though selection
// already validated the preconditions (executor_select.go), the live dial can
// still fail and must not wedge the join.
func newRealtimeRoomBridge(ctx context.Context, cfg Config, req RoomRequest, client *Client, room *lksdk.Room, logger *slog.Logger) (*realtimeRoomBridge, error) {
	dgCfg := deepgram.Config{
		APIKey:   cfg.DeepgramAPIKey,
		ASRModel: cfg.DGASRModel,
		Language: cfg.DGLanguage,
		Logger:   logger,
	}
	asr, err := deepgram.NewASRClient(dgCfg)
	if err != nil {
		return nil, fmt.Errorf("voice-agent realtime room: deepgram asr: %w", err)
	}

	rtClient, err := openai.NewRealtimeClient(openai.Config{APIKey: cfg.OpenAIAPIKey, Logger: logger}, cfg.RealtimeModel)
	if err != nil {
		return nil, fmt.Errorf("voice-agent realtime room: realtime client: %w", err)
	}
	session, err := rtClient.Connect(ctx, openai.SessionConfig{
		Instructions: req.Executor.SessionPersona.Instructions,
		Voice:        req.Executor.SessionPersona.Voice,
	})
	if err != nil {
		return nil, fmt.Errorf("voice-agent realtime room: realtime connect: %w", err)
	}

	// The model emits 24 kHz PCM16; the local track is constructed at that
	// rate so the media-sdk encoder upsamples to Opus on publish.
	local, err := lkmedia.NewPCMLocalTrack(audio.OpenAISampleRate, 1, protoLogger.GetLogger())
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("voice-agent realtime room: local track: %w", err)
	}
	if _, err := room.LocalParticipant.PublishTrack(local, &lksdk.TrackPublicationOptions{
		Name:   "agent-voice",
		Source: livekit.TrackSource_MICROPHONE,
	}); err != nil {
		session.Close()
		_ = local.Close()
		return nil, fmt.Errorf("voice-agent realtime room: publish track: %w", err)
	}

	sink := &localTrackSink{track: local}
	executor := NewRealtimeExecutor(ctx, CascadeConfig{
		SpaceID:   req.SpaceID,
		GaAgentID: req.GaAgentID,
		Thread:    threadContextFor(req),
	}, client, session, sink, req.Executor.SessionPersona, logger)

	// Cost guardrails (#459): bound the warm session by empty-room / idle /
	// max-duration / audio-token budget. The stop callback closes the realtime
	// session, which unwinds the executor's drain loops; on a cost-guardrail
	// trip ShouldDegradeToCascade is set for the caller's rebuild decision (the
	// rebuild itself is a JoinAndServe-level follow-up seam). The executor feeds
	// engage + audio-token usage; participant join/leave is forwarded below.
	lifecycle := NewRealtimeLifecycle(
		RealtimeBudgetFromConfig(cfg),
		func(reason TeardownReason) error { return session.Close() },
		logger,
		withLifecycleSpaceID(req.SpaceID),
	)
	executor.AttachLifecycle(lifecycle)
	executor.Start()
	// Seed the population from zero and let track-subscribe drive joins: the
	// empty-room guardrail must not fire before the first human's track lands,
	// so the idle watchdog (not empty-room) governs a never-populated session.
	// onTrackSubscribed feeds NoteHumanJoined (deduped per identity) for every
	// human, including those already present at join (LiveKit replays their
	// track subscriptions); onParticipantDisconnected feeds NoteHumanLeft.
	lifecycle.Start(0)

	return &realtimeRoomBridge{
		cfg:       cfg,
		roomReq:   req,
		logger:    logger,
		asr:       asr,
		session:   session,
		local:     local,
		executor:  executor,
		lifecycle: lifecycle,
		streams:   make(map[string]polyphon.ASRStream),
	}, nil
}

// onTrackSubscribed opens a Deepgram STT stream for a newly-subscribed human
// audio track (the labeled-transcript read side) and forwards the decoded PCM16
// frames into BOTH the STT stream and the realtime input buffer (the active-
// speaker prosody/barge-in hear side, #433). The per-track participant identity
// is registered on the executor roster so transcripts get a "[name . role]"
// label.
func (b *realtimeRoomBridge) onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if pub.Kind() != lksdk.TrackKindAudio {
		return
	}
	identity := rp.Identity()
	if identity == b.roomReq.GaAgentID {
		// Never feed our own published track back (feedback-loop guard).
		return
	}

	// Register the participant on the roster for labeled-transcript
	// attribution (#433 section 3). Display name + role resolution from memQL
	// is a follow-up; the identity is the stable key in the interim.
	b.executor.SetParticipant(identity, rp.Name(), "")

	stream, err := b.asr.StartStream(context.Background(), asrConfigFor(b.cfg))
	if err != nil {
		if b.logger != nil {
			b.logger.Warn("voice-agent realtime room: start STT stream failed",
				"identity", identity, "err", err)
		}
		return
	}
	b.mu.Lock()
	_, alreadyTracked := b.streams[identity]
	b.streams[identity] = stream
	b.mu.Unlock()

	// Feed the lifecycle's empty-room guardrail (#459) the first time we see a
	// human participant's track. Keyed on the streams map so a participant
	// publishing multiple tracks counts once. The matching NoteHumanLeft fires
	// in onParticipantDisconnected.
	if !alreadyTracked && b.lifecycle != nil {
		b.lifecycle.NoteHumanJoined()
	}

	go b.executor.ConsumeASR(identity, stream.Results())

	if _, err := lkmedia.NewPCMRemoteTrack(track, &realtimeSttSink{
		stream:   stream,
		executor: b.executor,
		logger:   b.logger,
	},
		lkmedia.WithTargetSampleRate(sttSampleRate),
		lkmedia.WithTargetChannels(1),
		lkmedia.WithHandleJitter(true),
	); err != nil && b.logger != nil {
		b.logger.Warn("voice-agent realtime room: NewPCMRemoteTrack failed",
			"identity", identity, "err", err)
	}
	if b.logger != nil {
		b.logger.Info("voice-agent realtime room: STT + audio wired for participant", "identity", identity)
	}
}

// onParticipantDisconnected tears down a departed participant's STT stream.
func (b *realtimeRoomBridge) onParticipantDisconnected(rp *lksdk.RemoteParticipant) {
	identity := rp.Identity()
	b.mu.Lock()
	stream, tracked := b.streams[identity]
	delete(b.streams, identity)
	b.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
	// Feed the empty-room guardrail (#459) only for a participant we counted on
	// join, so the join/leave deltas stay balanced and the last human leaving
	// tears the warm session down.
	if tracked && b.lifecycle != nil {
		b.lifecycle.NoteHumanLeft()
	}
}

// close tears down the bridge: executor (closes the realtime session) + every
// STT stream + the local track.
func (b *realtimeRoomBridge) close() {
	if b.lifecycle != nil {
		b.lifecycle.Close()
	}
	b.executor.Close()
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

// realtimeSttSink adapts NewPCMRemoteTrack's writer onto BOTH the Deepgram STT
// stream (labeled-transcript read side) and the realtime executor's active-
// speaker audio input (prosody/barge-in hear side, #433 section 2). Each
// decoded PCM16 frame is packed to little-endian bytes and fanned out to both.
type realtimeSttSink struct {
	stream   polyphon.ASRStream
	executor *RealtimeExecutor
	logger   *slog.Logger
}

func (s *realtimeSttSink) WriteSample(sample mediasdk.PCM16Sample) error {
	if len(sample) == 0 {
		return nil
	}
	pcm := pcm16ToBytes(sample)
	if err := s.stream.SendAudio(pcm); err != nil && s.logger != nil {
		s.logger.Debug("voice-agent realtime room: STT send failed", "err", err)
	}
	// Stream the same frame to the model for prosody + barge-in (#433 2b).
	s.executor.StreamAudio(pcm)
	return nil
}

func (s *realtimeSttSink) Close() error { return nil }
