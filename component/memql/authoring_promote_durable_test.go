package memql

// authoring_promote_durable_test.go -- coverage for the DB-persisted,
// reviewable, restart-durable promote (memql#1557, approach (b)).
//
// Internal (package memql) test so it can drive the unexported promoteStore /
// promoteRehydrateStore seams with fakes + the REAL engine compile/promote
// logic -- no live DB. It asserts the acceptance properties:
//
//   - promote persists a bundle/construct row pair AND registers the construct
//     into the shared registry (callable by a fresh session via the overlay);
//   - a non-owner (empty owner) cannot promote;
//   - a promoted construct does not shadow a core construct;
//   - a simulated restart (re-run the boot re-hydration against the persisted
//     rows on a fresh engine) restores callability.

import (
	"context"
	"strings"
	"testing"
)

// fakePromoteStore records the bundle/construct rows the durable promote
// persisted, so the test can assert the reviewable record exists.
type fakePromoteStore struct {
	bundles    []AuthoringBundleRow
	constructs []AuthoringConstructRow
}

func (s *fakePromoteStore) CreatePromoteBundle(_ context.Context, bundleId, title, summary string) error {
	s.bundles = append(s.bundles, AuthoringBundleRow{Id: bundleId, Title: title, Summary: summary, Status: BundleActive})
	return nil
}

func (s *fakePromoteStore) CreatePromoteConstruct(_ context.Context, constructId, bundleId, kind, name, targetNamespace, source string) error {
	s.constructs = append(s.constructs, AuthoringConstructRow{
		Id: constructId, BundleId: bundleId, Kind: kind, Name: name, TargetNamespace: targetNamespace, Source: source,
	})
	return nil
}

// authorOneSpec authors a single session spec into reg under owner and returns
// the compiled construct, so the durable-promote path has a real compiled form.
func authorOneSpec(t *testing.T, reg *AuthoredRuntimeRegistry, owner string) *AuthoredConstruct {
	t.Helper()
	if _, err := AuthorSessionBundle(reg, owner, sessionSpecSrc); err != nil {
		t.Fatalf("author session spec: %v", err)
	}
	c, ok := reg.Lookup(owner, "spec", "mcpSessSpec")
	if !ok {
		t.Fatal("session spec not registered")
	}
	return c
}

// TestPromoteConstructDurable_PersistsAndRegisters: a durable promote persists a
// bundle + construct row pair AND registers the construct into the shared
// registry so it is callable by every session.
func TestPromoteConstructDurable_PersistsAndRegisters(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	c := authorOneSpec(t, reg, "owner-1")

	store := &fakePromoteStore{}
	if err := e.promoteConstructDurableWithStore(context.Background(), store, "owner-1", c); err != nil {
		t.Fatalf("durable promote: %v", err)
	}

	// Persisted: exactly one bundle + one construct row, the construct capturing
	// kind + name + source.
	if len(store.bundles) != 1 {
		t.Fatalf("expected 1 persisted bundle, got %d", len(store.bundles))
	}
	if !strings.HasPrefix(store.bundles[0].Id, durablePromoteBundlePrefix) {
		t.Errorf("bundle id %q missing durable-promote prefix", store.bundles[0].Id)
	}
	if len(store.constructs) != 1 {
		t.Fatalf("expected 1 persisted construct, got %d", len(store.constructs))
	}
	row := store.constructs[0]
	if row.Kind != "spec" || row.Name != "mcpSessSpec" || strings.TrimSpace(row.Source) == "" {
		t.Errorf("persisted construct row not captured correctly: %+v", row)
	}

	// Registered into the shared registry: callable by all sessions.
	if _, ok := e.specs.Lookup("mcpSessSpec"); !ok {
		t.Error("promoted spec not in the shared spec registry")
	}
}

// TestPromoteConstructDurable_PromotedFunctionCallableInFreshSession: a promoted
// FUNCTION lands in the shared function registry, so it is resolvable by a FRESH
// session (a different owner, empty registry) through the core-first overlay --
// the "callable by all" acceptance.
func TestPromoteConstructDurable_PromotedFunctionCallableInFreshSession(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}

	// A net-new authored function (compiled form supplied directly, mirroring the
	// session author path) durably promotes into the shared function registry.
	c := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "promotedDurableQuery", Status: AuthoredActive,
		Source: `query promotedDurableQuery { }`,
		Compiled: &Function{Name: "promotedDurableQuery", FunctionKind: "query", Enabled: true}}
	if err := e.promoteConstructDurableWithStore(context.Background(), &fakePromoteStore{}, "owner-1", c); err != nil {
		t.Fatalf("durable promote: %v", err)
	}

	// A FRESH session (a different owner, empty registry) sees the promoted
	// construct through the core-first overlay -- it is in the shared registry.
	freshReg := NewAuthoredRuntimeRegistry()
	overlay := e.buildAuthoredFunctionOverlay("owner-2", freshReg)
	if got, _ := overlay.Get("promotedDurableQuery"); got == nil {
		t.Error("promoted construct not callable by a fresh session")
	}
}

// TestPromoteConstructDurable_RejectsNonOwner: an empty owner (no authenticated
// identity) cannot promote -- nothing is registered or persisted.
func TestPromoteConstructDurable_RejectsNonOwner(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	c := authorOneSpec(t, reg, "owner-1")

	store := &fakePromoteStore{}
	if err := e.promoteConstructDurableWithStore(context.Background(), store, "  ", c); err == nil {
		t.Fatal("expected an error promoting with no authenticated owner")
	}
	if len(store.bundles) != 0 || len(store.constructs) != 0 {
		t.Error("a refused promote must persist nothing")
	}
	if _, ok := e.specs.Lookup("mcpSessSpec"); ok {
		t.Error("a refused promote must register nothing")
	}
}

// TestPromoteConstructDurable_DoesNotShadowCore: a durable promote whose name a
// CORE construct owns is refused and persists nothing (registration is the
// authoritative gate and runs first).
func TestPromoteConstructDurable_DoesNotShadowCore(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	core := &Function{Name: "coreOwned", FunctionKind: "query", Enabled: true, ExprSource: "core"}
	if err := e.functions.Upsert(core); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	shadow := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "coreOwned", Status: AuthoredActive,
		Compiled: &Function{Name: "coreOwned", FunctionKind: "query", Enabled: true, ExprSource: "authored"}}

	store := &fakePromoteStore{}
	if err := e.promoteConstructDurableWithStore(context.Background(), store, "owner-1", shadow); err == nil {
		t.Fatal("durable promote must refuse to shadow a core construct")
	}
	if len(store.bundles) != 0 || len(store.constructs) != 0 {
		t.Error("a refused (core-shadow) promote must persist nothing")
	}
	if got, _ := e.functions.Get("coreOwned"); got == nil || got.ExprSource != "core" {
		t.Errorf("core construct was overwritten: %+v", got)
	}
}

// fakeRehydrateStore stands in for the persisted graph at boot: the promoted
// bundles + their member constructs.
type fakeRehydrateStore struct {
	bundles    []AuthoringBundleRow
	constructs map[string][]AuthoringConstructRow // bundleId -> constructs
}

func (s *fakeRehydrateStore) LoadPromotedBundles(_ context.Context) ([]AuthoringBundleRow, error) {
	return s.bundles, nil
}

func (s *fakeRehydrateStore) LoadConstructsForBundle(_ context.Context, _, bundleId string) ([]AuthoringConstructRow, error) {
	return s.constructs[bundleId], nil
}

// TestRehydratePromotedConstructs_RestoresCallabilityAfterRestart: a simulated
// restart -- a FRESH engine with empty registries -- re-hydrates the persisted
// promoted rows and the construct becomes callable again (shared registry).
func TestRehydratePromotedConstructs_RestoresCallabilityAfterRestart(t *testing.T) {
	// 1. Author + durably promote a context-spec on the "old" engine, capturing
	// the persisted rows. A context-spec recompiles standalone (it binds only to
	// the actor envelope, no concept/shape deps) -- the durably-promotable shape
	// the re-hydration recompiles core-first.
	old := &MemQLEngine{specs: newSpecRegistry()}
	authorReg := NewAuthoredRuntimeRegistry()
	c := authorOneSpec(t, authorReg, "owner-1")
	persist := &fakePromoteStore{}
	if err := old.promoteConstructDurableWithStore(context.Background(), persist, "owner-1", c); err != nil {
		t.Fatalf("durable promote: %v", err)
	}

	// 2. Simulate a restart: a fresh engine with EMPTY registries, fed the
	// persisted rows. The construct is gone until re-hydration runs.
	fresh := &MemQLEngine{specs: newSpecRegistry()}
	if _, ok := fresh.specs.Lookup("mcpSessSpec"); ok {
		t.Fatal("fresh engine should not have the promoted construct before re-hydration")
	}

	// Build the boot-time store from the persisted rows. The construct row needs
	// its ownerUserId (the persist fake doesn't stamp it; set it as the graph would).
	persisted := persist.constructs[0]
	persisted.OwnerUserId = "owner-1"
	store := &fakeRehydrateStore{
		bundles:    []AuthoringBundleRow{{Id: persist.bundles[0].Id, OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{persist.bundles[0].Id: {persisted}},
	}

	res, err := fresh.rehydratePromotedNow(context.Background(), store)
	if err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	if res.Rehydrated != 1 || res.Seen != 1 || len(res.Failed) != 0 {
		t.Fatalf("re-hydrate result = %+v, want seen=1 rehydrated=1 failed=0", res)
	}

	// 3. Callable again on the fresh engine (shared spec registry).
	if _, ok := fresh.specs.Lookup("mcpSessSpec"); !ok {
		t.Error("promoted construct not restored after restart re-hydration")
	}
}

// TestRehydratePromotedConstructs_Idempotent: running the re-hydration twice on
// the same engine is a no-op-equivalent (no error, still callable).
func TestRehydratePromotedConstructs_Idempotent(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: durablePromoteBundlePrefix + "b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			durablePromoteBundlePrefix + "b1": {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: durablePromoteBundlePrefix + "b1",
				Kind: "spec", Name: "mcpSessSpec", Source: sessionSpecSrc,
			}},
		},
	}
	if _, err := e.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("first re-hydrate: %v", err)
	}
	if _, err := e.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("second re-hydrate (should be idempotent): %v", err)
	}
	if _, ok := e.specs.Lookup("mcpSessSpec"); !ok {
		t.Error("idempotent re-hydrate lost the construct")
	}
}
