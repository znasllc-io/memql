package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptHealingOverride is the canonical concept id of the self-healing
// two-tier base/overlay store. Mirrors the concept declared at
// dsl/healing/concepts.memql:healedOverride. Named here so the
// immutability guard can branch on it without inlining the literal.
const conceptHealingOverride = "v1:healing:healedOverride"

// validateHealingBaseImmutable is the E4.2 (memql#2140) runtime-immutability
// guard for the BASE tier of the self-healing store. It rejects any
// NON-system-actor write to a tier=base v1:healing:healedOverride row, so the
// immutable base tier (the deploy spine's authored/embedded source of truth,
// never LLM-healed) can never be forged or mutated AS DATA at runtime. This
// is the override-is-data contract: a healed override is always tier=overlay
// data; only a system actor (the SeedMaterializer) may materialize a tier=base
// row.
//
// It is the exact analogue of validateRbacBaseRoleImmutable (the Epic 1 base-
// role guard): same hook point in executeWrite, same prior-row-flag defense
// against flip-to-overlay bypass, same system-actor carve-out. The two-tier
// resolution (resolveValidOverride + Go fallback) prefers a valid overlay
// override and falls back to base; this guard keeps the base tier authoritative
// by making it unforgeable from the runtime data plane.
//
// Why this catches both create-overwrite AND update: the write path
// (executeWrite) read-merges the supplied delta on top of the persisted row
// BEFORE this guard runs, so when the target id already names a base-tier row
// the merged payload carries tier=base even if the caller omitted the field.
// And a caller trying to flip tier to overlay in the same write still fails
// because the PRIOR row was base (see the priorBaseTier check below).
//
// Contract:
//
//   - merged payload tier != "base": pass (a normal overlay write).
//   - merged payload tier == "base": pass ONLY for a system actor
//     (UserIdentity.Role=="system" OR actor begins with "system:"). The
//     SeedMaterializer (system:seedMaterializer) materializes embedded base
//     rows under a system actor, so idempotent re-seed is unaffected; every
//     user / admin / owner runtime write to a base row is rejected.
func (e *MemQLEngine) validateHealingBaseImmutable(ctx context.Context, payload map[string]any, priorBaseTier bool, actor string) error {
	if payload == nil {
		return nil
	}

	merged := isBaseTier(payload["tier"])
	if !merged && !priorBaseTier {
		// Neither the proposed nor the persisted row is a base-tier row.
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	baseConstructId := strings.TrimSpace(stringFromAny(payload["baseConstructId"]))
	return fmt.Errorf(
		"v1:healing:healedOverride: base-tier override for %q is immutable at runtime -- the base tier is the "+
			"authored/embedded deploy spine (never LLM-healed) and may only be materialized by a system actor (the "+
			"SeedMaterializer). Runtime writes must be tier=overlay healed overrides, which become resolution-eligible "+
			"only after human validation (E4.5). See dsl/healing/concepts.memql:healedOverride.",
		baseConstructId,
	)
}

// isBaseTier reports whether a JSON-decoded `tier` value is the base tier.
// Treats a missing / non-string value as NOT base (the safe default for the
// immutability gate -- the concept default is overlay).
func isBaseTier(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s), "base")
}
