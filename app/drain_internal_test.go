package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/znasllc-io/memql/core/common"
)

// TestApplyShutdownDrain_CallsBeginDrainThenSleeps pins the #552 contract:
// the drain hook fires first (flip readiness to 503), then we keep serving
// for the delay before the Stop sweep.
func TestApplyShutdownDrain_CallsBeginDrainThenSleeps(t *testing.T) {
	began := false
	var slept time.Duration
	applyShutdownDrain(nil,
		func() { began = true },
		3*time.Second,
		func(d time.Duration) { slept = d },
	)
	assert.True(t, began, "BeginDrain must be invoked")
	assert.Equal(t, 3*time.Second, slept, "must sleep for the configured delay")
}

// A zero delay falls back to the package default.
func TestApplyShutdownDrain_ZeroUsesDefault(t *testing.T) {
	var slept time.Duration
	applyShutdownDrain(nil, nil, 0, func(d time.Duration) { slept = d })
	assert.Equal(t, DefaultShutdownDrainDelay, slept)
}

// A negative delay disables the wait entirely (sleep never called).
func TestApplyShutdownDrain_NegativeSkipsSleep(t *testing.T) {
	began := false
	slept := false
	applyShutdownDrain(nil,
		func() { began = true },
		-1,
		func(time.Duration) { slept = true },
	)
	assert.True(t, began, "BeginDrain still fires even when the wait is skipped")
	assert.False(t, slept, "a negative delay must skip the sleep")
}

// A nil BeginDrain hook is tolerated (the delay still applies).
func TestApplyShutdownDrain_NilHook(t *testing.T) {
	var slept time.Duration
	applyShutdownDrain(nil, nil, time.Second, func(d time.Duration) { slept = d })
	assert.Equal(t, time.Second, slept)
}

// --- memql#1269: in-flight drain (waitForInflightDrain) ---

// In-flight work that finishes within the grace window lets the drain proceed
// the instant the count hits zero -- without burning the full grace.
func TestWaitForInflightDrain_FinishesEarlyWhenWorkDrains(t *testing.T) {
	// Three in-flight units; each poll-tick retires one. The wait must return
	// after exactly 3 sleeps (count reaches 0), not the full grace.
	active := int64(3)
	var sleeps int
	waitForInflightDrain(nil,
		func() int64 { return atomic.LoadInt64(&active) },
		10*time.Second,
		func(time.Duration) {
			sleeps++
			atomic.AddInt64(&active, -1)
		},
	)
	assert.Equal(t, 3, sleeps, "must stop polling the moment in-flight hits zero")
	assert.Equal(t, int64(0), atomic.LoadInt64(&active))
}

// In-flight work that OUTLIVES the grace is cut off at the deadline: the wait
// returns after the bounded number of poll-ticks even though work is still
// active (the bounded GracefulStop then forces it closed).
func TestWaitForInflightDrain_CutsOffAtDeadline(t *testing.T) {
	var sleeps int
	var slept time.Duration
	grace := inflightPollInterval * 4 // exactly 4 poll-ticks of budget
	waitForInflightDrain(nil,
		func() int64 { return 5 }, // never drains
		grace,
		func(d time.Duration) {
			sleeps++
			slept += d
		},
	)
	assert.Equal(t, 4, sleeps, "must poll until the grace budget is exhausted, then give up")
	assert.Equal(t, grace, slept, "total wait is bounded by the grace period")
}

// A zero in-flight count up front returns immediately -- nothing to wait for.
func TestWaitForInflightDrain_NoWorkReturnsImmediately(t *testing.T) {
	slept := false
	waitForInflightDrain(nil,
		func() int64 { return 0 },
		10*time.Second,
		func(time.Duration) { slept = true },
	)
	assert.False(t, slept, "no in-flight work must skip the wait entirely")
}

// A nil ActiveWork hook (no in-flight accounting) returns immediately rather
// than blocking the full grace for nothing.
func TestWaitForInflightDrain_NilActiveWorkReturnsImmediately(t *testing.T) {
	slept := false
	waitForInflightDrain(nil, nil, 10*time.Second,
		func(time.Duration) { slept = true })
	assert.False(t, slept, "nil ActiveWork must skip the wait")
}

// A negative grace skips the in-flight wait entirely (work cut off immediately,
// relying on the bounded GracefulStop).
func TestWaitForInflightDrain_NegativeGraceSkips(t *testing.T) {
	slept := false
	waitForInflightDrain(nil,
		func() int64 { return 5 },
		-1,
		func(time.Duration) { slept = true },
	)
	assert.False(t, slept, "a negative grace must skip the in-flight wait")
}

// A zero grace falls back to the package default (work still active forces the
// full default budget; we only assert the fallback path runs at least one poll).
func TestWaitForInflightDrain_ZeroUsesDefault(t *testing.T) {
	var slept time.Duration
	waitForInflightDrain(nil,
		func() int64 { return 1 }, // never drains
		0,
		func(d time.Duration) { slept += d },
	)
	assert.Equal(t, DefaultShutdownGracePeriod, slept,
		"a zero grace must fall back to the default budget")
}

// --- memql#1269: clean startup (DefaultWaitForDependenciesReady) ---

// readyDep is a minimal common.Dependency whose Ready() channel the test
// controls, so we can assert the startup wait blocks until deps are up.
type readyDep struct {
	ready chan struct{}
}

func (d *readyDep) Start(context.Context)               {}
func (d *readyDep) Stop(context.Context)                {}
func (d *readyDep) IsRunning() bool                     { return true }
func (d *readyDep) Order() int                          { return 0 }
func (d *readyDep) ComponentName() common.ComponentName { return common.ComponentName("readyDep") }
func (d *readyDep) Ready() <-chan struct{}              { return d.ready }

// All deps already ready -> the wait returns promptly (clean startup proceeds).
func TestDefaultWaitForDependenciesReady_ReturnsWhenAllReady(t *testing.T) {
	d1 := &readyDep{ready: make(chan struct{})}
	d2 := &readyDep{ready: make(chan struct{})}
	close(d1.ready)
	close(d2.ready)

	done := make(chan struct{})
	go func() {
		DefaultWaitForDependenciesReady([]common.Dependency{d1, d2})
		close(done)
	}()

	select {
	case <-done:
		// proceeded once every dep was ready
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return even though every dependency was ready")
	}
}

// The wait BLOCKS while a dependency is not yet ready, then proceeds the moment
// it becomes ready -- this is the clean-startup guarantee (don't flip Ready
// until deps are actually up).
func TestDefaultWaitForDependenciesReady_BlocksUntilDepReady(t *testing.T) {
	slow := &readyDep{ready: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		DefaultWaitForDependenciesReady([]common.Dependency{slow})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("wait returned before the dependency was ready")
	case <-time.After(50 * time.Millisecond):
		// still blocking, as required
	}

	close(slow.ready)

	select {
	case <-done:
		// proceeded once the dep signalled ready
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after the dependency became ready")
	}
}

// An empty dependency slice is a no-op (non-mesh / subcommand paths).
func TestDefaultWaitForDependenciesReady_EmptyIsNoop(t *testing.T) {
	done := make(chan struct{})
	go func() {
		DefaultWaitForDependenciesReady(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("empty dependency slice must return immediately")
	}
}
