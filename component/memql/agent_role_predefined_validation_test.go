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
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor)
		if err == nil {
			t.Fatal("expected rejection for a user-actor rename of a predefined role; got nil")
		}
		for _, want := range []string{"v1:agents:agentRole", "it-support", "locked", "name"} {
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
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor)
		if err == nil {
			t.Fatal("expected rejection for a user-actor deactivation of a predefined role; got nil")
		}
		if !strings.Contains(err.Error(), "active") {
			t.Errorf("error should name the field that was changed; got %q", err.Error())
		}
	})

	t.Run("re-skilling a predefined role is rejected", func(t *testing.T) {
		// lockedSkillIds is a []any, so this also pins that the comparison is
		// deep rather than by-reference -- a == on two slices does not compile
		// and a shallow check would silently pass every list edit.
		prior := seeded()
		payload := merged(prior, map[string]any{"lockedSkillIds": []any{"skill-a", "skill-b"}})
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor)
		if err == nil {
			t.Fatal("expected rejection for a user-actor edit of lockedSkillIds on a predefined role; got nil")
		}
		if !strings.Contains(err.Error(), "lockedSkillIds") {
			t.Errorf("error should name lockedSkillIds; got %q", err.Error())
		}
	})

	t.Run("flipping predefined to false in the same delta is still rejected", func(t *testing.T) {
		// The bypass every guard in this family defends against: demote the
		// catalog row and edit it in one write. The PRIOR flag gates it, and
		// `predefined` is itself outside the editable set.
		prior := seeded()
		payload := merged(prior, map[string]any{"predefined": false, "name": "Mine Now"})
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor)
		if err == nil {
			t.Fatal("expected rejection: a prior predefined row must stay locked even when the " +
				"delta flips predefined=false")
		}
	})

	// The half that makes this NOT a copy of validateRbacBaseRoleImmutable.
	// The concept documents tier and recommendedPolicySlug as operator-tunable
	// on a predefined row; a whole-row lock would reject an edit the concept
	// says is supported.
	t.Run("tier and recommendedPolicySlug remain editable", func(t *testing.T) {
		for _, tc := range []struct {
			field string
			value any
		}{
			{"tier", "B"},
			{"recommendedPolicySlug", "strongReasoning"},
		} {
			prior := seeded()
			payload := merged(prior, map[string]any{tc.field: tc.value})
			if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor); err != nil {
				t.Errorf("%s must stay editable on a predefined role -- the concept documents it as "+
					"operator-tunable per deployment, and a whole-row lock copied from the rbac "+
					"guard would break it; got %v", tc.field, err)
			}
		}
	})

	t.Run("an unchanged re-write of a predefined role passes", func(t *testing.T) {
		// An idempotent write must not be rejected: the guard gates CHANGES,
		// not the act of writing. A guard that fired here would break every
		// no-op re-materialisation path.
		prior := seeded()
		payload := merged(prior, nil)
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor); err != nil {
			t.Fatalf("an unchanged re-write must pass; got %v", err)
		}
	})

	t.Run("a user-created role is fully editable", func(t *testing.T) {
		prior := seeded()
		prior["predefined"] = false
		prior["slug"] = "my-custom-role"
		payload := merged(prior, map[string]any{"name": "Whatever I Like", "active": false})
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, false, userActor); err != nil {
			t.Fatalf("a predefined=false role must stay fully editable -- that is the whole point of "+
				"the flag; got %v", err)
		}
	})

	t.Run("a non-system actor cannot mint a predefined role", func(t *testing.T) {
		// Without this arm the field-scoped check passes vacuously on a
		// create: there is no prior row, so no field "changed", and a caller
		// could mint a brand-new locked catalog row.
		payload := map[string]any{"slug": "forged", "name": "Forged", "predefined": true}
		err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, nil, false, userActor)
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
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(sysCtx, payload, prior, true, sysActor); err != nil {
			t.Fatalf("the SeedMaterializer must be able to re-materialize a predefined role; got %v", err)
		}
	})

	t.Run("creating a predefined role as a system actor passes", func(t *testing.T) {
		payload := map[string]any{"slug": "it-support", "name": "IT Support", "predefined": true}
		if err := validatorOnNilEngine().validateAgentRolePredefinedLock(sysCtx, payload, nil, false, sysActor); err != nil {
			t.Fatalf("the SeedMaterializer must be able to materialize a new catalog row; got %v", err)
		}
	})
}

// The editable set is an ALLOWLIST, and that direction is the security
// property: a field added to the concept later is locked by default rather
// than silently editable.
//
// Pinned separately because inverting it to a denylist is a natural-looking
// refactor that no other test here would catch -- every case above would stay
// green while every future field became editable.
func TestAgentRolePredefinedEditableSetIsAClosedAllowlist(t *testing.T) {
	if len(agentRolePredefinedEditable) == 0 {
		t.Fatal("the editable set is empty, which makes the guard a whole-row lock and rejects " +
			"the tier / recommendedPolicySlug edits the concept documents as supported")
	}
	for _, f := range []string{"tier", "recommendedPolicySlug"} {
		if !agentRolePredefinedEditable[f] {
			t.Errorf("%q is documented on dsl/agents/concepts.memql:agentRole.predefined as "+
				"editable on a predefined row, but the guard locks it", f)
		}
	}
	// A field NOT in the map must be locked. Asserted through the guard rather
	// than by reading the map, so it fails if the lookup is ever inverted.
	userCtx, userActor := userActorContext()
	prior := map[string]any{"slug": "it-support", "predefined": true, "someNewFieldAddedLater": "before"}
	payload := map[string]any{"slug": "it-support", "predefined": true, "someNewFieldAddedLater": "after"}
	if err := validatorOnNilEngine().validateAgentRolePredefinedLock(userCtx, payload, prior, true, userActor); err == nil {
		t.Error("a field absent from the editable allowlist was accepted on a predefined row. " +
			"The set must be an allowlist so a concept field added later is locked by default; " +
			"as a denylist, every new field ships editable and nothing says so.")
	}
}
