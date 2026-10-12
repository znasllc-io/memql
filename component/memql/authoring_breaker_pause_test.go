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

func (s *pauseRecordingStore) PauseBundleForOwner(_ context.Context, owner, bundleId, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPause != nil {
		return s.failPause
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

	if err := engA.pauseTrippedAutomationWithStore(ctx, store, depsA, armTestOwner, "autoTrip", tripReason); err != nil {
		t.Fatalf("pause the tripped automation: %v", err)
	}

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

	if err := eng.pauseTrippedAutomationWithStore(ctx, store, deps, armTestOwner, "autoTripGone", tripReason); err != nil {
		t.Fatalf("pause: %v", err)
	}
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

	err := eng.pauseTrippedAutomationWithStore(ctx, store, deps, armTestOwner, "autoTripLocal", tripReason)
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("a pause that could not be recorded must say so, got %v", err)
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
