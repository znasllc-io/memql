package agent

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedTransition captures a turn-taking callback firing.
type recordedTransition struct {
	mu             sync.Mutex
	finals         []string
	speaks         []SpeakDirective
	bargeIns       int
	speechStarteds int
}

func (r *recordedTransition) callbacks() TurnCallbacks {
	return TurnCallbacks{
		OnSpeechStarted: func() {
			r.mu.Lock()
			r.speechStarteds++
			r.mu.Unlock()
		},
		OnFinalTranscript: func(text string) {
			r.mu.Lock()
			r.finals = append(r.finals, text)
			r.mu.Unlock()
		},
		OnAssistantStart: func(req SpeakDirective) {
			r.mu.Lock()
			r.speaks = append(r.speaks, req)
			r.mu.Unlock()
		},
		OnBargeIn: func() {
			r.mu.Lock()
			r.bargeIns++
			r.mu.Unlock()
		},
	}
}

func TestTurnMachine_HappyPath_ListenHumanFinalSpeak(t *testing.T) {
	rec := &recordedTransition{}
	m := NewTurnMachine(rec.callbacks(), nil)

	assert.Equal(t, StateIdle, m.State())
	m.Start()
	assert.Equal(t, StateListening, m.State())

	// (A) human onset -> human-turn.
	m.OnSpeechStarted()
	assert.Equal(t, StateHumanTurn, m.State())

	m.OnInterim("hello wor")
	assert.Equal(t, StateHumanTurn, m.State())

	// (D) EOU commit -> listening; reports final; NEVER auto-speaks.
	m.OnFinal("hello world")
	assert.Equal(t, StateListening, m.State(), "EOU must return to listening, not assistant-turn")

	rec.mu.Lock()
	require.Equal(t, []string{"hello world"}, rec.finals)
	assert.Empty(t, rec.speaks, "conductor gate: EOU must not auto-start assistant audio")
	rec.mu.Unlock()

	// (B) external speak directive -> assistant-turn.
	ok := m.OnSpeak(SpeakDirective{Text: "hi there", UtteranceID: "u1"})
	assert.True(t, ok)
	assert.Equal(t, StateAssistantTurn, m.State())
	rec.mu.Lock()
	require.Len(t, rec.speaks, 1)
	assert.Equal(t, "hi there", rec.speaks[0].Text)
	rec.mu.Unlock()

	// Playout complete -> listening.
	m.OnAssistantDone()
	assert.Equal(t, StateListening, m.State())
}

func TestTurnMachine_ConductorGate_FinalNeverAutoSpeaks(t *testing.T) {
	rec := &recordedTransition{}
	m := NewTurnMachine(rec.callbacks(), nil)
	m.Start()

	for i := 0; i < 3; i++ {
		m.OnSpeechStarted()
		m.OnFinal("side chatter")
		assert.Equal(t, StateListening, m.State())
	}
	rec.mu.Lock()
	assert.Len(t, rec.finals, 3)
	assert.Empty(t, rec.speaks, "no auto-response on any EOU (conductor gate)")
	rec.mu.Unlock()
}

func TestTurnMachine_BargeIn_CancelsAssistant(t *testing.T) {
	rec := &recordedTransition{}
	m := NewTurnMachine(rec.callbacks(), nil)
	m.Start()

	require.True(t, m.OnSpeak(SpeakDirective{Text: "long reply"}))
	require.Equal(t, StateAssistantTurn, m.State())

	// (C) human onset while assistant speaks -> barge-in -> human-turn.
	m.OnSpeechStarted()
	assert.Equal(t, StateHumanTurn, m.State())
	rec.mu.Lock()
	assert.Equal(t, 1, rec.bargeIns, "human onset during assistant-turn must raise barge-in")
	rec.mu.Unlock()

	// A late "playout done" from the cancelled pump must not clobber the
	// human turn the barge-in established.
	m.OnAssistantDone()
	assert.Equal(t, StateHumanTurn, m.State())
}

func TestTurnMachine_SpeakDroppedWhenAlreadySpeaking(t *testing.T) {
	m := NewTurnMachine(TurnCallbacks{}, nil)
	m.Start()
	require.True(t, m.OnSpeak(SpeakDirective{Text: "first"}))
	assert.False(t, m.OnSpeak(SpeakDirective{Text: "second"}),
		"only one assistant turn at a time")
	assert.Equal(t, StateAssistantTurn, m.State())
}

func TestTurnMachine_SpeakDroppedWhenIdle(t *testing.T) {
	m := NewTurnMachine(TurnCallbacks{}, nil)
	// No Start() -> idle.
	assert.False(t, m.OnSpeak(SpeakDirective{Text: "nope"}))
	assert.Equal(t, StateIdle, m.State())
}

func TestTurnMachine_InterimImpliesOnset(t *testing.T) {
	m := NewTurnMachine(TurnCallbacks{}, nil)
	m.Start()
	// An interim arriving with no preceding SpeechStarted still enters
	// human-turn (some short utterances skip the VAD onset event).
	m.OnInterim("quick")
	assert.Equal(t, StateHumanTurn, m.State())
}

func TestTurnMachine_FinalFallsBackToPartial(t *testing.T) {
	rec := &recordedTransition{}
	m := NewTurnMachine(rec.callbacks(), nil)
	m.Start()
	m.OnSpeechStarted()
	m.OnInterim("buffered text")
	// EOU with empty text falls back to the accumulated partial.
	m.OnFinal("")
	rec.mu.Lock()
	require.Equal(t, []string{"buffered text"}, rec.finals)
	rec.mu.Unlock()
	assert.Equal(t, StateListening, m.State())
}

func TestTurnMachine_Stop_ReturnsToIdle(t *testing.T) {
	m := NewTurnMachine(TurnCallbacks{}, nil)
	m.Start()
	m.OnSpeechStarted()
	m.Stop()
	assert.Equal(t, StateIdle, m.State())
}
