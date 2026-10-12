package memql

// authoring_arming_robustness_test.go -- three ways the arming code used to
// leave a node disagreeing with the database (memql#6022, Task 3 re-review).
//
//  1. A REFUSED ACTIVATION LEFT ITS BUNDLE ACTIVE. The bundle and its
//     construct rows were flipped to active before registration, so an
//     activation refused at registration -- a name another live bundle holds,
//     a definition this node cannot compile, a row write that failed -- was
//     reported failed while its bundle read active: every reconcile pass and
//     every boot re-arm then armed it anyway, and two live bundles could
//     define one name. Now the bundle row flips last, only after everything
//     before it succeeded, and every failure undoes what came before.
//  2. A VERSION THAT CANNOT REGISTER ON A NODE TOOK THE LIVE ONE DOWN. A newer
//     version replaced the held one by taking it out first; when the newer one
//     was then refused here, the older one -- whose bundle may still read
//     active -- was gone, and every reconcile pass re-armed it only to tear it
//     down again. Now the newer version registers OVER the held one, and the
//     held one stays wired when the newer one is refused, unless its own
//     bundle no longer reads active.
//  3. A RETIRE ABANDONED BY ITS CALLER LEFT THE AUTOMATION ARMED. The teardown
//     after the retire was written read under the caller's context, so a
//     client that hung up between the write and the teardown left the retired
//     automation firing on this node until its next reconcile pass. Now the
//     retire, once written, finishes on a context the caller cannot cancel,
//     and knows the bundle it just retired is not active without asking.

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

// faultyStore is the shared database with faults injected, whose reads and
// writes honour a cancelled context the way a real driver does.
type faultyStore struct {
	*armTestStore
	mu          sync.Mutex
	failActive  error            // SetBundleActive
	failStatus  map[string]error // SetConstructStatus(id, "active"), by construct id
	readErr     map[string]error // LoadBundle, by bundle id
	afterRetire func()           // runs once a retire has been written: the caller hangs up
}

func newFaultyStore() *faultyStore {
	return &faultyStore{armTestStore: newArmTestStore(), failStatus: map[string]error{}, readErr: map[string]error{}}
}

func (s *faultyStore) LoadBundle(ctx context.Context, id string) (AuthoringBundleRow, error) {
	if err := ctx.Err(); err != nil {
		return AuthoringBundleRow{}, err
	}
	s.mu.Lock()
	err := s.readErr[id]
	s.mu.Unlock()
	if err != nil {
		return AuthoringBundleRow{}, err
	}
	return s.armTestStore.LoadBundle(ctx, id)
}

func (s *faultyStore) LoadConstructs(ctx context.Context, id string) ([]AuthoringConstructRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.armTestStore.LoadConstructs(ctx, id)
}

func (s *faultyStore) SetBundleActive(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	err := s.failActive
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.armTestStore.SetBundleActive(ctx, id)
}

func (s *faultyStore) SetBundleRetired(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.armTestStore.SetBundleRetired(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	hangUp := s.afterRetire
	s.afterRetire = nil
	s.mu.Unlock()
	if hangUp != nil {
		hangUp()
	}
	return nil
}

func (s *faultyStore) SetConstructStatus(ctx context.Context, constructId, status string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	err := s.failStatus[constructId]
	s.mu.Unlock()
	if err != nil && status == "active" {
		return err
	}
	return s.armTestStore.SetConstructStatus(ctx, constructId, status)
}

func (s *faultyStore) LoadBundleForOwner(ctx context.Context, owner, id string) (AuthoringBundleRow, error) {
	if err := ctx.Err(); err != nil {
		return AuthoringBundleRow{}, err
	}
	return s.armTestStore.LoadBundleForOwner(ctx, owner, id)
}

func (s *faultyStore) LoadConstructsForOwner(ctx context.Context, owner, id string) ([]AuthoringConstructRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.armTestStore.LoadConstructsForOwner(ctx, owner, id)
}

func (s *armTestStore) constructStatus(bundleId, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.constructs[bundleId] {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

// notHere is a definition this node cannot compile.
const notHere = "does not compile on this node"

// refusingDeps is a node whose scheduler refuses every definition carrying
// notHere, the way a node that cannot compile it does -- and, as a refused
// Activate does, leaves whatever it had wired under the name in place.
func refusingDeps(h *armHooks) AuthoredRuntimeDeps {
	deps := h.deps(NewAuthoredRuntimeRegistry())
	register := deps.RegisterHook
	deps.RegisterHook = func(c *AuthoredConstruct) error {
		if strings.Contains(c.Source, notHere) {
			h.mu.Lock()
			h.calls = append(h.calls, fmt.Sprintf("refuse %s@%d", c.Name, c.Version))
			h.mu.Unlock()
			return fmt.Errorf("automation %s %s", c.Name, notHere)
		}
		return register(c)
	}
	return deps
}

// callsSince is what a node's scheduler was asked to do after a point.
func callsSince(h *armHooks, n int) []string {
	calls, _ := h.snapshot()
	return calls[n:]
}

// assertNeverLive checks that a refused activation left its bundle as it was:
// not active -- so no reconcile pass and no reboot arms it -- with its
// construct rows back at draft.
func assertNeverLive(t *testing.T, store *faultyStore, bundleId string, names ...string) {
	t.Helper()
	if st := store.bundleStatus(bundleId); st != BundleDryRunPassed {
		t.Fatalf("a refused activation left bundle %s %q; it must stay dryRunPassed, or every reconcile pass and reboot arms it", bundleId, st)
	}
	for _, n := range names {
		if st := store.constructStatus(bundleId, n); st != "draft" {
			t.Fatalf("a refused activation left construct %s %q, want draft", n, st)
		}
	}
}

// TestARefusedActivationLeavesItsBundleInactive is item 1, for every way an
// activation can be refused after it has started.
func TestARefusedActivationLeavesItsBundleInactive(t *testing.T) {
	ctx := context.Background()

	t.Run("another live bundle holds a name it defines", func(t *testing.T) {
		store := newFaultyStore()
		holder := store.addBundle("v1:authoring:bundle:holder", 1, "", BundleActive, [2]string{"query", "zShared"})
		incoming := store.addBundle("v1:authoring:bundle:incoming", 1, "", BundleDryRunPassed,
			[2]string{"query", "aOwn"}, [2]string{"query", "zShared"})
		hooks := &armHooks{}
		deps := hooks.deps(NewAuthoredRuntimeRegistry())
		if _, err := armBundleWithStore(ctx, store, armTestOwner, holder, deps); err != nil {
			t.Fatalf("setup: arm the holder: %v", err)
		}

		_, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, incoming, deps)
		if err == nil || !strings.Contains(err.Error(), "already armed") {
			t.Fatalf("the activation must be refused for the name another live bundle holds, got %v", err)
		}
		assertNeverLive(t, store, incoming, "aOwn", "zShared")
		if held(t, deps, "query", "aOwn") != nil {
			t.Fatal("the refused activation left its other construct armed")
		}
		if c := held(t, deps, "query", "zShared"); c == nil || c.BundleId != holder {
			t.Fatalf("the refused activation disturbed the live bundle's construct: %+v", c)
		}
	})

	t.Run("this node refuses one of its automations", func(t *testing.T) {
		store := newFaultyStore()
		incoming := store.addBundle("v1:authoring:bundle:broken", 1, "", BundleDryRunPassed,
			[2]string{"automation", "aFine"}, [2]string{"automation", "zBroken"})
		store.setSource(incoming, "zBroken", "automation zBroken { "+notHere+" }")
		hooks := &armHooks{}
		deps := refusingDeps(hooks)

		if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, incoming, deps); err == nil {
			t.Fatal("an activation whose automation this node refuses must fail")
		}
		assertNeverLive(t, store, incoming, "aFine", "zBroken")
		if held(t, deps, "automation", "aFine") != nil || held(t, deps, "automation", "zBroken") != nil {
			t.Fatal("the refused activation left part of its bundle armed")
		}
		if _, live := hooks.snapshot(); len(live) != 0 {
			t.Fatalf("the refused activation left automations live in the scheduler: %v", live)
		}
	})

	t.Run("a construct row cannot be marked active", func(t *testing.T) {
		store := newFaultyStore()
		incoming := store.addBundle("v1:authoring:bundle:row-write", 1, "", BundleDryRunPassed,
			[2]string{"automation", "aFine"}, [2]string{"query", "zQuery"})
		store.failStatus[incoming+":zQuery"] = errors.New("row write failed")
		hooks := &armHooks{}
		deps := hooks.deps(NewAuthoredRuntimeRegistry())

		if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, incoming, deps); err == nil {
			t.Fatal("an activation whose construct row could not be written must fail")
		}
		assertNeverLive(t, store, incoming, "aFine", "zQuery")
		if held(t, deps, "automation", "aFine") != nil || held(t, deps, "query", "zQuery") != nil {
			t.Fatal("the refused activation left its constructs armed")
		}
		if _, live := hooks.snapshot(); len(live) != 0 {
			t.Fatalf("the refused activation left automations live in the scheduler: %v", live)
		}
	})

	t.Run("the bundle row cannot be marked active", func(t *testing.T) {
		store := newFaultyStore()
		incoming := store.addBundle("v1:authoring:bundle:bundle-write", 1, "", BundleDryRunPassed,
			[2]string{"automation", "aFine"}, [2]string{"query", "zQuery"})
		store.failActive = errors.New("bundle write failed")
		hooks := &armHooks{}
		deps := hooks.deps(NewAuthoredRuntimeRegistry())

		if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, incoming, deps); err == nil {
			t.Fatal("an activation whose bundle row could not be written must fail")
		}
		assertNeverLive(t, store, incoming, "aFine", "zQuery")
		if held(t, deps, "automation", "aFine") != nil || held(t, deps, "query", "zQuery") != nil {
			t.Fatal("an activation that could not mark its bundle active left it armed")
		}
	})

	t.Run("the bundle row cannot be marked active over a live version", func(t *testing.T) {
		store := newFaultyStore()
		hooks := &armHooks{}
		deps := hooks.deps(NewAuthoredRuntimeRegistry())
		v1 := store.addBundle("v1:authoring:bundle:prior-live", 1, "", BundleDryRunPassed, [2]string{"automation", "autoKept"})
		if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v1, deps); err != nil {
			t.Fatalf("setup: activate version 1: %v", err)
		}
		v2 := store.addBundle("v1:authoring:bundle:edit-unwritten", 2, v1, BundleDryRunPassed, [2]string{"automation", "autoKept"})
		store.failActive = errors.New("bundle write failed")

		if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v2, deps); err == nil {
			t.Fatal("an edit whose bundle row could not be written must fail")
		}
		assertNeverLive(t, store, v2, "autoKept")
		if c := held(t, deps, "automation", "autoKept"); c == nil || c.BundleId != v1 || c.Version != 1 {
			t.Fatalf("an edit that never went live left the node holding %+v; version 1 is still the live one", c)
		}
		if _, live := hooks.snapshot(); live["autoKept"] != 1 {
			t.Fatalf("an edit that never went live left version 1 out of the scheduler: %v", live)
		}
	})

	t.Run("the bundle holding a name it defines cannot be read", func(t *testing.T) {
		store := newFaultyStore()
		// Version 2 against an incoming version 1: replacing it needs a read.
		holder := store.addBundle("v1:authoring:bundle:unreadable", 2, "", BundleActive, [2]string{"query", "zShared"})
		incoming := store.addBundle("v1:authoring:bundle:asks", 1, "", BundleDryRunPassed,
			[2]string{"query", "aOwn"}, [2]string{"query", "zShared"})
		hooks := &armHooks{}
		deps := hooks.deps(NewAuthoredRuntimeRegistry())
		if _, err := armBundleWithStore(ctx, store, armTestOwner, holder, deps); err != nil {
			t.Fatalf("setup: arm the holder: %v", err)
		}
		store.readErr[holder] = errors.New("read timed out")

		if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, incoming, deps); err == nil {
			t.Fatal("an activation that could not decide a name it defines must fail")
		}
		assertNeverLive(t, store, incoming, "aOwn", "zShared")
		if held(t, deps, "query", "aOwn") != nil {
			t.Fatal("the refused activation left its other construct armed")
		}
		if c := held(t, deps, "query", "zShared"); c == nil || c.BundleId != holder {
			t.Fatalf("the refused activation disturbed the held construct: %+v", c)
		}
	})
}

// TestAFailedSupersedingActivationLeavesThePriorVersionArmed is items 1 and 2
// at the origin: an edit's new version is refused on the node activating it.
// The activation fails, the new version stays inactive, and the version that
// is still live keeps firing here, untouched.
func TestAFailedSupersedingActivationLeavesThePriorVersionArmed(t *testing.T) {
	ctx := context.Background()
	store := newFaultyStore()
	hooks := &armHooks{}
	deps := refusingDeps(hooks)
	v1 := store.addBundle("v1:authoring:bundle:edit-ok", 1, "", BundleDryRunPassed, [2]string{"automation", "autoEdit"})
	if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v1, deps); err != nil {
		t.Fatalf("setup: activate version 1: %v", err)
	}
	v2 := store.addBundle("v1:authoring:bundle:edit-refused", 2, v1, BundleDryRunPassed, [2]string{"automation", "autoEdit"})
	store.setSource(v2, "autoEdit", "automation autoEdit { "+notHere+" }")
	calls0, _ := hooks.snapshot()

	if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v2, deps); err == nil {
		t.Fatal("an edit this node refuses must fail to activate")
	}
	assertNeverLive(t, store, v2, "autoEdit")
	if st := store.bundleStatus(v1); st != BundleActive {
		t.Fatalf("a failed edit retired the live version: it reads %q", st)
	}
	c := held(t, deps, "automation", "autoEdit")
	if c == nil || c.BundleId != v1 || c.Version != 1 || c.Status != AuthoredActive {
		t.Fatalf("a failed edit took the live version out of the registry: %+v", c)
	}
	if _, live := hooks.snapshot(); live["autoEdit"] != 1 {
		t.Fatalf("a failed edit took the live version out of the scheduler: live %v", live)
	}
	if got := callsSince(hooks, len(calls0)); fmt.Sprint(got) != "[refuse autoEdit@2]" {
		t.Fatalf("the live version was disturbed by a refused edit: scheduler calls %v, want only the refusal", got)
	}
}

// TestAVersionThatCannotRegisterHereLeavesTheLiveOneArmed is item 2 on a peer:
// both versions read active (the edit's retire of the old one has not landed),
// and the new one cannot register on this node. Two reconcile passes leave the
// old one exactly where it was -- not torn down and re-armed each pass.
func TestAVersionThatCannotRegisterHereLeavesTheLiveOneArmed(t *testing.T) {
	ctx := context.Background()
	store := newFaultyStore()
	hooks := &armHooks{}
	deps := refusingDeps(hooks)
	eng := &MemQLEngine{}
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	a := store.addBundle("v1:authoring:bundle:live-a", 1, "", BundleActive, [2]string{"automation", "autoX"})
	b := store.addBundle("v1:authoring:bundle:refused-b", 2, a, BundleActive, [2]string{"automation", "autoX"})
	store.setSource(b, "autoX", "automation autoX { "+notHere+" }")
	if _, err := armBundleWithStore(ctx, store, armTestOwner, a, deps); err != nil {
		t.Fatalf("setup: arm version 1: %v", err)
	}
	calls0, _ := hooks.snapshot()

	for pass := 1; pass <= 2; pass++ {
		out, err := eng.reconcileAuthoredRuntime(ctx, store)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if fmt.Sprint(out.Failed) != fmt.Sprint([]string{b}) {
			t.Fatalf("pass %d reported failed bundles %v, want just the one this node refuses", pass, out.Failed)
		}
		c := held(t, deps, "automation", "autoX")
		if c == nil || c.BundleId != a || c.Version != 1 {
			t.Fatalf("after pass %d the node holds %+v; the live version 1 must stay armed", pass, c)
		}
		if _, live := hooks.snapshot(); live["autoX"] != 1 {
			t.Fatalf("after pass %d version 1 is not live in the scheduler: %v", pass, live)
		}
	}
	if got := callsSince(hooks, len(calls0)); fmt.Sprint(got) != "[refuse autoX@2 refuse autoX@2]" {
		t.Fatalf("two passes churned the live version: scheduler calls %v, want only the two refusals", got)
	}
}

// TestAStaleVersionGoesDarkWhenItsReplacementCannotRegister is the other half
// of item 2: when the held version's own bundle no longer reads active, a
// replacement this node refuses leaves nothing firing -- a retired definition
// must not keep running here because its successor cannot -- and the next
// pass finds nothing more to change. The replacement arrives as a broadcast,
// which has no sweep behind it to take the stale version out later.
func TestAStaleVersionGoesDarkWhenItsReplacementCannotRegister(t *testing.T) {
	ctx := context.Background()
	store := newFaultyStore()
	hooks := &armHooks{}
	deps := refusingDeps(hooks)
	eng := &MemQLEngine{}
	eng.SetAuthoredRuntimeDeps(func() AuthoredRuntimeDeps { return deps })
	a := store.addBundle("v1:authoring:bundle:stale-a", 1, "", BundleActive, [2]string{"automation", "autoY"})
	if _, err := armBundleWithStore(ctx, store, armTestOwner, a, deps); err != nil {
		t.Fatalf("setup: arm version 1: %v", err)
	}
	store.setBundleStatus(a, BundleRetired) // ...and this node missed its disarm
	b := store.addBundle("v1:authoring:bundle:refused-y", 2, a, BundleActive, [2]string{"automation", "autoY"})
	store.setSource(b, "autoY", "automation autoY { "+notHere+" }")

	if _, err := armBundleWithStore(ctx, store, armTestOwner, b, deps); err == nil {
		t.Fatal("arming a version this node refuses must report it")
	}
	if c := held(t, deps, "automation", "autoY"); c != nil {
		t.Fatalf("a retired version kept firing because its successor cannot register here: %+v", c)
	}
	if n := hooks.count("deregister autoY"); n != 1 {
		t.Fatalf("the retired version was taken out of the scheduler %d times, want 1", n)
	}
	calls1, _ := hooks.snapshot()
	if _, err := eng.reconcileAuthoredRuntime(ctx, store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := callsSince(hooks, len(calls1)); fmt.Sprint(got) != "[refuse autoY@2]" {
		t.Fatalf("a reconcile pass after the broadcast changed more than the refusal: %v", got)
	}
}

// TestARetireFinishesWhenItsCallerHangsUp is item 3 for the explicit retire:
// the client disconnects right after the retire is written. The automation
// still comes out of this node, its rows are still marked retired, and the
// disarm still goes to every other node.
func TestARetireFinishesWhenItsCallerHangsUp(t *testing.T) {
	store := newFaultyStore()
	r := store.addBundle("v1:authoring:bundle:hung-up", 1, "", BundleActive,
		[2]string{"automation", "autoR"}, [2]string{"query", "queryR"})
	eng, bus, deps, hooks := newArmNode(t, store)
	if _, err := armBundleWithStore(context.Background(), store, armTestOwner, r, deps); err != nil {
		t.Fatalf("setup: arm: %v", err)
	}
	var disarms atomic.Int64
	bus.Subscribe("authoring.disarm.#", func(events.Event) { disarms.Add(1) })

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	store.afterRetire = hangUp
	if err := eng.retireActiveBundleWithStore(ctx, store, armTestOwner, r, deps); err != nil {
		t.Fatalf("a retire whose caller hung up after it was written: %v", err)
	}

	if st := store.bundleStatus(r); st != BundleRetired {
		t.Fatalf("the bundle reads %q, want retired", st)
	}
	if held(t, deps, "automation", "autoR") != nil || held(t, deps, "query", "queryR") != nil {
		t.Fatal("the caller hung up after the retire was written, and the retired bundle stayed armed on this node")
	}
	if n := hooks.count("deregister autoR"); n != 1 {
		t.Fatalf("the retired automation was taken out of the scheduler %d times, want 1", n)
	}
	for _, n := range []string{"autoR", "queryR"} {
		if st := store.constructStatus(r, n); st != "retired" {
			t.Fatalf("construct %s reads %q after the retire, want retired", n, st)
		}
	}
	if !eventually(time.Second, func() bool { return disarms.Load() == 1 }) {
		t.Fatalf("the retire broadcast %d disarms, want 1", disarms.Load())
	}
}

// TestARetireDoesNotAskTheDatabaseAboutTheBundleItJustRetired: the retire has
// written the bundle retired, and a read of that bundle now fails. The
// teardown knows the bundle it retired is not active without reading it, so
// the automation still comes out.
func TestARetireDoesNotAskTheDatabaseAboutTheBundleItJustRetired(t *testing.T) {
	store := newFaultyStore()
	r := store.addBundle("v1:authoring:bundle:unreadable-after", 1, "", BundleActive, [2]string{"automation", "autoU"})
	eng, _, deps, _ := newArmNode(t, store)
	if _, err := armBundleWithStore(context.Background(), store, armTestOwner, r, deps); err != nil {
		t.Fatalf("setup: arm: %v", err)
	}
	store.afterRetire = func() {
		store.mu.Lock()
		store.readErr[r] = errors.New("read timed out")
		store.mu.Unlock()
	}
	if err := eng.retireActiveBundleWithStore(context.Background(), store, armTestOwner, r, deps); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if held(t, deps, "automation", "autoU") != nil {
		t.Fatal("the retire asked the database whether the bundle it had just retired was still active, and kept it armed when the read failed")
	}
}

// TestASupersedeRetiresTheOldVersionWhenItsCallerHangsUp is item 3 for an
// edit: the client disconnects right after the old version's retire is
// written. What only the old version defined still comes out of this node.
func TestASupersedeRetiresTheOldVersionWhenItsCallerHangsUp(t *testing.T) {
	store := newFaultyStore()
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	v1 := store.addBundle("v1:authoring:bundle:hang-v1", 1, "", BundleDryRunPassed,
		[2]string{"automation", "autoS"}, [2]string{"trait", "onlyInV1"})
	if _, err := ActivateApprovedBundleWithStore(context.Background(), store, armTestOwner, v1, deps); err != nil {
		t.Fatalf("setup: activate version 1: %v", err)
	}
	v2 := store.addBundle("v1:authoring:bundle:hang-v2", 2, v1, BundleDryRunPassed, [2]string{"automation", "autoS"})

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	store.afterRetire = hangUp
	if _, err := ActivateApprovedBundleWithStore(ctx, store, armTestOwner, v2, deps); err != nil {
		t.Fatalf("activate version 2: %v", err)
	}

	if held(t, deps, "trait", "onlyInV1") != nil {
		t.Fatal("the caller hung up after version 1's retire was written, and what only version 1 defined stayed armed")
	}
	if c := held(t, deps, "automation", "autoS"); c == nil || c.Version != 2 {
		t.Fatalf("autoS is %+v, want version 2", c)
	}
	if st := store.bundleStatus(v1); st != BundleRetired {
		t.Fatalf("version 1 reads %q, want retired", st)
	}
}
