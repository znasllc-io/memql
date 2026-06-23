package agent

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a deterministic monotonic clock for the lifecycle tests. advance
// moves time forward without sleeping so idle / max-duration windows can be
// crossed instantly. Safe for concurrent use (the watchdog goroutine reads it).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// recordingStop is a mock stop callback. It records every invocation (to assert
// single-shot teardown) and can be configured to fail (stop-callback-failure
// resilience).
type recordingStop struct {
	mu      sync.Mutex
	reasons []TeardownReason
	err     error
}

func (s *recordingStop) fn(reason TeardownReason) error {
	s.mu.Lock()
	s.reasons = append(s.reasons, reason)
	err := s.err
	s.mu.Unlock()
	return err
}

func (s *recordingStop) calls() []TeardownReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TeardownReason(nil), s.reasons...)
}

func (s *recordingStop) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reasons)
}

// newTestLifecycle builds a started lifecycle wired to a fake clock + recording
// stop, with the watchdog NOT auto-driving (a huge interval) so tests drive
// checkExpiry deterministically. humanCount seeds the room. The reflexive
// empty-room teardown is opted IN here so the empty-room teardown tests keep
// exercising that path explicitly (the production default is OFF -- #1449).
func newTestLifecycle(t *testing.T, budget RealtimeBudget, humanCount int) (*RealtimeSessionLifecycle, *fakeClock, *recordingStop) {
	t.Helper()
	clock := newFakeClock()
	stop := &recordingStop{}
	l := NewRealtimeLifecycle(budget, stop.fn, nil,
		withLifecycleClock(clock.Now),
		withWatchdogInterval(time.Hour), // inert; tests call checkExpiry directly
		withLifecyclePartitionID("space-test"),
		withEmptyRoomTeardown(true),
	)
	l.Start(humanCount)
	return l, clock, stop
}

func defaultTestBudget() RealtimeBudget {
	return RealtimeBudget{IdleTimeoutSec: 300, MaxSessionSec: 1800, MaxAudioTokens: 1_000_000}
}

func TestLifecycleTeardownOnEmptyRoom(t *testing.T) {
	l, _, stop := newTestLifecycle(t, defaultTestBudget(), 2)

	l.NoteHumanLeft()
	assert.False(t, l.IsTornDown(), "one human still present")
	assert.Equal(t, 1, l.HumanCount())

	l.NoteHumanLeft()
	assert.True(t, l.IsTornDown(), "last human left -> empty-room teardown")
	assert.Equal(t, ReasonEmptyRoom, l.TeardownReason())
	assert.True(t, l.ShouldDegradeToCascade(), "empty-room is a cost guardrail")
	require.Equal(t, []TeardownReason{ReasonEmptyRoom}, stop.calls())
}

func TestLifecycleZeroHumansStaysWarmByDefault(t *testing.T) {
	// #1449: with the reflexive empty-room teardown OFF (the production default),
	// the population reaching zero must NOT tear the session down -- a mic toggle
	// in the SPA surfaces as a transient participant disconnect+reconnect, and the
	// session must stay warm-but-muted so a re-enable within the grace finds the
	// SAME live session. The idle guardrail (not empty-room) governs a never-re-
	// populated room.
	clock := newFakeClock()
	stop := &recordingStop{}
	l := NewRealtimeLifecycle(defaultTestBudget(), stop.fn, nil,
		withLifecycleClock(clock.Now),
		withWatchdogInterval(time.Hour),
		withLifecyclePartitionID("space-test"),
		// no withEmptyRoomTeardown -> default OFF
	)
	l.Start(1)

	l.NoteHumanLeft()
	assert.False(t, l.IsTornDown(), "zero humans must stay warm-but-muted (no empty-room teardown)")
	assert.Equal(t, 0, l.HumanCount())
	assert.Equal(t, 0, stop.count(), "stop callback must not fire on mic-off")

	// Re-presence (mic back on) bumps the count and the session is still alive.
	l.NoteHumanJoined()
	assert.False(t, l.IsTornDown())
	assert.Equal(t, 1, l.HumanCount())

	// The idle guardrail still bounds a session that genuinely goes quiet.
	clock.advance(301 * time.Second)
	assert.True(t, l.checkExpiry())
	assert.Equal(t, ReasonIdleTimeout, l.TeardownReason(),
		"idle guardrail (not empty-room) tears down a quiet warm session")
}

func TestLifecycleTeardownOnIdle(t *testing.T) {
	l, clock, stop := newTestLifecycle(t, defaultTestBudget(), 1)

	// Just shy of the idle window -> no teardown.
	clock.advance(299 * time.Second)
	assert.False(t, l.checkExpiry())
	assert.False(t, l.IsTornDown())

	// Cross the idle window.
	clock.advance(2 * time.Second)
	assert.True(t, l.checkExpiry())
	assert.True(t, l.IsTornDown())
	assert.Equal(t, ReasonIdleTimeout, l.TeardownReason())
	assert.True(t, l.ShouldDegradeToCascade())
	require.Equal(t, []TeardownReason{ReasonIdleTimeout}, stop.calls())
}

func TestLifecycleIdleClockResetOnEngagement(t *testing.T) {
	l, clock, stop := newTestLifecycle(t, defaultTestBudget(), 1)

	// Almost idle, then the conductor engages -> idle clock resets.
	clock.advance(290 * time.Second)
	l.NoteEngaged()
	clock.advance(290 * time.Second)
	assert.False(t, l.checkExpiry(), "engagement reset the idle clock")
	assert.False(t, l.IsTornDown())

	// Now let it actually go idle past the reset point.
	clock.advance(300 * time.Second)
	assert.True(t, l.checkExpiry())
	assert.Equal(t, ReasonIdleTimeout, l.TeardownReason())
	assert.Equal(t, 1, stop.count())
}

func TestLifecycleTeardownOnMaxDuration(t *testing.T) {
	// Idle disabled so only the wall-clock cap can fire; engage repeatedly to
	// prove max-duration is independent of activity.
	budget := RealtimeBudget{IdleTimeoutSec: 0, MaxSessionSec: 1800, MaxAudioTokens: 0}
	l, clock, stop := newTestLifecycle(t, budget, 1)

	clock.advance(1700 * time.Second)
	l.NoteEngaged()
	assert.False(t, l.checkExpiry())

	clock.advance(200 * time.Second) // total 1900s > 1800s cap
	assert.True(t, l.checkExpiry())
	assert.Equal(t, ReasonMaxDuration, l.TeardownReason())
	assert.True(t, l.ShouldDegradeToCascade())
	require.Equal(t, []TeardownReason{ReasonMaxDuration}, stop.calls())
}

func TestLifecycleMaxDurationWinsOverIdle(t *testing.T) {
	// When both have elapsed, the wall-clock cap is reported.
	budget := RealtimeBudget{IdleTimeoutSec: 300, MaxSessionSec: 600, MaxAudioTokens: 0}
	l, clock, _ := newTestLifecycle(t, budget, 1)

	clock.advance(601 * time.Second) // both idle (>300) and duration (>600) tripped
	assert.True(t, l.checkExpiry())
	assert.Equal(t, ReasonMaxDuration, l.TeardownReason())
}

func TestLifecycleTeardownOnTokenBudget(t *testing.T) {
	budget := RealtimeBudget{IdleTimeoutSec: 0, MaxSessionSec: 0, MaxAudioTokens: 1000}
	l, _, stop := newTestLifecycle(t, budget, 1)

	l.NoteAudioTokens(400)
	assert.False(t, l.IsTornDown())
	assert.Equal(t, 400, l.AudioTokensUsed())

	l.NoteAudioTokens(700) // 1100 >= 1000
	assert.True(t, l.IsTornDown())
	assert.Equal(t, ReasonTokenBudget, l.TeardownReason())
	assert.True(t, l.ShouldDegradeToCascade())
	require.Equal(t, []TeardownReason{ReasonTokenBudget}, stop.calls())
}

func TestLifecycleNegativeTokensClamped(t *testing.T) {
	budget := RealtimeBudget{MaxAudioTokens: 1000}
	l, _, stop := newTestLifecycle(t, budget, 1)

	l.NoteAudioTokens(-500)
	assert.Equal(t, 0, l.AudioTokensUsed(), "negative usage clamped to zero")
	assert.False(t, l.IsTornDown())
	assert.Equal(t, 0, stop.count())
}

func TestLifecycleSingleShotIdempotency(t *testing.T) {
	l, _, stop := newTestLifecycle(t, defaultTestBudget(), 2)

	l.RequestTeardown(ReasonExternal)
	require.Equal(t, []TeardownReason{ReasonExternal}, stop.calls())

	// Every subsequent event/teardown is a no-op: first reason wins, stop runs once.
	l.RequestTeardown(ReasonExternal)
	l.NoteHumanLeft()
	l.NoteHumanLeft()
	l.NoteAudioTokens(10_000_000)
	l.NoteEngaged()
	assert.True(t, l.checkExpiry())

	assert.Equal(t, 1, stop.count(), "stop callback runs exactly once")
	assert.Equal(t, ReasonExternal, l.TeardownReason(), "first reason wins")
	assert.False(t, l.ShouldDegradeToCascade(), "external is a normal stop, not a cost guardrail")
}

func TestLifecycleStopCallbackFailureResilience(t *testing.T) {
	clock := newFakeClock()
	stop := &recordingStop{err: errors.New("stop boom")}
	l := NewRealtimeLifecycle(defaultTestBudget(), stop.fn, nil,
		withLifecycleClock(clock.Now),
		withWatchdogInterval(time.Hour),
	)
	l.Start(1)

	// Stop callback errors, but teardown state must still settle (no wedge, no
	// re-fire) so later events are no-ops.
	l.RequestTeardown(ReasonTokenBudget)
	assert.True(t, l.IsTornDown())
	assert.Equal(t, ReasonTokenBudget, l.TeardownReason())

	l.NoteAudioTokens(10_000_000)
	l.NoteHumanLeft()
	assert.Equal(t, 1, stop.count(), "torn-down state held despite stop failure")
}

func TestLifecycleDisabledGuardrails(t *testing.T) {
	// All ceilings <= 0 -> every time-based guardrail disabled. Only explicit
	// teardown (external / empty-room) can stop the session.
	budget := RealtimeBudget{IdleTimeoutSec: 0, MaxSessionSec: 0, MaxAudioTokens: 0}
	assert.False(t, budget.IdleEnabled())
	assert.False(t, budget.DurationEnabled())
	assert.False(t, budget.TokensEnabled())

	l, clock, stop := newTestLifecycle(t, budget, 1)

	clock.advance(10 * time.Hour)
	assert.False(t, l.checkExpiry(), "no time guardrail can fire when disabled")
	l.NoteAudioTokens(50_000_000)
	assert.False(t, l.IsTornDown(), "token guardrail disabled")
	assert.Equal(t, 0, stop.count())

	// Empty-room teardown still applies (it is participant-driven, not budget-gated).
	l.NoteHumanLeft()
	assert.True(t, l.IsTornDown())
	assert.Equal(t, ReasonEmptyRoom, l.TeardownReason())
}

func TestLifecycleDegradeToCascadeClassification(t *testing.T) {
	cases := []struct {
		reason  TeardownReason
		degrade bool
	}{
		{ReasonEmptyRoom, true},
		{ReasonIdleTimeout, true},
		{ReasonMaxDuration, true},
		{ReasonTokenBudget, true},
		{ReasonExternal, false},
		{ReasonRoomClosed, false},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.degrade, tc.reason.IsCostGuardrail(),
			"reason %q cost-guardrail classification", tc.reason)
	}
}

func TestLifecycleRoomClosedDoesNotDegrade(t *testing.T) {
	l, _, _ := newTestLifecycle(t, defaultTestBudget(), 1)
	l.RequestTeardown(ReasonRoomClosed)
	assert.True(t, l.IsTornDown())
	assert.False(t, l.ShouldDegradeToCascade(), "normal room close does not degrade to cascade")
}

func TestLifecycleWatchdogGoroutineFires(t *testing.T) {
	// Exercise the real goroutine driver (not just checkExpiry) with a short
	// interval + a real-time idle window, so the polling path is covered.
	clock := newFakeClock()
	stop := &recordingStop{}
	var stopped atomic.Bool
	l := NewRealtimeLifecycle(
		RealtimeBudget{IdleTimeoutSec: 1, MaxSessionSec: 0, MaxAudioTokens: 0},
		func(r TeardownReason) error { stopped.Store(true); return stop.fn(r) },
		nil,
		withLifecycleClock(clock.Now),
		withWatchdogInterval(10*time.Millisecond),
	)
	l.Start(1)

	// Push the fake clock past the 1s idle window; the watchdog ticker fires
	// every 10ms and re-reads the clock, so teardown lands quickly.
	clock.advance(2 * time.Second)

	require.Eventually(t, func() bool { return stopped.Load() }, 2*time.Second, 5*time.Millisecond,
		"watchdog goroutine should tear the idle session down")
	assert.Equal(t, ReasonIdleTimeout, l.TeardownReason())
	assert.Equal(t, 1, stop.count())
}

func TestLifecycleCloseStopsWatchdogWithoutTeardown(t *testing.T) {
	clock := newFakeClock()
	stop := &recordingStop{}
	l := NewRealtimeLifecycle(defaultTestBudget(), stop.fn, nil,
		withLifecycleClock(clock.Now),
		withWatchdogInterval(10*time.Millisecond),
	)
	l.Start(1)
	l.Close() // stops the watchdog; no teardown reason

	// Even after the idle window elapses, Close already silenced the watchdog and
	// no stop callback should have fired.
	clock.advance(2 * time.Hour)
	time.Sleep(50 * time.Millisecond)
	assert.False(t, l.IsTornDown())
	assert.Equal(t, 0, stop.count(), "Close does not invoke the stop callback")
}

func TestLifecycleStartIdempotent(t *testing.T) {
	l, clock, _ := newTestLifecycle(t, defaultTestBudget(), 1)
	firstStart := l.startedAt

	clock.advance(100 * time.Second)
	l.Start(5) // second Start must be a no-op (no clock/human reseed)
	assert.Equal(t, firstStart, l.startedAt)
	assert.Equal(t, 1, l.HumanCount())
}

func TestBudgetFromConfig(t *testing.T) {
	cfg := Config{
		RealtimeIdleTimeoutSec: 300,
		RealtimeMaxSessionSec:  1800,
		RealtimeMaxAudioTokens: 1_000_000,
	}
	b := RealtimeBudgetFromConfig(cfg)
	assert.Equal(t, 300, b.IdleTimeoutSec)
	assert.Equal(t, 1800, b.MaxSessionSec)
	assert.Equal(t, 1_000_000, b.MaxAudioTokens)
	assert.True(t, b.IdleEnabled())
	assert.True(t, b.DurationEnabled())
	assert.True(t, b.TokensEnabled())
}
