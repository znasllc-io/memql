package safety

import (
	"context"
	"sync"
	"testing"
)

// countingRecorder tracks how many times Record was called. Test-
// only sink; protected by mu so concurrent dispatch tests can use it.
type countingRecorder struct {
	mu    sync.Mutex
	calls int
}

func (c *countingRecorder) Record(_ context.Context, _ ActionDescriptor, _ Classification, _ Decision, _ Mode) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}
func (c *countingRecorder) Calls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

// panickyRecorder always panics -- exercises the per-recorder
// panic recovery in FanoutRecorder.
type panickyRecorder struct{}

func (panickyRecorder) Record(_ context.Context, _ ActionDescriptor, _ Classification, _ Decision, _ Mode) {
	panic("intentional test panic")
}

func TestFanoutRecorderCallsEvery(t *testing.T) {
	a := &countingRecorder{}
	b := &countingRecorder{}
	c := &countingRecorder{}
	f := NewFanoutRecorder(a, b, c)
	f.Record(context.Background(), ActionDescriptor{}, Classification{}, DecisionAllow, ModeShadow)
	if a.Calls() != 1 || b.Calls() != 1 || c.Calls() != 1 {
		t.Errorf("each recorder should be called once: a=%d b=%d c=%d", a.Calls(), b.Calls(), c.Calls())
	}
}

func TestFanoutRecorderDropsNilEntries(t *testing.T) {
	a := &countingRecorder{}
	// Nils interleaved with the real one -- must not panic, must
	// dispatch to the non-nil.
	f := NewFanoutRecorder(nil, a, nil)
	f.Record(context.Background(), ActionDescriptor{}, Classification{}, DecisionAllow, ModeShadow)
	if a.Calls() != 1 {
		t.Errorf("non-nil recorder should still fire: calls=%d", a.Calls())
	}
}

func TestFanoutRecorderRecoversPanicInOne(t *testing.T) {
	// Order matters here: panicky in the middle, counting on both
	// sides. After the panic the later recorder must still run.
	before := &countingRecorder{}
	after := &countingRecorder{}
	f := NewFanoutRecorder(before, panickyRecorder{}, after)
	f.Record(context.Background(), ActionDescriptor{}, Classification{}, DecisionAllow, ModeShadow)
	if before.Calls() != 1 {
		t.Errorf("recorder before panic should have fired: %d", before.Calls())
	}
	if after.Calls() != 1 {
		t.Errorf("recorder AFTER panic should still fire (panic recovered per-sink): %d", after.Calls())
	}
}

func TestFanoutRecorderZeroIsNoop(t *testing.T) {
	// Must not panic + must not allocate -- the contract is
	// "best-effort." Just exercise it.
	NewFanoutRecorder().Record(context.Background(), ActionDescriptor{}, Classification{}, DecisionAllow, ModeShadow)
}
