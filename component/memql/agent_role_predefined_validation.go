package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// The concept id `conceptAgentsAgentRole` is declared alongside its siblings in
// agent_lock_validation.go; this guard branches on the same constant rather
// than introducing a second spelling of the literal.

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
// # It IS a mirror of validateRbacBaseRoleImmutable, after review
//
// The first version was field-scoped: everything locked EXCEPT `tier` and
// `recommendedPolicySlug`, on the strength of the concept's field doc saying
// "only tier and recommendedPolicySlug edits are accepted". The adversarial
// review of memql#3061 retired that design, on the operator's ruling, for four
// measured reasons:
//
//  1. It was INERT. `prior` arrives JSON-decoded, so its numbers are float64,
//     while the merged payload carries an int64 from the `?? 5` coalesce
//     fallback the mutation renderer parses (mutation_templates.go says so in
//     its own comment). A type-strict compare therefore reported `maxSkills`
//     as changed on a row nobody touched, so a `tier`-only edit -- the single
//     case the carve-out existed to permit -- was rejected anyway. The guard
//     already behaved as a whole-row lock; only the code claimed otherwise.
//  2. The field doc it rested on is explicitly scoped to the UI ("LOCKED in
//     the role manager UI"), i.e. it describes what the cockpit renders
//     read-only, not an engine write contract.
//  3. Nothing exercises the carve-out. znasllc-io/memql-cockpit contains no
//     reference to `agentRole` at all (checked with a control search proving
//     the negative is a real absence), so no client edits these fields.
//  4. `recommendedPolicySlug` is not inert configuration. The agent factory
//     stamps it onto providerConfig.llm.policyName for every agent minted in
//     the role, and the router resolves the provider chain from it -- so
//     keeping it writable would let a non-system caller repoint every future
//     agent under a "locked" role at a different model. Activating the
//     carve-out would have INTRODUCED that exposure, not preserved it.
//
// And the seeder re-materializes predefined rows on every startup, so such an
// edit is reverted at the next boot regardless -- the concept's own field doc
// says as much. A carve-out that is inert, unused, unsurvivable and
// security-bearing is not one worth keeping.
//
// # Contract (identical in shape to the rbac guard)
//
//   - merged payload predefined != true AND the prior row was not predefined:
//     pass. An ordinary user-role write.
//   - otherwise: pass ONLY for a system actor. The SeedMaterializer
//     (system:seedMaterializer) re-materializes the catalog on every boot
//     under a system actor, so idempotent re-seed is unaffected; every other
//     runtime write to a predefined row is rejected.
//
// The prior-row `predefined` value is passed in by the caller (executeWrite has
// it from the read-merge) so a caller cannot bypass the guard by flipping
// `predefined` to false in the same write: if the row WAS predefined, the write
// is gated regardless of the new value. The create case -- minting a brand-new
// row with predefined=true -- is caught by the merged payload carrying the flag.
//
// Reachability worth recording, because it is not obvious: this sits in
// executeWrite, and the raw `insert(` short-circuit (parser.go's
// isInsertFunction) bypasses the mutation template, `args`, `accept` and
// `stamp` but still lands here. So the raw path is covered, unlike the
// template-level protections memql#3059 documents.
func (e *MemQLEngine) validateAgentRolePredefinedLock(ctx context.Context, payload map[string]any, priorPredefined bool, actor string) error {
	if payload == nil {
		return nil
	}

	merged := boolFromAny(payload["predefined"])
	if !merged && !priorPredefined {
		// Neither the proposed nor the persisted row is a catalog row.
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	slug := strings.TrimSpace(stringFromAny(payload["slug"]))
	if !priorPredefined {
		return fmt.Errorf(
			"v1:agents:agentRole: cannot create a predefined role (%q) as a non-system actor -- "+
				"the predefined catalog is authored in dsl/agents/roles/*.memql and materialized by "+
				"the SeedMaterializer. Create a user role with predefined=false, which stays fully "+
				"editable. See dsl/agents/concepts.memql:agentRole.predefined (memql#2985).",
			slug)
	}
	return fmt.Errorf(
		"v1:agents:agentRole: predefined role %q is locked -- a non-system actor may not modify it. "+
			"Predefined rows are seeded from dsl/agents/roles/*.memql on every startup, so an edit "+
			"here is reverted on the next boot even where it is accepted; the role manager renders "+
			"them read-only for the same reason. Edit the authored role and let the SeedMaterializer "+
			"re-materialize it, or create a user role (predefined=false) which stays fully editable. "+
			"See dsl/agents/concepts.memql:agentRole.predefined (memql#2985).",
		slug)
}
