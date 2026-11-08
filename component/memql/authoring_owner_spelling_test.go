package memql

// authoring_owner_spelling_test.go -- the activation and retirement owner gates
// compare PEOPLE, not spellings (memql#6022 follow-up).
//
// A bundle's ownerUserId is stamped from actor.userId and stored canonical
// ("v1:identity:user:<id>"), while a builtin hands the gates the caller's BARE
// id, as every wire seam does (integrations/router actingOwner). Compared as
// strings the two never matched, so a person was refused their own bundle and
// no routing rule was ever armed. The other half of the gate must not move: a
// different person is refused in either spelling.
//
// Internal (package memql) for the store seams and the arm test store
// (authoring_arm_propagate_test.go); the Postgres path through the builtin is
// app/routing_rule_arming_db_test.go.

import (
	"context"
	"testing"
)

func spellingTestBundle(store *armTestStore) string {
	return store.addBundle("v1:authoring:bundle:spelling", 1, "", BundleDryRunPassed,
		[2]string{"rule", "spellingRule"})
}

// TestTheOwnerActivatesAndRetiresTheirBundleInEitherSpelling: the owner, named
// canonical or bare, arms their bundle and turns it off again.
func TestTheOwnerActivatesAndRetiresTheirBundleInEitherSpelling(t *testing.T) {
	for _, spelling := range []string{armTestOwner, BareShortId(armTestOwner)} {
		t.Run(spelling, func(t *testing.T) {
			ctx := context.Background()
			store := newArmTestStore()
			bundleId := spellingTestBundle(store)
			registry := NewAuthoredRuntimeRegistry()
			deps := AuthoredRuntimeDeps{Registry: registry}
			eng := &MemQLEngine{}

			if _, err := eng.activateApprovedBundleWithStore(ctx, store, spelling, bundleId, deps); err != nil {
				t.Fatalf("the owner, as %q, could not activate their own bundle: %v", spelling, err)
			}
			if got := store.bundleStatus(bundleId); got != BundleActive {
				t.Fatalf("after the owner's activation the bundle reads %q, want active", got)
			}
			if _, ok := registry.Resolve(armTestOwner, "rule", "spellingRule"); !ok {
				t.Fatalf("after the owner's activation the rule is not armed: %+v", registry.ListAll())
			}

			if err := eng.retireActiveBundleWithStore(ctx, store, spelling, bundleId, deps); err != nil {
				t.Fatalf("the owner, as %q, could not retire their own bundle: %v", spelling, err)
			}
			if got := store.bundleStatus(bundleId); got != BundleRetired {
				t.Fatalf("after the owner's retire the bundle reads %q, want retired", got)
			}
			if _, ok := registry.Resolve(armTestOwner, "rule", "spellingRule"); ok {
				t.Fatal("after the owner's retire the rule is still armed")
			}
		})
	}
}

// TestSomebodyElseCannotActivateOrRetireTheBundleInEitherSpelling: comparing
// bare ids widens the gate to every spelling of the OWNER and to nobody else.
func TestSomebodyElseCannotActivateOrRetireTheBundleInEitherSpelling(t *testing.T) {
	const other = "v1:identity:user:someone-else"
	for _, spelling := range []string{other, BareShortId(other)} {
		t.Run(spelling, func(t *testing.T) {
			ctx := context.Background()
			store := newArmTestStore()
			bundleId := spellingTestBundle(store)
			registry := NewAuthoredRuntimeRegistry()
			deps := AuthoredRuntimeDeps{Registry: registry}
			eng := &MemQLEngine{}

			if _, err := eng.activateApprovedBundleWithStore(ctx, store, spelling, bundleId, deps); err == nil {
				t.Fatalf("%q activated a bundle owned by %q", spelling, armTestOwner)
			}
			if got := store.bundleStatus(bundleId); got != BundleDryRunPassed {
				t.Fatalf("a refused activation left the bundle %q, want dryRunPassed", got)
			}
			if armed := registry.ListAll(); len(armed) != 0 {
				t.Fatalf("a refused activation armed %+v", armed)
			}

			if _, err := eng.activateApprovedBundleWithStore(ctx, store, armTestOwner, bundleId, deps); err != nil {
				t.Fatalf("the owner could not activate their own bundle: %v", err)
			}
			if err := eng.retireActiveBundleWithStore(ctx, store, spelling, bundleId, deps); err == nil {
				t.Fatalf("%q retired a bundle owned by %q", spelling, armTestOwner)
			}
			if got := store.bundleStatus(bundleId); got != BundleActive {
				t.Fatalf("a refused retire left the bundle %q, want active", got)
			}
			if _, ok := registry.Resolve(armTestOwner, "rule", "spellingRule"); !ok {
				t.Fatal("a refused retire took the owner's rule out of the runtime")
			}
		})
	}
}
