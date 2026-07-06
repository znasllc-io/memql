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
	"github.com/znasllc-io/memql/integrations/stt"
)

// realtime_executor.go is the Go realtime voice loop: it slots BEHIND THE SAME
// SEAM as the cascade (cascade.go) -- same turn-taking machine, same gRPC
// VoiceAgent* contract, same SpeakSink registration -- but drives the OpenAI
// gpt-realtime speech-to-speech model instead of the OpenAI STT->cognition->
// OpenAI TTS pipeline. It is the Go analog of
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
	// async function-call path, #458/#1430). It does NOT itself create a
	// response -- the executor's tool worker fires the follow-up
	// response.create at the next quiet boundary (realtime_tools.go).
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

	// speakingSender emits the GA's speaking-state presence signal (#1421):
	// VoiceAgentRealtimeSpeaking{speaking:true} on the first output audio frame
	// of a response, {speaking:false} on response.done, so the frontend's orb
	// animates while the assistant speaks on the native realtime path. The
	// server writes the presence row (handleVoiceAgentRealtimeSpeaking) -- the
	// executor only OBSERVES the output stream, where the deltas land. Defaults
	// to the gRPC client; nil disables the signal (voice still plays). speaking
	// guards the fire-on-first-frame so one response emits exactly one
	// responding signal even though drainAudioOut sees many frames.
	speakingSender   realtimeSpeakingSender
	respondingSignal atomic.Bool
	// toolBridge dispatches model-driven function calls through the low-risk MCP
	// tool surface and mirrors them into cognition (#458). Nil disables
	// model-driven tools (a function-call event is logged and ignored).
	toolBridge *McpToolBridge

	// confidenceGate drops low-confidence input-transcription finals by their
	// mean token logprob (#1431, transcript_confidence.go). Nil disables the
	// gate; finals without logprobs always pass either way.
	confidenceGate *transcriptConfidenceGate

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
	//   - turnModeGatedCascade: turn_detection:null, the labeled ASR drives the turn
	//     machine, the conductor gate runs via runTurn (the pre-#481 multi-party
	//     path, still the default when the multi-party semantic_vad flag is off).
	//   - turnModeNative (#478): semantic_vad + create_response:true, gpt-realtime
	//     owns the turn, no conductor; the human transcript comes from the model's
	//     native input transcription, not a separate ASR stream. 1-on-1.
	//   - turnModeGatedSemanticVad (#481): semantic_vad + create_response:false,
	//     the model detects turn-end + transcribes the active speaker but does NOT
	//     auto-generate; the gate decides (runTurn) and the executor fires
	//     CreateResponse on engage. The labeled ASR is used ONLY for per-speaker
	//     attribution (labeled-item injection), not to drive turns. Multi-party.
	turnMode atomic.Int32

	// suppressCapture marks the in-flight response as a RE-VOICE of text the
	// cognition/text leg already committed to chat (the legacy authored-text
	// path: an unsolicited VoiceAgentSpeak push, or the A2 tool loop's
	// directive_mode-empty TurnComplete -- both carry text read off an
	// already-inserted AI utterance). RealtimeInstructionsForReply tells the
	// model NOT to read it verbatim, so capturing the spoken rendition would
	// land a second, diverging assistant bubble for the same reply. The
	// spoken-transcript bubble must have exactly one writer (#1427): set by
	// onAssistantStart before CreateResponse, consumed (and cleared) when the
	// response seals at EventResponseDone.
	suppressCapture atomic.Bool

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

	// turnResponseCreatedNanos is the wall-clock (UnixNano) of the model's most
	// recent response.created. drainAudioOut reads it to split the snappiness
	// window into [speech_stop -> response.created] (our trigger latency) and
	// [response.created -> first audio] (OpenAI generation TTFB) so we can tell
	// where the per-turn time actually goes (latency-probe instrumentation).
	turnResponseCreatedNanos atomic.Int64

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

	// Non-blocking tool-call scheduler state (#1430, realtime_tools.go): the
	// per-session FIFO of pending model-driven calls, the worker wakeup, and
	// the quiet-boundary signals the announcer reads. userSpeaking tracks the
	// model VAD's speech_started/stopped pair (the native / semantic_vad
	// paths; the labeled-ASR path is covered by the turn machine's human-turn
	// state); lastSpeechStopWall is the wall-clock of the most recent
	// speech_stopped / labeled-ASR final (the announce grace anchor);
	// gatePending counts in-flight conductor gate round-trips (their engage is
	// about to take the response writer slot).
	toolMu             sync.Mutex
	toolQueue          []*pendingToolCall
	toolKick           chan struct{}
	toolTimeout        time.Duration
	toolMaxPending     int
	toolTick           time.Duration
	toolAnnounceGrace  time.Duration
	userSpeaking       atomic.Bool
	lastSpeechStopWall atomic.Int64
	gatePending        atomic.Int32

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
		cfg:            cfg,
		client:         client,
		session:        session,
		sink:           sink,
		persona:        persona,
		speakingSender: client,
		roster:         newParticipantRoster(),
		logger:         logger,
		ctx:            ctx,
		cancel:         cancel,
		transcriptCh:   make(chan transcriptJob, transcriptQueueDepth),

		// #1430 async tool-call defaults; ConfigureToolCalls overrides.
		toolKick:          make(chan struct{}, 1),
		toolTimeout:       defaultToolCallTimeout,
		toolMaxPending:    defaultMaxPendingToolCalls,
		toolTick:          defaultToolLoopTick,
		toolAnnounceGrace: defaultToolAnnounceGrace,
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
	go e.runToolLoop()
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
// blocking the caller.
//
// Partials are drop-on-full: a dropped partial is invisible (cosmetic
// ghost-text). A FINAL is the user's committed utterance, though -- dropping it
// means the spoken turn never reaches chat (#1200: "DB shows only assistant
// utterances"). A final can be the very job that overflows a queue saturated by
// the burst of input-transcription partials that preceded it, so it must NEVER
// be dropped. When the queue is full, a final parks on its own goroutine that
// blocks until a slot frees (or the executor is torn down), so drainEvents still
// never blocks while the chat insert is guaranteed to be attempted. The
// forwardTranscriptLoop single consumer preserves enqueue order.
func (e *RealtimeExecutor) enqueueTranscript(j transcriptJob) {
	select {
	case e.transcriptCh <- j:
		return
	default:
	}
	if j.final {
		go func() {
			select {
			case e.transcriptCh <- j:
			case <-e.ctx.Done():
			}
		}()
		return
	}
	if e.logger != nil {
		e.logger.Debug("voice-agent realtime: transcript forward dropped (backpressure)",
			"final", j.final, "chars", len(j.text))
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
	turnModeGatedCascade     int32 = iota // null + labeled ASR + conductor gate (default multi-party)
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
			turnModeGatedCascade:     "gated-cascade (null + labeled ASR + conductor)",
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

// ConsumeASR drives the machine from one human track's labeled ASR result
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
	// Native mode does not consume the labeled ASR for turn-taking, but the native input
	// transcript (EventInputTranscriptDone) carries no speaker of its own -- it
	// reads getLastSpeaker() to attribute the user utterance. Without this the
	// forwarded VoiceAgentFinalTranscript has an empty speaker id, the BFF rejects
	// the insert (InvalidArgument), and the human's turn never reaches chat (the
	// "assistant replies but my utterances don't show" symptom).
	if id := strings.TrimSpace(speakerIdentity); id != "" {
		e.setLastSpeaker(id)
	}
	// Native 1-on-1 mode: gpt-realtime transcribes the human, detects the turn,
	// authors + speaks, and handles barge-in -- so labeled-ASR results are not
	// consumed for turn-taking (the labeled ASR stays warm only for the cascade fallback
	// and to seed the speaker id above). The only executor turn input on the
	// native path is StreamAudio (PCM -> model) + the native transcript events.
	if e.isNativeMode() {
		return
	}
	// Multi-party semantic_vad gate (#481): the model detects turn-end and
	// transcribes the active speaker, so the labeled ASR does NOT drive the turn here.
	// But the per-track labeled ASR is still the attribution source -- inject each
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
		// #1430: the labeled-ASR path has no model speech_stopped event; the
		// committed final IS its end-of-speech marker. Stamp the announce
		// grace anchor so a tool result cannot fire into the final ->
		// gate-round-trip gap.
		e.lastSpeechStopWall.Store(time.Now().UnixNano())
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
				SpaceId:       e.cfg.PartitionID,
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
	// not this labeled-ASR-driven commit. So we neither forward this final (the
	// native path inserts the user utterance, stamped transcript-only) nor
	// round-trip the conductor. Guarded defensively even though the room layer
	// also stops feeding labeled-ASR finals into the machine in native mode.
	// turnModeGatedSemanticVad (#481) likewise drives the turn from the model's
	// turn-end (EventInputTranscriptDone), not this labeled-ASR commit.
	if e.isNativeMode() || e.isGatedSemanticVad() {
		return
	}
	speaker := e.getLastSpeaker()
	e.forwardFinal(speaker, text, false)
	go e.runTurn(speaker, text)
}

// SetActiveSpeaker seeds the active speaker id from the room layer (the LiveKit
// participant identity), used when the labeled ASR is off the path so the native input
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

// forwardFinal sends a VoiceAgentFinalTranscript. nativeAuthored
// marks the #478 native path: the realtime model already authored AND spoke the
// reply, so the server stamps this user utterance transcript-only and cognition
// does not author a second reply. The send stays non-blocking, but the
// server's FinalAck / QueryError reply is consumed and logged instead of
// being discarded (#1403).
func (e *RealtimeExecutor) forwardFinal(speakerIdentity, text string, nativeAuthored bool) {
	sendFinalTranscript(e.ctx, e.client, e.logger, "realtime", &memqlv1.VoiceAgentFinalTranscript{
		SpaceId:        e.cfg.PartitionID,
		SpeakerUserId:  e.resolveSpeaker(speakerIdentity),
		FinalText:      text,
		NativeAuthored: nativeAuthored,
	})
	// T0 (#484): the human turn is committed. The headline decision->first-audio
	// window opens here; see docs/internal/design/voice-484-latency-fidelity-measurement.md.
	if e.logger != nil {
		e.logger.Info("voice trace: turntaking event",
			"stage", "voice.final", "executor", "realtime",
			"partition_id", e.cfg.PartitionID, "native_authored", nativeAuthored, "chars", len(text))
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
	// #1430: a gate round-trip is in flight -- its engage is about to take the
	// default-conversation writer slot, so the tool announcer holds back until
	// the decision lands (then either inFlight covers the engaged response or
	// the suppress/defer leaves the boundary quiet).
	e.gatePending.Add(1)
	defer func() {
		e.gatePending.Add(-1)
		e.kickToolLoop()
	}()
	// turnStart anchors the per-turn gate/cognition hop (#1426): turn request
	// sent -> conductor decision received. This whole window sits between the
	// model's end-of-speech and the response.create on gated paths, so it is
	// the prime suspect for the end-of-speech -> first-audio gap.
	turnStart := time.Now()
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{
			VoiceAgentTurnRequest: &memqlv1.VoiceAgentTurnRequest{
				SpaceId:       e.cfg.PartitionID,
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

	// logGate stamps the gate round-trip's terminal outcome (#1426).
	logGate := func(outcome string, extra ...any) {
		attrs := append([]any{"partition_id", e.cfg.PartitionID, "outcome", outcome}, extra...)
		logVoiceTiming(e.logger, "turn.gate_roundtrip", turnStart, attrs...)
	}

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
						logGate("defer", "request_id", requestID)
						if e.logger != nil {
							e.logger.Debug("voice-agent realtime: conductor gate deferred (no response.create)")
						}
						return
					}
					logGate("directive", "request_id", requestID, "mode", directiveMode)
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
					logGate("suppressed", "request_id", requestID, "error_code", done.GetErrorCode())
					if e.logger != nil {
						e.logger.Debug("voice-agent realtime: conductor suppressed turn (no response.create)")
					}
					return
				}
				logGate("authored", "request_id", requestID, "chars", len(final))
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

	// #1427: a directive with authored Text and no mode re-voices a reply that
	// is ALREADY a committed chat row (the relay returns FinalText off the
	// inserted utterance; the speak push carries an inserted utterance's text).
	// Suppress the output capture for this one response so the model's spoken
	// paraphrase does not land as a second, diverging assistant bubble. A
	// directive-mode response (the model authors) keeps capture on -- there the
	// capture IS the only writer.
	e.suppressCapture.Store(strings.TrimSpace(req.Mode) == "" && strings.TrimSpace(req.Text) != "")

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
		e.suppressCapture.Store(false)
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
			isFirstFrame := e.firstAudioPending.CompareAndSwap(true, false)
			if isFirstFrame {
				// #1421: the assistant just became audible -- signal the GA's
				// speaking-state presence so the frontend orb animates. The
				// server writes presence state=responding; response.done writes
				// idle. This is the only writer of responding on the native
				// realtime path (cognition reset it to idle at gate-publish).
				e.emitSpeaking(true)
			}
			if isFirstFrame && e.logger != nil {
				nowNanos := time.Now().UnixNano()
				if stop := e.turnSpeechStopNanos.Swap(0); stop > 0 {
					ms := (nowNanos - stop) / 1e6
					e.logger.Info("voice trace: turntaking event",
						"stage", "realtime.audio.first", "executor", "realtime", "partition_id", e.cfg.PartitionID,
						// #1426: the headline per-turn snappiness number, under
						// the discoverable voice_timing key.
						voiceTimingKey, "turn.speech_stop_to_first_audio", "duration_ms", ms,
						"speech_stop_to_audio_ms", ms)
				} else {
					e.logger.Info("voice trace: turntaking event",
						"stage", "realtime.audio.first", "executor", "realtime", "partition_id", e.cfg.PartitionID)
				}
				// Latency probe: split the snappiness window -- response.created ->
				// first audio is OpenAI's generation TTFB (the part NOT under our
				// control), isolated from the speech_stop -> response.created trigger
				// latency above. If this dominates, the per-turn delay is the model,
				// not our pipeline.
				if respCreated := e.turnResponseCreatedNanos.Swap(0); respCreated > 0 {
					genMs := (nowNanos - respCreated) / 1e6
					e.logger.Info("voice trace: turntaking event",
						"stage", "realtime.audio.first.gen", "executor", "realtime", "partition_id", e.cfg.PartitionID,
						voiceTimingKey, "turn.response_to_first_audio", "duration_ms", genMs)
				}
			}
			// Latency probe: time the first frame's sink write (our output-path
			// handling -- LiveKit publish + any backpressure). Expected ~0; a
			// non-trivial value would mean our forwarding, not OpenAI, adds delay.
			writeStartNanos := time.Now().UnixNano()
			if err := e.sink.WriteFrame(frame); err != nil {
				if e.logger != nil {
					e.logger.Warn("voice-agent realtime: room audio write failed", "err", err)
				}
				return
			}
			if isFirstFrame && e.logger != nil {
				e.logger.Info("voice trace: turntaking event",
					"stage", "realtime.audio.sink_write", "executor", "realtime", "partition_id", e.cfg.PartitionID,
					voiceTimingKey, "turn.sink_write_first", "duration_ms", (time.Now().UnixNano()-writeStartNanos)/1e6,
					"sink_write_us", (time.Now().UnixNano()-writeStartNanos)/1e3)
			}
		}
	}
}

// drainEvents consumes the non-audio event stream: response lifecycle (to clear
// the in-flight flag + collapse the turn), the captured transcript, and tool
// calls. Output capture (transcript -> VoiceAgentRealtimeOutput) and the MCP
// tool bridge are #458 -- the seams are marked below.
func (e *RealtimeExecutor) drainEvents() {
	// Assistant spoken-transcript capture is RESPONSE-scoped and append-only
	// (#1427): transcript accumulates the CURRENT content part's
	// output_audio_transcript deltas; each transcript.done seals one part into
	// sealedParts; response.done seals the response and forwards the assembled
	// transcript EXACTLY ONCE (captureOutput). GA emits transcript.done per
	// content part -- and also for interrupted/incomplete/cancelled responses --
	// so forwarding per transcript.done double-writes the bubble; the single
	// response.done forward is what keeps the spoken-reply bubble single-writer.
	var transcript strings.Builder
	var sealedParts []string
	// inputTranscript accumulates the model's native transcription of the
	// human's audio (#478 native mode), so the chat transcript comes from the
	// realtime model rather than a separate ASR pass on the critical path.
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
				// #1430: the user holds the floor -- the tool announcer must not
				// speak a result into their turn.
				e.userSpeaking.Store(true)
				// Observability: server-side VAD heard speech onset. Stamp nothing
				// yet (the snappiness window opens at speech_stopped); just trace it.
				if e.logger != nil {
					e.logger.Info("voice trace: turntaking event",
						"stage", "model.speech_started", "executor", "realtime", "partition_id", e.cfg.PartitionID)
				}
			case openai.EventInputSpeechStopped:
				// Observability: server-side VAD decided the human finished. This
				// opens the end-of-speech -> first-assistant-audio window that
				// drainAudioOut closes on the next response's first frame. This delta
				// is the headline "snappiness" number with no second ASR on the path.
				e.turnSpeechStopNanos.Store(time.Now().UnixNano())
				// #1430: the floor is free again (modulo the announce grace --
				// a native auto-response / gate engage may be about to claim
				// it). Wake the tool worker so a parked result can surface.
				e.userSpeaking.Store(false)
				e.lastSpeechStopWall.Store(time.Now().UnixNano())
				e.kickToolLoop()
				if e.logger != nil {
					e.logger.Info("voice trace: turntaking event",
						"stage", "model.speech_stopped", "executor", "realtime", "partition_id", e.cfg.PartitionID)
				}
			case openai.EventResponseCreated:
				// A new response opens a fresh transcript capture. Defensive
				// reset -- EventResponseDone already seals + clears, but a
				// response whose done never arrived (session drop) must not
				// leak its text into the next response's bubble (#1427).
				transcript.Reset()
				sealedParts = nil
				e.respMu.Lock()
				e.inFlight = true
				e.respMu.Unlock()
				// Arm the T3 first-audio stamp for this response (#484).
				e.firstAudioPending.Store(true)
				// Latency probe: stamp response.created so drainAudioOut can isolate
				// OpenAI's generation TTFB (response.created -> first audio) from our
				// trigger latency (speech_stop -> response.created).
				e.turnResponseCreatedNanos.Store(time.Now().UnixNano())
				if e.logger != nil {
					stage := "model.response_created"
					if stop := e.turnSpeechStopNanos.Load(); stop > 0 {
						ms := (time.Now().UnixNano() - stop) / 1e6
						e.logger.Info("voice trace: turntaking event",
							"stage", stage, "executor", "realtime", "partition_id", e.cfg.PartitionID,
							// #1426: end-of-speech -> response.create (the gate /
							// cognition hops live inside this window).
							voiceTimingKey, "turn.speech_stop_to_response", "duration_ms", ms,
							"speech_stop_to_response_ms", ms)
					} else {
						e.logger.Info("voice trace: turntaking event",
							"stage", stage, "executor", "realtime", "partition_id", e.cfg.PartitionID)
					}
				}
			case openai.EventResponseDone:
				e.clearInFlight()
				e.machine.OnAssistantDone()
				// #1421: the response is sealed -- the assistant stopped
				// speaking. Signal the GA's presence back to idle so the orb's
				// speaking animation stops. No-op if this response never went
				// audible (respondingSignal de-dupes), so a cancelled/empty
				// response doesn't write a spurious idle.
				e.emitSpeaking(false)
				// #1430: a sealed response is the canonical quiet boundary --
				// wake the tool worker so a parked result can be announced.
				e.kickToolLoop()
				// #458/#1427 output capture: the response is sealed -- assemble
				// its spoken transcript and forward it as ONE
				// VoiceAgentRealtimeOutput so chat/canvas/audit render the
				// realtime turn (parity with the cascade reply). Any unsealed
				// delta tail (a response cancelled before its part's
				// transcript.done -- barge-in) joins as the final part, so a
				// truncated response's bubble stops where its audio stopped.
				// After this seal nothing writes to the captured transcript
				// again: deltas appended, the seal forwarded, no later rewrite.
				if tail := transcript.String(); strings.TrimSpace(tail) != "" {
					sealedParts = append(sealedParts, tail)
				}
				final := strings.Join(sealedParts, "\n\n")
				sealedParts = nil
				transcript.Reset()
				if e.suppressCapture.Swap(false) {
					// Re-voice of a cognition-authored reply that is already a
					// chat row -- the spoken paraphrase must not become a
					// second bubble for the same reply (#1427).
					if e.logger != nil && strings.TrimSpace(final) != "" {
						e.logger.Debug("voice-agent realtime: output capture suppressed (re-voiced authored reply)",
							"chars", len(final))
					}
				} else {
					if ev.Status != "" && !strings.EqualFold(ev.Status, "completed") && e.logger != nil {
						e.logger.Info("voice-agent realtime: response truncated; transcript sealed at cancellation point",
							"status", ev.Status, "chars", len(final))
					}
					e.captureOutput(final)
				}
				// Feed the completed response's audio-token usage to the cost
				// guardrail (#459). Crossing the per-session budget tears the
				// session down and flags degrade-to-cascade.
				if e.lifecycle != nil && ev.AudioTokens > 0 {
					e.lifecycle.NoteAudioTokens(ev.AudioTokens)
				}
			case openai.EventTranscriptDelta:
				transcript.WriteString(ev.Text)
			case openai.EventTranscriptDone:
				// One content part's spoken transcript is final. Seal the PART
				// into the response-scoped assembly -- the forward happens once,
				// at EventResponseDone (#1427): GA emits transcript.done per
				// content part (and for interrupted responses), so forwarding
				// here would double-write the bubble. The done event's
				// transcript is authoritative for the part; when the server
				// omits it, the part's accumulated deltas ARE the transcript.
				part := ev.Text
				if part == "" {
					part = transcript.String()
				}
				transcript.Reset()
				if strings.TrimSpace(part) != "" {
					sealedParts = append(sealedParts, part)
				}
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
				if final != "" && !e.confidenceGate.pass(ev.Logprobs) {
					// #1431 confidence gate: the session requested per-token
					// logprobs on input transcription; a final whose mean
					// logprob falls below the floor is echo/noise-shaped, not
					// speech. Finals WITHOUT logprobs always pass (the signal
					// is intermittently missing). Composes with the #1199
					// denylist filter below.
					if e.logger != nil {
						e.logger.Info("voice-agent realtime: dropped low-confidence input transcript (#1431)",
							"partition_id", e.cfg.PartitionID, "text", final,
							"mean_logprob", meanLogprob(ev.Logprobs), "floor", e.confidenceGate.floor)
					}
					final = ""
				}
				if final != "" && !stt.NewTranscriptFilter().Keep(final, true, 0) {
					// #1199: the realtime input transcription bypasses the cascade
					// STT filter, so gpt-realtime's silence/non-speech
					// hallucinations ("thank you for watching", denylisted stock
					// phrases) + empty transcripts leak into chat. Apply the SAME
					// filter here. The realtime path carries no real confidence
					// signal, so confidence 0 -> the empty-drop + no-speech denylist
					// fire while genuine content is kept. (Stopping the model from
					// RESPONDING to silence is the deeper VAD fix, #481.)
					if e.logger != nil {
						e.logger.Info("voice-agent realtime: dropped hallucinated/empty input transcript (#1199)",
							"partition_id", e.cfg.PartitionID, "text", final)
					}
					final = ""
				}
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
				// #458/#1430 MCP tool bridge: enqueue the model's function call
				// on the async tool scheduler (realtime_tools.go). Returns
				// immediately -- execution runs in the background, the result
				// injects in call order when it lands, and the audible
				// follow-up response fires at the next quiet boundary. The
				// model's prompt-taught spoken acknowledgment never waits.
				e.dispatchToolCall(ev.CallID, ev.FuncName, ev.Arguments)
			}
		}
	}
}

// captureOutput forwards one completed assistant transcript to memQL as an AI
// utterance (#458 output capture). Runs on its own goroutine so the wire
// round-trip never blocks the event drain. A nil forwarder or a blank
// transcript is a no-op; a failed insert is logged (the voice turn still
// played). The Go analog of realtime_output.py::_schedule_forward.
//
// SPOKEN == SHOWN (#482), single writer + append-only (#1427). This is the
// single source of truth for the assistant utterance: `text` is the model's
// own spoken-audio transcript (the response-scoped assembly of
// EventTranscriptDelta / EventTranscriptDone parts, sealed once at
// EventResponseDone), forwarded VERBATIM (the forwarder only trims surrounding
// whitespace). Exactly one forward per response, nothing rewrites it later,
// and a truncated (barge-in) response seals at the cancellation point. There
// is no re-rendering between what was said and what is shown -- because the
// model both generates and speaks the reply (#478/#479), the spoken audio and
// the chat utterance share one source by construction. Pinned by
// TestRealtimeExecutor_SpokenEqualsShown*, TestRealtimeExecutor_OutputCapture_*.
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

// realtimeSpeakingSender is the minimal one-way gRPC surface the speaking-state
// signal needs (#1421): a fire-and-forget Send (no ack), satisfied by *Client
// (Send) and stubbed in tests. Separate from realtimeOutputSender (which awaits
// an ack) because the speaking signal is best-effort -- a dropped one only
// costs one frame of orb animation, never a chat row.
type realtimeSpeakingSender interface {
	Send(ctx context.Context, envelope *memqlv1.MemqlClientMessage) error
}

// Compile-time assurance the production gRPC Client satisfies the speaking
// seam in the CGO-free lane (drift caught here, not only in the voice lane).
var _ realtimeSpeakingSender = (*Client)(nil)

// emitSpeaking sends the GA's speaking-state presence signal (#1421). The orb
// animates off the v1:cognition:participant presence state=responding; on the
// native realtime path cognition resets presence to idle at gate-publish and
// the only output capture is the final transcript, so nothing writes responding
// while the assistant speaks. The executor OBSERVES the output stream -- so it
// emits speaking=true on the first output audio frame of a response and
// speaking=false on response.done -- and the SERVER writes the presence row
// (handleVoiceAgentRealtimeSpeaking) through the same engine + mesh routing rule
// every presence write uses, keeping it multi-node correct. respondingSignal
// de-dupes so one response emits exactly one responding (drainAudioOut sees
// many frames). Fire-and-forget + nil-safe: a failure only costs orb animation,
// never the voice turn.
func (e *RealtimeExecutor) emitSpeaking(speaking bool) {
	if e.speakingSender == nil {
		return
	}
	// One responding per response, one idle per done. The first audio frame
	// flips false->true (emit responding); response.done flips true->false
	// (emit idle). A done with no prior audio (cancelled/empty response) was
	// never responding, so it skips the redundant idle.
	if speaking {
		if !e.respondingSignal.CompareAndSwap(false, true) {
			return
		}
	} else {
		if !e.respondingSignal.CompareAndSwap(true, false) {
			return
		}
	}
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentRealtimeSpeaking{
			VoiceAgentRealtimeSpeaking: &memqlv1.VoiceAgentRealtimeSpeaking{
				SpaceId:   e.cfg.PartitionID,
				GaAgentId: e.cfg.GaAgentID,
				Speaking:  speaking,
			},
		},
	}
	if err := e.speakingSender.Send(e.ctx, envelope); err != nil {
		if e.logger != nil {
			e.logger.Debug("voice-agent realtime: speaking signal send failed",
				"speaking", speaking, "err", err)
		}
	}
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
