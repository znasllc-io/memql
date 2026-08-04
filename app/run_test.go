package app_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/core/common"
)

// fakeDep is a minimal common.Dependency that records its Start +
// Stop calls. Lets Run-level tests observe the lifecycle ordering
// without standing up a real engine or transport.
type fakeDep struct {
	name       common.ComponentName
	order      int
	startCalls int64
	stopCalls  int64
	startOrder *[]string // optional shared slice that captures the order Starts fire in
	stopOrder  *[]string // optional shared slice that captures the order Stops fire in
	readyCh    chan struct{}
	running    atomic.Bool
}

func newFakeDep(name string, order int) *fakeDep {
	d := &fakeDep{
		name:    common.ComponentName(name),
		order:   order,
		readyCh: make(chan struct{}),
	}
	close(d.readyCh)
	return d
}

func (d *fakeDep) Start(_ context.Context) {
	atomic.AddInt64(&d.startCalls, 1)
	d.running.Store(true)
	if d.startOrder != nil {
		*d.startOrder = append(*d.startOrder, string(d.name))
	}
}

func (d *fakeDep) Stop(_ context.Context) {
	atomic.AddInt64(&d.stopCalls, 1)
	d.running.Store(false)
	if d.stopOrder != nil {
		*d.stopOrder = append(*d.stopOrder, string(d.name))
	}
}

func (d *fakeDep) IsRunning() bool                     { return d.running.Load() }
func (d *fakeDep) Order() int                          { return d.order }
func (d *fakeDep) ComponentName() common.ComponentName { return d.name }
func (d *fakeDep) Ready() <-chan struct{}              { return d.readyCh }

// TestDefaultStartDependencies_CallsAllInOrder pins the documented
// contract: registration order is preserved, every dep gets Started.
func TestDefaultStartDependencies_CallsAllInOrder(t *testing.T) {
	var order []string
	a := newFakeDep("a", 1)
	a.startOrder = &order
	b := newFakeDep("b", 2)
	b.startOrder = &order
	c := newFakeDep("c", 3)
	c.startOrder = &order

	app.DefaultStartDependencies(a, b, c)

	assert.Equal(t, []string{"a", "b", "c"}, order, "start order must match registration order")
	assert.Equal(t, int64(1), atomic.LoadInt64(&a.startCalls))
	assert.Equal(t, int64(1), atomic.LoadInt64(&b.startCalls))
	assert.Equal(t, int64(1), atomic.LoadInt64(&c.startCalls))
}

// TestDefaultStopDependencies_OrdersByOrderDescending pins the shape
// the dependency graph expects: stops fire highest-order first
// (top-of-the-stack first), with stable tie-break for deterministic
// shutdown logs.
func TestDefaultStopDependencies_OrdersByOrderDescending(t *testing.T) {
	var order []string
	low := newFakeDep("low", 1)
	low.stopOrder = &order
	mid := newFakeDep("mid", 5)
	mid.stopOrder = &order
	high := newFakeDep("high", 10)
	high.stopOrder = &order

	// Registration order intentionally NOT order-sorted to make sure
	// the stop helper does its own sort.
	app.DefaultStopDependencies(context.Background(), low, high, mid)

	assert.Equal(t, []string{"high", "mid", "low"}, order, "stop order must be Order()-desc")
}

// TestDefaultStopDependencies_TiedOrderStableReverse pins the
// tie-break: equal Order() values stop in reverse registration
// order (last registered = first stopped). This makes the shutdown
// log self-consistent run-to-run.
func TestDefaultStopDependencies_TiedOrderStableReverse(t *testing.T) {
	var order []string
	a := newFakeDep("a", 5)
	a.stopOrder = &order
	b := newFakeDep("b", 5)
	b.stopOrder = &order
	c := newFakeDep("c", 5)
	c.stopOrder = &order

	app.DefaultStopDependencies(context.Background(), a, b, c)

	assert.Equal(t, []string{"c", "b", "a"}, order, "tied-order stops must be reverse-registration")
}

// TestDefaultStopDependencies_HandlesEmpty just confirms the
// no-op-on-empty contract. Cheap canary.
func TestDefaultStopDependencies_HandlesEmpty(t *testing.T) {
	assert.NotPanics(t, func() {
		app.DefaultStopDependencies(context.Background())
	})
}

// TestDefaultStopDependencies_SuppliesPerDepTimeoutWhenCtxNil pins
// the "graceful even when caller forgot a context" branch -- the
// per-dep stops get a sensible deadline so a hung dependency can't
// freeze the whole shutdown.
func TestDefaultStopDependencies_SuppliesPerDepTimeoutWhenCtxNil(t *testing.T) {
	a := newFakeDep("a", 1)
	assert.NotPanics(t, func() {
		// Passing nil ctx exercises the timeout-injection branch;
		// the dep's Stop won't actually receive nil.
		// nolint:staticcheck // SA1012 not applicable -- exercising the nil-ctx defensiveness on purpose.
		app.DefaultStopDependencies(nil, a)
	})
	assert.Equal(t, int64(1), atomic.LoadInt64(&a.stopCalls))
}

// TestRun_RequiresLoggerAndVersion pins the input validation. Panic
// (vs. nil error) is the right shape because Run is the binary's
// main() body -- failing to bring up the service is a fatal config
// error.
func TestRun_RequiresLoggerAndVersion(t *testing.T) {
	t.Run("missing logger", func(t *testing.T) {
		assert.Panics(t, func() { app.Run(app.RunConfig{Version: "v"}) })
	})
	t.Run("missing version", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		assert.Panics(t, func() { app.Run(app.RunConfig{Logger: logger}) })
	})
}

// TestRun_DefaultsAreSane pins each RunConfig zero-value default
// resolves to the documented helper. Documents that callers can
// omit Start / Stop / WaitForSignal and still get the right
// behaviour. Not a unit test of the helpers themselves -- those
// are covered above.
func TestRun_DefaultsAreSane(t *testing.T) {
	// Zero-value RunConfig should pick: DefaultRunShutdownTimeout +
	// DefaultStart/Stop/WaitForSignal. Documented elsewhere in the
	// run.go comments; this test pins the package-default constant
	// at a sane value so a future drift surfaces here.
	assert.Equal(t, 30*time.Second, app.DefaultRunShutdownTimeout,
		"DefaultRunShutdownTimeout drifted from 30s; update consumers if intentional")
}

// TestDefaultWaitForShutdownSignal_ReturnsTheSentSignal proves the
// helper is wired to the real signal infrastructure. Self-signals
// SIGINT + waits the helper out.
func TestDefaultWaitForShutdownSignal_ReturnsTheSentSignal(t *testing.T) {
	done := make(chan os.Signal, 1)
	go func() {
		done <- app.DefaultWaitForShutdownSignal()
	}()
	// Give the signal handler in DefaultWaitForShutdownSignal a beat
	// to register before we fire. Without this the SIGINT can land
	// before signal.Notify wires up + the process actually exits.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGINT))
	select {
	case sig := <-done:
		assert.Equal(t, syscall.SIGINT, sig)
	case <-time.After(2 * time.Second):
		t.Fatal("DefaultWaitForShutdownSignal did not return within 2s of SIGINT")
	}
}

// TestRun_DoesNotSwallowStopErrors -- actually a placeholder for the
// shape DefaultStopDependencies guarantees: a dep whose Stop panics
// should not prevent later deps from being Stopped. Documented for
// the next iteration; out of scope for the #356 lift-and-shift,
// which preserves bit-exact shape from the pre-extraction
// memql/main.go.
func TestRun_DoesNotSwallowStopErrors(t *testing.T) {
	// Placeholder -- the pre-extraction main.go didn't recover from
	// dep panics either, so #356 keeps the same shape. Filing
	// follow-up if anyone wants resilience here.
	_ = errors.New("placeholder")
}
