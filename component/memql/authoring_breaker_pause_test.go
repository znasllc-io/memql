package memql

// authoring_breaker_pause_test.go -- a tripped authored automation stops on
// EVERY replica and stays stopped across a restart (memql#6022, F2).
//
// The breaker used to pause the automation in the tripping node's scheduler
// and registry and nowhere else. With event rules now firing on whichever
// replica their write lands on, every other replica kept firing it, and the
// pause lived only in memory: a reboot (or this node's next reconcile pass,
// which reads the bundle as active) armed it again.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// pauseWrite is one recorded setAuthoringBundleStatus(paused) write.
type pauseWrite struct{ owner, bundleId, reason string }

// pauseRecordingStore is the shared database with the pause write recorded,
// and failable.
type pauseRecordingStore struct {
	*armTestStore
	mu        sync.Mutex
	pauses    []pauseWrite
	failPause error
}

func (s *pauseRecordingStore) PauseBundleForOwner(ctx context.Context, owner, bundleId, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPause != nil {
		return s.failPause
	}
	// A database driver refuses a write on a cancelled context; so does this.
	if err := ctx.Err(); err != nil {
		return err
	}
	s.pauses = append(s.pauses, pauseWrite{owner: owner, bundleId: bundleId, reason: reason})
	s.armTestStore.setBundleStatus(bundleId, BundlePaused)
	return nil
}

func (s *pauseRecordingStore) recorded() []pauseWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pauseWrite(nil), s.pauses...)
}

const tripReason = "Paused automatically after 5 failed runs in a row."

// tripOf is the trip a breaker reports for the automation armed from bundleId
// at version: the failing runs' own construct, captured when they failed.
func tripOf(name, bundleId string, version int) AuthoredTrip {
	return AuthoredTrip{Owner: armTestOwner, Automation: name, BundleId: bundleId, Version: version, Reason: tripReason}
}

// applyTrip applies a trip on eng and fails the test unless it did what want
// says it should.
func applyTrip(t *testing.T, ctx context.Context, eng *MemQLEngine, store pauseStore, deps AuthoredRuntimeDeps, trip AuthoredTrip, want AuthoredTripOutcome) {
	t.Helper()
	outcome, err := eng.pauseTrippedAutomationWithStore(ctx, store, deps, trip)
	if err != nil {
		t.Fatalf("trip of %s: %v", trip.Automation, err)
	}
	if outcome != want {
		t.Fatalf("the trip of %s did %q, want %q", trip.Automation, outcome, want)
	}
}

// TestABreakerTripPausesTheAutomationOnEveryReplica: node A's breaker trips on
// an automation both nodes hold. The trip records the pause on the bundle row,
// takes the bundle out of node A, and broadcasts the disarm, so node B takes it
// out too; and B's next reconcile pass, reading the bundle as paused, does not
// arm it again.
func TestABreakerTripPausesTheAutomationOnEveryReplica(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	bundleId := store.addBundle("v1:authoring:bundle:trip", 1, "", BundleActive,
		[2]string{"automation", "autoTrip"}, [2]string{"query", "queryTrip"})

	busA := events.NewBus()
	t.Cleanup(busA.Close)
	engA := &MemQLEngine{}
	engA.SetEventBus(busA)
	hooksA := &armHooks{}
	depsA := hooksA.deps(NewAuthoredRuntimeRegistry())
	engB, busB, depsB, hooksB := newArmNode(t, store)
	var crossed []string
	var crossedMu sync.Mutex
	busA.Subscribe("authoring.disarm.#", func(ev events.Event) {
		crossedMu.Lock()
		crossed = append(crossed, ev.Topic+"/"+ev.Kind.String())
		crossedMu.Unlock()
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	if _, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, depsA); err != nil {
		t.Fatalf("setup: arm on A: %v", err)
	}
	busB.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(bundleId), events.KindAuthoringArm, bundleId))
	if held(t, depsB, "automation", "autoTrip") == nil {
		t.Fatal("setup: node B should hold autoTrip")
	}

	applyTrip(t, ctx, engA, store, depsA, tripOf("autoTrip", bundleId, 1), TripPaused)

	// Durable: the bundle row says paused, with why, written as its owner.
	if got := store.recorded(); len(got) != 1 || got[0] != (pauseWrite{armTestOwner, bundleId, tripReason}) {
		t.Fatalf("the pause was not recorded on the bundle row as its owner: %+v", got)
	}
	if st := store.bundleStatus(bundleId); st != BundlePaused {
		t.Fatalf("the bundle reads %q after the trip, want paused: a reboot would arm it again", st)
	}

	// This node: the whole bundle is out, scheduler included.
	if held(t, depsA, "automation", "autoTrip") != nil || held(t, depsA, "query", "queryTrip") != nil {
		t.Fatal("the tripping node still holds the paused bundle")
	}
	if n := hooksA.count("deregister autoTrip"); n == 0 {
		t.Fatal("the tripping node never took the automation out of its scheduler")
	}

	// Every other node: the disarm broadcast, with the engine's kind.
	want := events.TopicAuthoringDisarmForBundle(bundleId) + "/" + events.KindAuthoringDisarm.String()
	if !eventually(2*time.Second, func() bool {
		return held(t, depsB, "automation", "autoTrip") == nil && held(t, depsB, "query", "queryTrip") == nil
	}) {
		crossedMu.Lock()
		defer crossedMu.Unlock()
		calls, _ := hooksB.snapshot()
		t.Fatalf("CROSS-NODE FAILURE: node B kept a tripped automation armed (broadcasts %v, B's scheduler calls %v)", crossed, calls)
	}
	crossedMu.Lock()
	if len(crossed) != 1 || crossed[0] != want {
		t.Errorf("the trip broadcast %v, want exactly %s", crossed, want)
	}
	crossedMu.Unlock()

	// And the database keeps it out: node B's reconcile reads it as paused.
	if _, err := engB.reconcileAuthoredRuntime(ctx, store); err != nil {
		t.Fatalf("reconcile node B: %v", err)
	}
	if held(t, depsB, "automation", "autoTrip") != nil || hooksB.count("register autoTrip") != 1 {
		t.Fatal("a reconcile pass armed a tripped automation again")
	}
}

// TestATripOnAnAutomationWhoseBundleIsNoLongerActiveRecordsNoPause: the bundle
// the node holds was retired (or superseded) elsewhere and this node had not
// heard yet. The trip takes it out here and broadcasts the disarm, but writes
// no pause: a retired bundle flipped to paused would read as resumable.
func TestATripOnAnAutomationWhoseBundleIsNoLongerActiveRecordsNoPause(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	bundleId := store.addBundle("v1:authoring:bundle:trip-retired", 1, "", BundleActive, [2]string{"automation", "autoTripGone"})
	eng, bus, deps, _ := newArmNode(t, store)
	if _, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
		t.Fatalf("setup: arm: %v", err)
	}
	store.setBundleStatus(bundleId, BundleRetired)
	var disarms atomic.Int64
	bus.Subscribe("authoring.disarm.#", func(events.Event) { disarms.Add(1) })

	applyTrip(t, ctx, eng, store, deps, tripOf("autoTripGone", bundleId, 1), TripBundleNotActive)
	if got := store.recorded(); len(got) != 0 {
		t.Fatalf("a trip wrote a pause onto a bundle that is not active: %+v", got)
	}
	if st := store.bundleStatus(bundleId); st != BundleRetired {
		t.Fatalf("the bundle reads %q, want it left retired", st)
	}
	if held(t, deps, "automation", "autoTripGone") != nil {
		t.Fatal("the tripping node still holds an automation whose bundle is retired")
	}
	if !eventually(time.Second, func() bool { return disarms.Load() == 1 }) {
		t.Fatalf("the trip broadcast %d disarms, want 1", disarms.Load())
	}
}

// TestATripThatCannotRecordThePauseStillStopsThisNode: the pause write fails.
// The automation stays stopped here -- paused in the registry, out of the
// scheduler, so this node's reconcile pass, which finds it held, leaves it
// alone -- the error says so, and nothing is broadcast: every other node still
// reads the bundle as active, and their own breakers stand.
func TestATripThatCannotRecordThePauseStillStopsThisNode(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore(), failPause: errors.New("database unavailable")}
	bundleId := store.addBundle("v1:authoring:bundle:trip-unrecorded", 1, "", BundleActive, [2]string{"automation", "autoTripLocal"})
	eng, bus, deps, hooks := newArmNode(t, store)
	if _, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
		t.Fatalf("setup: arm: %v", err)
	}
	var disarms atomic.Int64
	bus.Subscribe("authoring.disarm.#", func(events.Event) { disarms.Add(1) })

	outcome, err := eng.pauseTrippedAutomationWithStore(ctx, store, deps, tripOf("autoTripLocal", bundleId, 1))
	if err == nil || !strings.Contains(err.Error(), "database unavailable") || outcome != TripStoppedHere {
		t.Fatalf("a pause that could not be recorded must say so and stop it here only, got %q, %v", outcome, err)
	}
	c := held(t, deps, "automation", "autoTripLocal")
	if c == nil || c.Status != AuthoredPaused || hooks.count("deregister autoTripLocal") == 0 {
		t.Fatalf("the tripping node must keep the automation stopped: held %+v, deregisters %d", c, hooks.count("deregister autoTripLocal"))
	}
	if _, err := eng.reconcileAuthoredRuntime(ctx, store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c := held(t, deps, "automation", "autoTripLocal"); c == nil || c.Status != AuthoredPaused || hooks.count("register autoTripLocal") != 1 {
		t.Fatalf("a reconcile pass restarted a tripped automation whose pause was never recorded: %+v", c)
	}
	time.Sleep(50 * time.Millisecond)
	if n := disarms.Load(); n != 0 {
		t.Fatalf("a pause that was never recorded was broadcast (%d disarms); every peer reads the bundle as active and ignores it", n)
	}
}

// TestATripFinishesWhenItsCallerHangsUp: the trip is the system's own
// decision, so a caller that gives up -- the app bounds it with a minute --
// does not leave it half-done. With the caller's context already cancelled,
// the pause is still recorded, the listeners told, and the disarm broadcast.
func TestATripFinishesWhenItsCallerHangsUp(t *testing.T) {
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	bundleId := store.addBundle("v1:authoring:bundle:trip-hangup", 1, "", BundleActive, [2]string{"automation", "autoHangup"})
	eng, bus, deps, _ := newArmNode(t, store)
	if _, err := armBundleWithStore(context.Background(), store, armTestOwner, bundleId, deps); err != nil {
		t.Fatalf("setup: arm: %v", err)
	}
	var told, disarms atomic.Int32
	eng.SetAuthoredPauseListener("probe", func(ctx context.Context, _, _ string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		told.Add(1)
		return nil
	})
	bus.Subscribe("authoring.disarm.#", func(events.Event) { disarms.Add(1) })

	ctx, hangUp := context.WithCancel(context.Background())
	hangUp()
	applyTrip(t, ctx, eng, store, deps, tripOf("autoHangup", bundleId, 1), TripPaused)
	if st := store.bundleStatus(bundleId); st != BundlePaused || len(store.recorded()) != 1 {
		t.Fatalf("a trip whose caller hung up left the bundle %q (pauses written %+v)", st, store.recorded())
	}
	if n := told.Load(); n != 1 {
		t.Fatalf("a trip whose caller hung up told its listeners %d times, want once, on a live context", n)
	}
	if !eventually(2*time.Second, func() bool { return disarms.Load() == 1 }) {
		t.Fatalf("a trip whose caller hung up broadcast %d disarms, want 1", disarms.Load())
	}
}

// TestATripOfAnOlderVersionLeavesTheNewerOneArmed is the reviewer's probe B: a
// fix was saved just after version 1's fifth failure and armed before the
// trip's pause took the arming lock. The trip names version 1, which is no
// longer what this node holds, so the newer version stays armed and nothing is
// recorded against it.
func TestATripOfAnOlderVersionLeavesTheNewerOneArmed(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	v1 := store.addBundle("v1:authoring:bundle:trip-old", 1, "", BundleActive, [2]string{"automation", "autoFixed"})
	eng, _, deps, hooks := newArmNode(t, store)
	if _, err := armBundleWithStore(ctx, store, armTestOwner, v1, deps); err != nil {
		t.Fatal(err)
	}
	store.setBundleStatus(v1, BundleRetired)
	v2 := store.addBundle("v1:authoring:bundle:trip-fix", 2, v1, BundleActive, [2]string{"automation", "autoFixed"})
	if _, err := armBundleWithStore(ctx, store, armTestOwner, v2, deps); err != nil {
		t.Fatal(err)
	}
	calls0, _ := hooks.snapshot()

	applyTrip(t, ctx, eng, store, deps, tripOf("autoFixed", v1, 1), TripVersionNotHeld)
	if st := store.bundleStatus(v2); st != BundleActive || len(store.recorded()) != 0 {
		t.Fatalf("a trip caused by version 1 paused version 2: it reads %q, pauses written %+v", st, store.recorded())
	}
	if c := held(t, deps, "automation", "autoFixed"); c == nil || c.BundleId != v2 || c.Status != AuthoredActive {
		t.Fatalf("a trip caused by version 1 disturbed version 2: held %+v", c)
	}
	if got := callsSince(hooks, len(calls0)); len(got) != 0 {
		t.Fatalf("a trip caused by version 1 touched the scheduler: %v", got)
	}

	// An entry is the bundle AND the version it was armed from: a trip naming
	// this bundle id at another version -- an id written again at a later
	// version -- is not this entry's either.
	applyTrip(t, ctx, eng, store, deps, tripOf("autoFixed", v2, 1), TripVersionNotHeld)
	if st := store.bundleStatus(v2); st != BundleActive || len(store.recorded()) != 0 {
		t.Fatalf("a trip of version 1 of bundle %s paused its version 2: it reads %q, pauses written %+v", v2, st, store.recorded())
	}
	if c := held(t, deps, "automation", "autoFixed"); c == nil || c.BundleId != v2 || c.Version != 2 || c.Status != AuthoredActive {
		t.Fatalf("a trip of another version of the held bundle disturbed it: held %+v", c)
	}
}

// TestATripWhoseBreakerWasResetPausesNothing: the trip still names the version
// this node holds, but the breaker is no longer open -- the automation was
// re-activated (Activate resets it) between the trip and its pause. A fresh
// start is not paused for the failures that came before it.
func TestATripWhoseBreakerWasResetPausesNothing(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	b := store.addBundle("v1:authoring:bundle:trip-reset", 1, "", BundleActive, [2]string{"automation", "autoReset"})
	eng, _, deps, hooks := newArmNode(t, store)
	if _, err := armBundleWithStore(ctx, store, armTestOwner, b, deps); err != nil {
		t.Fatal(err)
	}
	calls0, _ := hooks.snapshot()
	trip := tripOf("autoReset", b, 1)
	trip.StillTripped = func() bool { return false }

	applyTrip(t, ctx, eng, store, deps, trip, TripBreakerReset)
	if st := store.bundleStatus(b); st != BundleActive || len(store.recorded()) != 0 {
		t.Fatalf("a reset breaker's trip paused the bundle: it reads %q, pauses written %+v", st, store.recorded())
	}
	if c := held(t, deps, "automation", "autoReset"); c == nil || c.Status != AuthoredActive {
		t.Fatalf("a reset breaker's trip stopped the automation: %+v", c)
	}
	if got := callsSince(hooks, len(calls0)); len(got) != 0 {
		t.Fatalf("a reset breaker's trip touched the scheduler: %v", got)
	}
}

// TestARecordedPauseIsReportedToTheListenersOnce: the node that writes the
// pause tells the listeners registered on it -- once, after the bundle row
// reads paused -- and no other node does: a peer that disarms by the broadcast
// reports nothing, nor does a trip that wrote no pause.
func TestARecordedPauseIsReportedToTheListenersOnce(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	bundleId := store.addBundle("v1:authoring:bundle:trip-heard", 1, "", BundleActive, [2]string{"automation", "autoHeard"})

	type heard struct{ owner, bundleId string }
	var mu sync.Mutex
	var onA, onB []heard
	listen := func(into *[]heard) AuthoredPauseListener {
		return func(_ context.Context, owner, bundleId string) error {
			mu.Lock()
			defer mu.Unlock()
			*into = append(*into, heard{owner, bundleId})
			return nil
		}
	}
	// reports copies what a node's listener heard; the lock is never held
	// across a pause, which calls the listener.
	reports := func(from *[]heard) []heard {
		mu.Lock()
		defer mu.Unlock()
		return append([]heard(nil), (*from)...)
	}
	busA := events.NewBus()
	t.Cleanup(busA.Close)
	engA := &MemQLEngine{}
	engA.SetEventBus(busA)
	depsA := (&armHooks{}).deps(NewAuthoredRuntimeRegistry())
	engA.SetAuthoredPauseListener("probe", listen(&onA))
	engB, busB, depsB, _ := newArmNode(t, store)
	engB.SetAuthoredPauseListener("probe", listen(&onB))
	busA.Subscribe("authoring.disarm.#", func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})
	for _, deps := range []AuthoredRuntimeDeps{depsA, depsB} {
		if _, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
			t.Fatalf("setup: arm: %v", err)
		}
	}

	applyTrip(t, ctx, engA, store, depsA, tripOf("autoHeard", bundleId, 1), TripPaused)
	if !eventually(2*time.Second, func() bool { return held(t, depsB, "automation", "autoHeard") == nil }) {
		t.Fatal("setup: node B never disarmed the paused bundle")
	}
	if got := reports(&onA); fmt.Sprint(got) != fmt.Sprint([]heard{{armTestOwner, bundleId}}) {
		t.Fatalf("the node that recorded the pause told its listeners %v, want exactly one report of the bundle", got)
	}
	if got := reports(&onB); len(got) != 0 {
		t.Fatalf("a peer that only disarmed reported the pause too (%v); the record is written once", got)
	}

	// A trip that writes no pause -- this node still held the bundle, which
	// another node had already retired -- reports nothing.
	store.setBundleStatus(bundleId, BundleRetired)
	if err := registerBundleConstructs([]*AuthoredConstruct{{OwnerUserId: armTestOwner, Kind: "automation", Name: "autoHeard",
		BundleId: bundleId, Version: 1, Source: "automation autoHeard { }"}}, depsA); err != nil {
		t.Fatalf("setup: hold the retired bundle again: %v", err)
	}
	applyTrip(t, ctx, engA, store, depsA, tripOf("autoHeard", bundleId, 1), TripBundleNotActive)
	if got := reports(&onA); len(got) != 1 {
		t.Fatalf("a trip that wrote no pause reported one: %v", got)
	}
}

// TestAStalledPauseListenerDoesNotHoldBackTheStop: the node that records a
// pause stops the automation on every replica FIRST and only then tells its
// listeners, on a bound of their own. A listener that stalls -- a slow
// database, a rule row that keeps changing -- must not delay the disarm the
// other replicas are waiting for, nor spend the time the broadcast needs.
func TestAStalledPauseListenerDoesNotHoldBackTheStop(t *testing.T) {
	ctx := context.Background()
	store := &pauseRecordingStore{armTestStore: newArmTestStore()}
	bundleId := store.addBundle("v1:authoring:bundle:trip-stall", 1, "", BundleActive, [2]string{"automation", "autoStall"})

	busA := events.NewBus()
	t.Cleanup(busA.Close)
	engA := &MemQLEngine{}
	engA.SetEventBus(busA)
	depsA := (&armHooks{}).deps(NewAuthoredRuntimeRegistry())
	_, busB, depsB, _ := newArmNode(t, store)
	busA.Subscribe("authoring.disarm.#", func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})
	for _, deps := range []AuthoredRuntimeDeps{depsA, depsB} {
		if _, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
			t.Fatalf("setup: arm: %v", err)
		}
	}
	// The listener stalls until the other replica has stopped the automation,
	// or two seconds pass.
	var stoppedWhileStalled atomic.Bool
	engA.SetAuthoredPauseListener("stall", func(context.Context, string, string) error {
		stoppedWhileStalled.Store(eventually(2*time.Second, func() bool { return held(t, depsB, "automation", "autoStall") == nil }))
		return nil
	})

	applyTrip(t, ctx, engA, store, depsA, tripOf("autoStall", bundleId, 1), TripPaused)
	if !stoppedWhileStalled.Load() {
		t.Fatal("the other replica was still running the automation while the pause listener stalled: the disarm went out after the listeners")
	}
}

// constructReadFailing is the shared store whose construct reads fail once
// failConstructs is set.
type constructReadFailing struct {
	*pauseRecordingStore
	failConstructs atomic.Bool
}

func (s *constructReadFailing) LoadConstructsForOwner(ctx context.Context, owner, id string) ([]AuthoringConstructRow, error) {
	if s.failConstructs.Load() {
		return nil, errors.New("database unavailable")
	}
	return s.pauseRecordingStore.LoadConstructsForOwner(ctx, owner, id)
}

// TestATripThatCannotFinishTakingTheBundleOutStillReportsThePause: the pause
// is recorded, and then this node cannot finish taking the bundle out -- its
// constructs cannot be read. The trip still reports the pause as recorded,
// because every replica's reconcile pass reads the bundle paused; the
// listeners are told; and the error says this node did not finish and that
// nothing was broadcast.
func TestATripThatCannotFinishTakingTheBundleOutStillReportsThePause(t *testing.T) {
	ctx := context.Background()
	store := &constructReadFailing{pauseRecordingStore: &pauseRecordingStore{armTestStore: newArmTestStore()}}
	bundleId := store.addBundle("v1:authoring:bundle:trip-half", 1, "", BundleActive, [2]string{"automation", "autoHalf"})
	eng, bus, deps, _ := newArmNode(t, store)
	if _, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
		t.Fatalf("setup: arm: %v", err)
	}
	var told, disarms atomic.Int32
	eng.SetAuthoredPauseListener("probe", func(context.Context, string, string) error { told.Add(1); return nil })
	bus.Subscribe("authoring.disarm.#", func(events.Event) { disarms.Add(1) })
	store.failConstructs.Store(true)

	outcome, err := eng.pauseTrippedAutomationWithStore(ctx, store, deps, tripOf("autoHalf", bundleId, 1))
	if outcome != TripPaused || err == nil || !strings.Contains(err.Error(), "not broadcast") {
		t.Fatalf("a recorded pause this node could not finish did %q, %v; want %q and an error saying so", outcome, err, TripPaused)
	}
	if st := store.bundleStatus(bundleId); st != BundlePaused {
		t.Fatalf("the bundle reads %q; the pause was recorded", st)
	}
	if n := told.Load(); n != 1 {
		t.Fatalf("the listeners were told %d times; the pause is recorded, so they are told once", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := disarms.Load(); n != 0 {
		t.Fatalf("%d disarms were broadcast for a take-out this node did not finish", n)
	}
}
