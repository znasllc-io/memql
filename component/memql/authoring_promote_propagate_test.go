package memql

// authoring_promote_propagate_test.go -- coverage for the bundle-level durable
// promote (PromoteBundleDurable) and its LIVE cross-node propagation (issue
// znasllc-io/memql-cockpit#232).
//
// Internal (package memql) test so it can drive the unexported store seams with
// fakes + the REAL engine compile/promote/broadcast logic -- no live DB. It
// asserts:
//
//   - the validate-fail path: a broken bundle promotes nothing and errors;
//   - the promote-success path: a valid bundle's plain construct is callable on
//     the promoting engine after the promote;
//   - the CROSS-NODE path (the load-bearing test): a promote on engine A makes
//     the construct callable on a DIFFERENT engine B WITHOUT a restart, purely
//     via the authoring.promote.* broadcast. This test FAILS against single-node
//     code (no subscriber on B) and PASSES with the propagation wired.

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// TestPromoteBundleDurable_ValidateFail: a broken bundle promotes nothing and
// returns an error with OK=false + diagnostics.
func TestPromoteBundleDurable_ValidateFail(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	store := &fakePromoteStore{}
	res, err := e.promoteBundleDurableWithStore(context.Background(), store, "owner-1", `spec broken { actor.role == }`)
	if err == nil {
		t.Fatal("expected a validation error for a broken bundle")
	}
	if res.OK {
		t.Error("a failed validation must report OK=false")
	}
	if len(res.Promoted) != 0 || len(store.bundles) != 0 || len(store.constructs) != 0 {
		t.Error("a failed validation must promote + persist nothing")
	}
	if len(res.Diagnostics) == 0 {
		t.Error("a failed validation must carry diagnostics")
	}
}

// TestPromoteBundleDurable_PromotesAndCallable: a valid bundle's plain construct
// is durably promoted and callable on the promoting engine (shared registry).
func TestPromoteBundleDurable_PromotesAndCallable(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	store := &fakePromoteStore{}
	res, err := e.promoteBundleDurableWithStore(context.Background(), store, "owner-1", sessionSpecSrc)
	if err != nil {
		t.Fatalf("durable promote bundle: %v", err)
	}
	if !res.OK {
		t.Fatalf("valid bundle should promote OK; diagnostics=%+v", res.Diagnostics)
	}
	if len(res.Promoted) != 1 || res.Promoted[0].Name != "mcpSessSpec" {
		t.Fatalf("expected mcpSessSpec promoted, got %+v", res.Promoted)
	}
	// Callable on this engine: in the shared spec registry.
	if _, ok := e.specs.Lookup("mcpSessSpec"); !ok {
		t.Error("promoted spec not callable (absent from shared spec registry)")
	}
	// Persisted exactly one reviewable bundle + construct row pair.
	if len(store.bundles) != 1 || len(store.constructs) != 1 {
		t.Errorf("expected 1 bundle + 1 construct persisted, got %d + %d", len(store.bundles), len(store.constructs))
	}
}

// crossNodeRehydrateStore is engine B's view of the SHARED DB: it serves the
// promoted bundle's construct rows for whatever bundle id the broadcast carries.
// It returns the recorded construct for ANY bundle id, so the test does not need
// to know the random id engine A minted -- exactly what a shared Postgres does
// (the row is there; the broadcast just names which bundle to re-hydrate).
type crossNodeRehydrateStore struct {
	row AuthoringConstructRow
}

func (s *crossNodeRehydrateStore) LoadPromotedBundles(context.Context) ([]AuthoringBundleRow, error) {
	return nil, nil // not used on the single-bundle propagation path
}

func (s *crossNodeRehydrateStore) LoadConstructsForBundle(_ context.Context, _, bundleId string) ([]AuthoringConstructRow, error) {
	row := s.row
	row.BundleId = bundleId
	return []AuthoringConstructRow{row}, nil
}

// TestAuthoringPromotePropagation_CrossNode is the load-bearing cross-node test.
// Two SEPARATE engines (independent spec registries) are bridged by forwarding
// the authoring.promote.* broadcast from engine A's bus to engine B's bus --
// simulating the mesh EventBridge + the single broadcast routing rule. A durable
// promote on engine A must make the construct callable on engine B WITHOUT a
// restart, purely via the broadcast.
//
// FAILS without the propagation: if engine B never started the authoring-promote
// subscriber, the broadcast is delivered to its bus and dropped, and the assert
// that mcpSessSpec is callable on B fails. PASSES with the subscriber wired.
func TestAuthoringPromotePropagation_CrossNode(t *testing.T) {
	busA := events.NewBus()
	busB := events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })

	// Bridge A -> B: forward authoring.promote.* from A's bus onto B's bus,
	// stamping an OriginNodeId so it reads as a remote (peer-bridged) event --
	// exactly what the node EventBridge does for a forwarded broadcast.
	busA.Subscribe("authoring.promote.#", func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	// Engine A: handles the promote, registers locally, and broadcasts.
	engA := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	engA.SetEventBus(busA)

	// Engine B: a DIFFERENT engine with its OWN empty spec registry. It only
	// learns about the promote via the broadcast + re-hydration from the shared
	// DB (the crossNodeRehydrateStore).
	engB := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	engB.SetEventBus(busB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bStore := &crossNodeRehydrateStore{row: AuthoringConstructRow{
		OwnerUserId: "owner-1", Kind: "spec", Name: "mcpSessSpec", Source: sessionSpecSrc,
	}}
	// THE propagation under test: B subscribes to the authoring-promote broadcast.
	engB.startAuthoringPromoteSubscriberWithStore(ctx, bStore)

	// Sanity: B does not have the construct before any promote.
	if _, ok := engB.specs.Lookup("mcpSessSpec"); ok {
		t.Fatal("engine B should not have the promoted spec before the promote")
	}

	// Promote on engine A (fake persist store, real bus broadcast).
	res, err := engA.promoteBundleDurableWithStore(ctx, &fakePromoteStore{}, "owner-1", sessionSpecSrc)
	if err != nil {
		t.Fatalf("promote on engine A: %v", err)
	}
	if !res.OK || len(res.Promoted) != 1 {
		t.Fatalf("promote on engine A did not succeed: %+v", res)
	}

	// Engine A: callable immediately (local registration).
	if _, ok := engA.specs.Lookup("mcpSessSpec"); !ok {
		t.Fatal("promoted spec not callable on engine A (local registration failed)")
	}

	// Engine B: callable within seconds via the broadcast -- NO restart. The
	// broadcast + bridge + B's subscriber are all async, so poll briefly.
	if !eventually(2*time.Second, func() bool {
		_, ok := engB.specs.Lookup("mcpSessSpec")
		return ok
	}) {
		t.Fatal("CROSS-NODE FAILURE: promoted spec never became callable on engine B " +
			"-- the authoring.promote broadcast did not propagate the promote (this is the bug the subscriber fixes)")
	}
}

// eventually polls cond until it returns true or the timeout elapses.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
