//go:build voice

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"

	mediasdk "github.com/livekit/media-sdk"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/audio"
	"github.com/znasllc-io/memql/integrations/openai"
)

// room_realtime_voice.go is the voice-tagged (CGO/libopus) glue binding the
// LiveKit room media plane to the pure-Go realtime executor
// (realtime_executor.go), the realtime analog of room_audio_voice.go for the
// cascade. It is selected behind the executor-selection seam (room_voice.go
// branches on RoomRequest.Executor.Kind).
//
// Two channels feed the realtime session (docs/voice/433-multiparty-audio-
// routing.md): per-human OpenAI STT for labeled transcripts (the read side,
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
// the OpenAI STT clients (read side), the realtime websocket session, the
// published local track (the model's output audio), and one realtime executor.
type realtimeRoomBridge struct {
	cfg     Config
	roomReq RoomRequest
	logger  *slog.Logger

	asr       *openai.ASRClient
	session   *openai.RealtimeSession
	local     *lkmedia.PCMLocalTrack
	executor  *RealtimeExecutor
	lifecycle *RealtimeSessionLifecycle // cost guardrails (#459)

	// Gate-mode control (#478): when nativeEnabled, the bridge flips the
	// realtime session between the conductor gate (>=2 humans) and native
	// model-owns-turn mode (exactly 1 human) via session.UpdateSession +
	// executor.SetNativeMode. sessionBase holds the persona/voice/tools so the
	// two configs can be rebuilt; gateNative (under gateMu) tracks the live mode.
	nativeEnabled         bool
	multiPartySemanticVad bool // #481: multi-party uses semantic_vad + gate when set
	grounding             bool // #490: route 1-on-1 through the gate so grounding can inject
	agentToolLoop         bool // #1198 (A2): route 1-on-1 through the gate so cognition runs the full tool loop
	nativeSTT             bool // labeled ASR off the realtime path; gpt-realtime owns STT/VAD
	transcriptionModel    string
	language              string
	sessionBase           openai.SessionConfig

	gateMu sync.Mutex
	// gateTurnMode is the executor turn mode currently applied (one of the
	// turnMode* constants). Zero value (turnModeGatedCascade) matches the
	// connect-time config (turn_detection:null), so the first transition is to
	// native or gated-semantic_vad as the human count resolves.
	gateTurnMode int32

	mu      sync.Mutex
	streams map[string]polyphon.ASRStream // by participant identity

	// NOTE(#433): v1 forwards every subscribed human track's audio into the
	// realtime input buffer and lets the model's prosody read the floor-holder.
	// A real active-speaker dwell/debounce off active_speakers_changed
	// (selecting a single identity to forward) is a tuning follow-up.
}

// newRealtimeRoomBridge builds the realtime media plane: an OpenAI STT client
// (read side), the gpt-realtime websocket session configured with the resolved
// session persona (turn_detection:null), a published local audio track for the
// model's output, and the realtime executor wired to the local track as its
// room sink. Returns an error so the caller can fall back -- though selection
// already validated the preconditions (executor_select.go), the live dial can
// still fail and must not wedge the join.
func newRealtimeRoomBridge(ctx context.Context, cfg Config, req RoomRequest, client *Client, room *lksdk.Room, logger *slog.Logger) (*realtimeRoomBridge, error) {
	// The labeled-transcript ASR is OFF the realtime path by default
	// (cfg.RealtimeNativeSTT): gpt-realtime owns STT + turn detection + voice,
	// so no second transcription stream sits on the critical path. We only
	// stand up the standalone OpenAI ASR client when native STT is disabled
	// (the opt-in labeled-transcript read side). The integration stays fully
	// wired -- this just doesn't dial it for the snappy realtime path.
	var asr *openai.ASRClient
	if !cfg.RealtimeNativeSTT {
		var err error
		asr, err = openai.NewASRClient(openai.Config{
			APIKey:   cfg.OpenAIAPIKey,
			ASRModel: cfg.CascadeASRModel,
			Logger:   logger,
		})
		if err != nil {
			return nil, fmt.Errorf("voice-agent realtime room: openai asr: %w", err)
		}
	}

	rtClient, err := openai.NewRealtimeClient(openai.Config{APIKey: cfg.OpenAIAPIKey, Logger: logger}, cfg.RealtimeModel)
	if err != nil {
		return nil, fmt.Errorf("voice-agent realtime room: realtime client: %w", err)
	}

	// MCP tool bridge (#458): fetch the registry, build the low-risk-only tool
	// set, and configure the session with it. A failed list degrades to no
	// model-driven tools (FetchToolDefinitions returns nil) -- privileged tools
	// were never exposed anyway, so this is a safe degradation.
	//
	// #1429: the memQL ListTools round-trip (engine agent-tool-scope query on a
	// slow DB) used to SERIALIZE with the OpenAI websocket dial. The two legs
	// are independent: fetch concurrently, dial with the tools-less base
	// config, then apply the tools via session.UpdateSession before the bridge
	// returns. No audio or turn can reach the session in the tools-less window
	// -- the room callbacks only wire to the bridge after this constructor
	// returns.
	toolDefsCh := make(chan []*memqlv1.ToolDefinition, 1)
	go func() {
		toolFetchStart := time.Now()
		defs := FetchToolDefinitions(ctx, client, logger)
		// Setup phase (#1426): memQL round-trip (ListTools -> agent tool-scope
		// engine queries server-side), now overlapped with the realtime dial.
		logVoiceTiming(logger, "setup.fetch_tools", toolFetchStart,
			"space_id", req.SpaceID, "tool_count", len(defs))
		toolDefsCh <- defs
	}()

	// Connect with the conductor-gated base config (turn_detection:null). The
	// bridge flips to the #478 native config (semantic_vad + input transcription)
	// via session.UpdateSession once exactly one human is present -- see
	// applyGateMode. sessionBase is retained so both configs can be rebuilt.
	sessionBase := openai.SessionConfig{
		Instructions: req.Executor.SessionPersona.Instructions,
		Voice:        req.Executor.SessionPersona.Voice,
		// Server-side input noise reduction (#1431): filters the input BEFORE
		// the model's VAD, the documented mitigation for phantom turns from
		// laptop-speaker echo. Carried on the base config so every posture
		// (conductor-gated, native 1-on-1, multi-party) inherits it.
		// MEMQL_REALTIME_NOISE_REDUCTION: far_field (default) / near_field /
		// off ("" omits the field).
		NoiseReduction: cfg.NoiseReductionMode(),
		// Tools are NOT on the connect-time config (#1429): the registry fetch
		// runs concurrently with the dial and is applied via UpdateSession on
		// the join below.
	}
	realtimeConnectStart := time.Now()
	session, err := rtClient.Connect(ctx, sessionBase)
	if err != nil {
		return nil, fmt.Errorf("voice-agent realtime room: realtime connect: %w", err)
	}
	// Setup phase (#1426): gpt-realtime websocket dial + session.update -- the
	// "realtime session established" milestone.
	logVoiceTiming(logger, "setup.realtime_connect", realtimeConnectStart, "space_id", req.SpaceID)

	// Join the concurrent tool fetch and apply the tool set. sessionBase
	// carries the tools from here on, so every later gate-mode UpdateSession
	// (applyGateMode rebuilds from sessionBase) re-asserts them; a failed
	// update here therefore self-heals on the first human's gate-mode
	// transition. Failure is a warn, never a session failure -- identical
	// degradation to a failed fetch (no model-driven tools until re-asserted).
	toolDefs := <-toolDefsCh
	toolBridge := NewMcpToolBridge(
		NewGrpcToolCallTransport(client),
		NewLogMirrorSink(logger),
		toolDefs,
		req.SpaceID,
		req.GaAgentID,
		logger,
	)
	sessionBase.Tools = toolBridge.RealtimeTools()
	if len(sessionBase.Tools) > 0 {
		if err := session.UpdateSession(sessionBase); err != nil && logger != nil {
			logger.Warn("voice-agent realtime room: tool session update failed (re-asserted on next gate-mode update)",
				"space_id", req.SpaceID, "tool_count", len(sessionBase.Tools), "err", err)
		}
	}

	// The model emits 24 kHz PCM16; the local track is constructed at that
	// rate so the media-sdk encoder upsamples to Opus on publish.
	publishStart := time.Now()
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
	// Setup phase (#1426): local output-track build + publish.
	logVoiceTiming(logger, "setup.publish_track", publishStart, "space_id", req.SpaceID)

	// Avatar wrap (#460/#1428): like the cascade bridge, wrap the room-publish
	// sink through maybeStartAvatar so the model's output PCM is ALSO forwarded
	// to the avatar participant for lip-sync. This wrap was missing on the
	// realtime path -- the executor wrote into a bare localTrackSink, so a
	// session avatar never received realtime output audio (dead lips). The
	// producer rate is the model's 24 kHz (audio.OpenAISampleRate), stamped on
	// the byte-stream header so the vendor engine resamples correctly. No
	// avatar configured / start failure -> the sink comes back unchanged
	// (audio-only), same non-fatal contract as the cascade.
	var sink audioSink = &localTrackSink{track: local}
	sink = maybeStartAvatar(ctx, cfg, req, room, sink, audio.OpenAISampleRate, logger)
	executor := NewRealtimeExecutor(ctx, CascadeConfig{
		SpaceID:   req.SpaceID,
		GaAgentID: req.GaAgentID,
		Thread:    threadContextFor(req),
	}, client, session, sink, req.Executor.SessionPersona, logger)

	// Output capture (#458): forward the model's final spoken transcript as a
	// VoiceAgentRealtimeOutput so chat/canvas/audit render the realtime turn.
	// The citation resolver is built from the session grounding (#456); with no
	// per-session grounding plumbed yet it is the strict no-op resolver, so an
	// ungrounded realtime reply lands byte-identical to an ungrounded text reply.
	executor.SetOutputForwarder(NewRealtimeOutputForwarder(
		client, req.SpaceID, req.GaAgentID, NewCitationResolver(GroundingContext{}),
	))
	executor.SetToolBridge(toolBridge)
	// #1430 async tool-call bounds: per-call execution timeout + concurrent
	// pending-call cap (spoken-failure-style results past either bound).
	executor.ConfigureToolCalls(
		time.Duration(cfg.RealtimeToolTimeoutSec)*time.Second, cfg.RealtimeMaxPendingTools)
	// #1431 confidence gate: drop input-transcription finals whose mean token
	// logprob falls below the floor (finals without logprobs always pass).
	executor.SetTranscriptConfidenceFloor(cfg.RealtimeTranscriptMinConfidence)

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
		cfg:           cfg,
		roomReq:       req,
		logger:        logger,
		asr:           asr,
		session:       session,
		local:         local,
		executor:      executor,
		lifecycle:     lifecycle,
		nativeEnabled: cfg.RealtimeNativeTurn,
		// Native STT (labeled ASR off) forces semantic_vad for multi-party too --
		// without a labeled-ASR read side there is no gated-cascade path to fall back
		// to, so a second human routes through the gated-semantic_vad mode (the
		// model still detects + transcribes the turn natively).
		multiPartySemanticVad: cfg.RealtimeMultiPartySemanticVad || cfg.RealtimeNativeSTT,
		grounding:             cfg.VoiceGrounding,
		agentToolLoop:         cfg.RealtimeAgentToolLoop,
		nativeSTT:             cfg.RealtimeNativeSTT,
		transcriptionModel:    cfg.RealtimeTranscriptionModel,
		// OpenAI's GA realtime input-transcription accepts only the bare
		// ISO-639-1 primary subtag ("en"), and rejects a region-qualified tag
		// ("en-US") with an api error that invalidates the whole session.update
		// (so semantic_vad never applies and the model never owns the turn).
		language:    primarySubtag(cfg.VoiceLanguage),
		sessionBase: sessionBase,
		streams:     make(map[string]polyphon.ASRStream),
	}, nil
}

// primarySubtag narrows a BCP-47 language tag to its ISO-639-1 primary subtag
// ("en-US" -> "en", "zh-Hans" -> "zh"), lowercased. Empty in -> empty out (the
// transcription config then omits language). This is the form OpenAI's GA
// realtime input-transcription accepts; a region-qualified tag is rejected.
func primarySubtag(tag string) string {
	tag = strings.TrimSpace(tag)
	for i, r := range tag {
		if r == '-' || r == '_' {
			tag = tag[:i]
			break
		}
	}
	return strings.ToLower(tag)
}

// nativeSessionConfig returns the base session config with semantic_vad turn
// detection + native input transcription enabled -- the #478 native 1-on-1
// posture where gpt-realtime owns the turn.
func (b *realtimeRoomBridge) nativeSessionConfig() openai.SessionConfig {
	cfg := b.sessionBase
	// server_vad, NOT semantic_vad: in practice semantic_vad stalled 6-15s before
	// committing a finished turn (its "is the turn semantically complete?" model
	// is far too conservative here), which also broke barge-in. server_vad is a
	// deterministic energy gate -- it commits SilenceDurationMs after the user
	// drops below Threshold, so end-of-turn is fast and predictable, and
	// interrupt_response gives clean barge-in. The knobs (Threshold raised off
	// the OpenAI 0.5 default so noise/silence no longer commits a phantom turn)
	// are tunable via MEMQL_REALTIME_VAD_* and built by realtimeServerVad (#1203).
	cfg.TurnDetection = realtimeServerVad(b.cfg)
	cfg.InputTranscription = &openai.InputTranscriptionConfig{
		Model:    b.transcriptionModel,
		Language: b.language,
	}
	// Per-token logprobs on the input transcription (#1431): the confidence
	// signal the executor's transcript gate reads. Requested whenever input
	// transcription is on; the gate tolerates the server omitting them.
	cfg.Include = []string{openai.IncludeInputTranscriptionLogprobs}
	return cfg
}

// multiPartySemanticVadConfig returns the base config with semantic_vad turn
// detection but create_response:FALSE -- the #481 multi-party posture. The model
// detects turn-end + transcribes the active speaker but does NOT auto-generate;
// the conductor gate decides engage/defer and the executor fires CreateResponse
// on engage. Barge-in is native (interrupt_response).
func (b *realtimeRoomBridge) multiPartySemanticVadConfig() openai.SessionConfig {
	cfg := b.sessionBase
	cfg.TurnDetection = &openai.TurnDetectionConfig{
		Type:              "semantic_vad",
		CreateResponse:    false,
		InterruptResponse: true,
		Eagerness:         "auto",
	}
	cfg.InputTranscription = &openai.InputTranscriptionConfig{
		Model:    b.transcriptionModel,
		Language: b.language,
	}
	// Logprobs for the #1431 confidence gate, same as nativeSessionConfig.
	cfg.Include = []string{openai.IncludeInputTranscriptionLogprobs}
	return cfg
}

// applyGateMode resolves the executor turn mode from the live human count + the
// deploy flags and applies it (session.update + executor.SetTurnMode) on a
// transition. One pipeline, one optional gate (#475):
//   - exactly one human -> native (#478): the model owns the turn.
//   - >=2 humans + multi-party semantic_vad enabled -> gated-semantic_vad
//     (#481): the model detects the turn, the conductor gate decides, the model
//     authors on engage.
//   - >=2 humans otherwise -> gated-cascade: turn_detection:null + labeled ASR +
//     the conductor gate (the pre-#481 multi-party path).
//
// Idempotent (no-op when the resolved mode already matches). Called after the
// lifecycle's human-count changes (track-subscribe / participant-disconnect).
//
// v1 limitation: reconfiguring turn_detection mid-call can race an in-flight
// response if the human count crosses a boundary as the model is speaking. The
// transition is rare; v1 reconfigures on the boundary and lets the current
// response finish; a settle/queue is a follow-up.
func (b *realtimeRoomBridge) applyGateMode() {
	if !b.nativeEnabled {
		return
	}
	b.mu.Lock()
	humanCount := len(b.streams)
	b.mu.Unlock()

	var mode int32
	var cfg openai.SessionConfig
	switch {
	case humanCount == 1 && (b.grounding || b.agentToolLoop):
		// 1-on-1 routed through the gate (create_response:false) instead of pure
		// native authorship for either of two reasons:
		//   - grounding (#490): the executor injects the retrieved grounding block
		//     before generation -- pure native mode has no inject window.
		//   - agent tool loop (#1198, A2): the model must NOT auto-author; the turn
		//     round-trips to cognition, which runs the full tool loop
		//     (produceArtifact etc.) and authors the reply, and the model re-voices
		//     the authored FinalText (runTurn's directive_mode-empty path).
		mode = turnModeGatedSemanticVad
		cfg = b.multiPartySemanticVadConfig()
	case humanCount == 1:
		mode = turnModeNative
		cfg = b.nativeSessionConfig()
	case b.multiPartySemanticVad:
		mode = turnModeGatedSemanticVad
		cfg = b.multiPartySemanticVadConfig()
	default:
		mode = turnModeGatedCascade
		cfg = b.sessionBase // turn_detection:null
	}

	b.gateMu.Lock()
	defer b.gateMu.Unlock()
	if mode == b.gateTurnMode {
		return
	}
	updateStart := time.Now()
	if err := b.session.UpdateSession(cfg); err != nil {
		if b.logger != nil {
			b.logger.Warn("voice-agent realtime room: gate-mode session update failed",
				"want_mode", mode, "err", err)
		}
		return
	}
	// Setup phase (#1426): gate-mode session.update -- fires on the first
	// human's track (part of bring-up) and on later human-count transitions.
	logVoiceTiming(b.logger, "setup.gate_mode_update", updateStart,
		"space_id", b.roomReq.SpaceID, "mode", mode, "human_count", humanCount)
	b.executor.SetTurnMode(mode)
	b.gateTurnMode = mode
	if b.logger != nil {
		b.logger.Info("voice-agent realtime room: gate mode applied",
			"mode", mode, "human_count", humanCount)
	}
}

// onTrackSubscribed opens a labeled STT stream for a newly-subscribed human
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

	// One audio pipeline per participant. The SPA publishes more than one mic
	// track per participant (and the connect->build replay can re-present a track
	// the live callback also fires for), so dedupe on identity BEFORE the slow
	// StartStream. A second track for an already-wired participant would otherwise
	// open a second STT stream AND a second PCM pump into the model's input
	// buffer -- doubled audio that garbles transcription, duplicates the chat
	// utterance, and breaks semantic_vad turn detection. Reserve the slot inside
	// the lock so a concurrent second track loses the race and returns here.
	b.mu.Lock()
	if _, exists := b.streams[identity]; exists {
		b.mu.Unlock()
		return
	}
	b.streams[identity] = nil // reservation; replaced with the real stream below
	b.mu.Unlock()

	// Register the participant on the roster for labeled-transcript
	// attribution (#433 section 3). Display name + role resolution from memQL
	// is a follow-up; the identity is the stable key in the interim.
	b.executor.SetParticipant(identity, rp.Name(), "")
	// Seed the active speaker from the LiveKit identity. With labeled ASR off the
	// path the native input transcript (EventInputTranscriptDone) has no speaker
	// of its own, so without this the user utterance would forward with an empty
	// speaker id and the BFF would reject the insert.
	b.executor.SetActiveSpeaker(identity)

	// The labeled-ASR read side is opt-in (cfg.RealtimeNativeSTT off). On the default
	// snappy path it is nil -- gpt-realtime does STT + turn detection itself and
	// the only executor input is the PCM pump (StreamAudio).
	var stream polyphon.ASRStream
	if !b.nativeSTT && b.asr != nil {
		sttStart := time.Now()
		s, err := b.asr.StartStream(context.Background(), asrConfigFor(b.cfg))
		logVoiceTiming(b.logger, "setup.stt_stream", sttStart,
			"space_id", b.roomReq.SpaceID, "identity", identity, "ok", err == nil)
		if err != nil {
			// Release the reservation so a later track for this identity can retry.
			b.mu.Lock()
			delete(b.streams, identity)
			b.mu.Unlock()
			if b.logger != nil {
				b.logger.Warn("voice-agent realtime room: start STT stream failed",
					"identity", identity, "err", err)
			}
			return
		}
		stream = s
	}
	b.mu.Lock()
	b.streams[identity] = stream // nil under native STT; still counts the human
	b.mu.Unlock()

	// First (and only) track for this participant: feed the empty-room guardrail
	// (#459) and resolve the gate mode (#478: exactly one human -> native model-
	// owns-turn; a second human -> conductor gate). The matching NoteHumanLeft
	// fires in onParticipantDisconnected.
	if b.lifecycle != nil {
		b.lifecycle.NoteHumanJoined()
	}
	b.applyGateMode()

	if stream != nil {
		go b.executor.ConsumeASR(identity, stream.Results())
	}

	if _, err := lkmedia.NewPCMRemoteTrack(track, &realtimeSttSink{
		stream:   stream, // nil under native STT -> WriteSample only feeds the model
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
	// A departure changes the human count -> re-resolve the gate mode (#478):
	// dropping back to one human re-enters native mode.
	if tracked {
		b.applyGateMode()
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
		if s != nil { // nil under native STT (no labeled STT stream opened)
			_ = s.Close()
		}
	}
	b.streams = make(map[string]polyphon.ASRStream)
	b.mu.Unlock()
	if b.local != nil {
		_ = b.local.Close()
	}
}

// realtimeSttSink adapts NewPCMRemoteTrack's writer onto BOTH the labeled STT
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
	// The labeled-ASR read side is optional (nil under native STT). When present, fan the
	// frame out to it for labeled transcripts; always stream to the model, which
	// owns STT + turn detection on the snappy path.
	if s.stream != nil {
		if err := s.stream.SendAudio(pcm); err != nil && s.logger != nil {
			s.logger.Debug("voice-agent realtime room: STT send failed", "err", err)
		}
	}
	s.executor.StreamAudio(pcm)
	return nil
}

func (s *realtimeSttSink) Close() error { return nil }
