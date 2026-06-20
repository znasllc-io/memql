package memql

// authoring_rearm_test.go -- coverage for the boot-time re-arm of already-active
// authored bundles (epic memql#954, issue #1039).
//
// Internal (package memql) test so it can drive the unexported rearmStore seam +
// rearmActiveBundlesWithStore with a fake store + the REAL
// AuthoredRuntimeRegistry -- no DB. It asserts the acceptance property: after a
// simulated restart (a fresh registry + scheduler), an active bundle's
// constructs are re-registered + its automation re-scheduled, across owners,
// without re-running any lifecycle mutation.

import (
	"context"
	"fmt"
	"testing"
)

// fakeRearmStore stands in for the persisted graph. LoadSystemActiveBundles is
// the admin-scoped enumeration (all owners); LoadConstructsForOwner is the
// owner-scoped per-bundle load, and it records the (owner, bundleId) pairs it
// was asked for so the test can assert the load ran under the AUTHOR's identity.
type fakeRearmStore struct {
	active     []AuthoringBundleRow
	constructs map[string][]AuthoringConstructRow // bundleId -> constructs
	loadCalls  []string                           // "owner|bundleId"
	loadErr    map[string]error                   // bundleId -> error to return
}

func (s *fakeRearmStore) LoadSystemActiveBundles(_ context.Context) ([]AuthoringBundleRow, error) {
	return s.active, nil
}

func (s *fakeRearmStore) LoadConstructsForOwner(_ context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	s.loadCalls = append(s.loadCalls, owner+"|"+bundleId)
	if s.loadErr != nil {
		if err, ok := s.loadErr[bundleId]; ok {
			return nil, err
		}
	}
	return s.constructs[bundleId], nil
}

// rearmableBundle builds an active bundle row + its two member constructs (an
// automation headline + a query dep) for owner.
func rearmableBundle(id, owner string) (AuthoringBundleRow, []AuthoringConstructRow) {
	bundle := AuthoringBundleRow{
		Id:          id,
		OwnerUserId: owner,
		Title:       "re-armable capability",
		Status:      BundleActive,
		Version:     1,
	}
	constructs := []AuthoringConstructRow{
		{
			Id:          id + ":auto",
			OwnerUserId: owner,
			BundleId:    id,
			Kind:        "automation",
			Name:        "autoHandleThing",
			Source:      "automation autoHandleThing { }",
		},
		{
			Id:          id + ":q",
			OwnerUserId: owner,
			BundleId:    id,
			Kind:        "query",
			Name:        "queryThing",
			Source:      "query queryThing { }",
		},
	}
	return bundle, constructs
}

// TestRearm_RegistersActiveBundlesAcrossOwners: two owners each have an active
// bundle; after re-arm BOTH owners' constructs are registered in the runtime and
// each automation went through the register hook (re-scheduled). This is the
// cross-owner acceptance: a system-scoped enumeration re-arms every owner.
func TestRearm_RegistersActiveBundlesAcrossOwners(t *testing.T) {
	bA, cA := rearmableBundle("v1:authoring:bundle:a", "owner-a")
	bB, cB := rearmableBundle("v1:authoring:bundle:b", "owner-b")

	store := &fakeRearmStore{
		active: []AuthoringBundleRow{bA, bB},
		constructs: map[string][]AuthoringConstructRow{
			bA.Id: cA,
			bB.Id: cB,
		},
	}

	registry := NewAuthoredRuntimeRegistry()
	var scheduled []string
	deps := AuthoredRuntimeDeps{
		Registry: registry,
		RegisterHook: func(c *AuthoredConstruct) error {
			scheduled = append(scheduled, c.OwnerUserId+"|"+c.Name)
			return nil
		},
	}

	res, err := rearmActiveBundlesWithStore(context.Background(), store, deps)
	if err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if res.Seen != 2 || res.Rearmed != 2 || len(res.FailedIds) != 0 {
		t.Fatalf("result mismatch: %+v", res)
	}

	// Each bundle has 2 constructs -> 4 registry entries across both owners.
	if registry.Count() != 4 {
		t.Errorf("expected 4 registered constructs, got %d", registry.Count())
	}
	// Each owner's construct resolves only under its OWN owner -- owner isolation.
	if _, ok := registry.Resolve("owner-a", "query", "queryThing"); !ok {
		t.Error("owner-a query not resolvable after re-arm")
	}
	if _, ok := registry.Resolve("owner-b", "automation", "autoHandleThing"); !ok {
		t.Error("owner-b automation not resolvable after re-arm")
	}
	if _, ok := registry.Resolve("owner-a", "automation", "autoHandleThing"); !ok {
		t.Error("owner-a automation not resolvable after re-arm")
	}

	// Only the automation construct of each bundle was scheduled (the dep query
	// is registered but not handed to the scheduler).
	if len(scheduled) != 2 {
		t.Fatalf("expected 2 automations scheduled, got %d (%v)", len(scheduled), scheduled)
	}

	// The per-bundle construct loads ran under each bundle's AUTHOR -- the load
	// call is keyed by the owner, never a cluster-owner bypass identity.
	wantLoads := map[string]bool{"owner-a|" + bA.Id: false, "owner-b|" + bB.Id: false}
	for _, lc := range store.loadCalls {
		if _, ok := wantLoads[lc]; ok {
			wantLoads[lc] = true
		} else {
			t.Errorf("unexpected construct load under identity %q", lc)
		}
	}
	for k, seen := range wantLoads {
		if !seen {
			t.Errorf("expected construct load %q under the bundle author", k)
		}
	}
}

// TestRearm_OneBundleFailureDoesNotAbortOthers: a bundle whose construct load
// errors is recorded as failed, but the other owner's bundle still re-arms --
// fault isolation, no cascade.
func TestRearm_OneBundleFailureDoesNotAbortOthers(t *testing.T) {
	bA, cA := rearmableBundle("v1:authoring:bundle:a", "owner-a")
	bBad, _ := rearmableBundle("v1:authoring:bundle:bad", "owner-x")

	store := &fakeRearmStore{
		active: []AuthoringBundleRow{bBad, bA},
		constructs: map[string][]AuthoringConstructRow{
			bA.Id: cA,
		},
		loadErr: map[string]error{bBad.Id: fmt.Errorf("graph read failed")},
	}

	registry := NewAuthoredRuntimeRegistry()
	deps := AuthoredRuntimeDeps{Registry: registry}

	res, err := rearmActiveBundlesWithStore(context.Background(), store, deps)
	if err != nil {
		t.Fatalf("re-arm should not hard-fail on a single bad bundle: %v", err)
	}
	if res.Seen != 2 || res.Rearmed != 1 {
		t.Fatalf("expected 2 seen / 1 rearmed, got %+v", res)
	}
	if len(res.FailedIds) != 1 || res.FailedIds[0] != bBad.Id {
		t.Fatalf("expected the bad bundle in FailedIds, got %v", res.FailedIds)
	}
	// The good owner's constructs are still registered.
	if registry.Count() != 2 {
		t.Errorf("expected 2 registered constructs from the good bundle, got %d", registry.Count())
	}
}

// TestRearm_EmptyActiveSetIsNoOp: no active bundles -> nothing registered, no
// error. The fresh-cluster / nothing-to-restore case.
func TestRearm_EmptyActiveSetIsNoOp(t *testing.T) {
	store := &fakeRearmStore{}
	registry := NewAuthoredRuntimeRegistry()
	res, err := rearmActiveBundlesWithStore(context.Background(), store, AuthoredRuntimeDeps{Registry: registry})
	if err != nil {
		t.Fatalf("re-arm empty: %v", err)
	}
	if res.Seen != 0 || res.Rearmed != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
	if registry.Count() != 0 {
		t.Errorf("expected nothing registered, got %d", registry.Count())
	}
}

// TestRearm_NilRegistryErrors: re-arm requires a registry.
func TestRearm_NilRegistryErrors(t *testing.T) {
	_, err := rearmActiveBundlesWithStore(context.Background(), &fakeRearmStore{}, AuthoredRuntimeDeps{})
	if err == nil {
		t.Fatal("expected an error when the registry is nil")
	}
}

// TestRearm_EmptyActiveBundleIsSkippedNotFailed: an ACTIVE bundle with zero
// member constructs must be counted as Skipped -- not Failed, not Rearmed.
// This is the core regression test for #1808: mcp-promote-* bundles (and any
// partially-created bundle whose construct write failed) are left ACTIVE in the
// DB with no constructs; before the fix every boot logged one WARN per such
// bundle. The Skipped counter tracks them and FailedIds stays empty, which means
// the WARN path (on non-nil err) is never reached for these bundles.
func TestRearm_EmptyActiveBundleIsSkippedNotFailed(t *testing.T) {
	// emptyBundle is active but has zero member constructs (simulates an
	// mcp-promote-* or partially-created bundle).
	emptyBundle := AuthoringBundleRow{
		Id:          "v1:authoring:bundle:mcp-promote-abc123",
		OwnerUserId: "owner-promote",
		Title:       "MCP durable promotion",
		Status:      BundleActive,
		Version:     1,
	}
	// goodBundle is a normal automation bundle that should still re-arm.
	goodBundle, goodConstructs := rearmableBundle("v1:authoring:bundle:good", "owner-good")

	store := &fakeRearmStore{
		active: []AuthoringBundleRow{emptyBundle, goodBundle},
		constructs: map[string][]AuthoringConstructRow{
			// emptyBundle intentionally omitted -- zero constructs
			goodBundle.Id: goodConstructs,
		},
	}

	registry := NewAuthoredRuntimeRegistry()
	deps := AuthoredRuntimeDeps{
		Registry: registry,
		RegisterHook: func(c *AuthoredConstruct) error {
			return nil
		},
	}

	res, err := rearmActiveBundlesWithStore(context.Background(), store, deps)
	if err != nil {
		t.Fatalf("re-arm: %v", err)
	}

	// The empty bundle must be counted as Skipped, not Failed or Rearmed.
	// If it were counted as Failed it would appear in FailedIds and trigger a
	// per-bundle WARN in the boot log -- the exact flood #1808 reports.
	if res.Skipped != 1 {
		t.Errorf("expected 1 skipped bundle, got %d", res.Skipped)
	}
	if len(res.FailedIds) != 0 {
		t.Errorf("expected no failed bundles, got %v", res.FailedIds)
	}
	if res.Rearmed != 1 {
		t.Errorf("expected 1 rearmed bundle (the good one), got %d", res.Rearmed)
	}
	if res.Seen != 2 {
		t.Errorf("expected 2 seen, got %d", res.Seen)
	}

	// The good bundle's constructs must still be registered (fault isolation).
	if registry.Count() != 2 {
		t.Errorf("expected 2 registered constructs from the good bundle, got %d", registry.Count())
	}
}

// TestRearm_MixOfEmptyAndNormalAndFailed: an empty bundle (skipped), a good
// bundle (rearmed), and a failing bundle (failed) in the same pass -- all three
// counters advance independently, no cascade.
func TestRearm_MixOfEmptyAndNormalAndFailed(t *testing.T) {
	emptyBundle := AuthoringBundleRow{
		Id:          "v1:authoring:bundle:mcp-promote-xyz",
		OwnerUserId: "owner-promote",
		Status:      BundleActive,
		Version:     1,
	}
	goodBundle, goodConstructs := rearmableBundle("v1:authoring:bundle:good", "owner-good")
	badBundle, _ := rearmableBundle("v1:authoring:bundle:bad", "owner-bad")

	store := &fakeRearmStore{
		active: []AuthoringBundleRow{emptyBundle, goodBundle, badBundle},
		constructs: map[string][]AuthoringConstructRow{
			goodBundle.Id: goodConstructs,
			// emptyBundle: no entry (zero constructs)
		},
		loadErr: map[string]error{
			badBundle.Id: fmt.Errorf("transient db read error"),
		},
	}

	registry := NewAuthoredRuntimeRegistry()
	deps := AuthoredRuntimeDeps{Registry: registry}

	res, err := rearmActiveBundlesWithStore(context.Background(), store, deps)
	if err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if res.Seen != 3 {
		t.Errorf("expected 3 seen, got %d", res.Seen)
	}
	if res.Rearmed != 1 {
		t.Errorf("expected 1 rearmed, got %d", res.Rearmed)
	}
	if res.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", res.Skipped)
	}
	if len(res.FailedIds) != 1 || res.FailedIds[0] != badBundle.Id {
		t.Errorf("expected 1 failed (the bad bundle), got %v", res.FailedIds)
	}
}
