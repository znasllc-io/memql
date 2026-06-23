package agent

import (
	"log/slog"
	"sync"
	"time"
)

// realtime_lifecycle.go is the Go analog of the RealtimeSessionLifecycle half of
// the Python voice-agent's realtime_lifecycle.py (#439 -> #459). It bounds one
// realtime speech-to-speech session in a multi-party room so that an always-on
// model listening to a polyphon room (up to 5 humans + 1 assistant) cannot burn
// audio tokens unbounded. It owns *when* a warm session lives, mutes, and dies;
// the executor (realtime_executor.go) stays the turn-taking / wire driver.
//
// It is PURE Go and CGO-free -- timers, counters, a mutex, and an injected stop
// callback. The watchdog is the Go analog of the Python asyncio poll loop: a
// single goroutine driven by a time.Ticker that re-reads idle / max-duration
// deltas against the live budget each tick, so an engagement event resetting the
// idle clock is honoured on the next tick. For deterministic unit tests the
// clock is injectable (a fake monotonic source) and the guardrail check is
// reachable directly (checkExpiry) without sleeping; the goroutine is only the
// production driver.
//
// Lifecycle decision (parity with the Python module): warm-but-muted (option a).
// The realtime session is built with turn_detection:null and the conductor is
// the sole driver of response.create (see realtime_executor.go / #457), so a
// built-but-not-yet-engaged session already sits warm and muted. This module
// makes the standing cost bounded:
//
//   - Empty-room teardown -- when the last human leaves, the warm session is torn
//     down immediately (ReasonEmptyRoom).
//   - Idle teardown -- if the conductor never engages within IdleTimeoutSec, the
//     session is torn down (ReasonIdleTimeout).
//   - Max-duration teardown -- a hard wall-clock backstop (ReasonMaxDuration).
//   - Token-budget teardown -- the running audio-token total crossing
//     MaxAudioTokens tears the session down (ReasonTokenBudget) and signals the
//     caller to degrade to the cascade.
//
// Degrade-to-cascade: every teardown carries a TeardownReason. When the reason
// is a cost guardrail (TeardownReason.IsCostGuardrail), ShouldDegradeToCascade
// is true so the caller can rebuild on the proven cascade path -- the same
// posture executor_select.go uses on a realtime build failure. This module does
// not itself rebuild; it decides THAT teardown should happen and WHY, and invokes
// the injected stop callback exactly once.

// realtimeStopFunc actually stops the realtime session. Injected so the
// lifecycle is testable without a live model: in the executor wiring it closes
// the session; in tests it records the call. A non-nil error is logged but does
// NOT unwind the torn-down state -- the session is considered down regardless so
// no later event re-fires teardown. Mirrors the Python StopCallback.
type realtimeStopFunc func(reason TeardownReason) error

// realtimeClock is a monotonic clock source, injectable so tests advance time
// without sleeping. Defaults to time.Now in NewRealtimeLifecycle. Mirrors the
// Python Clock seam.
type realtimeClock func() time.Time

// defaultWatchdogInterval is how often the production watchdog goroutine
// re-evaluates idle / max-duration expiry. Small relative to the guardrail
// windows (idle default 300s, max default 1800s) so teardown fires within one
// tick of the deadline.
const defaultWatchdogInterval = 5 * time.Second

// RealtimeSessionLifecycle owns the lifecycle of one realtime session in a
// multi-party room. Construction does NOT start the session -- the caller builds
// the model + session and the executor, then calls Start, which records the
// start time, seeds the room population, and (when an idle / duration guardrail
// is enabled) arms the watchdog goroutine. The caller feeds it room + activity
// events:
//
//   - NoteHumanJoined / NoteHumanLeft -- room participant lifecycle. The last
//     human leaving triggers an empty-room teardown.
//   - NoteEngaged -- the conductor drove a response.create; resets the idle clock.
//   - NoteAudioTokens -- usage reported for a completed response; trips the
//     token-budget guardrail when the running total crosses the ceiling.
//   - RequestTeardown -- external stop (kill switch / operator / session end).
//
// Teardown is idempotent and single-shot: the first reason wins, the injected
// stop callback runs exactly once, and subsequent events are no-ops. After
// teardown, ShouldDegradeToCascade tells the caller whether to rebuild on the
// cascade. Mirrors realtime_lifecycle.py::RealtimeSessionLifecycle.
type RealtimeSessionLifecycle struct {
	budget   RealtimeBudget
	stop     realtimeStopFunc
	spaceID  string
	clock    realtimeClock
	interval time.Duration
	logger   *slog.Logger

	// tearDownOnEmptyRoom controls whether the population reaching zero hard-tears
	// the warm session down immediately (ReasonEmptyRoom). Default OFF (#1449):
	// a mic toggle in the SPA surfaces as a transient LiveKit participant
	// disconnect+reconnect, so an instant empty-room teardown closes the
	// gpt-realtime session out from under a user who is about to re-enable their
	// mic -- then re-engage updates a dead handle and voice never recovers.
	// With this off the session stays warm-but-muted at zero humans (the #459
	// posture); the room-level idle watchdog (#1378, 60s grace counting real
	// participants) owns genuine departures, and the idle / max-duration / token
	// guardrails still bound a warm-but-idle session's cost. When ON, the legacy
	// reflexive teardown applies.
	tearDownOnEmptyRoom bool

	mu             sync.Mutex
	started        bool
	startedAt      time.Time
	lastEngagedAt  time.Time
	humanCount     int
	audioTokens    int
	tornDown       bool
	teardownReason TeardownReason

	// watchdog stop signalling. stopCh is closed once (single watchdog).
	stopCh   chan struct{}
	stopOnce sync.Once
}

// realtimeLifecycleOption configures a RealtimeSessionLifecycle at construction.
// The clock and watchdog interval are injectable for deterministic tests.
type realtimeLifecycleOption func(*RealtimeSessionLifecycle)

// withLifecycleClock injects a clock source (tests advance a fake monotonic
// clock without sleeping). Defaults to time.Now.
func withLifecycleClock(clock realtimeClock) realtimeLifecycleOption {
	return func(l *RealtimeSessionLifecycle) {
		if clock != nil {
			l.clock = clock
		}
	}
}

// withWatchdogInterval overrides the watchdog poll interval (tests use a short
// interval; production uses defaultWatchdogInterval).
func withWatchdogInterval(d time.Duration) realtimeLifecycleOption {
	return func(l *RealtimeSessionLifecycle) {
		if d > 0 {
			l.interval = d
		}
	}
}

// withLifecyclePartitionID stamps the space id on the lifecycle's log lines.
func withLifecyclePartitionID(spaceID string) realtimeLifecycleOption {
	return func(l *RealtimeSessionLifecycle) { l.spaceID = spaceID }
}

// withEmptyRoomTeardown opts the lifecycle back into the legacy reflexive
// empty-room hard-teardown when the population reaches zero (#1449 default is
// off: warm-but-muted, the idle watchdog + idle guardrail govern an empty room).
func withEmptyRoomTeardown(enabled bool) realtimeLifecycleOption {
	return func(l *RealtimeSessionLifecycle) { l.tearDownOnEmptyRoom = enabled }
}

// NewRealtimeLifecycle builds a lifecycle from a resolved budget and an injected
// stop callback. The caller still invokes Start once the session is up. Mirrors
// realtime_lifecycle.py::build_realtime_lifecycle.
func NewRealtimeLifecycle(
	budget RealtimeBudget,
	stop realtimeStopFunc,
	logger *slog.Logger,
	opts ...realtimeLifecycleOption,
) *RealtimeSessionLifecycle {
	l := &RealtimeSessionLifecycle{
		budget:   budget,
		stop:     stop,
		clock:    time.Now,
		interval: defaultWatchdogInterval,
		logger:   logger,
		stopCh:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Start begins the warm session's bookkeeping and arms the idle / duration
// watchdog. humanCount seeds the room population (the room layer waits for at
// least one participant before building the session, so this is normally >= 1).
// Starting with zero humans is allowed but arms the empty-room teardown on the
// next watchdog tick. Idempotent: a second Start is a no-op.
func (l *RealtimeSessionLifecycle) Start(humanCount int) {
	l.mu.Lock()
	if l.started || l.tornDown {
		l.mu.Unlock()
		return
	}
	now := l.clock()
	l.started = true
	l.startedAt = now
	l.lastEngagedAt = now
	if humanCount > 0 {
		l.humanCount = humanCount
	}
	armWatchdog := l.budget.IdleEnabled() || l.budget.DurationEnabled()
	l.mu.Unlock()

	if l.logger != nil {
		l.logger.Info("voice-agent realtime lifecycle started (warm-but-muted, #459)",
			"space", l.spaceID,
			"humans", humanCount,
			"idle_timeout_sec", l.budget.IdleTimeoutSec,
			"max_session_sec", l.budget.MaxSessionSec,
			"max_audio_tokens", l.budget.MaxAudioTokens)
	}
	if armWatchdog {
		go l.watchdogLoop()
	}
}

// Close stops the watchdog without invoking a teardown reason. For caller-side
// cleanup paths that already own session teardown and just need the watchdog
// goroutine stopped. Does not invoke the stop callback. Idempotent. Mirrors the
// Python aclose.
func (l *RealtimeSessionLifecycle) Close() {
	l.stopWatchdog()
}

// NoteHumanJoined records a human participant joining (bumps the room
// population). No-op after teardown.
func (l *RealtimeSessionLifecycle) NoteHumanJoined() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tornDown {
		return
	}
	l.humanCount++
	if l.logger != nil {
		l.logger.Debug("voice-agent realtime lifecycle human joined",
			"space", l.spaceID, "humans", l.humanCount)
	}
}

// NoteHumanLeft records a human participant leaving. When the population reaches
// zero the session is, by default (#1449), kept warm-but-muted rather than torn
// down: a mic toggle in the SPA surfaces as a transient participant
// disconnect+reconnect, and an instant empty-room teardown closes the
// gpt-realtime session out from under a user who is about to re-enable their mic
// (then the re-engage path updates a dead handle and voice never recovers). The
// room-level idle watchdog (#1378) owns genuine departures, and the idle /
// max-duration / token guardrails still bound the warm-but-idle session's cost.
// Opt back into the reflexive teardown with withEmptyRoomTeardown(true). No-op
// after teardown.
func (l *RealtimeSessionLifecycle) NoteHumanLeft() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tornDown {
		return
	}
	if l.humanCount > 0 {
		l.humanCount--
	}
	if l.logger != nil {
		l.logger.Debug("voice-agent realtime lifecycle human left",
			"space", l.spaceID, "humans", l.humanCount,
			"empty_room_teardown", l.tearDownOnEmptyRoom)
	}
	if l.humanCount == 0 && l.tearDownOnEmptyRoom {
		l.teardownLocked(ReasonEmptyRoom)
	}
}

// NoteEngaged records that the conductor engaged the session (drove a
// response.create). Resets the idle clock: an engaged session is doing useful
// work and should not be idle-reaped out from under an active conversation.
// No-op after teardown.
func (l *RealtimeSessionLifecycle) NoteEngaged() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tornDown {
		return
	}
	l.lastEngagedAt = l.clock()
	if l.logger != nil {
		l.logger.Debug("voice-agent realtime lifecycle engaged -- idle clock reset",
			"space", l.spaceID)
	}
}

// NoteAudioTokens records audio-token usage reported for a completed response
// (input+output audio tokens, from response.usage on EventResponseDone). When
// the running session total crosses MaxAudioTokens the session is torn down with
// ReasonTokenBudget and the caller should degrade to the cascade. Negative
// counts are clamped to zero. No-op after teardown.
func (l *RealtimeSessionLifecycle) NoteAudioTokens(tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tornDown {
		return
	}
	if tokens > 0 {
		l.audioTokens += tokens
	}
	if l.budget.TokensEnabled() && l.audioTokens >= l.budget.MaxAudioTokens {
		if l.logger != nil {
			l.logger.Warn("voice-agent realtime audio-token budget exhausted -- tearing down + degrading to cascade (#459)",
				"space", l.spaceID,
				"used", l.audioTokens,
				"budget", l.budget.MaxAudioTokens)
		}
		l.teardownLocked(ReasonTokenBudget)
	}
}

// RequestTeardown externally requests teardown (kill switch / operator stop /
// session end). Idempotent: the first teardown reason wins. The Go analog of the
// Python request_teardown entry point.
func (l *RealtimeSessionLifecycle) RequestTeardown(reason TeardownReason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.teardownLocked(reason)
}

// watchdogLoop polls for idle / max-duration expiry on the configured interval.
// Polling (rather than one-shot timers) keeps the logic robust to engagement
// events resetting the idle clock: each tick re-reads the current deltas against
// the live budget. The interval is small relative to the guardrail windows so
// teardown fires within one tick of the deadline. Exits on teardown or Close.
func (l *RealtimeSessionLifecycle) watchdogLoop() {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			if done := l.checkExpiry(); done {
				return
			}
		}
	}
}

// checkExpiry evaluates the idle / max-duration guardrails against the current
// clock and tears the session down if either has elapsed. It returns true when
// the watchdog should stop (session torn down or already down). Split out from
// the goroutine so tests can drive it deterministically against a fake clock
// without sleeping. Max-duration is checked before idle so a session that has
// both expired reports the wall-clock cap.
func (l *RealtimeSessionLifecycle) checkExpiry() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tornDown || !l.started {
		return true
	}
	now := l.clock()
	if l.budget.DurationEnabled() && now.Sub(l.startedAt) >= l.durationWindow() {
		if l.logger != nil {
			l.logger.Warn("voice-agent realtime max session duration reached -- tearing down (#459)",
				"space", l.spaceID,
				"elapsed_sec", int(now.Sub(l.startedAt).Seconds()),
				"cap_sec", l.budget.MaxSessionSec)
		}
		l.teardownLocked(ReasonMaxDuration)
		return true
	}
	if l.budget.IdleEnabled() && now.Sub(l.lastEngagedAt) >= l.idleWindow() {
		if l.logger != nil {
			l.logger.Warn("voice-agent realtime session idle -- tearing down (#459)",
				"space", l.spaceID,
				"idle_for_sec", int(now.Sub(l.lastEngagedAt).Seconds()),
				"timeout_sec", l.budget.IdleTimeoutSec)
		}
		l.teardownLocked(ReasonIdleTimeout)
		return true
	}
	return false
}

func (l *RealtimeSessionLifecycle) idleWindow() time.Duration {
	return time.Duration(l.budget.IdleTimeoutSec) * time.Second
}

func (l *RealtimeSessionLifecycle) durationWindow() time.Duration {
	return time.Duration(l.budget.MaxSessionSec) * time.Second
}

// teardownLocked performs the single-shot teardown. The caller must hold l.mu.
// It records the reason, invokes the injected stop callback exactly once, and
// signals the watchdog to exit. A failure in the stop callback is logged but
// does NOT unwind the torn-down state -- the session is considered down
// regardless so no later event re-fires teardown. Mirrors the Python
// _teardown_locked.
func (l *RealtimeSessionLifecycle) teardownLocked(reason TeardownReason) {
	if l.tornDown {
		return
	}
	l.tornDown = true
	l.teardownReason = reason
	if l.logger != nil {
		l.logger.Info("voice-agent realtime lifecycle teardown (#459)",
			"space", l.spaceID,
			"reason", string(reason),
			"degrade_to_cascade", reason.IsCostGuardrail(),
			"tokens_used", l.audioTokens)
	}
	if l.stop != nil {
		if err := l.stop(reason); err != nil && l.logger != nil {
			l.logger.Error("voice-agent realtime lifecycle stop callback failed",
				"space", l.spaceID, "reason", string(reason), "err", err)
		}
	}
	l.signalWatchdogStop()
}

// stopWatchdog signals the watchdog goroutine to exit (Close path). Idempotent.
func (l *RealtimeSessionLifecycle) stopWatchdog() {
	l.signalWatchdogStop()
}

// signalWatchdogStop closes the stop channel exactly once. Safe to call under or
// outside the lock; the sync.Once guards the double-close.
func (l *RealtimeSessionLifecycle) signalWatchdogStop() {
	l.stopOnce.Do(func() { close(l.stopCh) })
}

// IsTornDown reports whether the session has been torn down.
func (l *RealtimeSessionLifecycle) IsTornDown() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tornDown
}

// TeardownReason returns the reason the session was torn down, or the empty
// string if it has not been.
func (l *RealtimeSessionLifecycle) TeardownReason() TeardownReason {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.teardownReason
}

// ShouldDegradeToCascade reports whether the session was torn down by a cost
// guardrail. The caller reads this after teardown to decide whether to rebuild
// the voice path on the cascade (cost guardrail trip) or simply let the call end
// (normal room close / external stop). Mirrors the Python property.
func (l *RealtimeSessionLifecycle) ShouldDegradeToCascade() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tornDown && l.teardownReason.IsCostGuardrail()
}

// AudioTokensUsed returns the running audio-token total.
func (l *RealtimeSessionLifecycle) AudioTokensUsed() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.audioTokens
}

// HumanCount returns the current room human population.
func (l *RealtimeSessionLifecycle) HumanCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.humanCount
}
