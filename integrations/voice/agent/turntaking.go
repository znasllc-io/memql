package agent

import (
	"log/slog"
	"sync"
)

// turntaking.go is the Go turn-taking / endpointing state machine that
// replaces the orchestration the Python LiveKit Agents AgentSession ran
// for free. It is the implementation of the design in
// docs/voice/452-turntaking-orchestration-go.md, section 3.
//
// It is pure Go (no LiveKit, no CGO, no media types): it consumes already-
// decoded ASR turn-structure signals (start-of-speech / interim / final)
// and external "speak now" directives, and emits transition callbacks the
// cascade orchestrator (cascade.go) binds to. Keeping it free of the media
// layer is what lets the full turn-taking + barge-in logic be unit-tested
// in the default CGO-free CI lane; the voice-tagged room glue
// (room_audio_voice.go) only supplies PCM frames.
//
// Conductor-gate invariant (epic #440, docs/voice/452 section 1): a human
// end-of-utterance NEVER auto-enters assistant-turn. Transition (D) reports
// the transcript boundary and returns to listening; only an external
// directive (transition B, driven by VoiceAgentSpeak / a TurnComplete the
// orchestrator decides to play) enters assistant-turn.

// TurnState is the explicit state of the per-room turn-taking machine.
type TurnState int

const (
	// StateIdle is the resting state with no live session. Entered at
	// construction and on Stop.
	StateIdle TurnState = iota
	// StateListening is the session-live, no-one-speaking resting state.
	// The STT stream is open and audio flows; the machine waits for a
	// human onset or an external speak directive.
	StateListening
	// StateHumanTurn means a human is actively speaking. Interim partials
	// accumulate; the machine is recording a turn boundary, NOT deciding
	// whether the assistant replies.
	StateHumanTurn
	// StateAssistantTurn means the assistant is producing audio and is
	// interruptible. Entered ONLY on an external speak directive
	// (the conductor gate); a human onset here raises barge-in.
	StateAssistantTurn
)

// String renders a TurnState for logs/traces.
func (s TurnState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateListening:
		return "listening"
	case StateHumanTurn:
		return "human-turn"
	case StateAssistantTurn:
		return "assistant-turn"
	default:
		return "unknown"
	}
}

// TurnCallbacks are the side-effect seams the machine drives on each
// transition. The cascade orchestrator supplies them; all are optional
// (a nil callback is a no-op) so the machine can be exercised in
// isolation. Callbacks are invoked WITHOUT the machine's lock held so an
// implementation may call back into the machine (e.g. OnAssistantDone)
// without deadlocking.
type TurnCallbacks struct {
	// OnSpeechStarted fires on a human voice-activity onset while
	// listening or human-turn (not a barge-in). Cosmetic / tracing seam.
	OnSpeechStarted func()
	// OnFinalTranscript fires when a human turn ends (transition D) with
	// the committed transcript text. The orchestrator forwards it as a
	// VoiceAgentFinalTranscript and drives a VoiceAgentTurnRequest. Never
	// auto-starts assistant audio.
	OnFinalTranscript func(text string)
	// OnAssistantStart fires when an external speak directive enters
	// assistant-turn (transition B). The orchestrator begins TTS playout.
	OnAssistantStart func(req SpeakDirective)
	// OnBargeIn fires when a human onset interrupts an in-flight
	// assistant turn (transition C). The orchestrator cancels TTS playout
	// and flushes the publish buffer. After OnBargeIn the machine moves
	// to human-turn (the human now holds the floor).
	OnBargeIn func()
}

// SpeakDirective is the external "speak now" payload that drives
// transition (B). For the cascade path it is sourced from a
// VoiceAgentSpeak push or from the orchestrator's own decision to play a
// TurnComplete reply.
type SpeakDirective struct {
	// Text is the utterance to synthesize.
	Text string
	// UtteranceID correlates the spoken audio to the chat utterance row
	// (VoiceAgentSpeak.utterance_id / VoiceAgentTurnComplete.utterance_id).
	UtteranceID string
	// RequestID is the originating request id for tracing/correlation.
	RequestID string
	// Mode / Brevity carry the conductor GATE decision (#477/#479). When Mode is
	// non-empty, the realtime executor renders a content-free mode+brevity
	// directive (RealtimeInstructionsForDirective) and the MODEL authors the
	// reply; when empty, it falls back to conveying Text
	// (RealtimeInstructionsForReply, the legacy path). Wire-string forms of
	// DirectiveMode (primary|brief_ack|chimein|defer) and Brevity
	// (short|normal|detailed).
	Mode    string
	Brevity string
}

// TurnMachine is the five-state turn-taking machine (section 3 of the
// #452 design). It is safe for concurrent use: the Deepgram event loop,
// the external directive path, and the assistant-done callback can drive
// it from different goroutines.
type TurnMachine struct {
	logger *slog.Logger
	cb     TurnCallbacks

	mu      sync.Mutex
	state   TurnState
	partial string // accumulated interim text for the current human turn
}

// NewTurnMachine builds a machine in StateIdle. cb may be the zero value
// (all transitions become no-ops, useful for tests that only assert
// state). logger may be nil.
func NewTurnMachine(cb TurnCallbacks, logger *slog.Logger) *TurnMachine {
	return &TurnMachine{logger: logger, cb: cb, state: StateIdle}
}

// State returns the current state (snapshot; for tests/diagnostics).
func (m *TurnMachine) State() TurnState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Start moves idle -> listening at session bring-up. Idempotent: calling
// it when already live is a no-op.
func (m *TurnMachine) Start() {
	m.mu.Lock()
	if m.state != StateIdle {
		m.mu.Unlock()
		return
	}
	m.transition(StateListening)
	m.mu.Unlock()
}

// Stop forces the machine back to idle (session teardown). Any in-flight
// assistant turn is dropped silently (the orchestrator owns its own
// teardown of the audio pump). Idempotent.
func (m *TurnMachine) Stop() {
	m.mu.Lock()
	m.partial = ""
	m.transition(StateIdle)
	m.mu.Unlock()
}

// OnSpeechStarted feeds a human voice-activity onset (Deepgram
// SpeechStarted, surfaced as polyphon.ASRKindSpeechStarted). Transition
// (A) while listening -> human-turn; transition (C) [barge-in] while
// assistant-turn. A no-op in idle.
func (m *TurnMachine) OnSpeechStarted() {
	m.mu.Lock()
	switch m.state {
	case StateListening, StateHumanTurn:
		m.partial = ""
		m.transition(StateHumanTurn)
		m.mu.Unlock()
		if m.cb.OnSpeechStarted != nil {
			m.cb.OnSpeechStarted()
		}
	case StateAssistantTurn:
		// (C) barge-in: the human took the floor mid-assistant-reply.
		// Cancel the assistant turn, then take the human turn.
		m.partial = ""
		m.transition(StateHumanTurn)
		m.mu.Unlock()
		if m.logger != nil {
			m.logger.Info("voice trace: turntaking event", "stage", "turntaking.bargein.detected")
		}
		if m.cb.OnBargeIn != nil {
			m.cb.OnBargeIn()
		}
	default:
		m.mu.Unlock()
	}
}

// OnInterim feeds an interim (non-final) transcript update. It keeps the
// running partial so a final-before-onset edge (Deepgram commits a phrase
// without a SpeechStarted in front) still has text. While listening, an
// interim is treated as an implicit onset (some short utterances arrive
// as a Results event before any SpeechStarted), moving to human-turn.
func (m *TurnMachine) OnInterim(text string) {
	if text == "" {
		return
	}
	m.mu.Lock()
	m.partial = text
	switch m.state {
	case StateListening:
		m.transition(StateHumanTurn)
	}
	m.mu.Unlock()
}

// OnFinal feeds an end-of-utterance commit (transition D): the human turn
// is over. It reports the committed transcript and returns to listening.
// It NEVER auto-starts assistant audio -- the speak decision is the
// conductor's (OnSpeak). A final while assistant-turn is ignored for
// state (the assistant has the floor) but still reported so the boundary
// is forwarded.
func (m *TurnMachine) OnFinal(text string) {
	if text == "" {
		text = m.snapshotPartial()
	}
	m.mu.Lock()
	switch m.state {
	case StateHumanTurn:
		m.partial = ""
		m.transition(StateListening)
	case StateListening:
		// A final with no preceding onset (very short utterance). Report
		// it; we are already listening.
	}
	m.mu.Unlock()
	if text == "" {
		return
	}
	if m.logger != nil {
		m.logger.Info("voice trace: turntaking event", "stage", "turntaking.eou", "chars", len(text))
	}
	if m.cb.OnFinalTranscript != nil {
		m.cb.OnFinalTranscript(text)
	}
}

// OnSpeak feeds an external "speak now" directive (transition B): the
// conductor decided the assistant should speak. Enters assistant-turn and
// starts playout. If the machine is already in assistant-turn, the new
// directive is dropped (one assistant turn at a time -- the orchestrator's
// one-active-turn discipline owns supersede). Returns true when the
// directive was accepted (assistant-turn entered), false when dropped.
func (m *TurnMachine) OnSpeak(req SpeakDirective) bool {
	m.mu.Lock()
	switch m.state {
	case StateListening, StateHumanTurn:
		m.partial = ""
		m.transition(StateAssistantTurn)
		m.mu.Unlock()
		if m.cb.OnAssistantStart != nil {
			m.cb.OnAssistantStart(req)
		}
		return true
	default:
		// idle (no session) or already assistant-turn: drop.
		m.mu.Unlock()
		if m.logger != nil {
			m.logger.Debug("voice-agent speak directive dropped",
				"state", m.State().String(), "request_id", req.RequestID)
		}
		return false
	}
}

// OnAssistantDone reports that the assistant finished or was cancelled,
// collapsing assistant-turn back to listening. A no-op unless we are in
// assistant-turn (a barge-in already moved us to human-turn, so a late
// "playout done" from the cancelled pump must not clobber the human turn).
func (m *TurnMachine) OnAssistantDone() {
	m.mu.Lock()
	if m.state == StateAssistantTurn {
		m.transition(StateListening)
	}
	m.mu.Unlock()
}

// snapshotPartial returns the current accumulated interim text.
func (m *TurnMachine) snapshotPartial() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.partial
}

// transition sets the state and traces the change. Caller holds m.mu.
func (m *TurnMachine) transition(to TurnState) {
	if m.state == to {
		return
	}
	from := m.state
	m.state = to
	if m.logger != nil {
		m.logger.Debug("voice-agent turn state",
			"from", from.String(), "to", to.String())
	}
}
