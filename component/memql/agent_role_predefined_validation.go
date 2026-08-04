package memql

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// The concept id `conceptAgentsAgentRole` is declared alongside its siblings in
// agent_lock_validation.go; this guard branches on the same constant rather
// than introducing a second spelling of the literal.

// agentRolePredefinedEditable is the CLOSED set of fields a non-system actor
// may change on a predefined v1:agents:agentRole row.
//
// It is an ALLOWLIST, not a denylist, and that direction is the point: a field
// added to the concept later is locked by default rather than silently
// editable. A denylist would have to be remembered at every schema change, and
// forgetting is silent -- which is the failure this guard exists to prevent.
//
// The two entries come from the concept's own field documentation, which is
// the authority on what "predefined" means here:
//
//	"Predefined rows are LOCKED in the role manager UI: name / description /
//	 category / locked + forbidden fields + maxSkills are read-only; only tier
//	 and recommendedPolicySlug edits are accepted."
var agentRolePredefinedEditable = map[string]bool{
	"tier":                  true,
	"recommendedPolicySlug": true,
}

// validateAgentRolePredefinedLock is the write-side guard for predefined
// agent-role catalog rows (memql#2985).
//
// # The gap it closes
//
// `predefined` was a UI hint with nothing behind it. The cockpit reads it to
// decide whether to draw the lock icon, and the concept's own doc calls the
// row "LOCKED" -- but no engine guard enforced any of that, so any caller
// could rename, re-categorise, re-skill or deactivate a seeded catalog row
// straight through the mutation surface. The comparable concept `v1:rbac:role`
// has TWO write guards (validateRbacBaseRoleImmutable and
// validateRbacCustomRoleRankBound); `agentRole` had none, and memql#2918 leant
// on the read-side rbac analogy to justify making the catalog `@public`. This
// closes the write-side half of that analogy.
//
// # Why it is NOT a copy of validateRbacBaseRoleImmutable
//
// That guard rejects EVERY non-system write to a predefined row, because a
// base RBAC role is wholly authored in the DSL and runtime mutation is
// reserved for custom roles.
//
// `agentRole` is documented differently and deliberately so: a predefined row
// is PARTIALLY locked. `tier` and `recommendedPolicySlug` are operator-tunable
// per deployment -- a deployment may want a role treated as safety-relevant,
// or pointed at a different AI Router policy, without forking the catalog.
// Copying the rbac guard verbatim would reject those two edits, breaking a
// behaviour the concept documents as supported. So this is a FIELD-scoped
// lock, and the field set is read off the concept's own documentation rather
// than invented here.
//
// # Why it catches the flip-to-false bypass
//
// Same defence as the rbac and healing guards: executeWrite read-merges the
// delta onto the persisted row BEFORE this runs, and the PRIOR `predefined`
// flag is captured before that merge. A caller who sets predefined=false in
// the same delta that renames the row is still gated, because the row WAS
// predefined. `predefined` is itself outside the editable set, so demoting a
// catalog row to a user role is rejected on its own.
//
// # System-actor carve-out
//
// The SeedMaterializer re-materializes the catalog from dsl/agents/roles/*.memql
// on every startup under a system actor, so idempotent re-seed is unaffected --
// exactly as it is for the rbac and healing guards.
//
// prior is the snapshot of the persisted row's locked fields taken before the
// read-merge; nil means the row did not exist (a create), where there is no
// prior value to protect and only the predefined-flag rule applies.
func (e *MemQLEngine) validateAgentRolePredefinedLock(
	ctx context.Context,
	payload map[string]any,
	prior map[string]any,
	priorPredefined bool,
	actor string,
) error {
	if payload == nil {
		return nil
	}

	merged := boolFromAny(payload["predefined"])
	if !merged && !priorPredefined {
		// Neither the proposed nor the persisted row is a catalog row: an
		// ordinary user-created role, fully editable by design.
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	slug := strings.TrimSpace(stringFromAny(payload["slug"]))
	if slug == "" {
		slug = strings.TrimSpace(stringFromAny(prior["slug"]))
	}

	// A non-system actor MINTING a predefined row is rejected outright: the
	// catalog is authored in dsl/agents/roles/*.memql and materialized by the
	// SeedMaterializer. Without this, the field-scoped check below would let a
	// caller create a brand-new row with predefined=true (no prior row, so no
	// field "changed") and pass.
	if !priorPredefined {
		return fmt.Errorf(
			"v1:agents:agentRole: cannot create a predefined role (%q) as a non-system actor -- "+
				"the predefined catalog is authored in dsl/agents/roles/*.memql and materialized by "+
				"the SeedMaterializer. Create a user role with predefined=false, which stays fully "+
				"editable. See dsl/agents/concepts.memql:agentRole.predefined (memql#2985).",
			slug)
	}

	var changed []string
	for field, next := range payload {
		if agentRolePredefinedEditable[field] {
			continue
		}
		if !reflect.DeepEqual(next, prior[field]) {
			changed = append(changed, field)
		}
	}
	// A field the caller DROPPED cannot appear in the loop above, because the
	// read-merge inherits omitted fields from the prior row -- so a drop is
	// invisible by construction and needs no separate check. A field the
	// caller cleared to an empty value DOES appear, because the merge takes
	// the restated value.
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)

	editable := make([]string, 0, len(agentRolePredefinedEditable))
	for f := range agentRolePredefinedEditable {
		editable = append(editable, f)
	}
	sort.Strings(editable)

	return fmt.Errorf(
		"v1:agents:agentRole: predefined role %q is locked -- a non-system actor may not change %s. "+
			"Predefined rows are seeded from dsl/agents/roles/*.memql on every startup, so an edit here "+
			"is silently reverted on the next boot even where it is accepted; the role manager renders "+
			"them read-only for the same reason. Only %s are operator-tunable per deployment. To change "+
			"anything else, edit the authored role and let the SeedMaterializer re-materialize it, or "+
			"create a user role (predefined=false) which stays fully editable. "+
			"See dsl/agents/concepts.memql:agentRole.predefined (memql#2985).",
		slug, strings.Join(changed, ", "), strings.Join(editable, " and "))
}

// snapshotAgentRoleLockedFields copies the prior row's fields so the guard can
// compare against them AFTER the read-merge has overwritten the live map.
//
// A copy is required, not a reference: executeWrite merges the delta INTO the
// prior map and then uses that same map as the payload (`payload = priorPayload`),
// so holding the map would compare the merged row against itself and report
// nothing ever changed -- a guard that is present, green, and inert.
//
// Every field is copied rather than just the locked ones: the locked set is
// "everything not in the editable allowlist", so it grows with the concept and
// a fixed list here would silently stop covering new fields.
func snapshotAgentRoleLockedFields(prior map[string]any) map[string]any {
	if prior == nil {
		return nil
	}
	out := make(map[string]any, len(prior))
	for k, v := range prior {
		out[k] = v
	}
	return out
}
