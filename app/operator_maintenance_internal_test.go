package app

import (
	"io"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/server"
)

// newOperatorDrainTestApp builds a minimal mesh-style App with the
// operator-drain channel + lifecycle wired the way cluster.go wires them,
// so the memql#1270 operator trigger can be exercised without the full
// dependency graph.
func newOperatorDrainTestApp(t *testing.T) *App {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := node.NewPeerManager(&node.Identity{ID: "n1", Type: node.NodeTypeBFF}, logger)

	a := &App{
		Logger:        logger,
		nodeIdentity:  &node.Identity{ID: "n1", Type: node.NodeTypeBFF},
		nodeLifecycle: pm.Lifecycle(),
		opDrain:       make(chan struct{}),
	}
	a.nodeLifecycle.SetObserver(func(_, newState node.LifecycleState) {
		if newState == node.LifecycleDraining || newState == node.LifecycleStopped {
			server.SetDraining(true)
		}
	})
	return a
}

// TestRequestOperatorDrain_ClosesSignalOnce is the core funnel contract
// (memql#1270): the operator trigger closes the same channel app.Run's
// wait path selects on, exactly once, regardless of how many times it is
// called -- so a second operator call (or a SIGTERM that races it)
// converges on the one drain already under way.
func TestRequestOperatorDrain_ClosesSignalOnce(t *testing.T) {
	a := newOperatorDrainTestApp(t)

	sig := a.OperatorDrainSignal()
	require.NotNil(t, sig, "mesh app must expose an operator-drain signal")
	select {
	case <-sig:
		t.Fatal("drain signal must not be closed before a request")
	default:
	}

	// First request initiates the drain.
	initiated := a.RequestOperatorDrain("debugging bff-2")
	assert.True(t, initiated, "first RequestOperatorDrain must initiate")
	select {
	case <-sig:
		// Closed -- Run's wait path would now proceed into the drain.
	case <-time.After(time.Second):
		t.Fatal("operator drain signal must be closed after the request")
	}
	assert.Equal(t, "debugging bff-2", a.OperatorDrainReason())

	// Second request is a no-op success (already under way).
	assert.False(t, a.RequestOperatorDrain("second"), "second request must not re-initiate")
	assert.Equal(t, "debugging bff-2", a.OperatorDrainReason(), "reason must not be overwritten")
}

// TestRequestOperatorDrain_NoMeshIsUnavailable asserts a non-mesh App (no
// opDrain channel) reports the trigger as unavailable rather than
// pretending to drain.
func TestRequestOperatorDrain_NoMeshIsUnavailable(t *testing.T) {
	a := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	assert.False(t, a.RequestOperatorDrain("x"), "non-mesh App has no operator entrypoint")
	assert.Nil(t, a.OperatorDrainSignal())
}

// TestOperatorTrigger_FunnelsIntoSameDrainSequence ties the trigger to the
// lifecycle: an operator drain, like a SIGTERM, drives the node through
// Draining (readiness flips) and on to the terminal Stopped via the same
// BeginNodeDrain / MarkNodeStopped seams Run calls. This asserts the
// operator path is a TRIGGER into the one mechanism, not a separate one.
func TestOperatorTrigger_FunnelsIntoSameDrainSequence(t *testing.T) {
	server.SetDraining(false)
	t.Cleanup(func() { server.SetDraining(false) })

	a := newOperatorDrainTestApp(t)
	a.MarkNodeReady()
	require.True(t, a.nodeLifecycle.IsReady())
	assert.False(t, server.IsDraining())

	// Operator triggers the drain; Run's wait path observes the closed
	// signal and runs the identical sequence -- here we drive the same
	// seams Run drives after the trigger.
	require.True(t, a.RequestOperatorDrain("manual roll"))
	<-a.OperatorDrainSignal()

	a.BeginNodeDrain()
	require.Equal(t, node.LifecycleDraining, a.nodeLifecycle.State())
	assert.True(t, server.IsDraining(), "operator Draining must de-route the node (readiness 503)")

	a.MarkNodeStopped()
	assert.Equal(t, node.LifecycleStopped, a.nodeLifecycle.State(),
		"operator drain must reach the terminal Stopped state")
}

// TestWaitForShutdownTrigger_OperatorWins asserts the wait multiplexer
// returns the operator trigger when the operator channel closes before any
// OS signal (the SIGTERM wait parks forever in this test).
func TestWaitForShutdownTrigger_OperatorWins(t *testing.T) {
	opDrain := make(chan struct{})
	parkedSignal := func() os.Signal {
		select {} // never returns -- simulates no OS signal
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(opDrain)
	}()

	trigger := waitForShutdownTrigger(parkedSignal, opDrain)
	assert.Equal(t, "operator", trigger)
}

// TestWaitForShutdownTrigger_SignalWins asserts the OS signal still wins
// when it fires first (the deploy SIGTERM path is unchanged).
func TestWaitForShutdownTrigger_SignalWins(t *testing.T) {
	opDrain := make(chan struct{}) // never closed
	wait := func() os.Signal { return syscall.SIGTERM }

	trigger := waitForShutdownTrigger(wait, opDrain)
	assert.Equal(t, "signal:terminated", trigger)
}

// TestWaitForShutdownTrigger_NilOpDrainIsPlainSignal asserts a non-mesh
// binary (nil operator channel) degenerates to the plain OS-signal wait.
func TestWaitForShutdownTrigger_NilOpDrainIsPlainSignal(t *testing.T) {
	wait := func() os.Signal { return syscall.SIGINT }
	trigger := waitForShutdownTrigger(wait, nil)
	assert.Equal(t, "signal:interrupt", trigger)
}
