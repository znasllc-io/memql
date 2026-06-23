package memql

import (
	"strings"
	"testing"
)

// Epic 4 / memql#2140: the self-healing base-tier immutability guard.
//
// validateHealingBaseImmutable rejects any NON-system-actor write to a
// tier=base v1:healing:healedOverride row (whether tier=base is in the
// proposed payload or only in the prior row), while allowing a system actor
// (the SeedMaterializer) and allowing a tier=overlay healed-override write by
// a user actor. The guard touches no engine field, so it runs through a nil
// receiver (DB-free). Mirrors TestRBACBaseRoleImmutableGuard.
func TestHealingBaseImmutableGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	sysCtx, sysActor := systemSeedContext()

	t.Run("user-actor write of tier=base-in-payload is rejected", func(t *testing.T) {
		payload := map[string]any{"baseConstructId": "deployStaging", "tier": "base", "overrideData": map[string]any{"x": 1}}
		err := validatorOnNilEngine().validateHealingBaseImmutable(userCtx, payload, false, userActor)
		if err == nil {
			t.Fatal("expected rejection for user-actor write of a base-tier override; got nil")
		}
		for _, want := range []string{"immutable", "v1:healing:healedOverride", "system actor"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing substring %q", err.Error(), want)
			}
		}
	})

	t.Run("user-actor write of prior-base row is rejected even if delta flips tier to overlay", func(t *testing.T) {
		// The caller tries to flip tier to overlay on an existing base row; the
		// prior-row flag still gates the write.
		payload := map[string]any{"baseConstructId": "deployStaging", "tier": "overlay"}
		err := validatorOnNilEngine().validateHealingBaseImmutable(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected rejection: a prior base-tier row must stay immutable even when the delta flips tier=overlay")
		}
	})

	t.Run("system actor may materialize a base-tier override", func(t *testing.T) {
		payload := map[string]any{"baseConstructId": "deployStaging", "tier": "base", "overrideData": map[string]any{"x": 1}}
		if err := validatorOnNilEngine().validateHealingBaseImmutable(sysCtx, payload, true, sysActor); err != nil {
			t.Fatalf("system actor must be able to materialize a base-tier row; got %v", err)
		}
	})

	t.Run("user actor may write a tier=overlay healed override", func(t *testing.T) {
		payload := map[string]any{"baseConstructId": "deployStaging", "tier": "overlay", "valid": false}
		if err := validatorOnNilEngine().validateHealingBaseImmutable(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("a tier=overlay healed-override write must pass for a user actor; got %v", err)
		}
	})

	t.Run("default tier (absent) is treated as overlay and passes for a user actor", func(t *testing.T) {
		// The concept default is overlay; an absent tier field must not be
		// treated as base.
		payload := map[string]any{"baseConstructId": "deployStaging"}
		if err := validatorOnNilEngine().validateHealingBaseImmutable(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("an absent tier (concept default overlay) must pass for a user actor; got %v", err)
		}
	})
}
