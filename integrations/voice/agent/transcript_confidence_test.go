package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/openai"
)

// TestTranscriptConfidenceGate_Pass pins the #1431 gate contract: low mean
// logprob drops, high mean passes, and -- CRITICAL -- missing logprobs always
// pass (the signal is intermittently absent even when requested, and absence
// must never be treated as low confidence). A nil gate (never armed) passes
// everything.
func TestTranscriptConfidenceGate_Pass(t *testing.T) {
	g := &transcriptConfidenceGate{floor: -1.0}

	// Confident transcription: mean well above the floor.
	assert.True(t, g.pass([]float64{-0.05, -0.1, -0.2}))
	// Exactly at the floor passes (>=).
	assert.True(t, g.pass([]float64{-1.0, -1.0}))
	// Noise-shaped final: mean below the floor -> dropped.
	assert.False(t, g.pass([]float64{-2.5, -1.8, -3.0}))
	// Missing logprobs ALWAYS pass.
	assert.True(t, g.pass(nil))
	assert.True(t, g.pass([]float64{}))

	// A nil gate is disabled: everything passes.
	var off *transcriptConfidenceGate
	assert.True(t, off.pass([]float64{-9.0}))
}

func TestMeanLogprob(t *testing.T) {
	assert.Equal(t, 0.0, meanLogprob(nil))
	assert.Equal(t, -2.0, meanLogprob([]float64{-1.0, -3.0}))
}

// TestRealtimeExecutor_InputTranscript_ConfidenceGate verifies the #1431 wiring
// end-to-end through the drain loop: with the gate armed, an input-transcription
// FINAL carrying a below-floor mean logprob is dropped (never forwarded as a
// user utterance), while a confident final and a final WITHOUT logprobs both
// still forward. Composes with -- does not replace -- the #1199 denylist filter.
func TestRealtimeExecutor_InputTranscript_ConfidenceGate(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetTurnMode(turnModeNative)
	e.SetTranscriptConfidenceFloor(-1.0)

	// Low-confidence final (mean -2.0 < -1.0): dropped.
	rt.events <- openai.RealtimeServerEvent{
		Kind:     openai.EventInputTranscriptDone,
		Text:     "rush of the fan blades",
		Logprobs: []float64{-1.5, -2.5},
	}
	time.Sleep(150 * time.Millisecond)
	assert.Nil(t, findFinalTranscript(fs), "a below-floor final must not forward as a user utterance")

	// Confident final: forwards.
	rt.events <- openai.RealtimeServerEvent{
		Kind:     openai.EventInputTranscriptDone,
		Text:     "list the ten most beautiful birds",
		Logprobs: []float64{-0.05, -0.1},
	}
	var final *memqlv1.VoiceAgentFinalTranscript
	require.Eventually(t, func() bool {
		final = findFinalTranscript(fs)
		return final != nil
	}, 2*time.Second, 10*time.Millisecond, "a confident final must still forward")
	assert.Equal(t, "list the ten most beautiful birds", final.GetFinalText())
}

// TestRealtimeExecutor_InputTranscript_MissingLogprobsPass verifies the
// never-drop-on-absence contract at the executor level: with the gate armed, a
// final that arrives WITHOUT logprobs passes straight through.
func TestRealtimeExecutor_InputTranscript_MissingLogprobsPass(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetTurnMode(turnModeNative)
	e.SetTranscriptConfidenceFloor(-1.0)

	rt.events <- openai.RealtimeServerEvent{
		Kind: openai.EventInputTranscriptDone,
		Text: "cut the cloud spend",
	}
	var final *memqlv1.VoiceAgentFinalTranscript
	require.Eventually(t, func() bool {
		final = findFinalTranscript(fs)
		return final != nil
	}, 2*time.Second, 10*time.Millisecond, "a final without logprobs must always pass the gate")
	assert.Equal(t, "cut the cloud spend", final.GetFinalText())
}
