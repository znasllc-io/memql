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

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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

// setSource rewrites one construct's source in place: the same bundle id and
// version carrying different text, which is what an email rule edited while
// paused and then resumed leaves behind.
func (s *armTestStore) setSource(bundleId, name, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.constructs[bundleId] {
		if s.constructs[bundleId][i].Name == name {
			s.constructs[bundleId][i].Source = source
		}
	}
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

// --- fix round 1: identity is bundle + version + source, and the database decides ---

// newArmNode stands up one peer: its own bus, registry and recording
// scheduler, and the arm subscriber over the shared store -- and NOT the
// reconcile loop, so a test sees exactly what the broadcasts do.
func newArmNode(t *testing.T, store armStore) (*MemQLEngine, *events.Bus, AuthoredRuntimeDeps, *armHooks) {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	eng := &MemQLEngine{}
	eng.SetEventBus(bus)
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng.startAuthoringArmSubscriberWithStore(ctx, store)
	return eng, bus, deps, hooks
}

func held(t *testing.T, deps AuthoredRuntimeDeps, kind, name string) *AuthoredConstruct {
	t.Helper()
	c, ok := deps.Registry.Lookup(armTestOwner, kind, name)
	if !ok {
		return nil
	}
	return c
}

// TestAnArmIsNotHeldByAnotherBundlesEntry is case (a). A rule's edit armed
// bundle Y at version 2; the rule was then paused (Y retired) and resumed,
// which restarts at version 1 under a different bundle id, X. This node gets
// X's arm BEFORE Y's disarm. Comparing versions alone, X@1 reads as "already
// held" by Y@2 and is skipped, and Y's disarm then removes the name: the rule
// is active in the database and armed nowhere on this node.
func TestAnArmIsNotHeldByAnotherBundlesEntry(t *testing.T) {
	store := newArmTestStore()
	y := store.addBundle("v1:authoring:bundle:rule-y", 2, "", BundleActive, [2]string{"automation", "autoRule"})
	_, bus, deps, _ := newArmNode(t, store)
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(y), events.KindAuthoringArm, y))
	if c := held(t, deps, "automation", "autoRule"); c == nil || c.BundleId != y {
		t.Fatalf("setup: the node should hold autoRule for bundle Y, got %+v", c)
	}

	store.setBundleStatus(y, BundleRetired)
	x := store.addBundle("v1:authoring:bundle:rule-x", 1, "", BundleActive, [2]string{"automation", "autoRule"})
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(x), events.KindAuthoringArm, x))
	bus.PublishSync(peerEvent(events.TopicAuthoringDisarmForBundle(y), events.KindAuthoringDisarm, y))

	c := held(t, deps, "automation", "autoRule")
	if c == nil || c.BundleId != x || c.Version != 1 || c.Status != AuthoredActive {
		t.Fatalf("the rule is active as bundle X and this node holds %+v; version 2 of a retired bundle is not a newer copy of it", c)
	}
}

// TestAnArmReplacesAStaleDefinitionOfTheSameBundle is case (b). This node
// missed a disarm, so it still holds bundle X at version 1 -- and X was
// edited while paused and resumed, which writes the SAME bundle id at the SAME
// version with new source. An arm that compares versions keeps the old
// definition for good: the wrong trigger, the wrong condition, the wrong mail.
func TestAnArmReplacesAStaleDefinitionOfTheSameBundle(t *testing.T) {
	store := newArmTestStore()
	x := store.addBundle("v1:authoring:bundle:paused", 1, "", BundleActive, [2]string{"automation", "autoPaused"})
	_, bus, deps, hooks := newArmNode(t, store)
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(x), events.KindAuthoringArm, x))

	const edited = `automation autoPaused { edited while paused }`
	store.setSource(x, "autoPaused", edited)
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(x), events.KindAuthoringArm, x))

	c := held(t, deps, "automation", "autoPaused")
	if c == nil || c.Source != edited {
		t.Fatalf("the node kept the definition from before the edit: %+v", c)
	}
	calls, active := hooks.snapshot()
	if n := hooks.count("register autoPaused"); n != 2 || active["autoPaused"] != 1 {
		t.Fatalf("the edited definition must be handed to the scheduler again: calls %v, live %v", calls, active)
	}
}

// TestADisarmRetiresAnEntryWhoseOwnBundleIsRetired is case (c). The node holds
// R@1. Elsewhere Y@2 supersedes R and is then retired, and the node hears Y's
// disarm first and Y's arm late. Y's disarm sees R@1 under Y's construct name
// and skips it (a different version); the late arm reads Y retired and stops.
// Nothing is active, and the node still fires R.
func TestADisarmRetiresAnEntryWhoseOwnBundleIsRetired(t *testing.T) {
	store := newArmTestStore()
	r := store.addBundle("v1:authoring:bundle:old-r", 1, "", BundleActive, [2]string{"automation", "autoChain"})
	_, bus, deps, hooks := newArmNode(t, store)
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(r), events.KindAuthoringArm, r))

	store.setBundleStatus(r, BundleRetired)
	y := store.addBundle("v1:authoring:bundle:new-y", 2, r, BundleRetired, [2]string{"automation", "autoChain"})
	bus.PublishSync(peerEvent(events.TopicAuthoringDisarmForBundle(y), events.KindAuthoringDisarm, y))
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(y), events.KindAuthoringArm, y))

	if c := held(t, deps, "automation", "autoChain"); c != nil {
		t.Fatalf("no bundle of this rule reads active, and the node still holds %+v", c)
	}
	if n := hooks.count("deregister autoChain"); n != 1 {
		t.Fatalf("the retired version was never taken out of the scheduler (%d deregisters)", n)
	}
}

// TestAPeerFollowsASupersedingActivation: node B holds version 1 -- an
// automation and a trait version 2 drops -- when node A activates version 2
// through the bridge. B must end on version 2, live in its scheduler, with the
// dropped trait gone.
func TestAPeerFollowsASupersedingActivation(t *testing.T) {
	store := newArmTestStore()
	busA := events.NewBus()
	t.Cleanup(busA.Close)
	engA := &MemQLEngine{}
	engA.SetEventBus(busA)
	depsA := (&armHooks{}).deps(NewAuthoredRuntimeRegistry())
	_, busB, depsB, hooksB := newArmNode(t, store)
	bridge := func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	}
	busA.Subscribe("authoring.arm.#", bridge)
	busA.Subscribe("authoring.disarm.#", bridge)

	ctx := context.Background()
	v1 := store.addBundle("v1:authoring:bundle:sup-v1", 1, "", BundleDryRunPassed,
		[2]string{"automation", "autoSup"}, [2]string{"trait", "droppedTrait"})
	if _, err := engA.activateApprovedBundleWithStore(ctx, store, armTestOwner, v1, depsA); err != nil {
		t.Fatalf("activate version 1 on A: %v", err)
	}
	if !eventually(2*time.Second, func() bool {
		return held(t, depsB, "automation", "autoSup") != nil && held(t, depsB, "trait", "droppedTrait") != nil
	}) {
		t.Fatal("setup: node B never armed version 1")
	}

	v2 := store.addBundle("v1:authoring:bundle:sup-v2", 2, v1, BundleDryRunPassed, [2]string{"automation", "autoSup"})
	if _, err := engA.activateApprovedBundleWithStore(ctx, store, armTestOwner, v2, depsA); err != nil {
		t.Fatalf("activate version 2 on A: %v", err)
	}
	if !eventually(2*time.Second, func() bool {
		c := held(t, depsB, "automation", "autoSup")
		return c != nil && c.Version == 2 && held(t, depsB, "trait", "droppedTrait") == nil
	}) {
		calls, active := hooksB.snapshot()
		t.Fatalf("node B did not follow the supersede: autoSup %+v, droppedTrait %+v, calls %v, live %v",
			held(t, depsB, "automation", "autoSup"), held(t, depsB, "trait", "droppedTrait"), calls, active)
	}
	calls, active := hooksB.snapshot()
	if calls[len(calls)-1] != "register autoSup@2" || active["autoSup"] != 2 {
		t.Fatalf("node B's scheduler must end on version 2: calls %v, live %v", calls, active)
	}
}

// gatedArmStore holds one lifecycle write open, so a test can deliver a peer's
// broadcast while the node is in the middle of its own activation or retire.
type gatedArmStore struct {
	*armTestStore
	block   string // "active" or "retired": the write to hold
	entered chan struct{}
	release chan struct{}
}

func (s *gatedArmStore) hold() {
	close(s.entered)
	<-s.release
}

func (s *gatedArmStore) SetBundleActive(ctx context.Context, id string) error {
	if s.block == "active" {
		s.hold()
	}
	return s.armTestStore.SetBundleActive(ctx, id)
}

func (s *gatedArmStore) SetBundleRetired(ctx context.Context, id string) error {
	if s.block == "retired" {
		s.hold()
	}
	return s.armTestStore.SetBundleRetired(ctx, id)
}

// TestTheOriginAppliesNoBroadcastInTheMiddleOfItsOwnChange: a node's own
// activation or retire and a peer's broadcast both rewrite this node's runtime,
// so they take the same lock. A broadcast that lands mid-change waits for it.
func TestTheOriginAppliesNoBroadcastInTheMiddleOfItsOwnChange(t *testing.T) {
	for _, step := range []string{"activate", "retire"} {
		t.Run(step, func(t *testing.T) {
			base := newArmTestStore()
			peer := base.addBundle("v1:authoring:bundle:peer-"+step, 1, "", BundleActive, [2]string{"automation", "autoPeer"})
			mine := base.addBundle("v1:authoring:bundle:mine-"+step, 1, "", BundleDryRunPassed, [2]string{"automation", "autoMine"})
			store := &gatedArmStore{armTestStore: base, entered: make(chan struct{}), release: make(chan struct{})}
			eng, bus, deps, _ := newArmNode(t, store)
			ctx := context.Background()

			change := func() error {
				_, err := eng.activateApprovedBundleWithStore(ctx, store, armTestOwner, mine, deps)
				return err
			}
			if step == "retire" {
				if err := change(); err != nil {
					t.Fatalf("setup: activate: %v", err)
				}
				change = func() error { return eng.retireActiveBundleWithStore(ctx, store, armTestOwner, mine, deps) }
				store.block = "retired"
			} else {
				store.block = "active"
			}

			done := make(chan error, 1)
			go func() { done <- change() }()
			<-store.entered
			reads := store.armReads.Load()
			bus.Publish(peerEvent(events.TopicAuthoringArmForBundle(peer), events.KindAuthoringArm, peer))
			time.Sleep(200 * time.Millisecond)
			if got := store.armReads.Load(); got != reads {
				t.Fatalf("a peer's broadcast was applied in the middle of this node's own %s", step)
			}
			close(store.release)
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if !eventually(2*time.Second, func() bool { return held(t, deps, "automation", "autoPeer") != nil }) {
				t.Fatalf("the peer's broadcast was never applied after the %s finished", step)
			}
		})
	}
}

// TestARetireLeavesAConstructAnotherLiveBundleHolds: the explicit retire tore
// its constructs down by NAME. When the name is held for another bundle that
// still reads active -- the resumed version of the same rule -- that is the
// live definition, and the retire of an old bundle must not take it.
func TestARetireLeavesAConstructAnotherLiveBundleHolds(t *testing.T) {
	ctx := context.Background()
	store := newArmTestStore()
	live := store.addBundle("v1:authoring:bundle:shared-live", 1, "", BundleActive, [2]string{"automation", "autoShared"})
	old := store.addBundle("v1:authoring:bundle:shared-old", 2, "", BundleActive, [2]string{"automation", "autoShared"})
	eng := &MemQLEngine{}
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	if _, err := armBundleWithStore(ctx, store, armTestOwner, live, deps); err != nil {
		t.Fatalf("setup: arm the live bundle: %v", err)
	}

	if err := eng.retireActiveBundleWithStore(ctx, store, armTestOwner, old, deps); err != nil {
		t.Fatalf("retire the old bundle: %v", err)
	}
	if c := held(t, deps, "automation", "autoShared"); c == nil || c.BundleId != live {
		t.Fatalf("retiring the old bundle took the live bundle's construct: %+v", c)
	}
	if n := hooks.count("deregister autoShared"); n != 0 {
		t.Fatalf("retiring the old bundle deregistered the live one from the scheduler (%d deregisters)", n)
	}
	if st := store.bundleStatus(old); st != BundleRetired {
		t.Fatalf("the old bundle is %q, want retired", st)
	}
}

// TestReactivatingOverAStaleEntryReplacesIt: the node handling a resume can be
// the one that missed the pause's disarm. It holds the paused definition --
// same bundle id, same version -- and the activation's registration refused the
// resume outright ("does not supersede"), failing the rule.
func TestReactivatingOverAStaleEntryReplacesIt(t *testing.T) {
	ctx := context.Background()
	store := newArmTestStore()
	x := store.addBundle("v1:authoring:bundle:resumed", 1, "", BundleDryRunPassed, [2]string{"automation", "autoResumed"})
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	stale := &AuthoredConstruct{
		OwnerUserId: armTestOwner, Kind: "automation", Name: "autoResumed",
		BundleId: x, Version: 1, Source: "automation autoResumed { before the edit }",
	}
	if err := registerBundleConstructs([]*AuthoredConstruct{stale}, deps); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, x, deps); err != nil {
		t.Fatalf("resuming over a stale copy of the same bundle failed: %v", err)
	}
	c := held(t, deps, "automation", "autoResumed")
	if c == nil || c.Source != "automation autoResumed { }" {
		t.Fatalf("the node kept the definition from before the edit: %+v", c)
	}
	if n := hooks.count("register autoResumed"); n != 2 {
		t.Fatalf("the resumed definition must be handed to the scheduler (%d registers, want 2)", n)
	}
}

// --- fix round 1: converge on the database ---

// LoadActiveBundles is the reconcile pass's listing: every active bundle.
func (s *armTestStore) LoadActiveBundles(_ context.Context) ([]AuthoringBundleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuthoringBundleRow
	for _, b := range s.bundles {
		if b.Status == BundleActive {
			out = append(out, b)
		}
	}
	return out, nil
}

// TestAReconcileRepairsALostArmAndALostDisarm: broadcasts are best effort --
// a dropped transport or a node still starting misses them -- so each node
// reconciles against the database. A pass arms an active bundle the node
// never heard of, takes out one whose disarm it missed, and is a no-op on a
// node that already agrees.
func TestAReconcileRepairsALostArmAndALostDisarm(t *testing.T) {
	ctx := context.Background()
	store := newArmTestStore()
	missed := store.addBundle("v1:authoring:bundle:missed-arm", 1, "", BundleActive, [2]string{"automation", "autoMissed"})
	gone := store.addBundle("v1:authoring:bundle:missed-disarm", 1, "", BundleActive,
		[2]string{"automation", "autoGone"}, [2]string{"query", "queryGone"})
	eng, _, deps, hooks := newArmNode(t, store)
	if _, err := armBundleWithStore(ctx, store, armTestOwner, gone, deps); err != nil {
		t.Fatalf("setup: arm the bundle whose disarm will be lost: %v", err)
	}
	store.setBundleStatus(gone, BundleRetired) // ...and its disarm never arrives

	if _, err := eng.reconcileAuthoredRuntime(ctx, store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c := held(t, deps, "automation", "autoMissed"); c == nil || c.BundleId != missed {
		t.Fatalf("the reconcile did not arm a bundle whose arm was lost: %+v", c)
	}
	if held(t, deps, "automation", "autoGone") != nil || held(t, deps, "query", "queryGone") != nil {
		t.Fatal("the reconcile left a retired bundle armed: its disarm was lost and nothing took it out")
	}
	if n := hooks.count("deregister autoGone"); n != 1 {
		t.Fatalf("the retired automation was not taken out of the scheduler (%d deregisters)", n)
	}

	before, _ := hooks.snapshot()
	if _, err := eng.reconcileAuthoredRuntime(ctx, store); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if after, _ := hooks.snapshot(); len(after) != len(before) {
		t.Fatalf("a reconcile over a node that already agrees changed it: %v -> %v", before, after)
	}
}

// TestArmingStartsWithAReconcile: a node misses every broadcast sent before it
// subscribed (the boot window). Starting the arming runs a reconcile pass, so
// a bundle activated in that window is armed without any broadcast.
func TestArmingStartsWithAReconcile(t *testing.T) {
	store := newArmTestStore()
	store.addBundle("v1:authoring:bundle:before-start", 1, "", BundleActive, [2]string{"automation", "autoBefore"})
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	eng := &MemQLEngine{}
	eng.SetEventBus(bus)
	deps := (&armHooks{}).deps(NewAuthoredRuntimeRegistry())
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	eng.startAuthoringArming(ctx, store)
	if !eventually(2*time.Second, func() bool { return held(t, deps, "automation", "autoBefore") != nil }) {
		t.Fatal("a bundle activated before this node subscribed was never armed: starting the arming ran no reconcile")
	}
}

// TestTheActiveBundleReadWalksEveryPage: the listing is paginated (50 a page),
// and a reconcile that saw only the first page would never arm the rest.
func TestTheActiveBundleReadWalksEveryPage(t *testing.T) {
	page := func(next string, more bool, ids ...string) *ExecuteResult {
		nodes := make([]*memqlv1.MemoryNode, 0, len(ids))
		for _, id := range ids {
			payload, _ := structpb.NewStruct(map[string]any{"ownerUserId": armTestOwner, "status": "active", "version": 1})
			nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: payload})
		}
		res := newExecuteResult(&memqlv1.GraphBundle{Nodes: nodes})
		res.SetCursor(next)
		res.SetHasMore(more)
		return res
	}
	pages := map[string]*ExecuteResult{
		"":   page("c1", true, "b1", "mcp-promote-x", "b2"),
		"c1": page("", false, "b3"),
	}
	var asked []string
	rows, err := pageActiveBundles(context.Background(), func(ctx context.Context) (*ExecuteResult, error) {
		c := CursorFromContext(ctx)
		asked = append(asked, c)
		return pages[c], nil
	})
	if err != nil {
		t.Fatalf("page the active bundles: %v", err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.Id)
	}
	if fmt.Sprint(ids) != "[b1 b2 b3]" || fmt.Sprint(asked) != "[ c1]" {
		t.Fatalf("read pages %q and got bundles %v; want both pages, the durable-promote bundle skipped", asked, ids)
	}

	if _, err := pageActiveBundles(context.Background(), func(context.Context) (*ExecuteResult, error) {
		return page("", true, "b1"), nil
	}); err == nil {
		t.Fatal("a full page with no cursor to the rest must be refused, not read as the end of the list")
	}
}

// TestAnArmLeavesANameAnotherLiveBundleHolds: two bundles that both read active
// define one name. The node holds it for the newer one, and an arm of the older
// one leaves it there -- the registry's version rule, which the activation that
// armed the older one elsewhere obeyed too. Replacing it would make the
// replicas disagree, depending on which broadcast each heard last.
func TestAnArmLeavesANameAnotherLiveBundleHolds(t *testing.T) {
	store := newArmTestStore()
	newer := store.addBundle("v1:authoring:bundle:dup-newer", 2, "", BundleActive, [2]string{"automation", "autoDup"})
	older := store.addBundle("v1:authoring:bundle:dup-older", 1, "", BundleActive, [2]string{"automation", "autoDup"})
	_, bus, deps, hooks := newArmNode(t, store)
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(newer), events.KindAuthoringArm, newer))
	bus.PublishSync(peerEvent(events.TopicAuthoringArmForBundle(older), events.KindAuthoringArm, older))

	if c := held(t, deps, "automation", "autoDup"); c == nil || c.BundleId != newer {
		t.Fatalf("an arm of the older live bundle took the name from the newer one: %+v", c)
	}
	if n := hooks.count("register autoDup"); n != 1 {
		t.Fatalf("the scheduler was handed autoDup %d times, want once", n)
	}
}
