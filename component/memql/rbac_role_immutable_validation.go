package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptRbacRole is the canonical concept id of the RBAC role catalog.
// Mirrors the concept declared at dsl/rbac/concepts.memql:role. Named here
// so the immutability guard can branch on it without inlining the literal.
const conceptRbacRole = "v1:rbac:role"

// validateRbacBaseRoleImmutable is the E1.2 (memql#2070) runtime-immutability
// guard for the base RBAC roles. It rejects any NON-system-actor write to a
// predefined v1:rbac:role row, so a base role (owner / developer / admin /
// user) can never be redefined, re-ranked, renamed, or deactivated AS DATA at
// runtime -- the escalation-by-redefinition block the hybrid-storage decision
// calls for. Base roles live in the DSL (dsl/rbac/seeds.memql) and are the
// only authority on their rank + identity; runtime mutation is reserved for
// custom (predefined=false) roles.
//
// Why this catches both create-overwrite AND update: the write path
// (executeWrite) read-merges the supplied delta on top of the persisted row
// BEFORE this guard runs, so when the target id already names a base-role row
// the merged payload carries predefined=true even if the caller omitted the
// field (or tried to flip it to false -- the merge takes the prior value for
// fields the caller restates, and a caller flipping predefined to false still
// fails because the PRIOR row was predefined; see the prior-row check below).
//
// Contract:
//
//   - merged payload predefined != true: pass (a normal custom-role write).
//   - merged payload predefined == true: pass ONLY for a system actor
//     (UserIdentity.Role=="system" OR actor begins with "system:"). The
//     SeedMaterializer (system:seedMaterializer) re-materializes the base
//     roles on every boot under a system actor, so idempotent re-seed is
//     unaffected; every user / admin / owner runtime edit is rejected.
//
// The prior-row predefined value is passed in by the caller (executeWrite has
// it from the read-merge) so a caller cannot bypass the guard by flipping
// predefined to false in the same write: if the row WAS predefined, the write
// is gated regardless of the new value.
//
// Companion to validateAgentKindActorScope (same hook point in
// executeWrite); both are pre-insert actor-scope guards that keep the DSL
// grammar small by sitting above the row write rather than encoding per-arg
// actor preconditions. Closes part of memql#2070.
func (e *MemQLEngine) validateRbacBaseRoleImmutable(ctx context.Context, payload map[string]any, priorPredefined bool, actor string) error {
	if payload == nil {
		return nil
	}

	merged := boolFromAny(payload["predefined"])
	if !merged && !priorPredefined {
		// Neither the proposed nor the persisted row is a base role.
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	slug := strings.TrimSpace(stringFromAny(payload["slug"]))
	return fmt.Errorf(
		"v1:rbac:role: base role %q is immutable at runtime -- predefined roles are authored in dsl/rbac/seeds.memql and "+
			"may only be (re)materialized by a system actor (the SeedMaterializer). Runtime edits are reserved for custom "+
			"(predefined=false) roles created through the rank-bounded createRole path. See dsl/rbac/concepts.memql:role.",
		slug,
	)
}

// boolFromAny coerces a JSON-decoded value to bool. Treats a missing /
// non-bool value as false (the safe default for the predefined gate).
func boolFromAny(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "true")
	default:
		return false
	}
}
