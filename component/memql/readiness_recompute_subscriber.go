package memql

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// READINESS RECOMPUTES ON THE EVENTS THAT CHANGE IT (design record
// docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md, D5),
// AND ON TWO MECHANISMS THAT DO NOT NEED AN EVENT (design record
// docs/superpowers/specs/2026-09-14-readiness-convergence-design.md, D2).
//
// The shipped rows were rewritten at boot, on a providers reload, on an email
// configure, or by a manual recompute -- and nothing else. So pairing a
// machine, revoking one, or a cockpit re-advertising its models left the `ai`
// verdict exactly as it was at boot. The event subscription fixed that for
// every node the event reaches.
//
// ===========================================================================
// WHAT "BROADCAST" DELIVERS, AND WHY EVENTS ALONE ARE NOT ENOUGH
// ===========================================================================
// A routing rule with TargetType "" is broadcast, and D5 was written on the
// premise that a broadcast reaches every replica. Until memql#5338 it did not
// (memql#5259): the mesh pushed events only along a DIAL, so a node nobody
// dialed -- in the cloud, the edge and a product bff -- heard no mesh event at
// all. On 2026-09-09 that left seven of fourteen `ai` rows at their boot
// answer after a machine was paired; on 2026-09-13 every node's boot write
// failed inside a rolling deploy's database saturation, the dialed nodes
// recovered on the next registration heartbeat, and the undialed ones kept a
// failed evaluation until the next deploy.
//
// memql#5338 made every stream carry events both ways, so a broadcast now
// reaches every node holding any stream to the mesh. Events are still not
// enough on their own:
//
//   - identity is excluded from every broadcast by design
//     (component/node/eventbridge.go meshEventParticipants), so it never
//     hears one even though the bff dials it.
//   - Delivery is best effort: a node whose every stream is down at that
//     moment -- mid-reconnect, or on the far side of a rolling update, where
//     an old pod drops what a new one sends -- misses the event, and nothing
//     queues it.
//
// So this loop keeps its own floor:
//
//	RETRY   A failed or unknown pass re-runs on an exponential, jittered
//	        backoff (ReadinessRetryBase doubling to ReadinessRetryMax) until
//	        one pass lands. A Notify during the backoff COLLAPSES into the
//	        retry -- the retry is a full re-evaluation, so it covers whatever
//	        the notification was about, and running early on every heartbeat
//	        would turn a saturated database into a tight loop.
//	SAFETY  A pass every ReadinessSafetyNetInterval, jittered +-20%, whether
//	        or not anything arrived: the bound on how long any node's row can
//	        lag the cluster. It costs one cluster read per node per period,
//	        and writes nothing when nothing changed (readinessRewriteNeeded).
//
// The VERDICT does not wait for either: the fold sets aside a row whose
// cluster-scoped lanes lag the freshest evaluation (readiness.Fold rule 4),
// and the node that wrote a registration always hears its own event. These two
// mechanisms bound how long a lagging ROW stays lagging.
//
// ===========================================================================
// DEBOUNCED, BECAUSE A RECONNECT IS A BURST
// ===========================================================================
// A cockpit reconnecting re-advertises every model and every app it holds, as
// a run of graph.node.updated events inside one second. One rewrite per event
// would be one full module evaluation per event, each reading every
// registration in the cluster, on every node that hears it.
//
// ===========================================================================
// AND IT SERIALIZES
// ===========================================================================
// Every rewrite this node performs -- event, retry, safety net, boot -- goes
// through one loop, so two triggers arriving together cannot run two
// evaluations that race on the same deterministic row ids. That is why the
// providers-reload subscriber and app/run.go's boot path notify this rather
// than calling WriteModuleReadiness themselves.
type ReadinessRecomputeSubscriber struct {
	write   func(context.Context) (int, error)
	logger  *slog.Logger
	timings readinessTimings

	// pending is a buffered channel of ONE, and that IS the coalescing: a
	// burst of a thousand events costs one slot and one rewrite.
	pending chan string

	mu   sync.Mutex
	stop context.CancelFunc
}

// readinessTimings is every duration the loop uses, so a test can shrink all
// of them together and the production set is one value.
type readinessTimings struct {
	Debounce  time.Duration
	RetryBase time.Duration
	RetryMax  time.Duration
	SafetyNet time.Duration
}

// ReadinessRecomputeDebounce is how long a burst is collapsed for.
//
// Two seconds: long enough to swallow a reconnect's whole advertisement, short
// enough that a person who has just paired a machine sees the wizard move
// before they have finished reading the confirmation.
const ReadinessRecomputeDebounce = 2 * time.Second

// ReadinessRetryBase and ReadinessRetryMax bound the retry ladder after a
// failed or unknown pass: 2 s, 4 s, 8 s ... 60 s, each jittered down to half.
const (
	ReadinessRetryBase = 2 * time.Second
	ReadinessRetryMax  = 60 * time.Second
)

// ReadinessSafetyNetInterval is the period of the pass that needs no event,
// jittered by ReadinessSafetyNetJitter either way. It is the bound on how long
// a node that hears no mesh event keeps a row that lags the cluster.
const (
	ReadinessSafetyNetInterval = 10 * time.Minute
	ReadinessSafetyNetJitter   = 0.2
)

// ReadinessBootJitterMax spreads the boot write: every pod in a rollout runs
// it the moment waitForReady returns, on top of the seed materializer, the
// cluster registration and provider resolution, which is the peak of the
// surge. app/run.go waits ReadinessBootJitter() before notifying.
const ReadinessBootJitterMax = 5 * time.Second

// ReadinessBootRewriteDelay is the ONE re-write after boot.
//
// It covers a node whose integration materialized lazily AFTER the boot write
// ran -- the case the email plug-in's own resolution logged twenty seconds
// late on the owner's cluster. It goes through this loop, so a failure there
// is retried like any other pass.
const ReadinessBootRewriteDelay = 30 * time.Second

func productionReadinessTimings() readinessTimings {
	return readinessTimings{
		Debounce:  ReadinessRecomputeDebounce,
		RetryBase: ReadinessRetryBase,
		RetryMax:  ReadinessRetryMax,
		SafetyNet: ReadinessSafetyNetInterval,
	}
}

// ReadinessBootJitter answers a uniform delay in [0, ReadinessBootJitterMax).
func ReadinessBootJitter() time.Duration {
	return time.Duration(rand.Int63n(int64(ReadinessBootJitterMax)))
}

// readinessRegistrationPatterns is the one graph subscription this node needs.
//
// Composed through GraphSubscriptionPatterns rather than spelled here, so the
// `graph.node.<action>.<concept>` grammar has one author. With no actions it
// yields `graph.node.*.<concept>` -- `*` is exactly the one action segment,
// and a concept id carries no dots, so the trailing remainder is one segment
// too.
//
// ALL THREE VERBS MATTER and each is a different fact: created is a machine
// paired, updated is a reconnect re-advertising its models and apps or an
// owner revoking it, deleted is a registration removed. All three carry a
// broadcast rule (component/node/routing.go); see the header for what a
// broadcast actually reaches, and for why the node that WROTE the registration
// is the one that always hears it.
func readinessRegistrationPatterns() []string {
	return events.GraphSubscriptionPatterns(WorkerRegistrationConcept, nil)
}

// NewReadinessRecomputeSubscriber builds this node's subscriber.
func NewReadinessRecomputeSubscriber(eng *MemQLEngine) *ReadinessRecomputeSubscriber {
	if eng == nil {
		return nil
	}
	sub := newReadinessRecomputeSubscriberFor(eng.WriteModuleReadiness, productionReadinessTimings())
	sub.logger = eng.safeLogger()
	return sub
}

func newReadinessRecomputeSubscriberFor(write func(context.Context) (int, error), timings readinessTimings) *ReadinessRecomputeSubscriber {
	return &ReadinessRecomputeSubscriber{
		write:   write,
		timings: timings,
		pending: make(chan string, 1),
	}
}

// Notify records that something changed. NON-BLOCKING and coalescing, so a
// caller on a hot path never waits and a burst never queues.
func (s *ReadinessRecomputeSubscriber) Notify(reason string) {
	if s == nil {
		return
	}
	select {
	case s.pending <- reason:
	default:
	}
}

// Start runs the loop until ctx is done or Stop is called. Calling it twice
// is a no-op, so a node that wires it in two places does not get two loops
// writing the same rows.
func (s *ReadinessRecomputeSubscriber) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.mu.Unlock()
	go s.loop(runCtx)
}

// Stop ends the loop. Safe to call twice, and safe on a nil receiver.
func (s *ReadinessRecomputeSubscriber) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

// backoffFor is the wait before retry number `failures` (1-based): the base
// doubled per failure, capped, then jittered to [step/2, step] so two nodes
// that failed together do not retry together.
func (s *ReadinessRecomputeSubscriber) backoffFor(failures int) time.Duration {
	step := s.timings.RetryBase
	for i := 1; i < failures && step < s.timings.RetryMax; i++ {
		step *= 2
	}
	if step > s.timings.RetryMax || step <= 0 {
		step = s.timings.RetryMax
	}
	half := step / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// jitteredSafetyNet is the period +-ReadinessSafetyNetJitter, so every node
// in a rollout does not re-evaluate in the same second ten minutes later.
func (s *ReadinessRecomputeSubscriber) jitteredSafetyNet() time.Duration {
	base := float64(s.timings.SafetyNet)
	spread := base * ReadinessSafetyNetJitter
	return time.Duration(base - spread + rand.Float64()*2*spread)
}

// drainPending clears the slot: whatever arrived is covered by the pass about
// to run.
func (s *ReadinessRecomputeSubscriber) drainPending() {
	select {
	case <-s.pending:
	default:
	}
}

// sleepUnless waits d, or answers false when ctx ends first.
func sleepUnless(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *ReadinessRecomputeSubscriber) loop(ctx context.Context) {
	safety := time.NewTimer(s.jitteredSafetyNet())
	defer safety.Stop()
	failures := 0
	for {
		var reason string
		if failures > 0 {
			// IN BACKOFF: pending is deliberately NOT selected on, so a
			// notification collapses into the retry rather than running the
			// pass early against a database that just refused it.
			retry := time.NewTimer(s.backoffFor(failures))
			select {
			case <-ctx.Done():
				retry.Stop()
				return
			case <-retry.C:
				reason = "retry"
			case <-safety.C:
				retry.Stop()
				reason = "safetyNet"
				safety.Reset(s.jitteredSafetyNet())
			}
			s.drainPending()
		} else {
			select {
			case <-ctx.Done():
				return
			case reason = <-s.pending:
				// Wait out the window BEFORE writing, so a burst that is still
				// arriving is one rewrite rather than the first of many.
				if !sleepUnless(ctx, s.timings.Debounce) {
					return
				}
				s.drainPending()
			case <-safety.C:
				reason = "safetyNet"
				safety.Reset(s.jitteredSafetyNet())
			}
		}
		written, err := s.write(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A FAILURE LEAVES THE PREVIOUS ROWS STANDING, THE LOOP ALIVE, AND
			// A RETRY SCHEDULED. An unknown pass is a failure here too:
			// however many rows it wrote, it did not describe this node.
			failures++
			if s.logger != nil {
				s.logger.Warn("module readiness: rewrite failed; the previous rows stand and the pass will retry",
					"component", ComponentName, "reason", reason, "unknown", IsReadinessUnknown(err),
					"written", written, "failures", failures, "retryIn", s.backoffFor(failures).String(), "error", err)
			}
			continue
		}
		recovered := failures > 0
		failures = 0
		if s.logger == nil {
			continue
		}
		// SAID AT INFO WHEN IT MEANS SOMETHING: a pass that wrote a row, or
		// the one that ended a failure streak, so a log shows the recovery and
		// not only the failures before it (on 2026-09-13 only the failures
		// warned). A safety-net pass that found nothing changed writes nothing
		// and says so at Debug -- an INFO line every ten minutes on every node
		// announcing that nothing happened is the noise a reader learns to
		// skip, which is how the one that matters gets skipped too.
		if written > 0 || recovered {
			s.logger.Info("module readiness: rows written",
				"component", ComponentName, "reason", reason, "modules", written, "recovered", recovered)
		} else {
			s.logger.Debug("module readiness: pass found nothing changed",
				"component", ComponentName, "reason", reason)
		}
	}
}

// StartReadinessRecomputeSubscriber wires this node's readiness rewrites to the
// events that change them, and returns the subscriber so a caller can Notify it
// from a path that is not an event (the providers reload, the boot write).
//
// The subscription is scoped to ctx, exactly as StartProvidersReloadSubscriber
// is: when ctx is cancelled the unsubscribe runs and the loop exits.
func (e *MemQLEngine) StartReadinessRecomputeSubscriber(ctx context.Context) *ReadinessRecomputeSubscriber {
	if e == nil {
		return nil
	}
	return e.startReadinessRecompute(ctx, NewReadinessRecomputeSubscriber(e))
}

// startReadinessRecompute is the wiring half, split out so a test can hand in
// a subscriber whose write needs no database and still exercise the exact
// start, store and subscribe sequence a node runs.
func (e *MemQLEngine) startReadinessRecompute(ctx context.Context, sub *ReadinessRecomputeSubscriber) *ReadinessRecomputeSubscriber {
	sub.Start(ctx)
	// Stored BEFORE the subscriptions below, so a notification arriving from
	// another goroutine during startup finds the running subscriber rather
	// than a nil it silently drops.
	e.readinessRecompute.Store(sub)

	if e.eventBus == nil {
		return sub
	}
	sub.subscribe(ctx, e.eventBus)
	return sub
}

// subscribe opens the registration subscription on bus, scoped to ctx.
func (s *ReadinessRecomputeSubscriber) subscribe(ctx context.Context, bus *events.Bus) {
	for _, pattern := range readinessRegistrationPatterns() {
		unsubscribe := bus.Subscribe(
			pattern,
			func(events.Event) { s.Notify("registration") },
			events.WithSubscriberName("readiness:recompute"),
		)
		if unsubscribe == nil {
			continue
		}
		go func() {
			<-ctx.Done()
			unsubscribe()
		}()
	}
}

// NotifyReadinessRecompute asks this node to rewrite its readiness rows, on the
// debounce, from a path that is not a graph event.
//
// A no-op when the subscriber is not running, which is every hand-built engine
// in a test and every binary that has not called
// StartReadinessRecomputeSubscriber. The caller that needs the rewrite to have
// HAPPENED calls WriteModuleReadiness directly instead; this one is for the
// callers that only need it to happen soon.
//
// IT REPORTS WHETHER IT WAS DELIVERED, so a caller that must not simply lose
// the rewrite can fall back to a direct write (app/run.go's boot write does
// exactly that). Without the answer, "notify" and "silently dropped" are the
// same call -- which is the shape of silence this whole loop is about.
func (e *MemQLEngine) NotifyReadinessRecompute(reason string) bool {
	if e == nil {
		return false
	}
	sub := e.readinessRecompute.Load()
	if sub == nil {
		return false
	}
	sub.Notify(reason)
	return true
}

// ReadinessProbeTimings are the loop's durations for a caller outside this
// package that drives the REAL loop against a stand-in write -- the cross-node
// hop tests in component/node, which need the production retry, safety-net and
// debounce logic at test speed.
type ReadinessProbeTimings struct {
	Debounce, RetryBase, RetryMax, SafetyNet time.Duration
}

// StartReadinessRecomputeProbe runs the production recompute loop, subscribed
// to bus exactly as a node's is, around write. FOR TESTS: it exists so a test
// in another package can put the real loop on each replica of an in-process
// mesh and observe which replicas rewrite, and when, without a database.
func StartReadinessRecomputeProbe(ctx context.Context, bus *events.Bus, write func(context.Context) (int, error), t ReadinessProbeTimings) *ReadinessRecomputeSubscriber {
	sub := newReadinessRecomputeSubscriberFor(write, readinessTimings(t))
	sub.Start(ctx)
	if bus != nil {
		sub.subscribe(ctx, bus)
	}
	return sub
}
