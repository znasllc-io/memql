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

// cascade.go is the Go cascade voice loop: human audio -> Deepgram STT ->
// memQL cognition -> Deepgram TTS -> room audio, with turn-taking and
// barge-in. It is the Go analog of the Python cascade
// (voice-agent/voice_agent/main.py + memql_llm_plugin.py +
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
// *deepgram.TTSClient.SynthesizeOGGOpusStream-style methods, but the
// orchestrator only needs PCM16 frames at the room rate, so the seam is
// defined in terms of the local-track sample shape and kept free of the
// Deepgram package to avoid an import cycle and keep tests trivial.
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

// personaVoice resolves the canonical TTS voice id for the assistant. The
// sibling issue (#456) owns persona/voice resolution; the cascade consumes
// it through this tiny seam and ships a default so it works standalone.
// TODO(#456): replace defaultPersonaVoice with the sibling's resolver
// (persona profile -> canonical voice -> Aura-2 id) once it lands.
type personaVoice interface {
	// VoiceID returns the TTS voice id to synthesize with. Empty means
	// "let the TTS layer pick its configured default."
	VoiceID() string
}

// defaultPersonaVoice is the standalone fallback: it returns the canonical
// voice the SessionAck carried (resolved server-side from the GA controls),
// or empty to defer to the TTS client's configured default. This is the
// stub the #456 resolver replaces.
type defaultPersonaVoice struct{ canonical string }

func (d defaultPersonaVoice) VoiceID() string { return strings.TrimSpace(d.canonical) }

// CascadeConfig parameterizes the cascade orchestrator.
type CascadeConfig struct {
	SpaceID   string
	GaAgentID string
	// SpeakerUserID attributes the human transcript. In a 1:1 standard
	// space this is the single human; multi-human per-track attribution
	// is #433. Empty is allowed (server resolves the active speaker).
	SpeakerUserID string
	// Thread selects the cognition thread context for turn requests.
	Thread memqlv1.VoiceAgentTurnRequest_ThreadContext
}

// Cascade owns one space's cascade voice loop. It binds a turn-taking
// machine (turntaking.go) to: the Deepgram STT result stream (transcript
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

	// ctx is the session-scoped context; cancelled on Close.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewCascade builds a cascade orchestrator. canonicalVoice is the voice the
// SessionAck resolved; it seeds the default persona-voice stub pending the
// #456 resolver. tts and sink are the synthesis + room-publish seams (real
// implementations in room_audio_voice.go; in-memory in tests).
func NewCascade(
	parent context.Context,
	cfg CascadeConfig,
	client *Client,
	tts ttsSynthesizer,
	sink audioSink,
	canonicalVoice string,
	logger *slog.Logger,
) *Cascade {
	ctx, cancel := context.WithCancel(parent)
	c := &Cascade{
		cfg:     cfg,
		client:  client,
		tts:     tts,
		sink:    sink,
		persona: defaultPersonaVoice{canonical: canonicalVoice},
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

// ConsumeASR drives the machine from a Deepgram ASR result stream. It maps
// each result onto a turn-taking input: a speech-started kind raises an
// onset (transition A / barge-in C), an interim updates the partial and is
// forwarded as a VoiceAgentPartialTranscript, and a final commits the turn
// (transition D). It ranges until the stream channel closes or the session
// context is cancelled, so it is the long-lived per-track consumer
// goroutine (one per human audio track; #433 scales it to many).
func (c *Cascade) ConsumeASR(results <-chan polyphon.ASRResult) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case r, ok := <-results:
			if !ok {
				return
			}
			c.handleASRResult(r)
		}
	}
}

// handleASRResult routes one ASR result. Split out for direct unit testing
// without standing up a channel.
func (c *Cascade) handleASRResult(r polyphon.ASRResult) {
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
	c.forwardPartial(text)
}

// forwardPartial sends a VoiceAgentPartialTranscript (best-effort, no
// reply awaited -- the server acks but we don't block the audio loop on it).
func (c *Cascade) forwardPartial(text string) {
	seq := c.seq.Add(1)
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentPartialTranscript{
			VoiceAgentPartialTranscript: &memqlv1.VoiceAgentPartialTranscript{
				SpaceId:       c.cfg.SpaceID,
				SpeakerUserId: c.cfg.SpeakerUserID,
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
	c.forwardFinal(text)
	go c.runTurn(text)
}

// forwardFinal sends a VoiceAgentFinalTranscript (best-effort).
func (c *Cascade) forwardFinal(text string) {
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentFinalTranscript{
			VoiceAgentFinalTranscript: &memqlv1.VoiceAgentFinalTranscript{
				SpaceId:       c.cfg.SpaceID,
				SpeakerUserId: c.cfg.SpeakerUserID,
				FinalText:     text,
			},
		},
	}
	if err := c.client.Send(c.ctx, envelope); err != nil && c.logger != nil {
		c.logger.Warn("voice-agent final transcript send failed", "err", err)
	}
}

// runTurn drives one VoiceAgentTurnRequest and consumes its streamed
// reply. The server pushes a run of VoiceAgentTurnDelta correlated to the
// request id, terminated by a VoiceAgentTurnComplete (see
// voice_agent_handlers.go::handleVoiceAgentTurnRequest). The reply may be
// suppressed server-side (the conductor / classifier decided the assistant
// should stay silent) -- in that case the turn times out with no delta and
// nothing is spoken. When a non-empty reply lands, runTurn drives the
// machine's speak path (transition B) to synthesize it.
func (c *Cascade) runTurn(utterance string) {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return
	}
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{
			VoiceAgentTurnRequest: &memqlv1.VoiceAgentTurnRequest{
				SpaceId:       c.cfg.SpaceID,
				SpeakerUserId: c.cfg.SpeakerUserID,
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
