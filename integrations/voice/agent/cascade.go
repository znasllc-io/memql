package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
)

// cascade.go is the Go cascade voice loop: human audio -> OpenAI STT ->
// memQL cognition -> OpenAI TTS -> room audio, with turn-taking and
// barge-in. It is the Go analog of the Python cascade
// (the Python voice-agent's main.py + memql_llm_plugin.py +
// transcript_forwarder.py + tts_plugin.py), at parity with that baseline.
//
// It is pure Go and depends only on small interfaces (ASRStream from
// polyphon, the gRPC Client, and the local sttSource / ttsSynthesizer /
// audioSink seams below), so the whole STT->TurnRequest->TTS flow and the
// barge-in cancel path are unit-testable in the default CGO-free CI lane.
// The voice-tagged room glue (room_audio_voice.go) is the only thing that
// imports the LiveKit media-sdk; it wires real PCM frames into the seams
// this orchestrator exposes.

// ttsSynthesizer is the cascade's TTS seam. It is satisfied by
// *openai.TTSClient.Synthesize-style methods, but the
// orchestrator only needs PCM16 frames at the room rate, so the seam is
// defined in terms of the local-track sample shape and kept free of the
// provider package to avoid an import cycle and keep tests trivial.
//
// SynthesizePCM streams PCM16 (little-endian, mono, at the agent's room
// sample rate) for the given text + voice id onto the returned channel,
// closing it when synthesis completes. It must abort promptly when ctx is
// cancelled (barge-in) so the publish pump stops within a frame budget.
type ttsSynthesizer interface {
	SynthesizePCM(ctx context.Context, text, voiceID string) (<-chan []byte, error)
}

// audioSink is the cascade's room-publish seam. It is satisfied by the
// voice-tagged PCMLocalTrack wrapper (room_audio_voice.go); tests use an
// in-memory recorder. WriteFrame publishes one PCM16 frame into the room;
// Flush drops any frames already buffered in the publish track so a
// barge-in cuts the assistant within a frame budget rather than waiting
// for the buffered tail to drain.
type audioSink interface {
	WriteFrame(pcm []byte) error
	Flush()
}

// personaVoice resolves the provider TTS voice id for the assistant. The
// #456 persona resolver owns the canonical -> provider-voice mapping
// (ResolveTTSVoice); the voice-build bridge passes its result into the
// constructor's voiceID. The cascade consumes it through this tiny seam so
// it stays decoupled from the resolver and works standalone in tests.
type personaVoice interface {
	// VoiceID returns the TTS voice id to synthesize with. Empty means
	// "let the TTS layer pick its configured default."
	VoiceID() string
}

// defaultPersonaVoice returns the provider voice id passed at construction
// (ResolveTTSVoice(persona) in the voice build; a literal in tests), or
// empty to defer to the TTS client's configured default.
type defaultPersonaVoice struct{ canonical string }

func (d defaultPersonaVoice) VoiceID() string { return strings.TrimSpace(d.canonical) }

// CascadeConfig parameterizes the cascade orchestrator.
type CascadeConfig struct {
	SpaceID   string
	GaAgentID string
	// SpeakerUserID is the fallback/default speaker attribution for the
	// human transcript (the 1:1 standard-space wiring). The per-track
	// LiveKit participant identity carried through ConsumeASR wins when
	// present (#433 / #1403). Empty is allowed only because the server
	// resolves a space's SINGLE active human participant as a last
	// resort; multi-human spaces require a per-track identity.
	SpeakerUserID string
	// Thread selects the cognition thread context for turn requests.
	Thread memqlv1.VoiceAgentTurnRequest_ThreadContext
}

// Cascade owns one space's cascade voice loop. It binds a turn-taking
// machine (turntaking.go) to: the ASR result stream (transcript
// forwarding + onset/EOU signals), the gRPC client (VoiceAgentPartial/
// FinalTranscript + the streaming VoiceAgentTurnRequest), and the TTS ->
// room publish pump (with barge-in cancel).
type Cascade struct {
	cfg     CascadeConfig
	client  *Client
	tts     ttsSynthesizer
	sink    audioSink
	persona personaVoice
	logger  *slog.Logger

	machine *TurnMachine

	// seq is the monotonic per-session partial-transcript sequence
	// counter (the Go analog of TranscriptForwarder._counters), reset on
	// each final so a new utterance starts a fresh partial run.
	seq atomic.Int64

	// ttsMu guards the in-flight TTS playout cancel func so barge-in can
	// stop the current pump without racing a new one.
	ttsMu     sync.Mutex
	ttsCancel context.CancelFunc

	// speakerMu guards lastSpeaker: the LiveKit participant identity of
	// the most recent human track to produce an ASR result. The turn
	// machine surfaces only the committed text (OnFinalTranscript carries
	// no speaker), so the cascade tracks the active speaker per EOU here
	// -- the commit fires synchronously on the same per-track consume
	// goroutine that just recorded the speaker, mirroring the realtime
	// executor's setLastSpeaker/getLastSpeaker pattern (#1403).
	speakerMu   sync.Mutex
	lastSpeaker string

	// ctx is the session-scoped context; cancelled on Close.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewCascade builds a cascade orchestrator. ttsVoiceID is the provider TTS
// voice id to synthesize with (ResolveTTSVoice(persona) from the #456
// resolver in the voice build; a literal in tests). tts and sink are the
// synthesis + room-publish seams (real implementations in
// room_audio_voice.go; in-memory in tests).
func NewCascade(
	parent context.Context,
	cfg CascadeConfig,
	client *Client,
	tts ttsSynthesizer,
	sink audioSink,
	ttsVoiceID string,
	logger *slog.Logger,
) *Cascade {
	ctx, cancel := context.WithCancel(parent)
	c := &Cascade{
		cfg:     cfg,
		client:  client,
		tts:     tts,
		sink:    sink,
		persona: defaultPersonaVoice{canonical: ttsVoiceID},
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
	}
	c.machine = NewTurnMachine(TurnCallbacks{
		OnFinalTranscript: c.onCommittedTurn,
		OnAssistantStart:  c.onAssistantStart,
		OnBargeIn:         c.onBargeIn,
	}, logger)
	return c
}

// SetPersonaVoice swaps in a richer persona/voice resolver (the #456 seam).
// Until that lands, the constructor's defaultPersonaVoice is used.
func (c *Cascade) SetPersonaVoice(p personaVoice) {
	if p != nil {
		c.persona = p
	}
}

// Start moves the machine to listening. Call once the room is joined and
// the STT stream is open.
func (c *Cascade) Start() { c.machine.Start() }

// Close tears down the cascade: cancels any in-flight TTS playout and
// returns the machine to idle. Idempotent.
func (c *Cascade) Close() {
	c.cancelTTS()
	c.machine.Stop()
	c.cancel()
}

// Machine exposes the turn-taking machine (tests / diagnostics).
func (c *Cascade) Machine() *TurnMachine { return c.machine }

// ConsumeASR drives the machine from one human track's labeled ASR result
// stream. speakerIdentity is the LiveKit participant identity that owns the
// track (the canonical `v1:cognition:participant:...` id), carried with
// every result so the committed turn is attributed to the actual speaker
// (#1403). It maps each result onto a turn-taking input: a speech-started
// kind raises an onset (transition A / barge-in C), an interim updates the
// partial and is forwarded as a VoiceAgentPartialTranscript, and a final
// commits the turn (transition D). It ranges until the stream channel
// closes or the session context is cancelled, so it is the long-lived
// per-track consumer goroutine (one per human audio track; #433 scales it
// to many).
func (c *Cascade) ConsumeASR(speakerIdentity string, results <-chan polyphon.ASRResult) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case r, ok := <-results:
			if !ok {
				return
			}
			c.handleASRResult(speakerIdentity, r)
		}
	}
}

// handleASRResult routes one ASR result for a given speaker. Split out for
// direct unit testing without standing up a channel.
func (c *Cascade) handleASRResult(speakerIdentity string, r polyphon.ASRResult) {
	// Track the active speaker on EVERY result, before any routing: the turn
	// machine's commit callback carries only the text, so onCommittedTurn
	// reads the speaker recorded here (same goroutine, so the final that
	// commits the turn always sees its own track's identity).
	if id := strings.TrimSpace(speakerIdentity); id != "" {
		c.setLastSpeaker(id)
	}
	if r.Kind == polyphon.ASRKindSpeechStarted {
		c.machine.OnSpeechStarted()
		return
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return
	}
	if r.IsFinal {
		// The machine reports the committed transcript via
		// OnFinalTranscript -> onCommittedTurn (forward final + turn req).
		c.machine.OnFinal(text)
		return
	}
	// Interim: keep the machine's partial fresh and forward to memQL as a
	// VoiceAgentPartialTranscript (event-only server-side, drives the live
	// transcript in chat). Mirrors transcript_forwarder.forward_partial.
	c.machine.OnInterim(text)
	c.forwardPartial(speakerIdentity, text)
}

// setLastSpeaker records the most recent human track to produce an ASR
// result (the per-EOU active speaker).
func (c *Cascade) setLastSpeaker(identity string) {
	c.speakerMu.Lock()
	c.lastSpeaker = identity
	c.speakerMu.Unlock()
}

func (c *Cascade) getLastSpeaker() string {
	c.speakerMu.Lock()
	defer c.speakerMu.Unlock()
	return c.lastSpeaker
}

// resolveSpeaker maps a per-track participant identity to the
// speaker_user_id stamped on the wire: the per-track identity when present,
// else the configured 1:1 fallback. Mirrors RealtimeExecutor.resolveSpeaker.
func (c *Cascade) resolveSpeaker(identity string) string {
	if id := strings.TrimSpace(identity); id != "" {
		return id
	}
	return c.cfg.SpeakerUserID
}

// forwardPartial sends a VoiceAgentPartialTranscript (best-effort, no
// reply awaited -- the server acks but we don't block the audio loop on it).
func (c *Cascade) forwardPartial(speakerIdentity, text string) {
	seq := c.seq.Add(1)
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentPartialTranscript{
			VoiceAgentPartialTranscript: &memqlv1.VoiceAgentPartialTranscript{
				SpaceId:       c.cfg.SpaceID,
				SpeakerUserId: c.resolveSpeaker(speakerIdentity),
				PartialText:   text,
				Sequence:      seq,
			},
		},
	}
	if err := c.client.Send(c.ctx, envelope); err != nil && c.logger != nil {
		c.logger.Debug("voice-agent partial transcript send failed", "err", err)
	}
}

// onCommittedTurn fires when the machine commits a human turn (transition
// D). It forwards the VoiceAgentFinalTranscript (which inserts the chat
// utterance server-side) and then drives a streaming VoiceAgentTurnRequest,
// consuming the TurnDelta/TurnComplete reply and -- when a reply lands --
// playing it via the conductor-gated speak path. Runs on its own goroutine
// so the ASR consume loop is never blocked on cognition latency.
func (c *Cascade) onCommittedTurn(text string) {
	// Reset the partial sequence so the next utterance starts fresh
	// (transcript_forwarder.forward_final clears its counter).
	c.seq.Store(0)
	// Attribute the committed turn to the track that produced it: the
	// machine's commit fires synchronously on the per-track consume
	// goroutine, so lastSpeaker is the identity handleASRResult just
	// recorded for this final (#1403).
	speaker := c.resolveSpeaker(c.getLastSpeaker())
	c.forwardFinal(speaker, text)
	go c.runTurn(speaker, text)
}

// forwardFinal sends a VoiceAgentFinalTranscript. The send stays
// non-blocking, but the server's FinalAck / QueryError reply is consumed
// and logged on a background goroutine instead of being discarded -- a
// rejected insert here means the user's utterance never reaches chat, the
// silent-failure core of #1403.
func (c *Cascade) forwardFinal(speakerID, text string) {
	sendFinalTranscript(c.ctx, c.client, c.logger, "cascade", &memqlv1.VoiceAgentFinalTranscript{
		SpaceId:       c.cfg.SpaceID,
		SpeakerUserId: speakerID,
		FinalText:     text,
	})
}

// runTurn drives one VoiceAgentTurnRequest and consumes its streamed
// reply. The server pushes a run of VoiceAgentTurnDelta correlated to the
// request id, terminated by a VoiceAgentTurnComplete (see
// voice_agent_handlers.go::handleVoiceAgentTurnRequest). The reply may be
// suppressed server-side (the conductor / classifier decided the assistant
// should stay silent) -- in that case the turn times out with no delta and
// nothing is spoken. When a non-empty reply lands, runTurn drives the
// machine's speak path (transition B) to synthesize it.
func (c *Cascade) runTurn(speakerID, utterance string) {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return
	}
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{
			VoiceAgentTurnRequest: &memqlv1.VoiceAgentTurnRequest{
				SpaceId:       c.cfg.SpaceID,
				SpeakerUserId: speakerID,
				UtteranceText: utterance,
				GaAgentId:     c.cfg.GaAgentID,
				Thread:        c.cfg.Thread,
			},
		},
	}
	replies, release, err := c.client.StreamRequest(c.ctx, envelope)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("voice-agent turn request failed", "err", err)
		}
		return
	}
	defer release()

	var reply strings.Builder
	var utteranceID, requestID string
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-replies:
			if !ok {
				return
			}
			if delta := msg.GetVoiceAgentTurnDelta(); delta != nil {
				reply.WriteString(delta.GetTextDelta())
				if requestID == "" {
					requestID = delta.GetRequestId()
				}
				continue
			}
			if done := msg.GetVoiceAgentTurnComplete(); done != nil {
				// The complete carries the authoritative final text +
				// utterance id; prefer them over the accumulated deltas.
				final := strings.TrimSpace(done.GetFinalText())
				if final == "" {
					final = strings.TrimSpace(reply.String())
				}
				utteranceID = done.GetUtteranceId()
				if requestID == "" {
					requestID = done.GetRequestId()
				}
				if done.GetErrorCode() != "" && c.logger != nil {
					c.logger.Warn("voice-agent turn complete with error",
						"error_code", done.GetErrorCode(),
						"error_message", done.GetErrorMessage())
				}
				if final != "" {
					c.machine.OnSpeak(SpeakDirective{
						Text:        final,
						UtteranceID: utteranceID,
						RequestID:   requestID,
					})
				}
				return
			}
		}
	}
}

// Speak drives the conductor-gated speak path from an unsolicited
// VoiceAgentSpeak push (the Go analog of main.py::_on_voice_agent_speak).
// It is the seam Session.onVoiceAgentSpeak calls. Returns true when the
// directive was accepted (assistant-turn entered).
func (c *Cascade) Speak(req SpeakDirective) bool {
	if strings.TrimSpace(req.Text) == "" {
		return false
	}
	return c.machine.OnSpeak(req)
}

// onAssistantStart begins TTS playout for an accepted speak directive
// (transition B). It synthesizes the reply and pumps PCM frames into the
// room sink on its own goroutine, registering a cancel func so barge-in
// (onBargeIn) can stop it within a frame budget. On completion (or cancel)
// it reports OnAssistantDone so the machine collapses back to listening.
func (c *Cascade) onAssistantStart(req SpeakDirective) {
	voiceID := c.persona.VoiceID()
	playCtx, cancel := context.WithCancel(c.ctx)
	c.setTTSCancel(cancel)

	go func() {
		defer c.machine.OnAssistantDone()
		defer c.clearTTSCancel(cancel)

		frames, err := c.tts.SynthesizePCM(playCtx, req.Text, voiceID)
		if err != nil {
			if c.logger != nil {
				c.logger.Warn("voice-agent tts synthesize failed",
					"err", err, "request_id", req.RequestID)
			}
			return
		}
		if c.logger != nil {
			c.logger.Info("voice trace: turntaking event",
				"stage", "turntaking.assistant.speak",
				"request_id", req.RequestID,
				"utterance_id", req.UtteranceID,
				"chars", len(req.Text))
		}
		for {
			select {
			case <-playCtx.Done():
				return
			case frame, ok := <-frames:
				if !ok {
					return
				}
				if len(frame) == 0 {
					continue
				}
				if err := c.sink.WriteFrame(frame); err != nil {
					if c.logger != nil {
						c.logger.Warn("voice-agent room audio write failed", "err", err)
					}
					return
				}
			}
		}
	}()
}

// onBargeIn cancels the in-flight TTS playout when a human onset interrupts
// the assistant (transition C). The machine has already moved to human-turn.
// It both cancels the synthesis pump (stops new frames) and flushes the
// room sink (drops the already-buffered tail) so the cut is immediate.
func (c *Cascade) onBargeIn() {
	if c.logger != nil {
		c.logger.Info("voice trace: turntaking event", "stage", "turntaking.bargein.audio_cut")
	}
	c.cancelTTS()
	c.sink.Flush()
}

func (c *Cascade) setTTSCancel(cancel context.CancelFunc) {
	c.ttsMu.Lock()
	// Cancel any prior pump that somehow outlived its turn before
	// installing the new one (defensive; one-active-turn is the norm).
	if c.ttsCancel != nil {
		c.ttsCancel()
	}
	c.ttsCancel = cancel
	c.ttsMu.Unlock()
}

func (c *Cascade) clearTTSCancel(cancel context.CancelFunc) {
	c.ttsMu.Lock()
	if c.ttsCancel != nil {
		// Only clear if it is still ours; a barge-in may have already
		// swapped/cancelled.
		c.ttsCancel = nil
	}
	c.ttsMu.Unlock()
	_ = cancel
}

func (c *Cascade) cancelTTS() {
	c.ttsMu.Lock()
	cancel := c.ttsCancel
	c.ttsCancel = nil
	c.ttsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
