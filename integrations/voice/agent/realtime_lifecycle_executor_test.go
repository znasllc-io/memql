package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/openai"
)

// TestExecutorFeedsLifecycleEngage verifies the executor feeds NoteEngaged to the
// attached lifecycle on a conductor engage, so the idle clock is reset when the
// assistant actually speaks (#459 wiring in onAssistantStart).
func TestExecutorFeedsLifecycleEngage(t *testing.T) {
	fs := newFakeStream()
	replyToTurn(fs, "Here is the answer.")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	clock := newFakeClock()
	stop := &recordingStop{}
	l := NewRealtimeLifecycle(defaultTestBudget(), stop.fn, nil,
		withLifecycleClock(clock.Now), withWatchdogInterval(time.Hour))
	e.AttachLifecycle(l)
	l.Start(1)

	// Advance most of the idle window, then drive an engage; the engage must
	// reset the idle clock so a subsequent checkExpiry does not fire.
	clock.advance(290 * time.Second)
	e.handleASRResult("user-1", polyphon.ASRResult{Text: "what's the plan", IsFinal: true})
	require.Eventually(t, func() bool { return rt.responseCount() == 1 },
		2*time.Second, 10*time.Millisecond, "engage drives a response.create")

	clock.advance(290 * time.Second) // 580s since start, but only 290s since engage
	assert.False(t, l.checkExpiry(), "engage reset the idle clock")
	assert.False(t, l.IsTornDown())
}

// TestExecutorFeedsLifecycleTokenBudget verifies a completed response's audio-token
// usage (parsed onto EventResponseDone) is fed to the lifecycle and trips the
// per-session budget, tearing the session down + flagging degrade-to-cascade.
func TestExecutorFeedsLifecycleTokenBudget(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	stop := &recordingStop{}
	l := NewRealtimeLifecycle(
		RealtimeBudget{MaxAudioTokens: 1000},
		stop.fn, nil,
		withLifecycleClock(newFakeClock().Now), withWatchdogInterval(time.Hour))
	e.AttachLifecycle(l)
	l.Start(1)

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, ResponseID: "r1", AudioTokens: 600}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, ResponseID: "r2", AudioTokens: 600}

	require.Eventually(t, func() bool { return l.IsTornDown() },
		2*time.Second, 10*time.Millisecond, "crossing the audio-token budget tears the session down")
	assert.Equal(t, ReasonTokenBudget, l.TeardownReason())
	assert.True(t, l.ShouldDegradeToCascade())
}

// TestExecutorZeroTokenDoneDoesNotTeardown verifies a zero-audio-token
// response.done (no usage block) does NOT feed the budget, while a subsequent
// positive-token done does -- the executor only forwards usage when present.
func TestExecutorZeroTokenDoneDoesNotTeardown(t *testing.T) {
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, newFakeStream(), rt)

	stop := &recordingStop{}
	l := NewRealtimeLifecycle(RealtimeBudget{MaxAudioTokens: 100}, stop.fn, nil,
		withLifecycleClock(newFakeClock().Now), withWatchdogInterval(time.Hour))
	e.AttachLifecycle(l)
	l.Start(1)

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, AudioTokens: 0}
	// Give the drain a moment; a zero-token done must NOT tear down.
	time.Sleep(50 * time.Millisecond)
	assert.False(t, l.IsTornDown())

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, AudioTokens: 150}
	require.Eventually(t, func() bool { return l.IsTornDown() },
		2*time.Second, 10*time.Millisecond)
	assert.Equal(t, ReasonTokenBudget, l.TeardownReason())
}
