package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/openai"
)

// fakeSpeakingSender records the VoiceAgentRealtimeSpeaking signals the executor
// emits (#1421). Fire-and-forget: Send always succeeds unless err is set.
type fakeSpeakingSender struct {
	mu   sync.Mutex
	sent []*memqlv1.VoiceAgentRealtimeSpeaking
	err  error
}

func (f *fakeSpeakingSender) Send(_ context.Context, env *memqlv1.MemqlClientMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sig := env.GetVoiceAgentRealtimeSpeaking(); sig != nil {
		f.sent = append(f.sent, sig)
	}
	return f.err
}

func (f *fakeSpeakingSender) states() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.sent))
	for i, s := range f.sent {
		out[i] = s.GetSpeaking()
	}
	return out
}

func (f *fakeSpeakingSender) last() *memqlv1.VoiceAgentRealtimeSpeaking {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[len(f.sent)-1]
}

// newSpeakingExecutor builds an executor with a fakeSpeakingSender swapped in,
// not started (the tests drive emitSpeaking / the drains directly).
func newSpeakingExecutor(t *testing.T) (*RealtimeExecutor, *fakeSpeakingSender) {
	t.Helper()
	rt := newFakeRealtimeSession()
	sink := &recordingSink{}
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{
		SpaceID:   "s1",
		GaAgentID: "s1-ga",
	}, nil, rt, sink, SessionPersona{Instructions: "be helpful"}, nil)
	t.Cleanup(e.Close)
	sender := &fakeSpeakingSender{}
	e.speakingSender = sender
	return e, sender
}

// TestEmitSpeaking_WireShape verifies the responding signal carries the GA
// attribution and the speaking flag.
func TestEmitSpeaking_WireShape(t *testing.T) {
	e, sender := newSpeakingExecutor(t)

	e.emitSpeaking(true)

	sig := sender.last()
	require.NotNil(t, sig, "a speaking signal is emitted")
	assert.Equal(t, "s1", sig.GetSpaceId())
	assert.Equal(t, "s1-ga", sig.GetGaAgentId())
	assert.True(t, sig.GetSpeaking(), "first emit is responding")
}

// TestEmitSpeaking_Dedup verifies one response emits exactly one responding even
// though drainAudioOut calls emitSpeaking(true) only on the first frame but the
// guard must also absorb a redundant true; and that idle only fires after a
// responding (a done with no prior audio is a no-op).
func TestEmitSpeaking_Dedup(t *testing.T) {
	e, sender := newSpeakingExecutor(t)

	// Two trues in a row (defensive: only the first should emit responding).
	e.emitSpeaking(true)
	e.emitSpeaking(true)
	assert.Equal(t, []bool{true}, sender.states(), "exactly one responding per response")

	// Done flips back to idle exactly once.
	e.emitSpeaking(false)
	e.emitSpeaking(false)
	assert.Equal(t, []bool{true, false}, sender.states(), "exactly one idle after responding")
}

// TestEmitSpeaking_DoneWithoutAudioIsNoop verifies a response.done that never
// went audible (cancelled / empty) does not write a spurious idle.
func TestEmitSpeaking_DoneWithoutAudioIsNoop(t *testing.T) {
	e, sender := newSpeakingExecutor(t)

	e.emitSpeaking(false)
	assert.Empty(t, sender.states(), "idle without a prior responding is suppressed")
}

// TestEmitSpeaking_NilSenderNoPanic verifies the signal is a no-op when no
// sender is wired (voice still plays; only the orb animation is skipped).
func TestEmitSpeaking_NilSenderNoPanic(t *testing.T) {
	e, _ := newSpeakingExecutor(t)
	e.speakingSender = nil
	assert.NotPanics(t, func() { e.emitSpeaking(true) })
}

// TestRealtimeExecutor_SpeakingSignal_FirstAudioThenDone is the end-to-end
// drain test: a response.created arms the first-audio stamp, the first output
// audio frame emits responding, and response.done emits idle -- in that order.
// This pins the ordering relative to the gate-publish idle reset: responding is
// only produced once the model goes audible (after the directive is published),
// so it lands AFTER cognition's gate-publish idle and is not stomped.
func TestRealtimeExecutor_SpeakingSignal_FirstAudioThenDone(t *testing.T) {
	rt := newFakeRealtimeSession()
	sink := &recordingSink{}
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{
		SpaceID:   "s1",
		GaAgentID: "s1-ga",
	}, nil, rt, sink, SessionPersona{Instructions: "be helpful"}, nil)
	t.Cleanup(e.Close)
	sender := &fakeSpeakingSender{}
	e.speakingSender = sender
	e.Start()

	// Arm the first-audio stamp the way the model's response.created does.
	// Set it directly so the test does not depend on cross-goroutine ordering
	// between drainEvents (which handles response.created) and drainAudioOut
	// (which reads the flag) -- in production they are driven by the model's
	// own ordered event+audio streams.
	e.firstAudioPending.Store(true)

	// First audio frame -> responding. Multiple frames -> still one responding.
	rt.audioOut <- []byte{0x01, 0x02}
	rt.audioOut <- []byte{0x03, 0x04}

	require.Eventually(t, func() bool {
		return len(sender.states()) == 1
	}, time.Second, 5*time.Millisecond, "first audio frame emits exactly one responding")
	assert.Equal(t, []bool{true}, sender.states())

	// Response done -> idle.
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, Status: "completed"}

	require.Eventually(t, func() bool {
		return len(sender.states()) == 2
	}, time.Second, 5*time.Millisecond, "response.done emits idle")
	assert.Equal(t, []bool{true, false}, sender.states(), "responding then idle, in order")
}
