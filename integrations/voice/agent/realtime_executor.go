package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/openai"
)

// realtime_executor.go is the Go realtime voice loop: it slots BEHIND THE SAME
// SEAM as the cascade (cascade.go) -- same turn-taking machine, same gRPC
// VoiceAgent* contract, same SpeakSink registration -- but drives the OpenAI
// gpt-realtime speech-to-speech model instead of the Deepgram STT->cognition->
// Deepgram TTS pipeline. It is the Go analog of
// the Python voice-agent's realtime_executor.py + the realtime branch of
// main.py, and implements the merged designs in
// docs/internal/design/voice-432-conductor-response-gate.md (conductor gate) and
// docs/internal/design/voice-433-multiparty-audio-routing.md (multi-party routing).
//
// It is pure Go and CGO-free: it depends only on small interfaces (the
// realtimeSession seam below, the gRPC Client, the audioSink). The websocket
// client (integrations/openai/realtime.go) is itself CGO-free; the room
// audio-publish glue is the only voice-tagged code. So the conductor gate
// (response.create on engage, suppress on defer, cancel on barge-in) and the
// multi-party labeled-item injection are unit-testable in the default CI lane.
//
// Conductor gate (the heart, #432). The executor reuses the EXACT decision the
// cascade reuses: a human final transcript drives a VoiceAgentTurnRequest; the
// server's VoiceAgentTurnComplete carries the conductor's decision -- non-empty
// final text means "the assistant should speak" (engage), empty means the
// conductor/classifier suppressed the turn (defer/silence). On engage the
// executor drives exactly one realtime response.create with a per-response
// instructions directive; on suppress it sends nothing. There is exactly one
// response.create emitter and it is gated on the conductor's decision -- the
// model never self-triggers (turn_detection:null on the session). Barge-in
// reuses the turn-taking machine's OnBargeIn signal (transition C) and issues
// response.cancel + output_audio_buffer.clear.
//
// Multi-party routing (#433). Each human's final transcript is injected into
// the realtime session as a labeled conversation.item ("[name . role] text")
// so the model can attribute statements to the right speaker even when it never
// heard their audio. The active-speaker PCM is streamed to the model
// (SendAudio) for prosody + barge-in; under turn_detection:null this never
// auto-triggers a response.

// realtimeSession is the executor's seam onto the gpt-realtime websocket
// client (integrations/openai.RealtimeSession). Defined locally so the executor
// is testable with a mock and so the room-publish glue stays the only thing
// importing the live client. It is the wider-vocabulary analog of the cascade's
// ttsSynthesizer seam.
type realtimeSession interface {
	// SendAudio streams one PCM16 16 kHz active-speaker chunk to the model.
	SendAudio(pcm16k []byte) error
	// CommitInput commits the buffered input audio as a user item (no response).
	CommitInput() error
	// InjectItem injects a labeled conversation item (multi-party / grounding).
	InjectItem(item openai.ConversationItem) error
	// CreateResponse drives one response.create (the conductor gate engage).
	CreateResponse(instructions string) error
	// CancelResponse cancels the in-flight response + clears output audio.
	CancelResponse() error
	// SendFunctionResult returns a tool result by call_id (the MCP tool bridge
	// async function-call path, #458). It does NOT itself create a response --
	// the executor chains CreateResponse so the model continues with the result.
	SendFunctionResult(callID, output string) error
	// AudioOut is the decoded PCM16 (24 kHz) output-audio channel.
	AudioOut() <-chan []byte
	// Events is the transcript / function-call / lifecycle event channel.
	Events() <-chan openai.RealtimeServerEvent
	// Close tears down the session.
	Close() error
}

// RealtimeExecutor owns one space's realtime voice loop. It binds a turn-taking
// machine (turntaking.go) to the realtime session and the gRPC client, mirroring
// the Cascade's structure so it registers as a SpeakSink and consumes ASR
// results through the same surface.
type RealtimeExecutor struct {
	cfg     CascadeConfig
	client  *Client
	session realtimeSession
	sink    audioSink
	persona SessionPersona
	roster  *participantRoster
	logger  *slog.Logger

	// outputForwarder captures the model's final spoken transcript and forwards
	// it as a VoiceAgentRealtimeOutput so chat/canvas/audit render the realtime
	// turn with citations (#458). Nil disables capture (voice still plays; chat
	// goes dark, same as the pre-#458 state).
	outputForwarder *RealtimeOutputForwarder
	// toolBridge dispatches model-driven function calls through the low-risk MCP
	// tool surface and mirrors them into cognition (#458). Nil disables
	// model-driven tools (a function-call event is logged and ignored).
	toolBridge *McpToolBridge

	machine *TurnMachine

	// lifecycle bounds this session's cost (empty-room / idle / max-duration /
	// token-budget teardown -> degrade to cascade, #459). Optional: nil when no
	// guardrails are wired. Fed NoteEngaged on a conductor engage and
	// NoteAudioTokens on each completed response; cancelled on Close. See
	// realtime_lifecycle.go.
	lifecycle *RealtimeSessionLifecycle

	seq atomic.Int64

	// turnMode is the realtime turn-detection / gating mode, switchable at
	// runtime by the room layer. One pipeline, one optional gate (#475):
	//   - turnModeGatedCascade: turn_detection:null, Deepgram drives the turn
	//     machine, the conductor gate runs via runTurn (the pre-#481 multi-party
	//     path, still the default when the multi-party semantic_vad flag is off).
	//   - turnModeNative (#478): semantic_vad + create_response:true, gpt-realtime
	//     owns the turn, no conductor; the human transcript comes from the model's
	//     native input transcription, not Deepgram. 1-on-1.
	//   - turnModeGatedSemanticVad (#481): semantic_vad + create_response:false,
	//     the model detects turn-end + transcribes the active speaker but does NOT
	//     auto-generate; the gate decides (runTurn) and the executor fires
	//     CreateResponse on engage. Deepgram is used ONLY for per-speaker
	//     attribution (labeled-item injection), not to drive turns. Multi-party.
	turnMode atomic.Int32

	// firstAudioPending is set when a response is created and cleared on its
	// first audio frame, so drainAudioOut can stamp the T3 "realtime.audio.first"
	// voice-trace exactly once per response (the decision->first-audio
	// measurement, #484).
	firstAudioPending atomic.Bool

	// turnSpeechStopNanos is the wall-clock (UnixNano) of the model's most recent
	// input_audio_buffer.speech_stopped -- when server-side VAD decided the human
	// finished. drainAudioOut reads it to log the end-of-speech -> first-assistant-
	// audio latency (the snappiness metric) on the next response's first frame.
	turnSpeechStopNanos atomic.Int64

	// respMu guards the in-flight output-pump cancel + the response-in-flight
	// flag so barge-in can stop playout without racing a new response.
	respMu     sync.Mutex
	pumpCancel context.CancelFunc
	inFlight   bool

	// lastSpeaker is the per-track identity of the human whose final
	// transcript most recently committed a turn. Captured at OnFinal time so
	// onCommittedTurn (which the machine invokes with only the text) can stamp
	// the correct speaker on the VoiceAgentTurnRequest in a multi-human room
	// (#433). Guarded since the ASR consume goroutine writes it and the turn
	// goroutine reads it.
	speakerMu   sync.Mutex
	lastSpeaker string

	// transcriptCh carries human-transcript forwards (partials + finals) to the
	// dedicated forwardTranscriptLoop goroutine, so the realtime event loop never
	// blocks on a gRPC send to insert the chat utterance. Buffered + drop-on-full.
	transcriptCh chan transcriptJob

	ctx    context.Context
	cancel context.CancelFunc
}

// NewRealtimeExecutor builds a realtime executor. persona is the resolved
// static session persona (#456, BuildSessionPersona) the websocket client was
// configured with; session is the live realtime session seam; sink is the
// room-publish seam (real PCMLocalTrack in the voice build, in-memory in tests).
func NewRealtimeExecutor(
	parent context.Context,
	cfg CascadeConfig,
	client *Client,
	session realtimeSession,
	sink audioSink,
	persona SessionPersona,
	logger *slog.Logger,
) *RealtimeExecutor {
	ctx, cancel := context.WithCancel(parent)
	e := &RealtimeExecutor{
		cfg:          cfg,
		client:       client,
		session:      session,
		sink:         sink,
		persona:      persona,
		roster:       newParticipantRoster(),
		logger:       logger,
		ctx:          ctx,
		cancel:       cancel,
		transcriptCh: make(chan transcriptJob, transcriptQueueDepth),
	}
	e.machine = NewTurnMachine(TurnCallbacks{
		OnFinalTranscript: e.onCommittedTurn,
		OnAssistantStart:  e.onAssistantStart,
		OnBargeIn:         e.onBargeIn,
	}, logger)
	return e
}

// Start moves the machine to listening and begins draining the model's
// output-audio channel into the room sink. Call once the room is joined and
// the session is configured.
func (e *RealtimeExecutor) Start() {
	e.machine.Start()
	go e.drainAudioOut()
	go e.drainEvents()
	go e.forwardTranscriptLoop()
}

// transcriptQueueDepth bounds the human-transcript forward queue. Sized for a
// full turn's burst of input-transcription tokens so a final is never dropped
// in practice; on overflow enqueueTranscript drops rather than stalling the
// conversation loop.
const transcriptQueueDepth = 256

// transcriptJob is one human-transcript forward (a streamed partial or a
// committed final), carried off the realtime event loop to the dedicated
// forward goroutine so a slow gRPC send can never delay audio / barge-in /
// response lifecycle.
type transcriptJob struct {
	speaker string
	text    string
	final   bool
	native  bool
}

// forwardTranscriptLoop is the parallel goroutine that actually sends the human
// transcript to memQL. It owns ALL forwardPartial/forwardFinal blocking, so the
// realtime conversation path (drainEvents) never waits on the chat-utterance
// insert -- the human transcript is a best-effort, parallel concern (exactly the
// "utterances run in their own goroutine, added to chat later" contract).
func (e *RealtimeExecutor) forwardTranscriptLoop() {
	for {
		select {
		case <-e.ctx.Done():
			return
		case j := <-e.transcriptCh:
			if j.final {
				e.forwardFinal(j.speaker, j.text, j.native)
			} else {
				e.forwardPartial(j.speaker, j.text)
			}
		}
	}
}

// enqueueTranscript hands a transcript forward to the parallel loop without ever
// blocking the caller. On a full queue (sustained gRPC backpressure) it drops
// the update: a dropped partial is invisible (cosmetic ghost-text) and a dropped
// final only delays the chat row, never the voice conversation.
func (e *RealtimeExecutor) enqueueTranscript(j transcriptJob) {
	select {
	case e.transcriptCh <- j:
	default:
		if e.logger != nil {
			e.logger.Debug("voice-agent realtime: transcript forward dropped (backpressure)",
				"final", j.final, "chars", len(j.text))
		}
	}
}

// Close tears down the executor: cancels any in-flight playout, stops the
// machine, closes the session. Idempotent.
func (e *RealtimeExecutor) Close() {
	e.cancelPump()
	e.machine.Stop()
	if e.lifecycle != nil {
		e.lifecycle.Close()
	}
	_ = e.session.Close()
	e.cancel()
}

// Machine exposes the turn-taking machine (tests / diagnostics).
func (e *RealtimeExecutor) Machine() *TurnMachine { return e.machine }

// AttachLifecycle wires the cost-guardrail lifecycle (#459) onto the executor so
// conductor engagements feed the idle clock and completed-response audio-token
// usage feeds the per-session token budget. The room layer (room_realtime_voice.go,
// voice-tagged) builds the lifecycle with the room teardown as its stop callback
// and forwards participant join/leave to it directly; this attach is the
// executor-internal feed for engage + token usage. Call before Start. Optional --
// when unset the executor runs without guardrails (the existing behaviour).
func (e *RealtimeExecutor) AttachLifecycle(l *RealtimeSessionLifecycle) { e.lifecycle = l }

// SetParticipant registers a participant's display name + role so the
// multi-party labeled-item injection can prefix their transcripts with
// "[name . role]" (#433 section 3). Safe for concurrent use.
func (e *RealtimeExecutor) SetParticipant(identity, displayName, role string) {
	e.roster.set(identity, displayName, role)
}

// SetOutputForwarder attaches the realtime output capture (#458). Call before
// Start (the drain loop reads it). Nil leaves capture disabled.
func (e *RealtimeExecutor) SetOutputForwarder(f *RealtimeOutputForwarder) {
	e.outputForwarder = f
}

// SetToolBridge attaches the MCP tool bridge (#458). Call before Start (the
// drain loop reads it). Nil leaves model-driven tools disabled.
func (e *RealtimeExecutor) SetToolBridge(b *McpToolBridge) {
	e.toolBridge = b
}

// Turn-mode values for SetTurnMode (see the turnMode field doc).
const (
	turnModeGatedCascade     int32 = iota // null + Deepgram + conductor gate (default multi-party)
	turnModeNative                        // semantic_vad + create_response:true (1-on-1, #478)
	turnModeGatedSemanticVad              // semantic_vad + create_response:false + gate (multi-party, #481)
)

// SetTurnMode sets the realtime turn-detection / gating mode. The room layer
// drives this from the live human count + the deploy flags and pairs it with a
// session.update that flips turn_detection / create_response. Safe for
// concurrent use.
func (e *RealtimeExecutor) SetTurnMode(mode int32) {
	e.turnMode.Store(mode)
	if e.logger != nil {
		names := map[int32]string{
			turnModeGatedCascade:     "gated-cascade (null + Deepgram + conductor)",
			turnModeNative:           "native (model-owns-turn)",
			turnModeGatedSemanticVad: "gated-semantic_vad (model turn-end + conductor gate)",
		}
		e.logger.Info("voice-agent realtime: turn mode set", "mode", names[mode])
	}
}

// isNativeMode reports whether the 1-on-1 native gate (#478) is active.
func (e *RealtimeExecutor) isNativeMode() bool { return e.turnMode.Load() == turnModeNative }

// isGatedSemanticVad reports whether the multi-party semantic_vad gate (#481)
// is active.
func (e *RealtimeExecutor) isGatedSemanticVad() bool {
	return e.turnMode.Load() == turnModeGatedSemanticVad
}

// ConsumeASR drives the machine from one human track's Deepgram ASR result
// stream (one goroutine per human track -- the multi-party fan-out, #433). It
// maps each result onto a turn-taking input exactly as the cascade does, and
// additionally streams the active-speaker audio path is wired by the room glue
// via SendAudio (not here -- this consumes the per-track STT result stream).
func (e *RealtimeExecutor) ConsumeASR(speakerIdentity string, results <-chan polyphon.ASRResult) {
	for {
		select {
		case <-e.ctx.Done():
			return
		case r, ok := <-results:
			if !ok {
				return
			}
			e.handleASRResult(speakerIdentity, r)
		}
	}
}

// StreamAudio appends one active-speaker PCM16 16 kHz chunk to the realtime
// input buffer (the prosody + barge-in audio path, #433 section 2b). The room
// glue calls this for the LiveKit-reported active human's decoded frames. Under
// turn_detection:null it never auto-triggers a response.
func (e *RealtimeExecutor) StreamAudio(pcm16k []byte) {
	if len(pcm16k) == 0 {
		return
	}
	if err := e.session.SendAudio(pcm16k); err != nil && e.logger != nil {
		e.logger.Debug("voice-agent realtime: send audio failed", "err", err)
	}
}

// handleASRResult routes one ASR result for a given speaker. Split out for
// direct unit testing.
func (e *RealtimeExecutor) handleASRResult(speakerIdentity string, r polyphon.ASRResult) {
	// Track the active speaker on EVERY result, before any mode-specific return.
	// Native mode does not consume Deepgram for turn-taking, but the native input
	// transcript (EventInputTranscriptDone) carries no speaker of its own -- it
	// reads getLastSpeaker() to attribute the user utterance. Without this the
	// forwarded VoiceAgentFinalTranscript has an empty speaker id, the BFF rejects
	// the insert (InvalidArgument), and the human's turn never reaches chat (the
	// "assistant replies but my utterances don't show" symptom).
	if id := strings.TrimSpace(speakerIdentity); id != "" {
		e.setLastSpeaker(id)
	}
	// Native 1-on-1 mode: gpt-realtime transcribes the human, detects the turn,
	// authors + speaks, and handles barge-in -- so Deepgram results are not
	// consumed for turn-taking (Deepgram stays warm only for the cascade fallback
	// and to seed the speaker id above). The only executor turn input on the
	// native path is StreamAudio (PCM -> model) + the native transcript events.
	if e.isNativeMode() {
		return
	}
	// Multi-party semantic_vad gate (#481): the model detects turn-end and
	// transcribes the active speaker, so Deepgram does NOT drive the turn here.
	// But Deepgram per-track ASR is still the attribution source -- inject each
	// speaker's labeled final as context ("[name . role] text") so the model can
	// attribute ("as Maria said..."), and track the last speaker for the turn's
	// speaker id. We do not touch the turn machine in this mode.
	if e.isGatedSemanticVad() {
		if r.IsFinal {
			if text := strings.TrimSpace(r.Text); text != "" {
				e.injectLabeledTranscript(speakerIdentity, text)
				e.setLastSpeaker(speakerIdentity)
			}
		}
		return
	}
	if r.Kind == polyphon.ASRKindSpeechStarted {
		e.machine.OnSpeechStarted()
		return
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return
	}
	if r.IsFinal {
		// Inject the labeled conversation item BEFORE committing the turn so
		// the model has the attributed text in context regardless of whether
		// it heard this speaker's audio (#433 section 2a). The turn machine
		// then commits the human turn (transition D) -> onCommittedTurn drives
		// the VoiceAgentTurnRequest + the conductor gate.
		e.injectLabeledTranscript(speakerIdentity, text)
		e.setLastSpeaker(speakerIdentity)
		e.machine.OnFinal(text)
		return
	}
	e.machine.OnInterim(text)
	e.forwardPartial(speakerIdentity, text)
}

// injectLabeledTranscript injects a human final as a labeled user
// conversation.item ("[name . role] text") so the model can attribute it
// (#433). The input audio buffer also carries the active speaker's audio; this
// is the text channel that makes "as Maria mentioned..." possible.
func (e *RealtimeExecutor) injectLabeledTranscript(speakerIdentity, text string) {
	label := e.roster.label(speakerIdentity)
	labeled := text
	if label != "" {
		labeled = "[" + label + "] " + text
	}
	if err := e.session.InjectItem(openai.ConversationItem{Role: "user", Text: labeled}); err != nil && e.logger != nil {
		e.logger.Debug("voice-agent realtime: inject labeled transcript failed", "err", err)
	}
}

// forwardPartial sends a VoiceAgentPartialTranscript (best-effort), identical
// to the cascade so the live chat transcript renders the same on both paths.
func (e *RealtimeExecutor) forwardPartial(speakerIdentity, text string) {
	seq := e.seq.Add(1)
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentPartialTranscript{
			VoiceAgentPartialTranscript: &memqlv1.VoiceAgentPartialTranscript{
				SpaceId:       e.cfg.SpaceID,
				SpeakerUserId: e.resolveSpeaker(speakerIdentity),
				PartialText:   text,
				Sequence:      seq,
			},
		},
	}
	if err := e.client.Send(e.ctx, envelope); err != nil && e.logger != nil {
		e.logger.Debug("voice-agent realtime: partial transcript send failed", "err", err)
	}
}

// onCommittedTurn fires when the machine commits a human turn (transition D).
// It forwards the VoiceAgentFinalTranscript (inserts the chat utterance +
// fires the cognition automation, exactly as the cascade does) and drives a
// streaming VoiceAgentTurnRequest. The TurnRequest reply IS the conductor gate:
// a non-empty VoiceAgentTurnComplete final text means engage, empty means
// suppress. Runs on its own goroutine so the ASR consume loop is never blocked.
func (e *RealtimeExecutor) onCommittedTurn(text string) {
	e.seq.Store(0)
	// Native 1-on-1 mode: gpt-realtime owns the turn -- it detects end-of-turn
	// (semantic_vad), authors + speaks natively, and the human transcript is
	// captured from the model's native input-transcription events (drainEvents),
	// not this Deepgram-driven commit. So we neither forward this final (the
	// native path inserts the user utterance, stamped transcript-only) nor
	// round-trip the conductor. Guarded defensively even though the room layer
	// also stops feeding Deepgram finals into the machine in native mode.
	// turnModeGatedSemanticVad (#481) likewise drives the turn from the model's
	// turn-end (EventInputTranscriptDone), not this Deepgram commit.
	if e.isNativeMode() || e.isGatedSemanticVad() {
		return
	}
	speaker := e.getLastSpeaker()
	e.forwardFinal(speaker, text, false)
	go e.runTurn(speaker, text)
}

// SetActiveSpeaker seeds the active speaker id from the room layer (the LiveKit
// participant identity), used when Deepgram is off the path so the native input
// transcript still attributes the user utterance. Public seam for the room glue.
func (e *RealtimeExecutor) SetActiveSpeaker(identity string) {
	if id := strings.TrimSpace(identity); id != "" {
		e.setLastSpeaker(id)
	}
}

func (e *RealtimeExecutor) setLastSpeaker(identity string) {
	e.speakerMu.Lock()
	e.lastSpeaker = identity
	e.speakerMu.Unlock()
}

func (e *RealtimeExecutor) getLastSpeaker() string {
	e.speakerMu.Lock()
	defer e.speakerMu.Unlock()
	return e.lastSpeaker
}

// forwardFinal sends a VoiceAgentFinalTranscript (best-effort). nativeAuthored
// marks the #478 native path: the realtime model already authored AND spoke the
// reply, so the server stamps this user utterance transcript-only and cognition
// does not author a second reply.
func (e *RealtimeExecutor) forwardFinal(speakerIdentity, text string, nativeAuthored bool) {
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentFinalTranscript{
			VoiceAgentFinalTranscript: &memqlv1.VoiceAgentFinalTranscript{
				SpaceId:        e.cfg.SpaceID,
				SpeakerUserId:  e.resolveSpeaker(speakerIdentity),
				FinalText:      text,
				NativeAuthored: nativeAuthored,
			},
		},
	}
	if err := e.client.Send(e.ctx, envelope); err != nil && e.logger != nil {
		e.logger.Warn("voice-agent realtime: final transcript send failed", "err", err)
	}
	// T0 (#484): the human turn is committed. The headline decision->first-audio
	// window opens here; see docs/internal/design/voice-484-latency-fidelity-measurement.md.
	if e.logger != nil {
		e.logger.Info("voice trace: turntaking event",
			"stage", "voice.final", "executor", "realtime",
			"space_id", e.cfg.SpaceID, "native_authored", nativeAuthored, "chars", len(text))
	}
}

// runTurn drives one VoiceAgentTurnRequest and consumes its streamed reply --
// the conductor gate. The server pushes VoiceAgentTurnDelta + a terminal
// VoiceAgentTurnComplete; the complete's final text carries the conductor's
// decision (non-empty = engage, empty = suppress). On engage it commits the
// input buffer, renders the per-response directive, and drives the machine's
// speak path (transition B), which calls CreateResponse. On suppress nothing
// is spoken -- the model never self-triggers.
func (e *RealtimeExecutor) runTurn(speakerIdentity, utterance string) {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return
	}
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{
			VoiceAgentTurnRequest: &memqlv1.VoiceAgentTurnRequest{
				SpaceId:       e.cfg.SpaceID,
				SpeakerUserId: e.resolveSpeaker(speakerIdentity),
				UtteranceText: utterance,
				GaAgentId:     e.cfg.GaAgentID,
				Thread:        e.cfg.Thread,
			},
		},
	}
	replies, release, err := e.client.StreamRequest(e.ctx, envelope)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("voice-agent realtime: turn request failed", "err", err)
		}
		return
	}
	defer release()

	var reply strings.Builder
	var utteranceID, requestID string
	for {
		select {
		case <-e.ctx.Done():
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
				final := strings.TrimSpace(done.GetFinalText())
				if final == "" {
					final = strings.TrimSpace(reply.String())
				}
				utteranceID = done.GetUtteranceId()
				if requestID == "" {
					requestID = done.GetRequestId()
				}
				if done.GetErrorCode() != "" && e.logger != nil {
					e.logger.Warn("voice-agent realtime: turn complete with error",
						"error_code", done.GetErrorCode(),
						"error_message", done.GetErrorMessage())
				}
				// Gate path (#477/#479): when the conductor sent a directive_mode
				// it decided WHEN + brevity and the MODEL authors WHAT. "defer"
				// suppresses; any other mode engages with a content-free directive
				// (no authored text). When directive_mode is empty we take the
				// legacy authored-text path below (model re-voices final_text).
				if directiveMode := strings.TrimSpace(done.GetDirectiveMode()); directiveMode != "" {
					if strings.EqualFold(directiveMode, "defer") {
						if e.logger != nil {
							e.logger.Debug("voice-agent realtime: conductor gate deferred (no response.create)")
						}
						return
					}
					e.machine.OnSpeak(SpeakDirective{
						UtteranceID: utteranceID,
						RequestID:   requestID,
						Mode:        directiveMode,
						Brevity:     strings.TrimSpace(done.GetBrevity()),
						Grounding:   done.GetGrounding(),
					})
					return
				}

				// Legacy conductor gate: empty final -> suppress (defer / silence)
				// -> emit NO response.create. Non-empty -> engage -> drive exactly
				// one response.create conveying the authored text.
				if final == "" {
					if e.logger != nil {
						e.logger.Debug("voice-agent realtime: conductor suppressed turn (no response.create)")
					}
					return
				}
				e.machine.OnSpeak(SpeakDirective{
					Text:        final,
					UtteranceID: utteranceID,
					RequestID:   requestID,
				})
				return
			}
		}
	}
}

// Speak drives the conductor-gated speak path from an unsolicited
// VoiceAgentSpeak push (the SpeakSink seam Session.onVoiceAgentSpeak calls).
// Returns true when the directive was accepted (assistant-turn entered).
func (e *RealtimeExecutor) Speak(req SpeakDirective) bool {
	if strings.TrimSpace(req.Text) == "" {
		return false
	}
	return e.machine.OnSpeak(req)
}

// onAssistantStart drives one realtime response.create for an accepted speak
// directive (transition B / conductor gate engage). It commits the input
// buffer (so the model's response is grounded in the active speaker's audio),
// renders the per-response instructions directive from the conductor's reply,
// and fires CreateResponse. The model then streams output audio (drained by
// drainAudioOut) and its transcript (drained by drainEvents). A pump-cancel is
// registered so barge-in (onBargeIn) can stop playout.
func (e *RealtimeExecutor) onAssistantStart(req SpeakDirective) {
	// Commit the buffered active-speaker audio as the user turn before the
	// response so the model answers with the latest prosody context.
	if err := e.session.CommitInput(); err != nil && e.logger != nil {
		e.logger.Debug("voice-agent realtime: commit input failed", "err", err)
	}

	// Conductor engage: this is the one response.create per "speak" decision
	// (the model never self-triggers under turn_detection:null), so it is the
	// canonical point to reset the idle clock on the cost-guardrail lifecycle
	// (#459) -- an engaged session is doing useful work and must not be
	// idle-reaped mid-conversation.
	if e.lifecycle != nil {
		e.lifecycle.NoteEngaged()
	}

	// Gate path (#479): a non-empty Mode means the conductor sent a directive,
	// so render the content-free mode+brevity directive and let the MODEL author
	// the reply. Otherwise convey the authored Text (the legacy re-voice path,
	// also used by the unsolicited VoiceAgentSpeak chat-reply push).
	instructions := RealtimeInstructionsForReply(req.Text)
	if strings.TrimSpace(req.Mode) != "" {
		instructions = RealtimeInstructionsForDirective(req.Mode, req.Brevity)
	}

	// Grounding (#490): inject the retrieved context as an out-of-band system
	// item BEFORE response.create so the model conditions its native generation
	// on it. Empty = no-op (grounding disabled / nothing retrieved).
	if g := strings.TrimSpace(req.Grounding); g != "" {
		if err := e.session.InjectItem(openai.ConversationItem{Role: "system", Text: g}); err != nil && e.logger != nil {
			e.logger.Debug("voice-agent realtime: grounding inject failed", "err", err)
		}
	}

	_, cancel := context.WithCancel(e.ctx)
	e.setInFlight(cancel)

	if e.logger != nil {
		e.logger.Info("voice trace: turntaking event",
			"stage", "turntaking.assistant.speak",
			"executor", "realtime",
			"request_id", req.RequestID,
			"utterance_id", req.UtteranceID,
			"chars", len(req.Text))
	}

	if err := e.session.CreateResponse(instructions); err != nil {
		if e.logger != nil {
			e.logger.Warn("voice-agent realtime: response.create failed", "err", err, "request_id", req.RequestID)
		}
		e.clearInFlight()
		e.machine.OnAssistantDone()
		return
	}
	// The response lifecycle (EventResponseDone) collapses assistant-turn back
	// to listening via drainEvents; the output-audio pump is the AudioOut
	// drain. We do NOT call OnAssistantDone here -- the response is in flight.
}

// onBargeIn cancels the in-flight realtime response when a human onset
// interrupts the assistant (transition C). It cancels the response (stops the
// model + clears its output buffer) and flushes the room sink so the already-
// published tail is dropped immediately.
func (e *RealtimeExecutor) onBargeIn() {
	if e.logger != nil {
		e.logger.Info("voice trace: turntaking event", "stage", "turntaking.bargein.audio_cut", "executor", "realtime")
	}
	if e.isInFlight() {
		if err := e.session.CancelResponse(); err != nil && e.logger != nil {
			e.logger.Warn("voice-agent realtime: response.cancel failed", "err", err)
		}
	}
	e.cancelPump()
	e.sink.Flush()
}

// drainAudioOut pumps the model's decoded PCM16 output frames into the room
// sink. It runs for the life of the session; the channel closes when the
// session ends.
func (e *RealtimeExecutor) drainAudioOut() {
	for {
		select {
		case <-e.ctx.Done():
			return
		case frame, ok := <-e.session.AudioOut():
			if !ok {
				return
			}
			if len(frame) == 0 {
				continue
			}
			// T3 (#484): first audio frame of a response -- the assistant is
			// audible. Log the end-of-speech -> first-audio latency (the snappiness
			// metric) using the most recent speech_stopped stamp, then clear it so a
			// multi-frame response only measures once.
			if e.firstAudioPending.CompareAndSwap(true, false) && e.logger != nil {
				if stop := e.turnSpeechStopNanos.Swap(0); stop > 0 {
					e.logger.Info("voice trace: turntaking event",
						"stage", "realtime.audio.first", "executor", "realtime", "space_id", e.cfg.SpaceID,
						"speech_stop_to_audio_ms", (time.Now().UnixNano()-stop)/1e6)
				} else {
					e.logger.Info("voice trace: turntaking event",
						"stage", "realtime.audio.first", "executor", "realtime", "space_id", e.cfg.SpaceID)
				}
			}
			if err := e.sink.WriteFrame(frame); err != nil {
				if e.logger != nil {
					e.logger.Warn("voice-agent realtime: room audio write failed", "err", err)
				}
				return
			}
		}
	}
}

// drainEvents consumes the non-audio event stream: response lifecycle (to clear
// the in-flight flag + collapse the turn), the captured transcript, and tool
// calls. Output capture (transcript -> VoiceAgentRealtimeOutput) and the MCP
// tool bridge are #458 -- the seams are marked below.
func (e *RealtimeExecutor) drainEvents() {
	var transcript strings.Builder
	// inputTranscript accumulates the model's native transcription of the
	// human's audio (#478 native mode), so the chat transcript comes from the
	// realtime model rather than a Deepgram pass on the critical path.
	var inputTranscript strings.Builder
	for {
		select {
		case <-e.ctx.Done():
			return
		case ev, ok := <-e.session.Events():
			if !ok {
				return
			}
			switch ev.Kind {
			case openai.EventInputSpeechStarted:
				// Observability: server-side VAD heard speech onset. Stamp nothing
				// yet (the snappiness window opens at speech_stopped); just trace it.
				if e.logger != nil {
					e.logger.Info("voice trace: turntaking event",
						"stage", "model.speech_started", "executor", "realtime", "space_id", e.cfg.SpaceID)
				}
			case openai.EventInputSpeechStopped:
				// Observability: server-side VAD decided the human finished. This
				// opens the end-of-speech -> first-assistant-audio window that
				// drainAudioOut closes on the next response's first frame. This delta
				// is the headline "snappiness" number with Deepgram off the path.
				e.turnSpeechStopNanos.Store(time.Now().UnixNano())
				if e.logger != nil {
					e.logger.Info("voice trace: turntaking event",
						"stage", "model.speech_stopped", "executor", "realtime", "space_id", e.cfg.SpaceID)
				}
			case openai.EventResponseCreated:
				e.respMu.Lock()
				e.inFlight = true
				e.respMu.Unlock()
				// Arm the T3 first-audio stamp for this response (#484).
				e.firstAudioPending.Store(true)
				if e.logger != nil {
					stage := "model.response_created"
					if stop := e.turnSpeechStopNanos.Load(); stop > 0 {
						e.logger.Info("voice trace: turntaking event",
							"stage", stage, "executor", "realtime", "space_id", e.cfg.SpaceID,
							"speech_stop_to_response_ms", (time.Now().UnixNano()-stop)/1e6)
					} else {
						e.logger.Info("voice trace: turntaking event",
							"stage", stage, "executor", "realtime", "space_id", e.cfg.SpaceID)
					}
				}
			case openai.EventResponseDone:
				e.clearInFlight()
				e.machine.OnAssistantDone()
				transcript.Reset()
				// Feed the completed response's audio-token usage to the cost
				// guardrail (#459). Crossing the per-session budget tears the
				// session down and flags degrade-to-cascade.
				if e.lifecycle != nil && ev.AudioTokens > 0 {
					e.lifecycle.NoteAudioTokens(ev.AudioTokens)
				}
			case openai.EventTranscriptDelta:
				transcript.WriteString(ev.Text)
			case openai.EventTranscriptDone:
				// #458 output capture: forward the assistant's final transcript
				// as a VoiceAgentRealtimeOutput so chat/canvas/audit render the
				// realtime turn with citations (parity with the cascade reply).
				final := ev.Text
				if final == "" {
					final = transcript.String()
				}
				e.captureOutput(final)
			case openai.EventInputTranscriptDelta:
				// Native human transcript (#478): stream the running text as a
				// partial so chat ghost-texts the in-progress user utterance. Handed
				// to the parallel forward loop (never sent inline) so a slow gRPC
				// insert can't stall audio / barge-in / response lifecycle.
				inputTranscript.WriteString(ev.Text)
				e.enqueueTranscript(transcriptJob{speaker: e.getLastSpeaker(), text: inputTranscript.String()})
			case openai.EventInputTranscriptDone:
				// The human's final utterance, native-authored: the model has
				// already authored + spoken its reply, so forward the final
				// marked native so the server stamps it transcript-only and
				// cognition does not author a second reply.
				final := strings.TrimSpace(ev.Text)
				if final == "" {
					final = strings.TrimSpace(inputTranscript.String())
				}
				inputTranscript.Reset()
				e.seq.Store(0)
				if final != "" {
					if e.isGatedSemanticVad() {
						// Multi-party gate (#481): the model detected turn-end +
						// transcribed the active speaker but did NOT auto-generate
						// (create_response:false). Forward as a normal (stt) final
						// so cognition runs the gate and publishes the directive,
						// then drive runTurn -- the relay returns the directive and
						// the executor fires CreateResponse on engage. The model
						// authors; cognition never authors the words.
						speaker := e.getLastSpeaker()
						e.enqueueTranscript(transcriptJob{speaker: speaker, text: final, final: true, native: false})
						go e.runTurn(speaker, final)
					} else {
						// Native 1-on-1 (#478): the model already authored + spoke,
						// so mark the final native (transcript-only) -- cognition
						// does not author a second reply. Attribute it to the active
						// speaker so the BFF accepts the insert. Handed to the parallel
						// loop so the chat insert never delays the next turn / barge-in.
						e.enqueueTranscript(transcriptJob{speaker: e.getLastSpeaker(), text: final, final: true, native: true})
					}
				}
			case openai.EventFunctionArgsDone:
				// #458 MCP tool bridge: dispatch the model's function call on a
				// worker goroutine (OFF this drain loop so a long tool never
				// blocks audio), then SendFunctionResult + CreateResponse so the
				// model continues with the tool result in context.
				e.dispatchToolCall(ev.CallID, ev.FuncName, ev.Arguments)
			}
		}
	}
}

// captureOutput forwards one completed assistant transcript to memQL as an SI
// utterance (#458 output capture). Runs on its own goroutine so the wire
// round-trip never blocks the event drain. A nil forwarder or a blank
// transcript is a no-op; a failed insert is logged (the voice turn still
// played). The Go analog of realtime_output.py::_schedule_forward.
//
// SPOKEN == SHOWN (#482). This is the single source of truth for the assistant
// utterance: `text` is the model's own spoken-audio transcript
// (EventTranscriptDone / the accumulated EventTranscriptDelta stream), forwarded
// VERBATIM (the forwarder only trims surrounding whitespace). There is no
// re-rendering between what was said and what is shown -- because the model both
// generates and speaks the reply (#478/#479), the spoken audio and the chat
// utterance share one source by construction. Pinned by
// TestRealtimeExecutor_SpokenEqualsShown.
func (e *RealtimeExecutor) captureOutput(text string) {
	if e.outputForwarder == nil {
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	go func() {
		// replyToId is left empty: the realtime model speaks directly and the
		// triggering human utterance id is not threaded through the gpt-realtime
		// event stream (matches the Python forwarder's default).
		utteranceID, err := e.outputForwarder.Forward(e.ctx, text, "")
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("voice-agent realtime: output capture forward failed", "err", err)
			}
			return
		}
		if e.logger != nil {
			e.logger.Info("voice-agent realtime: output captured",
				"utterance_id", utteranceID, "chars", len(text))
		}
	}()
}

// dispatchToolCall runs one model-driven function call through the MCP tool
// bridge on a worker goroutine (off the event drain so a long tool never
// blocks audio), returns the result to the model via SendFunctionResult, and
// chains a CreateResponse so the model continues with the result in context.
// A nil bridge logs and ignores the call (model-driven tools disabled). The Go
// analog of the async function-call path in realtime_executor.py.
func (e *RealtimeExecutor) dispatchToolCall(callID, funcName, arguments string) {
	if e.toolBridge == nil {
		if e.logger != nil {
			e.logger.Debug("voice-agent realtime: function call with no tool bridge (ignored)",
				"name", funcName, "call_id", callID)
		}
		return
	}
	go func() {
		output := e.toolBridge.Dispatch(e.ctx, funcName, arguments)
		if err := e.session.SendFunctionResult(callID, output); err != nil {
			if e.logger != nil {
				e.logger.Warn("voice-agent realtime: send function result failed",
					"name", funcName, "call_id", callID, "err", err)
			}
			return
		}
		// Continue the model with the tool result in context. An empty
		// instructions string falls back to the session-level persona.
		if err := e.session.CreateResponse(""); err != nil && e.logger != nil {
			e.logger.Warn("voice-agent realtime: response.create after tool failed",
				"name", funcName, "call_id", callID, "err", err)
		}
	}()
}

func (e *RealtimeExecutor) setInFlight(cancel context.CancelFunc) {
	e.respMu.Lock()
	if e.pumpCancel != nil {
		e.pumpCancel()
	}
	e.pumpCancel = cancel
	e.inFlight = true
	e.respMu.Unlock()
}

func (e *RealtimeExecutor) clearInFlight() {
	e.respMu.Lock()
	e.inFlight = false
	e.pumpCancel = nil
	e.respMu.Unlock()
}

func (e *RealtimeExecutor) isInFlight() bool {
	e.respMu.Lock()
	defer e.respMu.Unlock()
	return e.inFlight
}

func (e *RealtimeExecutor) cancelPump() {
	e.respMu.Lock()
	cancel := e.pumpCancel
	e.pumpCancel = nil
	e.inFlight = false
	e.respMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// resolveSpeaker maps a per-track participant identity to the speaker_user_id
// stamped on the wire. In a 1:1 standard space the cascade leaves this empty
// (server resolves the active speaker); in multi-human rooms the per-track
// identity IS the speaker, so we forward it verbatim (#433 section 3).
func (e *RealtimeExecutor) resolveSpeaker(identity string) string {
	if id := strings.TrimSpace(identity); id != "" {
		return id
	}
	return e.cfg.SpeakerUserID
}

// participantRoster resolves a participant identity to a "[name . role]" label
// for the multi-party labeled-item injection (#433 section 3). Resolved at
// join (one lookup, cached) by the room glue via RealtimeExecutor.SetParticipant.
type participantRoster struct {
	mu      sync.RWMutex
	entries map[string]string // identity -> "name . role"
}

func newParticipantRoster() *participantRoster {
	return &participantRoster{entries: make(map[string]string)}
}

func (r *participantRoster) set(identity, displayName, role string) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return
	}
	name := strings.TrimSpace(displayName)
	role = strings.TrimSpace(role)
	var label string
	switch {
	case name != "" && role != "":
		label = name + " · " + role
	case name != "":
		label = name
	case role != "":
		label = role
	}
	r.mu.Lock()
	r.entries[identity] = label
	r.mu.Unlock()
}

func (r *participantRoster) label(identity string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[strings.TrimSpace(identity)]
}
