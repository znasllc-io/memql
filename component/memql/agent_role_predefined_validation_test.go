package memql

import (
	"strings"
	"testing"
)

// memql#2985: the agent-role predefined-lock guard.
//
// `predefined` was a UI hint with nothing behind it -- the cockpit reads it to
// draw a lock icon and the concept's own doc calls the row LOCKED, but no
// engine guard enforced any of it, so any caller could rename, re-categorise,
// re-skill or deactivate a seeded catalog row through the mutation surface.
// `v1:rbac:role`, the concept memql#2918 leant on to justify making this
// catalog @public, has TWO write guards; `agentRole` had none.
//
// The guard touches no engine field, so it runs through a nil receiver
// (DB-free), the same way TestHealingBaseImmutableGuard and
// TestRBACBaseRoleImmutableGuard do.
func TestAgentRolePredefinedLockGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	sysCtx, sysActor := systemSeedContext()

	// seeded is the persisted catalog row every subtest edits against. Built
	// fresh per subtest so a mutation in one cannot leak into another.
	seeded := func() map[string]any {
		return map[string]any{
			"slug":                  "it-support",
			"name":                  "IT Support",
			"description":           "Helps with computers.",
			"category":              "professional",
			"tier":                  "A",
			"maxSkills":             5,
			"lockedSkillIds":        []any{"skill-a"},
			"forbiddenSkillIds":     []any{},
			"recommendedPolicySlug": "balancedChat",
			"active":                true,
			"predefined":            true,
		}
	}

	// merged models what executeWrite hands the guard: the prior row with the
	// caller's delta merged on top. Building it this way rather than writing
	// the merged map by hand is what keeps these cases honest -- the guard
	// only ever sees merged payloads, and a hand-written one could omit a
	// field the real merge always inherits.
	merged := func(prior map[string]any, delta map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range prior {
			out[k] = v
		}
		for k, v := range delta {
			out[k] = v
		}
		return out
	}

	t.Run("renaming a predefined role is rejected", func(t *testing.T) {
		prior := seeded()
		payload := merged(prior, map[string]any{"name": "IT Helpdesk"})
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected rejection for a user-actor rename of a predefined role; got nil")
		}
		// The message names the ROW, not the field: a whole-row lock has no
		// diff to report, and the per-field wording went with the carve-out
		// it belonged to (memql#3061).
		for _, want := range []string{"v1:agents:agentRole", "it-support", "locked"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing substring %q", err.Error(), want)
			}
		}
	})

	t.Run("deactivating a predefined role is rejected", func(t *testing.T) {
		// The case the issue names directly: nothing stopped a caller
		// deactivating a catalog row, which removes it from every picker.
		prior := seeded()
		payload := merged(prior, map[string]any{"active": false})
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected rejection for a user-actor deactivation of a predefined role; got nil")
		}
		if !strings.Contains(err.Error(), "it-support") {
			t.Errorf("error should name the locked role; got %q", err.Error())
		}
	})

	t.Run("re-skilling a predefined role is rejected", func(t *testing.T) {
		// A list-shaped edit. Under the field-scoped design this also pinned
		// that the comparison was deep rather than by-reference; the whole-row
		// lock has no comparison at all, which is one fewer thing to get
		// subtly wrong (memql#3061).
		prior := seeded()
		payload := merged(prior, map[string]any{"lockedSkillIds": []any{"skill-a", "skill-b"}})
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected rejection for a user-actor edit of lockedSkillIds on a predefined role; got nil")
		}
		if !strings.Contains(err.Error(), "locked") {
			t.Errorf("error should say the role is locked; got %q", err.Error())
		}
	})

	t.Run("flipping predefined to false in the same delta is still rejected", func(t *testing.T) {
		// The bypass every guard in this family defends against: demote the
		// catalog row and edit it in one write. The PRIOR flag gates it, and
		// `predefined` is itself outside the editable set.
		prior := seeded()
		payload := merged(prior, map[string]any{"predefined": false, "name": "Mine Now"})
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected rejection: a prior predefined row must stay locked even when the " +
				"delta flips predefined=false")
		}
	})

	// The design this REPLACED, pinned so it cannot creep back in silently.
	//
	// The first version carved tier + recommendedPolicySlug out as
	// operator-tunable, on the strength of the concept field doc. The
	// memql#3061 review retired that, on the operator's ruling: the carve-out
	// was inert in production (a JSON-decoded float64 prior compared
	// type-strictly against an int64 coalesce default reported maxSkills as
	// changed, so a tier-only edit was rejected anyway), nothing exercises it
	// (memql-cockpit contains no reference to agentRole at all), the edit does
	// not survive the next re-seed, and recommendedPolicySlug is stamped onto
	// every agent minted in the role and resolves the router's provider chain
	// -- so making it writable would have been a NEW exposure, not a preserved
	// one.
	t.Run("tier and recommendedPolicySlug are locked too", func(t *testing.T) {
		for _, tc := range []struct {
			field string
			value any
		}{
			{"tier", "B"},
			{"recommendedPolicySlug", "strongReasoning"},
		} {
			prior := seeded()
			payload := merged(prior, map[string]any{tc.field: tc.value})
			if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, true, userActor); err == nil {
				t.Errorf("%s must be locked on a predefined role: the carve-out was retired in "+
					"memql#3061 because it was inert, unused, reverted on the next boot, and "+
					"(for recommendedPolicySlug) steered the AI router", tc.field)
			}
		}
	})

	t.Run("even an unchanged re-write by a non-system actor is rejected", func(t *testing.T) {
		// A whole-row lock gates the WRITE, not the diff -- exactly as
		// validateRbacBaseRoleImmutable does. There is no legitimate
		// non-system re-write of a catalog row: the SeedMaterializer owns
		// them, and it runs as a system actor.
		prior := seeded()
		payload := merged(prior, nil)
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, true, userActor); err == nil {
			t.Fatal("a non-system re-write of a predefined role must be rejected even when it " +
				"changes nothing; the guard locks the row, not the delta")
		}
	})

	t.Run("a user-created role is fully editable", func(t *testing.T) {
		prior := seeded()
		prior["predefined"] = false
		prior["slug"] = "my-custom-role"
		payload := merged(prior, map[string]any{"name": "Whatever I Like", "active": false})
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("a predefined=false role must stay fully editable -- that is the whole point of "+
				"the flag; got %v", err)
		}
	})

	t.Run("a non-system actor cannot mint a predefined role", func(t *testing.T) {
		// Without this arm the field-scoped check passes vacuously on a
		// create: there is no prior row, so no field "changed", and a caller
		// could mint a brand-new locked catalog row.
		payload := map[string]any{"slug": "forged", "name": "Forged", "predefined": true}
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, false, userActor)
		if err == nil {
			t.Fatal("expected rejection: a non-system actor must not be able to create a " +
				"predefined catalog row")
		}
		if !strings.Contains(err.Error(), "forged") {
			t.Errorf("error should name the slug being forged; got %q", err.Error())
		}
	})

	t.Run("the SeedMaterializer may re-materialize the catalog", func(t *testing.T) {
		// dsl/agents/roles/*.memql is re-seeded on every startup. If the guard
		// caught the system actor, boot would fail on the first changed role.
		prior := seeded()
		payload := merged(prior, map[string]any{"name": "IT Support (renamed upstream)", "active": false})
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(sysCtx, payload, true, sysActor); err != nil {
			t.Fatalf("the SeedMaterializer must be able to re-materialize a predefined role; got %v", err)
		}
	})

	t.Run("creating a predefined role as a system actor passes", func(t *testing.T) {
		payload := map[string]any{"slug": "it-support", "name": "IT Support", "predefined": true}
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(sysCtx, payload, false, sysActor); err != nil {
			t.Fatalf("the SeedMaterializer must be able to materialize a new catalog row; got %v", err)
		}
	})
}
