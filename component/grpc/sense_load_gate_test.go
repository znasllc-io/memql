package memql

// sense_load_gate_test.go -- a stream runs at most one Sense load pass at a
// time, answers every request, and stops with the stream (memql#5434 review).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// blockingLoads is a loading registry whose every load pass waits to be
// released -- or for its context to end -- and which counts how many run at
// once.
type blockingLoads struct {
	loadingRegistry
	release chan struct{}

	mu        sync.Mutex
	calls     int
	inFlight  int
	maxFlight int
	cancelled int
}

func newBlockingLoads() *blockingLoads {
	return &blockingLoads{release: make(chan struct{})}
}

func (b *blockingLoads) LoadDiagnostics(ctx context.Context, source, filePath string) []sense.Diagnostic {
	b.mu.Lock()
	b.calls++
	b.inFlight++
	b.maxFlight = max(b.maxFlight, b.inFlight)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	select {
	case <-b.release:
		return b.loadingRegistry.LoadDiagnostics(ctx, source, filePath)
	case <-ctx.Done():
		b.mu.Lock()
		b.cancelled++
		b.mu.Unlock()
		return nil
	}
}

func (b *blockingLoads) state() (calls, inFlight, maxFlight, cancelled int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.inFlight, b.maxFlight, b.cancelled
}

const gateTestSource = "@unbounded(\"fixture\")\nquery widget shiningBetaWidgets {\n  filter row => row.shine == true\n}\n"

func gateSession(t *testing.T, reg *blockingLoads, ctx context.Context) (*streamSession, *captureStream) {
	t.Helper()
	s, cs := newAuthoringSession(t, auth.RoleWriter, "writer-1")
	cs.ctx = ctx
	s.service.sense = sense.New(reg)
	return s, cs
}

func sendDiagnose(t *testing.T, s *streamSession, requestId, filePath string) {
	t.Helper()
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m-" + requestId,
		Payload: &memqlv1.MemqlClientMessage_SenseDiagnose{SenseDiagnose: &memqlv1.SenseDiagnoseMsg{
			RequestId: requestId, Source: gateTestSource, FilePath: filePath,
		}},
	}
	require.NoError(t, s.handleSenseDiagnose(env, env.GetSenseDiagnose()))
}

// replyTo returns the Diagnose reply for requestId, or nil when none was sent.
func replyTo(cs *captureStream, requestId string) *memqlv1.SenseDiagnoseResult {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, m := range cs.sent {
		if r := m.GetSenseDiagnoseResult(); r != nil && r.GetRequestId() == requestId {
			return r
		}
	}
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func gateWaiting(s *streamSession) bool {
	s.senseLoad.mu.Lock()
	defer s.senseLoad.mu.Unlock()
	return s.senseLoad.waiting != nil
}

func carriesLoadRefusal(r *memqlv1.SenseDiagnoseResult) bool {
	for _, d := range r.GetDiagnostics() {
		if d.GetCode() == "lower_unknown_field" {
			return true
		}
	}
	return false
}

// A stream typing as fast as it likes runs one load at a time. The requests in
// between are answered at once, with Diagnose's diagnostics and no load; the
// newest one waits its turn and gets the load's answer.
func TestSenseDiagnose_OneLoadPassInFlightPerStream(t *testing.T) {
	reg := newBlockingLoads()
	s, cs := gateSession(t, reg, context.Background())

	sendDiagnose(t, s, "r1", "beta/queries.memql")
	eventually(t, "r1's load to start", func() bool { _, n, _, _ := reg.state(); return n == 1 })

	sendDiagnose(t, s, "r2", "beta/queries.memql")
	eventually(t, "r2 to wait behind r1, or start a load of its own", func() bool {
		calls, _, _, _ := reg.state()
		return gateWaiting(s) || calls > 1
	})
	_, _, maxFlight, _ := reg.state()
	require.Equal(t, 1, maxFlight, "a second request on the stream started a second load while the first ran")
	sendDiagnose(t, s, "r3", "beta/queries.memql")
	eventually(t, "r2 to be answered when r3 displaces it", func() bool { return replyTo(cs, "r2") != nil })
	sendDiagnose(t, s, "r4", "beta/queries.memql")
	eventually(t, "r3 to be answered when r4 displaces it", func() bool { return replyTo(cs, "r3") != nil })

	for _, id := range []string{"r2", "r3"} {
		assert.False(t, carriesLoadRefusal(replyTo(cs, id)), "%s was displaced: answered with Diagnose's diagnostics, no load", id)
	}
	assert.Nil(t, replyTo(cs, "r1"), "r1's load is still running")

	reg.release <- struct{}{} // r1's pass
	eventually(t, "r1's answer", func() bool { return replyTo(cs, "r1") != nil })
	assert.True(t, carriesLoadRefusal(replyTo(cs, "r1")), "r1 ran its load")
	eventually(t, "r4's load to start", func() bool { c, n, _, _ := reg.state(); return c == 2 && n == 1 })
	reg.release <- struct{}{} // r4's pass
	eventually(t, "r4's answer", func() bool { return replyTo(cs, "r4") != nil })
	assert.True(t, carriesLoadRefusal(replyTo(cs, "r4")), "the newest request ran its load")

	calls, _, maxFlight, _ := reg.state()
	assert.Equal(t, 1, maxFlight, "one load pass in flight per stream")
	assert.Equal(t, 2, calls, "r2 and r3 were displaced before their loads ran")
}

// The fast answer is never held behind a load: a Diagnose with no file_path is
// answered while the stream's load pass is still running.
func TestSenseDiagnose_TheFastAnswerIsNotHeldByALoad(t *testing.T) {
	reg := newBlockingLoads()
	s, cs := gateSession(t, reg, context.Background())
	sendDiagnose(t, s, "r1", "beta/queries.memql")
	eventually(t, "r1's load to start", func() bool { _, n, _, _ := reg.state(); return n == 1 })

	sendDiagnose(t, s, "fast", "")
	eventually(t, "the path-less request's answer", func() bool { return replyTo(cs, "fast") != nil })

	reg.release <- struct{}{}
	eventually(t, "r1's answer", func() bool { return replyTo(cs, "r1") != nil })
}

// The stream's end stops the load in flight and the one waiting: nothing runs
// for a client that has gone.
func TestSenseDiagnose_LoadPassEndsWithTheStream(t *testing.T) {
	reg := newBlockingLoads()
	ctx, cancel := context.WithCancel(context.Background())
	s, _ := gateSession(t, reg, ctx)

	sendDiagnose(t, s, "r1", "beta/queries.memql")
	eventually(t, "r1's load to start", func() bool { _, n, _, _ := reg.state(); return n == 1 })
	sendDiagnose(t, s, "r2", "beta/queries.memql")
	eventually(t, "r2 to wait behind r1, or start a load of its own", func() bool {
		calls, _, _, _ := reg.state()
		return gateWaiting(s) || calls > 1
	})

	cancel()
	eventually(t, "the load in flight to stop with its stream", func() bool { _, n, _, c := reg.state(); return n == 0 && c == 1 })
	eventually(t, "the gate to empty", func() bool {
		s.senseLoad.mu.Lock()
		defer s.senseLoad.mu.Unlock()
		return !s.senseLoad.running && s.senseLoad.waiting == nil
	})
	calls, _, _, _ := reg.state()
	assert.Equal(t, 1, calls, "r2's load never ran: its stream had ended")
}

// The gate's own rules, without a stream: a waiter displaced by a newer one
// runs nothing, and the gate hands itself on in order.
func TestSenseLoadGate_HandsOnAndDisplaces(t *testing.T) {
	var g senseLoadGate
	ctx := context.Background()
	hold := make(chan struct{})
	started := make(chan struct{})
	var ran []string
	var mu sync.Mutex
	record := func(name string) func(context.Context) {
		return func(context.Context) { mu.Lock(); ran = append(ran, name); mu.Unlock() }
	}

	first := make(chan bool, 1)
	go func() {
		first <- g.run(ctx, func(context.Context) { close(started); <-hold })
	}()
	<-started
	second, third := make(chan bool, 1), make(chan bool, 1)
	go func() { second <- g.run(ctx, record("second")) }()
	eventually(t, "second to wait", func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.waiting != nil })
	go func() { third <- g.run(ctx, record("third")) }()
	assert.False(t, <-second, "second was displaced by third")
	close(hold)
	assert.True(t, <-first)
	assert.True(t, <-third)
	assert.Equal(t, []string{"third"}, ran)
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.False(t, g.running, "the gate is free once the last pass ends")
}
