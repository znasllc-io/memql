package memql

// authoring_arm_propagate_test.go -- an activated authored bundle is armed on
// EVERY replica, and a retired one disarmed on every replica (memql#6022, F1).
//
// Internal (package memql) so it can drive the unexported store seams and the
// engine's broadcast with fakes -- no live DB. One fake stands in for the shared
// Postgres both "nodes" read, which is the property under test: the broadcast
// carries only a bundle id and an owner, and every replica reads the rest from
// the one database.
//
// The hop through the real routing rule, against a real Postgres, is
// test/clustere2e/email_rule_arming_db_test.go.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

const armTestOwner = "v1:identity:user:arm-owner"

// armTestStore is the shared database: it serves the activation orchestrator
// (ActivationStore) on the node that activates and the arm subscriber
// (armStore) on every node, over one set of rows. Thread-safe, because the
// subscriber reads it from the bus's delivery goroutines.
type armTestStore struct {
	mu         sync.Mutex
	bundles    map[string]AuthoringBundleRow
	constructs map[string][]AuthoringConstructRow
	// armReads counts the arm subscriber's bundle reads, so a test can tell
	// "the subscriber skipped this event" from "it read and found nothing to do".
	armReads atomic.Int64
}

func newArmTestStore() *armTestStore {
	return &armTestStore{
		bundles:    map[string]AuthoringBundleRow{},
		constructs: map[string][]AuthoringConstructRow{},
	}
}

// addBundle writes a bundle row and one construct row per (kind, name) pair.
func (s *armTestStore) addBundle(id string, version int, supersedes string, status BundleStatus, members ...[2]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles[id] = AuthoringBundleRow{
		Id: id, OwnerUserId: armTestOwner, Title: "arm test " + id,
		Status: status, Version: version, SupersedesBundleId: supersedes,
	}
	rows := make([]AuthoringConstructRow, 0, len(members))
	for _, m := range members {
		rows = append(rows, AuthoringConstructRow{
			Id: id + ":" + m[1], OwnerUserId: armTestOwner, BundleId: id,
			Kind: m[0], Name: m[1], Source: m[0] + " " + m[1] + " { }", Status: "draft",
		})
	}
	s.constructs[id] = rows
	return id
}

func (s *armTestStore) bundleStatus(id string) BundleStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundles[id].Status
}

func (s *armTestStore) setBundleStatus(id string, status BundleStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bundles[id]
	b.Status = status
	s.bundles[id] = b
}

// --- ActivationStore (the activating node) ---

func (s *armTestStore) LoadBundle(_ context.Context, id string) (AuthoringBundleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundles[id], nil
}

func (s *armTestStore) LoadConstructs(_ context.Context, id string) ([]AuthoringConstructRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuthoringConstructRow(nil), s.constructs[id]...), nil
}

func (s *armTestStore) SetBundleActive(_ context.Context, id string) error {
	s.setBundleStatus(id, BundleActive)
	return nil
}

func (s *armTestStore) SetBundleRetired(_ context.Context, id string) error {
	s.setBundleStatus(id, BundleRetired)
	return nil
}

func (s *armTestStore) SetConstructStatus(_ context.Context, constructId, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for bundleId, rows := range s.constructs {
		for i := range rows {
			if rows[i].Id == constructId {
				s.constructs[bundleId][i].Status = status
			}
		}
	}
	return nil
}

// --- armStore (every node's subscriber) ---

func (s *armTestStore) LoadBundleForOwner(_ context.Context, owner, id string) (AuthoringBundleRow, error) {
	s.armReads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bundles[id]
	if b.OwnerUserId != owner {
		return AuthoringBundleRow{}, nil // owner-scoped: another owner's bundle reads as absent
	}
	return b, nil
}

func (s *armTestStore) LoadConstructsForOwner(_ context.Context, owner, id string) ([]AuthoringConstructRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuthoringConstructRow
	for _, c := range s.constructs[id] {
		if c.OwnerUserId == owner {
			out = append(out, c)
		}
	}
	return out, nil
}

// armHooks records one node's scheduler: every register and deregister in
// order, and which automations are live at the end.
type armHooks struct {
	mu     sync.Mutex
	calls  []string
	active map[string]int // name -> live version
}

func (h *armHooks) deps(registry *AuthoredRuntimeRegistry) AuthoredRuntimeDeps {
	return AuthoredRuntimeDeps{
		Registry: registry,
		RegisterHook: func(c *AuthoredConstruct) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.active == nil {
				h.active = map[string]int{}
			}
			h.calls = append(h.calls, fmt.Sprintf("register %s@%d", c.Name, c.Version))
			h.active[c.Name] = c.Version
			return nil
		},
		DeregisterHook: func(_, name string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.calls = append(h.calls, "deregister "+name)
			delete(h.active, name)
		},
	}
}

func (h *armHooks) count(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func (h *armHooks) snapshot() ([]string, map[string]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	active := map[string]int{}
	for k, v := range h.active {
		active[k] = v
	}
	return append([]string(nil), h.calls...), active
}

// peerEvent is an arm or disarm broadcast as a PEER's copy arrives: the engine's
// kind, and the origin node the mesh stamps on everything it delivers.
func peerEvent(topic string, kind events.Kind, bundleId string) events.Event {
	ev := events.NewEvent(topic, kind, map[string]any{"bundleId": bundleId, "owner": armTestOwner})
	ev.OriginNodeId = "node-A"
	return ev
}

// TestActivatingASupersedingBundleKeepsTheNewVersionArmed: an edit activates
// version 2 of a bundle whose automation keeps its name. The activation retires
// version 1 -- and that retire used to tear its constructs down BY NAME, which
// is version 2's name: the new version was deregistered from the scheduler and
// deleted from the registry the moment it went live, so an edited rule stopped
// firing at all until the next boot re-armed it.
func TestActivatingASupersedingBundleKeepsTheNewVersionArmed(t *testing.T) {
	ctx := context.Background()
	store := newArmTestStore()
	v1 := store.addBundle("v1:authoring:bundle:edit-v1", 1, "", BundleDryRunPassed,
		[2]string{"automation", "autoEdited"}, [2]string{"trait", "droppedInV2"})

	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v1, deps); err != nil {
		t.Fatalf("activate version 1: %v", err)
	}

	v2 := store.addBundle("v1:authoring:bundle:edit-v2", 2, v1, BundleDryRunPassed,
		[2]string{"automation", "autoEdited"})
	if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v2, deps); err != nil {
		t.Fatalf("activate version 2: %v", err)
	}

	got, ok := deps.Registry.Lookup(armTestOwner, "automation", "autoEdited")
	if !ok {
		t.Fatal("the superseding activation left autoEdited unregistered: retiring version 1 by name deleted version 2")
	}
	if got.Version != 2 || got.Status != AuthoredActive {
		t.Fatalf("autoEdited is version %d (%s), want version 2 active", got.Version, got.Status)
	}
	calls, active := hooks.snapshot()
	if len(calls) == 0 || calls[len(calls)-1] != "register autoEdited@2" {
		t.Fatalf("the scheduler's last word on autoEdited must be version 2's register, got %v", calls)
	}
	if active["autoEdited"] != 2 {
		t.Fatalf("autoEdited is not live in the scheduler at version 2: %v (calls %v)", active, calls)
	}

	// What version 2 does NOT register still goes: the keep is by kind and
	// name, not "skip the retire".
	if _, ok := deps.Registry.Lookup(armTestOwner, "trait", "droppedInV2"); ok {
		t.Fatal("droppedInV2 is in version 1 only and must be retired with it")
	}
	// And the rows record the supersession exactly as before.
	if st := store.bundleStatus(v1); st != BundleRetired {
		t.Fatalf("version 1's bundle is %q, want retired", st)
	}
}

// TestAuthoringArmPropagation_CrossNode is the load-bearing cross-node test.
// Two engines with their own registries and schedulers, one shared store, and a
// bridge carrying authoring.arm.* and authoring.disarm.* from A's bus to B's the
// way the mesh does. Before F1 an activation armed only the node that handled
// it; B learned of the bundle at its next boot, and a retire on A left B firing
// until B restarted.
func TestAuthoringArmPropagation_CrossNode(t *testing.T) {
	busA, busB := events.NewBus(), events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })
	bridge := func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	}
	busA.Subscribe("authoring.arm.#", bridge)
	busA.Subscribe("authoring.disarm.#", bridge)

	store := newArmTestStore()
	bundleId := store.addBundle("v1:authoring:bundle:cross", 1, "", BundleDryRunPassed,
		[2]string{"automation", "autoCross"}, [2]string{"query", "queryCross"})

	engA := &MemQLEngine{}
	engA.SetEventBus(busA)
	engB := &MemQLEngine{}
	engB.SetEventBus(busB)

	hooksA, hooksB := &armHooks{}, &armHooks{}
	depsA := hooksA.deps(NewAuthoredRuntimeRegistry())
	depsB := hooksB.deps(NewAuthoredRuntimeRegistry())
	engB.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return depsB })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	engB.startAuthoringArmSubscriberWithStore(ctx, store)

	if _, err := engA.activateApprovedBundleWithStore(ctx, store, armTestOwner, bundleId, depsA); err != nil {
		t.Fatalf("activate on A: %v", err)
	}
	if _, ok := depsA.Registry.Lookup(armTestOwner, "automation", "autoCross"); !ok {
		t.Fatal("the activating node did not register its own bundle")
	}

	if !eventually(2*time.Second, func() bool {
		_, ok := depsB.Registry.Lookup(armTestOwner, "automation", "autoCross")
		return ok && hooksB.count("register autoCross") == 1
	}) {
		calls, _ := hooksB.snapshot()
		t.Fatalf("CROSS-NODE FAILURE: node B never armed the bundle node A activated (B's scheduler calls %v)", calls)
	}
	if _, ok := depsB.Registry.Lookup(armTestOwner, "query", "queryCross"); !ok {
		t.Fatal("node B armed the automation but not the query it depends on")
	}

	if err := engA.retireActiveBundleWithStore(ctx, store, armTestOwner, bundleId, depsA); err != nil {
		t.Fatalf("retire on A: %v", err)
	}
	if !eventually(2*time.Second, func() bool {
		_, ok := depsB.Registry.Lookup(armTestOwner, "automation", "autoCross")
		return !ok && hooksB.count("deregister autoCross") == 1
	}) {
		calls, _ := hooksB.snapshot()
		t.Fatalf("CROSS-NODE FAILURE: node B kept firing a bundle node A retired (B's scheduler calls %v)", calls)
	}
	if _, ok := depsB.Registry.Lookup(armTestOwner, "query", "queryCross"); ok {
		t.Fatal("node B disarmed the automation but kept the query")
	}
	if n := hooksB.count("register autoCross"); n != 1 {
		t.Fatalf("node B registered autoCross %d times, want 1", n)
	}
}

// TestTheOriginSkipsItsOwnArmEvent: the node that activates a bundle armed it
// in the call that published the broadcast, so its own copy of the broadcast --
// the one with no origin node -- must not send it back to the database for a
// second look. A PEER's copy of the same event is read: the control that makes
// the skip mean something.
func TestTheOriginSkipsItsOwnArmEvent(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	store := newArmTestStore()
	bundleId := store.addBundle("v1:authoring:bundle:origin", 1, "", BundleDryRunPassed,
		[2]string{"automation", "autoOrigin"})

	eng := &MemQLEngine{}
	eng.SetEventBus(bus)
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng.startAuthoringArmSubscriberWithStore(ctx, store)

	if _, err := eng.activateApprovedBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// The activation's own broadcast is in flight on this bus. Deliver a copy
	// of it synchronously, so the assertion is not racing the bus.
	own := events.NewEvent(events.TopicAuthoringArmForBundle(bundleId), events.KindAuthoringArm,
		map[string]any{"bundleId": bundleId, "owner": armTestOwner})
	bus.PublishSync(own)
	if n := store.armReads.Load(); n != 0 {
		t.Fatalf("the origin re-read its own bundle for its own arm event (%d reads); it armed the bundle in the call that published it", n)
	}
	if n := hooks.count("register autoOrigin"); n != 1 {
		t.Fatalf("the origin registered autoOrigin %d times, want 1", n)
	}

	// Control: the same broadcast from a peer IS read, and a version this node
	// already holds is not registered a second time.
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(bundleId), events.KindAuthoringArm, bundleId))
	if store.armReads.Load() == 0 {
		t.Fatal("control failed: a peer's arm event was never read, so the skip above proves nothing")
	}
	if n := hooks.count("register autoOrigin"); n != 1 {
		t.Fatalf("a peer's arm for a version this node already holds registered it again (%d registers)", n)
	}
	// And holding it is an answer, not a failure: the registry would refuse an
	// equal version, so an arm that tried would log a failed propagation for a
	// node that is exactly where it should be.
	out, err := armBundleWithStore(ctx, store, armTestOwner, bundleId, deps)
	if err != nil || out.AlreadyHeld != 1 || out.Registered != 0 {
		t.Fatalf("arming a bundle this node already holds: outcome %+v, error %v; want it held, no error", out, err)
	}
}

// TestDisarmLeavesANewerVersionArmed: node B holds version 2 of a bundle's
// automation when version 1's disarm arrives (a retire on one node racing an
// edit on another, or a broadcast delivered late). Version 1 and version 2
// share the construct name, so a disarm by name would take version 2 down.
func TestDisarmLeavesANewerVersionArmed(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	store := newArmTestStore()
	v1 := store.addBundle("v1:authoring:bundle:keep-v1", 1, "", BundleRetired, [2]string{"automation", "autoKeep"})
	v2 := store.addBundle("v1:authoring:bundle:keep-v2", 2, v1, BundleActive, [2]string{"automation", "autoKeep"})

	eng := &MemQLEngine{}
	eng.SetEventBus(bus)
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng.startAuthoringArmSubscriberWithStore(ctx, store)

	// B armed version 2 from its own broadcast.
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(v2), events.KindAuthoringArm, v2))
	if got, ok := deps.Registry.Lookup(armTestOwner, "automation", "autoKeep"); !ok || got.Version != 2 {
		t.Fatalf("setup: node B should hold autoKeep version 2, got %+v (ok=%v)", got, ok)
	}
	reads := store.armReads.Load()

	bus.PublishSync(peerEvent(events.TopicAuthoringDisarmForBundle(v1), events.KindAuthoringDisarm, v1))
	if store.armReads.Load() == reads {
		t.Fatal("control failed: the disarm for version 1 was never read, so the assertions below prove nothing")
	}
	got, ok := deps.Registry.Lookup(armTestOwner, "automation", "autoKeep")
	if !ok || got.Version != 2 || got.Status != AuthoredActive {
		t.Fatalf("version 1's disarm took down version 2: %+v (ok=%v)", got, ok)
	}
	calls, active := hooks.snapshot()
	if n := hooks.count("deregister autoKeep"); n != 0 || active["autoKeep"] != 2 {
		t.Fatalf("version 1's disarm deregistered version 2 from the scheduler: calls %v, live %v", calls, active)
	}
}

// TestTheDatabaseDecidesWhatABroadcastChanges: the broadcast names a bundle and
// the database says what to do with it. A disarm for a bundle the database
// still calls active changes nothing (a late broadcast, or one the engine did
// not send); an arm for a bundle that is not active arms nothing; and an event
// on these topics without the engine's kind -- what an authored event step
// publishing to authoring.disarm.* produces -- is not read at all.
func TestTheDatabaseDecidesWhatABroadcastChanges(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	store := newArmTestStore()
	live := store.addBundle("v1:authoring:bundle:live", 1, "", BundleActive, [2]string{"automation", "autoLive"})
	retired := store.addBundle("v1:authoring:bundle:gone", 1, "", BundleRetired, [2]string{"automation", "autoGone"})

	eng := &MemQLEngine{}
	eng.SetEventBus(bus)
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng.startAuthoringArmSubscriberWithStore(ctx, store)

	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(live), events.KindAuthoringArm, live))
	if _, ok := deps.Registry.Lookup(armTestOwner, "automation", "autoLive"); !ok {
		t.Fatal("setup: a peer's arm for an active bundle should arm it")
	}

	// Not the engine's kind: not read, nothing changes.
	reads := store.armReads.Load()
	forged := peerEvent(events.TopicAuthoringDisarmForBundle(live), events.KindMessage, live)
	bus.PublishSync(forged)
	if store.armReads.Load() != reads {
		t.Fatal("a disarm-topic event without the engine's kind was read; any automation could publish one")
	}

	// The engine's kind, for a bundle the database still calls active.
	bus.PublishSync(peerEvent(events.TopicAuthoringDisarmForBundle(live), events.KindAuthoringDisarm, live))
	if store.armReads.Load() == reads {
		t.Fatal("control failed: the engine-kind disarm was never read")
	}
	if _, ok := deps.Registry.Lookup(armTestOwner, "automation", "autoLive"); !ok || hooks.count("deregister autoLive") != 0 {
		t.Fatal("a disarm for a bundle the database calls active took it down")
	}

	// An arm for a bundle that is not active.
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(retired), events.KindAuthoringArm, retired))
	if _, ok := deps.Registry.Lookup(armTestOwner, "automation", "autoGone"); ok || hooks.count("register autoGone") != 0 {
		t.Fatal("an arm for a retired bundle armed it")
	}
}
