package memql

import (
	"strings"
	"testing"
)

// TestSiteSystemOwnedDeleteGuard pins the #3717 write guard: a systemOwned
// v1:platform:site row -- the portal's own seed, re-seeded at boot -- must
// never be deletable by anyone but a system actor. The guard touches no
// engine field, so every case here runs through the nil receiver (DB-free),
// mirroring TestHealingValidationRankBoundGuard.
func TestSiteSystemOwnedDeleteGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	ownerCtx, ownerActor := ownerActorContext()
	sysCtx, sysActor := systemSeedContext()

	t.Run("a non-delete write passes regardless of systemOwned", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "status": "live"}
		if err := validatorOnNilEngine().validateSiteSystemOwnedDelete(userCtx, payload, true, userActor); err != nil {
			t.Fatalf("a write that does not set deleted:true must pass; got %v", err)
		}
	})

	t.Run("deleting a non-systemOwned site passes", func(t *testing.T) {
		payload := map[string]any{"hostname": "shop.example.com", "deleted": true}
		if err := validatorOnNilEngine().validateSiteSystemOwnedDelete(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("deleting an ordinary site must pass; got %v", err)
		}
	})

	t.Run("a user actor deleting a systemOwned site is refused", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "deleted": true}
		err := validatorOnNilEngine().validateSiteSystemOwnedDelete(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected the delete to be refused for a systemOwned row")
		}
		for _, want := range []string{"portal.example.com", "systemOwned", "v1:platform:site"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing substring %q", err.Error(), want)
			}
		}
	})

	t.Run("an owner actor deleting a systemOwned site is still refused -- ownership does not exempt", func(t *testing.T) {
		// systemOwned is not an ownership question -- it is what stops an
		// operator (owner included) from bricking cluster management, so the
		// cluster owner gets no special escape here.
		payload := map[string]any{"hostname": "portal.example.com", "deleted": true}
		if err := validatorOnNilEngine().validateSiteSystemOwnedDelete(ownerCtx, payload, true, ownerActor); err == nil {
			t.Fatal("expected the delete to be refused even for a cluster-owner actor")
		}
	})

	t.Run("a raw write cannot smuggle systemOwned:false in the same delta as deleted:true", func(t *testing.T) {
		// priorSystemOwned (the persisted value, read-merged before the
		// delta) is true even though the delta in payload claims
		// systemOwned:false -- the guard must trust the PRIOR value, not the
		// caller's claim, or a single raw write flips the flag off and
		// deletes in the same call.
		payload := map[string]any{"hostname": "portal.example.com", "systemOwned": false, "deleted": true}
		if err := validatorOnNilEngine().validateSiteSystemOwnedDelete(userCtx, payload, true, userActor); err == nil {
			t.Fatal("expected the delete to be refused -- flipping systemOwned in the same write must not bypass the guard")
		}
	})

	t.Run("system actor may delete (kept symmetric with every other guard in this file)", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "deleted": true}
		if err := validatorOnNilEngine().validateSiteSystemOwnedDelete(sysCtx, payload, true, sysActor); err != nil {
			t.Fatalf("system actor must be able to delete; got %v", err)
		}
	})

	t.Run("nil payload passes", func(t *testing.T) {
		if err := validatorOnNilEngine().validateSiteSystemOwnedDelete(userCtx, nil, true, userActor); err != nil {
			t.Fatalf("nil payload must pass; got %v", err)
		}
	})
}
