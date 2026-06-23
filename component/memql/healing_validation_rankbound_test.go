package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func ownerActorContext() (context.Context, string) {
	token := &auth.TokenInfo{
		Subject: "user-owner",
		Claims:  map[string]any{"sub": "user-owner", "role": "owner"},
	}
	return auth.ContextWithToken(context.Background(), token), "user-owner"
}

// Epic 4 / memql#2143: the blast-radius-scaled validation guard.
//
// validateHealingValidationRankBound gates the heal-ACCEPT transition (valid
// false->true) on the actor's role rank meeting the override's blastRadius-
// required rank. It touches no engine field for the pass-paths, so they run
// through a nil receiver (DB-free). The non-owner rank lookup needs a DB; with
// a nil receiver the lookup fails closed (rank 0), which is exactly the deny
// case we assert. Mirrors TestRBACBaseRoleImmutableGuard.
func TestHealingValidationRankBoundGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	ownerCtx, ownerActor := ownerActorContext()
	sysCtx, sysActor := systemSeedContext()

	t.Run("non-acceptance write passes (valid not true)", func(t *testing.T) {
		// A propose (valid=false) or reject (valid stays false) is not an
		// acceptance, so the rank gate does not apply.
		payload := map[string]any{"baseConstructId": "x", "valid": false, "blastRadius": "spine_adjacent"}
		if err := validatorOnNilEngine().validateHealingValidationRankBound(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("a non-acceptance write must pass; got %v", err)
		}
	})

	t.Run("re-write of an already-valid override passes (not a NEW acceptance)", func(t *testing.T) {
		payload := map[string]any{"baseConstructId": "x", "valid": true, "blastRadius": "spine_adjacent"}
		if err := validatorOnNilEngine().validateHealingValidationRankBound(userCtx, payload, true, userActor); err != nil {
			t.Fatalf("re-writing an already-accepted override must pass; got %v", err)
		}
	})

	t.Run("owner may validate any blast radius", func(t *testing.T) {
		payload := map[string]any{"baseConstructId": "x", "valid": true, "blastRadius": "spine_adjacent"}
		if err := validatorOnNilEngine().validateHealingValidationRankBound(ownerCtx, payload, false, ownerActor); err != nil {
			t.Fatalf("owner must be able to validate a spine_adjacent heal; got %v", err)
		}
	})

	t.Run("system actor may validate (e.g. seed re-materialization)", func(t *testing.T) {
		payload := map[string]any{"baseConstructId": "x", "valid": true, "blastRadius": "spine_adjacent"}
		if err := validatorOnNilEngine().validateHealingValidationRankBound(sysCtx, payload, false, sysActor); err != nil {
			t.Fatalf("system actor must be able to validate; got %v", err)
		}
	})

	t.Run("non-owner accepting a heal is denied when rank cannot meet the threshold (fail-closed)", func(t *testing.T) {
		// A real but DB-less engine: resolveActorRank's catalog lookup finds no
		// role rows and returns rank 0, below every threshold -> deny. This is
		// the fail-closed property (an unresolvable rank never validates).
		eng, err := New(nil)
		if err != nil {
			t.Fatalf("New(nil): %v", err)
		}
		payload := map[string]any{"baseConstructId": "deployStaging", "valid": true, "blastRadius": "shared"}
		gerr := eng.validateHealingValidationRankBound(userCtx, payload, false, userActor)
		if gerr == nil {
			t.Fatalf("expected the acceptance to be denied (fail-closed) for an unresolved-rank non-owner actor")
		}
		for _, want := range []string{"blast-radius", "v1:healing:healedOverride", "rank"} {
			if !strings.Contains(gerr.Error(), want) {
				t.Errorf("error %q missing substring %q", gerr.Error(), want)
			}
		}
	})
}

// blastRadiusMinRank maps each radius to the documented Epic-1 rank floor.
func TestBlastRadiusMinRank(t *testing.T) {
	cases := map[string]int{
		"personal":       healRankUser,
		"shared":         healRankAdmin,
		"spine_adjacent": healRankDeveloper,
		"":               healRankUser, // default personal
		"unknown":        healRankUser, // unknown -> most permissive floor
	}
	for radius, want := range cases {
		if got := blastRadiusMinRank(radius); got != want {
			t.Errorf("blastRadiusMinRank(%q) = %d, want %d", radius, got, want)
		}
	}
}
