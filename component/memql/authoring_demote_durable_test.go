package memql

// authoring_demote_durable_test.go -- coverage for the DB-persisted,
// restart-durable DEMOTE (memql#2163), the inverse of the durable promote tests.
//
// Internal (package memql) test so it can drive the unexported demoteStore /
// promoteRehydrateStore seams with fakes + the REAL engine demote logic -- no
// live DB. It asserts the acceptance properties:
//
//   - demote removes the construct from the shared registry AND flips the
//     persisted construct row to retired (and retires the bundle once empty);
//   - a non-owner (empty owner) cannot demote;
//   - a name that was never author-promoted (a core construct) is refused and
//     nothing is retired;
//   - a simulated restart (re-run the boot re-hydration against the now-retired
//     rows on a fresh engine) does NOT bring the construct back -- the demote
//     survives the restart.

import (
	"context"
	"testing"
)

// fakeDemoteStore stands in for the persisted graph: the durable-promote bundles
// + their member constructs (carrying status), and records the retire writes the
// demote issued. It mutates its own rows on retire so the bundle-retire rollup +
// the re-load see the new status.
type fakeDemoteStore struct {
	bundles    []AuthoringBundleRow
	constructs map[string][]AuthoringConstructRow // bundleId -> constructs
	retiredCs  []string                           // constructIds retired
	retiredBs  []string                           // bundleIds retired
}

func (s *fakeDemoteStore) LoadPromotedBundles(context.Context) ([]AuthoringBundleRow, error) {
	return s.bundles, nil
}

func (s *fakeDemoteStore) LoadConstructsForBundle(_ context.Context, _, bundleId string) ([]AuthoringConstructRow, error) {
	return s.constructs[bundleId], nil
}

func (s *fakeDemoteStore) RetireConstruct(_ context.Context, _, constructId string) error {
	s.retiredCs = append(s.retiredCs, constructId)
	for bid, rows := range s.constructs {
		for i := range rows {
			if rows[i].Id == constructId {
				s.constructs[bid][i].Status = string(BundleRetired)
			}
		}
	}
	return nil
}

func (s *fakeDemoteStore) RetireBundle(_ context.Context, _, bundleId string) error {
	s.retiredBs = append(s.retiredBs, bundleId)
	for i := range s.bundles {
		if s.bundles[i].Id == bundleId {
			s.bundles[i].Status = BundleRetired
		}
	}
	return nil
}

// seedPromotedSpec promotes a spec into e's shared registry via the durable
// promote path and returns a fakeDemoteStore reflecting the persisted rows, so a
// demote can find + retire them.
func seedPromotedSpec(t *testing.T, e *MemQLEngine, owner string) *fakeDemoteStore {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	c := authorOneSpec(t, reg, owner)
	persist := &fakePromoteStore{}
	if err := e.promoteConstructDurableWithStore(context.Background(), persist, owner, c); err != nil {
		t.Fatalf("seed durable promote: %v", err)
	}
	bundleId := persist.bundles[0].Id
	row := persist.constructs[0]
	row.OwnerUserId = owner
	row.Status = string(BundleActive)
	return &fakeDemoteStore{
		bundles:    []AuthoringBundleRow{{Id: bundleId, OwnerUserId: owner, Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{bundleId: {row}},
	}
}

// TestDemoteConstructDurable_UnregistersAndRetires: a durable demote removes the
// construct from the shared registry AND flips the persisted construct + bundle
// rows to retired.
func TestDemoteConstructDurable_UnregistersAndRetires(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	store := seedPromotedSpec(t, e, "owner-1")

	// Promoted and callable before the demote.
	if _, ok := e.specs.Lookup("mcpSessSpec"); !ok {
		t.Fatal("seed promote did not register the spec")
	}

	if err := e.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "spec", "mcpSessSpec"); err != nil {
		t.Fatalf("durable demote: %v", err)
	}

	// Removed from the shared registry.
	if _, ok := e.specs.Lookup("mcpSessSpec"); ok {
		t.Error("demoted spec is still in the shared spec registry")
	}
	// The persisted construct row was retired, and the now-empty bundle retired.
	if len(store.retiredCs) != 1 {
		t.Errorf("expected 1 construct retired, got %d", len(store.retiredCs))
	}
	if len(store.retiredBs) != 1 {
		t.Errorf("expected the emptied bundle retired, got %d", len(store.retiredBs))
	}
}

// TestDemoteBundleDurable_DemotesNamedConstruct: the bundle-level entry point
// demotes every plain construct the source names, by name, without compiling.
func TestDemoteBundleDurable_DemotesNamedConstruct(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	store := seedPromotedSpec(t, e, "owner-1")

	res, err := e.demoteBundleDurableWithStore(context.Background(), store, "owner-1", sessionSpecSrc)
	if err != nil {
		t.Fatalf("durable demote bundle: %v", err)
	}
	if !res.OK || len(res.Demoted) != 1 || res.Demoted[0].Name != "mcpSessSpec" {
		t.Fatalf("expected mcpSessSpec demoted, got OK=%v %+v", res.OK, res.Demoted)
	}
	if _, ok := e.specs.Lookup("mcpSessSpec"); ok {
		t.Error("demoted spec still callable after bundle demote")
	}
}

// TestDemoteConstructDurable_RejectsNonOwner: an empty owner cannot demote;
// nothing is removed or retired.
func TestDemoteConstructDurable_RejectsNonOwner(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	store := seedPromotedSpec(t, e, "owner-1")

	if err := e.demoteConstructDurableWithStore(context.Background(), store, "  ", "spec", "mcpSessSpec"); err == nil {
		t.Fatal("expected an error demoting with no authenticated owner")
	}
	if len(store.retiredCs) != 0 || len(store.retiredBs) != 0 {
		t.Error("a refused demote must retire nothing")
	}
	if _, ok := e.specs.Lookup("mcpSessSpec"); !ok {
		t.Error("a refused demote must leave the construct registered")
	}
}

// TestDemoteConstructDurable_RefusesNonPromotedName: a name that was never
// author-promoted -- e.g. a core construct -- is refused (the safety gate runs
// first), and nothing is retired. A core construct can NEVER be removed by a
// demote.
func TestDemoteConstructDurable_RefusesNonPromotedName(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	// Seed a CORE function (no promotedAuthored marker -- exactly a sealed core
	// construct's shape) and a store that has no rows for it.
	core := &Function{Name: "coreOwned", FunctionKind: "query", Enabled: true, ExprSource: "core"}
	if err := e.functions.Upsert(core); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	store := &fakeDemoteStore{constructs: map[string][]AuthoringConstructRow{}}

	if err := e.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "query", "coreOwned"); err == nil {
		t.Fatal("durable demote must refuse a non-author-promoted (core) name")
	}
	if got, _ := e.functions.Get("coreOwned"); got == nil || got.ExprSource != "core" {
		t.Errorf("core construct was removed/altered by a refused demote: %+v", got)
	}
	if len(store.retiredCs) != 0 || len(store.retiredBs) != 0 {
		t.Error("a refused demote must retire nothing")
	}
}

// TestDemote_RehydrationSkipsRetiredAfterRestart: the load-bearing durability
// test. Demote a promoted spec (retiring its rows), then simulate a restart by
// running the boot re-hydration on a FRESH engine against those retired rows.
// The construct must NOT come back -- the demote survives the restart.
func TestDemote_RehydrationSkipsRetiredAfterRestart(t *testing.T) {
	old := &MemQLEngine{specs: newSpecRegistry()}
	store := seedPromotedSpec(t, old, "owner-1")
	if err := old.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "spec", "mcpSessSpec"); err != nil {
		t.Fatalf("durable demote: %v", err)
	}

	// Build the boot re-hydration store from the (now retired) persisted rows.
	bundleId := store.bundles[0].Id
	rehydrate := &fakeRehydrateStore{
		bundles:    []AuthoringBundleRow{{Id: bundleId, OwnerUserId: "owner-1", Status: BundleRetired}},
		constructs: map[string][]AuthoringConstructRow{bundleId: store.constructs[bundleId]},
	}

	fresh := &MemQLEngine{specs: newSpecRegistry()}
	res, err := fresh.rehydratePromotedNow(context.Background(), rehydrate)
	if err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	// The retired construct is skipped -- not seen, not rehydrated.
	if res.Seen != 0 || res.Rehydrated != 0 {
		t.Fatalf("re-hydration must skip the retired construct; got %+v", res)
	}
	if _, ok := fresh.specs.Lookup("mcpSessSpec"); ok {
		t.Error("RESTART REGRESSION: a demoted (retired) construct re-registered after restart re-hydration")
	}
}
