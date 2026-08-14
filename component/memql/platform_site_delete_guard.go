package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptPlatformSite is v1:platform:site's canonical concept id. Mirrors
// the concept declared at dsl/platform/concepts.memql:site. Named here so
// the systemOwned-delete guard can branch on it without inlining the
// literal.
const conceptPlatformSite = "v1:platform:site"

// validateSiteSystemOwnedDelete is the #3717 write guard closing the gap
// Ruling 3 of the Sites-surface brief measured: `systemOwned` was a field in
// the schema with NOTHING enforcing it, and no delete path existed at all.
// The concept's own field doc says "Blocks deletion" -- this is what makes
// that true rather than aspirational.
//
// A systemOwned row is the platform's own portal site (dsl/platform/seeds.memql
// stamps it true, memql#3711): the row every deploy re-seeds at boot, and the
// one an operator deleting it would need a second boot to get back. A UI-only
// block on the delete control is not a block -- anything holding the gRPC
// surface (a raw ExecuteQuery mutation, an SDK caller, a script) can reach the
// deleteSite mutation, or any other write that lands here with deleted:true,
// directly. So the block lives at the write path, same altitude as every
// other systemOwned-style guard in this package (validateRbacBaseRoleImmutable,
// validateAgentRolePredefinedLock).
//
// Contract, mirroring rbac_custom_role_rankbound.go's shape:
//
//   - merged payload deleted != true: pass. Not a delete write (create,
//     publish, status flip -- none of those set deleted).
//   - the PRIOR row was not systemOwned: pass. Nothing here to protect.
//   - the caller is a system actor (UserIdentity.Role=="system" OR actor
//     begins with "system:"): pass. Re-seeding a soft-deleted portal row back
//     to life at boot is the SeedMaterializer's job, not a delete, but the
//     exemption is kept symmetric with every other guard in this file so a
//     future system-actor cleanup path is not blocked by construction.
//   - otherwise: refused.
//
// priorSystemOwned is read from the PRIOR row (captured in executeWrite's
// read-merge, before the delta overwrites it) rather than from the merged
// payload passed in here: deleteSite's own delta never touches systemOwned,
// so the two agree for the mutation this guard exists for, but a raw write
// that tried to smuggle systemOwned:false alongside deleted:true in the same
// call must not slip past a check that only looked at the post-merge value.
func (e *MemQLEngine) validateSiteSystemOwnedDelete(ctx context.Context, payload map[string]any, priorSystemOwned bool, actor string) error {
	if payload == nil {
		return nil
	}
	if !boolFromAny(payload["deleted"]) {
		return nil
	}
	if !priorSystemOwned {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	hostname := strings.TrimSpace(stringFromAny(payload["hostname"]))
	return fmt.Errorf(
		"v1:platform:site: %q is systemOwned and cannot be deleted -- its row is re-seeded at boot "+
			"and deleting it would leave the cluster with no way to manage sites until the next restart. "+
			"See dsl/platform/concepts.memql:site.systemOwned.",
		hostname,
	)
}
