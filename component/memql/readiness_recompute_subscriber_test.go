package memql

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// The debounce these cases run at. Short enough that the suite does not wait,
// long enough that a burst genuinely lands inside one window on a loaded
// machine. The PRODUCTION values are asserted separately, below, because a
// test that ran at the real seconds would be a slow test asserting constants.
const testDebounce = 60 * time.Millisecond

// testTimings keeps the safety net an hour away so only the cases about it
// ever see a safety-net pass, and the retry base a few debounces so a retry
// is observable without being immediate.
func testTimings() readinessTimings {
	return readinessTimings{
		Debounce:  testDebounce,
		RetryBase: 3 * testDebounce,
		RetryMax:  6 * testDebounce,
		SafetyNet: time.Hour,
	}
}

func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, limit)
}

func countingWrite(writes *atomic.Int32) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		writes.Add(1)
		return 7, nil
	}
}

// lockedBuffer is a log sink the loop's goroutine and the test may both
// touch.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A BURST IS ONE REWRITE.
//
// A cockpit reconnecting re-advertises every model and every app it holds,
// which is a run of graph.node.updated events inside a second. One rewrite per
// event would be one full module evaluation per event -- each of which reads
// every registration in the cluster, on every node that hears it.
func TestABurstRewritesOnce(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	for i := 0; i < 50; i++ {
		sub.Notify("registration")
	}
	waitFor(t, 3*time.Second, "the first rewrite", func() bool { return writes.Load() >= 1 })
	time.Sleep(4 * testDebounce)
	if got := writes.Load(); got != 1 {
		t.Fatalf("%d rewrites for one burst, want 1", got)
	}
}

// A CHANGE AFTER THE WINDOW IS ITS OWN REWRITE. Collapsing everything forever
// would be the opposite failure: the second change would never be seen.
func TestAChangeAfterTheWindowRewritesAgain(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("registration")
	waitFor(t, 3*time.Second, "the first rewrite", func() bool { return writes.Load() == 1 })
	sub.Notify("providers")
	waitFor(t, 3*time.Second, "the second rewrite", func() bool { return writes.Load() == 2 })
}

// NO BURST WITHOUT AN EVENT, ONE PASS PER SAFETY-NET PERIOD (D2 of the
// 2026-09-14 readiness-convergence record).
//
// This replaces TestNothingRewritesWithoutAnEvent, which pinned "no timer".
// The premise behind "events only" -- that the registration broadcast reaches
// every replica -- is false: identity is excluded from every broadcast, and a
// node nobody dials receives no mesh event at all, so a node whose row lagged
// had no further chance until the next deploy (memql#5259). The safety net is
// the floor under that: one cluster read per node per period, which writes
// nothing when nothing changed.
func TestASafetyNetPassRunsWithoutAnEvent(t *testing.T) {
	var writes atomic.Int32
	timings := testTimings()
	timings.SafetyNet = 10 * testDebounce
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), timings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	sub.Start(ctx)

	// Nothing early: the period is jittered +-20%, so nothing before 8 windows.
	time.Sleep(5 * testDebounce)
	if got := writes.Load(); got != 0 {
		t.Fatalf("%d rewrites inside the first half-period with no event; a fast poll is what D5 refused and D2 still refuses", got)
	}
	waitFor(t, 3*time.Second, "the first safety-net pass", func() bool { return writes.Load() == 1 })
	first := time.Since(started)
	if first < 8*testDebounce {
		t.Fatalf("the first pass ran at %s, before the jittered period's floor of %s", first, 8*testDebounce)
	}
	waitFor(t, 3*time.Second, "the second safety-net pass", func() bool { return writes.Load() == 2 })
	if gap := time.Since(started) - first; gap < 8*testDebounce {
		t.Fatalf("the second pass followed the first by %s; one pass per period, not a burst", gap)
	}
}

// A FAILED REWRITE IS RETRIED WITHOUT AN EVENT (D2).
//
// The boot write and the 30 s re-write of a node nobody dials both failed
// inside the 2026-09-13 rollout's database saturation, and there was no third
// attempt: the loop survived and waited for an event that never reaches that
// node. Now a failed pass schedules its own retry, exponentially backed off
// and jittered, until one pass lands.
func TestAFailedRewriteIsRetriedWithoutAnEvent(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		if writes.Add(1) <= 2 {
			return 0, errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")
		}
		return 7, nil
	}, testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("boot")
	waitFor(t, 3*time.Second, "the third attempt, which succeeds", func() bool { return writes.Load() == 3 })
	// And then it STOPS: a success ends the retry ladder.
	time.Sleep(8 * testDebounce)
	if got := writes.Load(); got != 3 {
		t.Fatalf("%d writes after the success; the retry must end on the first pass that lands", got)
	}
}

// AN UNKNOWN PASS IS RETRIED TOO. WriteModuleReadiness answers
// *ReadinessUnknownError when a module could not be evaluated, and that is a
// pass that did not describe the node, however many rows it wrote.
func TestAnUnknownPassIsRetried(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		if writes.Add(1) == 1 {
			return 6, &ReadinessUnknownError{Modules: []string{"ai"}, Reasons: map[string]string{"ai": "fleetReadFailed"}}
		}
		return 7, nil
	}, testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)
	sub.Notify("boot")
	waitFor(t, 3*time.Second, "the retry after an unknown pass", func() bool { return writes.Load() == 2 })
}

// A NOTIFY DURING A BACKOFF COLLAPSES INTO THE RETRY. The retry is a full
// re-evaluation, so it covers whatever the notification was about; running
// the pass early on every event would turn a saturated database's failure
// into a tight loop driven by the 15 s heartbeat.
func TestANotifyDuringBackoffCollapsesIntoIt(t *testing.T) {
	var writes atomic.Int32
	timings := testTimings()
	timings.RetryBase = 10 * testDebounce
	timings.RetryMax = 10 * testDebounce
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		if writes.Add(1) == 1 {
			return 0, errors.New("the write did not land")
		}
		return 7, nil
	}, timings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("registration")
	waitFor(t, 3*time.Second, "the failing pass", func() bool { return writes.Load() == 1 })
	for i := 0; i < 20; i++ {
		sub.Notify("registration")
	}
	// Half the backoff (its jitter floor is 50%): nothing may run yet.
	time.Sleep(4 * testDebounce)
	if got := writes.Load(); got != 1 {
		t.Fatalf("%d writes during the backoff; a notification must not run the pass early", got)
	}
	waitFor(t, 3*time.Second, "the retry", func() bool { return writes.Load() == 2 })
	// And the twenty notifications produced no pass of their own.
	time.Sleep(6 * testDebounce)
	if got := writes.Load(); got != 2 {
		t.Fatalf("%d writes; the notifications during the backoff ran a second pass instead of collapsing", got)
	}
}

// THE BACKOFF IS BOUNDED AND JITTERED. Exponential from the base, capped at
// the max, with a jitter floor of half the step so two nodes that failed
// together do not retry together.
func TestBackoffIsBoundedAndJittered(t *testing.T) {
	sub := newReadinessRecomputeSubscriberFor(nil, productionReadinessTimings())
	for failures := 1; failures <= 12; failures++ {
		step := ReadinessRetryBase << (failures - 1)
		if step > ReadinessRetryMax || step <= 0 {
			step = ReadinessRetryMax
		}
		for i := 0; i < 50; i++ {
			d := sub.backoffFor(failures)
			if d < step/2 || d > step {
				t.Fatalf("failures=%d: backoff %s outside [%s, %s]", failures, d, step/2, step)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[sub.backoffFor(6)] = true
	}
	if len(seen) < 2 {
		t.Fatal("fifty backoffs at the same failure count were all equal; there is no jitter")
	}
	if sub.backoffFor(1) > ReadinessRetryBase || sub.backoffFor(40) > ReadinessRetryMax {
		t.Fatal("the backoff exceeds its bounds")
	}
}

// The safety-net period is jittered +-20% so every node in a rollout does not
// re-evaluate in the same second ten minutes after the deploy.
func TestTheSafetyNetIsJittered(t *testing.T) {
	sub := newReadinessRecomputeSubscriberFor(nil, productionReadinessTimings())
	lo := time.Duration(float64(ReadinessSafetyNetInterval) * (1 - ReadinessSafetyNetJitter))
	hi := time.Duration(float64(ReadinessSafetyNetInterval) * (1 + ReadinessSafetyNetJitter))
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := sub.jitteredSafetyNet()
		if d < lo || d > hi {
			t.Fatalf("safety net %s outside [%s, %s]", d, lo, hi)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("no jitter on the safety net")
	}
}

// The boot jitter spreads a rollout's simultaneous boot writes (every pod
// runs its write the moment waitForReady returns) across five seconds.
func TestBootJitterIsWithinBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		d := ReadinessBootJitter()
		if d < 0 || d >= ReadinessBootJitterMax {
			t.Fatalf("boot jitter %s outside [0, %s)", d, ReadinessBootJitterMax)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("a hundred boot jitters were all equal")
	}
}

// A PASS THAT WROTE, OR THAT ENDED A FAILURE STREAK, IS LOGGED AT INFO with
// its reason and count, so an operator reading a pod's log can see the
// recovery and not only the failures that preceded it (on 2026-09-13 only the
// failures warned). A pass that found nothing changed is Debug: an INFO line
// every ten minutes on every node saying nothing happened is noise.
func TestASuccessfulPassIsLoggedWithReasonAndCount(t *testing.T) {
	var buf lockedBuffer
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		switch writes.Add(1) {
		case 1:
			return 7, nil
		case 2:
			return 0, nil
		case 3:
			return 0, errors.New("the write did not land")
		default:
			return 0, nil
		}
	}, testTimings())
	sub.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("boot")
	waitFor(t, 3*time.Second, "the first pass", func() bool { return writes.Load() == 1 })
	waitFor(t, time.Second, "its log line", func() bool { return strings.Contains(buf.String(), "rows written") })
	if line := buf.String(); !strings.Contains(line, "reason=boot") || !strings.Contains(line, "modules=7") {
		t.Fatalf("the success line lacks its reason or count: %s", line)
	}

	// A pass that wrote nothing says nothing at INFO.
	before := strings.Count(buf.String(), "rows written")
	sub.Notify("registration")
	waitFor(t, 3*time.Second, "the unchanged pass", func() bool { return writes.Load() == 2 })
	time.Sleep(2 * testDebounce)
	if got := strings.Count(buf.String(), "rows written"); got != before {
		t.Fatalf("a pass that wrote nothing logged at INFO: %s", buf.String())
	}

	// The pass that ENDS a failure streak is said even though it wrote
	// nothing: it is the recovery.
	sub.Notify("registration")
	waitFor(t, 3*time.Second, "the failing pass and its retry", func() bool { return writes.Load() == 4 })
	waitFor(t, time.Second, "the recovery line", func() bool { return strings.Contains(buf.String(), "recovered=true") })
}

// Cancelling the context ends the loop, retries and safety net included.
// Without this a node that stopped its engine would keep rewriting rows
// through a drain.
func TestStoppingEndsTheLoop(t *testing.T) {
	var writes atomic.Int32
	timings := testTimings()
	timings.SafetyNet = 3 * testDebounce
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		writes.Add(1)
		return 0, errors.New("always")
	}, timings)
	ctx, cancel := context.WithCancel(context.Background())
	sub.Start(ctx)
	cancel()
	time.Sleep(2 * testDebounce)
	sub.Notify("registration")
	time.Sleep(8 * testDebounce)
	if got := writes.Load(); got != 0 {
		t.Fatalf("%d rewrites after the context was cancelled", got)
	}
}

// Every method is nil-safe, which is what lets NotifyReadinessRecompute be
// called from a path that runs on a hand-built engine in a test and on a
// binary that never started the subscriber.
func TestANilSubscriberIsANoOp(t *testing.T) {
	var sub *ReadinessRecomputeSubscriber
	sub.Notify("registration")
	sub.Start(context.Background())
	sub.Stop()
	var eng *MemQLEngine
	if eng.NotifyReadinessRecompute("registration") {
		t.Error("a nil engine reported the notification delivered")
	}
	if (&MemQLEngine{}).NotifyReadinessRecompute("registration") {
		t.Error("an engine with no subscriber reported the notification delivered -- a caller " +
			"relying on that answer would skip its fallback and lose the rewrite entirely")
	}
}

// The one graph subscription this node opens, and it must actually match the
// topics the CDC path publishes.
//
// A pattern that matched nothing would be the worst kind of failure here: the
// subscription registers, every unit test of the loop passes through Notify,
// and on a cluster the wizard's rail simply never moves.
func TestTheRegistrationPatternsMatchTheCdcTopics(t *testing.T) {
	patterns := readinessRegistrationPatterns()
	if len(patterns) == 0 {
		t.Fatal("no registration patterns at all")
	}
	for _, topic := range []string{
		events.TopicNodeCreated(WorkerRegistrationConcept),
		events.TopicNodeUpdated(WorkerRegistrationConcept),
		events.TopicNodeDeleted(WorkerRegistrationConcept),
	} {
		matched := false
		for _, p := range patterns {
			if events.Match(p, topic) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%q matches none of %v", topic, patterns)
		}
	}
	for _, topic := range []string{
		events.TopicNodeCreated("v1:identity:user"),
		events.TopicNodeUpdated("v1:work:step"),
		events.TopicNodeCreated(ModuleReadinessConcept),
	} {
		for _, p := range patterns {
			if events.Match(p, topic) {
				t.Errorf("%q matches %q; the subscription must be the registration concept alone", p, topic)
			}
		}
	}
	for _, p := range patterns {
		if strings.Contains(p, ModuleReadinessConcept) {
			t.Fatalf("pattern %q names the readiness concept itself, which is a write loop", p)
		}
	}
}

// The production constants, asserted where a reader looking for them will find
// them rather than only in a comment.
func TestTheProductionTimingsAreWhatTheRecordSays(t *testing.T) {
	if ReadinessRecomputeDebounce != 2*time.Second {
		t.Errorf("the debounce is %s; D5 (2026-09-07) says two seconds", ReadinessRecomputeDebounce)
	}
	if ReadinessBootRewriteDelay != 30*time.Second {
		t.Errorf("the boot re-write delay is %s; D5 (2026-09-07) says thirty seconds", ReadinessBootRewriteDelay)
	}
	if ReadinessRetryBase != 2*time.Second || ReadinessRetryMax != 60*time.Second {
		t.Errorf("retry %s..%s; D2 (2026-09-14) says 2 s doubling to 60 s", ReadinessRetryBase, ReadinessRetryMax)
	}
	if ReadinessSafetyNetInterval != 10*time.Minute || ReadinessSafetyNetJitter != 0.2 {
		t.Errorf("safety net %s +-%.0f%%; D2 says ten minutes +-20%%", ReadinessSafetyNetInterval, ReadinessSafetyNetJitter*100)
	}
	if ReadinessBootJitterMax != 5*time.Second {
		t.Errorf("boot jitter %s; D2 says 0-5 s", ReadinessBootJitterMax)
	}
	p := productionReadinessTimings()
	if p.Debounce != ReadinessRecomputeDebounce || p.RetryBase != ReadinessRetryBase || p.RetryMax != ReadinessRetryMax || p.SafetyNet != ReadinessSafetyNetInterval {
		t.Errorf("productionReadinessTimings does not carry the constants: %+v", p)
	}
}

// THE WIRE, from a real event bus to a real rewrite (epic memql#5118, D5).
//
// Everything above tests the loop through `Notify`, and
// TestTheRegistrationPatternsMatchTheCdcTopics tests that the pattern matches
// the topic. Neither proves the two are CONNECTED -- that the engine opens the
// subscription at all, on the bus it was given.
//
// That gap is the one this package has been bitten by before: a subscriber
// that is registered and inert is green on every unit test and does nothing on
// a cluster, and the symptom is the exact defect the feature exists to fix --
// a wizard whose rail does not move when you pair a machine. So this drives a
// real `events.Bus` through the engine's own wiring and asserts a rewrite came
// out the far end.
//
// It uses a REAL bus rather than a fake for the reason engineWithProviders
// does: a fabricated bus that called the handler directly would pass against a
// subscription that was never registered. The subscriber is handed in rather
// than swapped after start, so its write -- which would otherwise need a
// database -- is never assigned under a running loop.
func TestAGraphEventOnTheBusReachesTheRewrite(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	eng := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	eng.SetEventBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var writes atomic.Int32
	sub := eng.startReadinessRecompute(ctx, newReadinessRecomputeSubscriberFor(countingWrite(&writes), testTimings()))
	if sub == nil || eng.readinessRecompute.Load() != sub {
		t.Fatal("the subscriber was not stored; NotifyReadinessRecompute would find nothing")
	}

	// A REACHABLE POSITIVE FIRST. If Notify does not produce a rewrite, the
	// assertion below would pass over a broken loop rather than over a broken
	// subscription, and it would name the wrong thing.
	if !eng.NotifyReadinessRecompute("probe") {
		t.Fatal("the engine reported the notification undelivered")
	}
	waitFor(t, 3*time.Second, "the probe rewrite", func() bool { return writes.Load() == 1 })

	// THE ACTUAL EVENT, published exactly as the CDC path publishes it.
	bus.Publish(events.NewEvent(
		events.TopicNodeCreated(WorkerRegistrationConcept),
		events.KindNodeCreated,
		map[string]any{"id": "v1:worker:registration:m-1"},
	))
	waitFor(t, 3*time.Second, "the rewrite a paired machine causes", func() bool {
		return writes.Load() == 2
	})

	// AND NOT EVERY GRAPH EVENT. A readiness rewrite on every row written
	// anywhere would be a cluster-wide registration read per write, on every
	// replica -- and a rewrite triggered by a READINESS write would loop.
	bus.Publish(events.NewEvent(
		events.TopicNodeUpdated(ModuleReadinessConcept),
		events.KindNodeUpdated,
		map[string]any{"id": "v1:platform:moduleReadiness:ai--n"},
	))
	bus.Publish(events.NewEvent(
		events.TopicNodeCreated("v1:identity:user"),
		events.KindNodeCreated,
		map[string]any{"id": "v1:identity:user:u-1"},
	))
	time.Sleep(6 * testDebounce)
	if got := writes.Load(); got != 2 {
		t.Fatalf("%d rewrites; the subscription is matching topics beyond the registration concept", got)
	}
}

// The engine's own subscriber is built around the engine's own write, with the
// production timings -- the constructor a node actually runs.
func TestTheEnginesSubscriberCarriesTheProductionTimings(t *testing.T) {
	eng := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	sub := NewReadinessRecomputeSubscriber(eng)
	if sub == nil || sub.write == nil {
		t.Fatal("no subscriber, or no write")
	}
	if sub.timings != productionReadinessTimings() {
		t.Fatalf("the engine's subscriber runs %+v, not the production timings", sub.timings)
	}
}
