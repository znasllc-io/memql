package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// last_owner_deletion_validation.go -- memql#3967.
//
// # THE DEFECT
//
// Nothing counted owners on the deletion path. `accountDeletionSweep` runs
// daily and calls `deleteUserHard` unconditionally for every user whose
// 30-day cooldown has elapsed (dsl/identity/logic.memql: "the automation's
// forEach hard-deletes each unconditionally"). If the last owner was among
// them, the cluster woke up with no owner at all -- and no owner means no
// route back to owner, because promoting somebody requires an owner.
//
// THE GUARDS THAT EXIST DO NOT COVER THIS, and it is worth saying which ones,
// because from a distance they look like they would. `CanDeleteUser`
// (component/auth/rbac.go) refuses an owner deleting THEMSELVES, and
// `GovernPrincipal` (component/auth/rbac_governance.go) refuses an owner
// DEMOTING themselves. Both are self-action guards on the HTTP surface, both
// compare the actor against the target, and neither counts how many owners
// remain. The sweep is a cron under a system actor with no human in it at all,
// so neither is even on its path.
//
// # WHY IT IS LOAD-BEARING FOR THE RECOVERY KEY
//
// A recovery key is bound to one owner (memql#3964). Deleting that owner
// leaves a key that can never be redeemed -- the break-glass gate resolves a
// user that is soft-deleted and PII-scrubbed. So the credential this epic adds
// would be silently voidable by an unrelated cron, which is the worst kind of
// broken: nothing fails, nothing logs, and the key stops working on a schedule.
//
// # WHY THE GUARD LIVES HERE
//
// At the row write, mirroring validateIdentityCredentialActorScope and
// validateAgentRolePredefinedLock. That placement covers the sweep AND a raw
// `mutation deleteUserHard(...)` over ExecuteQuery, which a DSL-side filter
// could not: the mutation takes the target id as a caller argument.
//
// # WHAT IT REFUSES, PRECISELY
//
// A write that would leave the cluster with ZERO active owners. That is
// narrower than "refuse deleting an owner": deleting one of two owners is
// fine, and deleting a non-owner is not this guard's business. It is also
// narrower than "refuse any deactivation" -- it fires only when the row being
// deactivated is an owner and is the last one.
//
// It deliberately does NOT consult the actor. A system-actor carve-out is what
// several sibling guards have, and it would defeat this one entirely: the
// sweep IS a system actor, and the sweep is the caller this exists to stop.

const conceptIdentityUser = "v1:identity:user"

// ErrLastOwnerDeletion is returned when a write would remove the final owner.
var ErrLastOwnerDeletion = errors.New("refusing to deactivate the cluster's last owner")

// validateLastOwnerNotDeleted refuses a write that would leave no active owner.
//
// Contract:
//   - payload does not deactivate (no `active`, or `active` true): pass. This
//     guard is about deletion, not about every user write.
//   - the row being deactivated is not an owner: pass.
//   - another active owner remains: pass.
//   - it is the last active owner: refuse.
//
// priorRole is the role read off the prior row during the read-merge, so a
// partial update that deactivates without re-passing `role` is still judged
// correctly. `deleteUserHard` is exactly that shape -- it writes `active:
// false` and nothing else.
func (e *MemQLEngine) validateLastOwnerNotDeleted(ctx context.Context, payload map[string]any, rowID, priorRole string) error {
	if payload == nil {
		return nil
	}
	rawActive, present := payload["active"]
	if !present {
		return nil
	}
	if deactivating, ok := rawActive.(bool); !ok || deactivating {
		// Not a deactivation. A non-bool `active` is a schema problem the
		// validator will report; it is not this guard's to diagnose.
		return nil
	}

	role := strings.ToLower(strings.TrimSpace(stringFromAny(payload["role"])))
	if role == "" {
		role = strings.ToLower(strings.TrimSpace(priorRole))
	}
	if role != "owner" {
		return nil
	}

	remaining, err := e.countOtherActiveOwners(ctx, rowID)
	if err != nil {
		// FAIL CLOSED. A guard whose only job is "do not make the cluster
		// unrecoverable" must never read "I could not tell" as "go ahead" --
		// that is the original defect reached by a different route. A refused
		// deletion is retried by the next day's sweep at no cost; an
		// un-refused one is not undoable.
		return fmt.Errorf("%w: could not count the cluster's remaining owners, so the write is "+
			"refused rather than risked: %v", ErrLastOwnerDeletion, err)
	}
	if remaining > 0 {
		return nil
	}

	return fmt.Errorf(
		"%w: %s is the only active owner, and deactivating them would leave the cluster with none. "+
			"Nothing could then promote a replacement, because promoting an owner requires an owner -- "+
			"and any recovery key bound to this user becomes permanently unredeemable, since the "+
			"break-glass gate resolves a row that is soft-deleted and PII-scrubbed. "+
			"Promote another user to owner first, then retry. See "+
			"component/memql/last_owner_deletion_validation.go and memql#3967.",
		ErrLastOwnerDeletion, rowID,
	)
}

// countOtherActiveOwners counts active owner rows OTHER than excludeID.
//
// Two steps for the reason validateAgentRoleSlugUnique documents: rows are
// versioned, so "has ever been an owner" and "is an owner now" are different
// questions and only the LATEST version of each id answers the second.
//
// staged-data: MUST-NOT-GATE -- withholding rows here converts "I cannot see
// every owner" into a confident "there are no other owners", and the caller
// treats that as definitive: `remaining > 0` is the entire gate, so an
// under-count refuses a legitimate deactivation for a reason no operator can
// reproduce (they can SEE the other owner; the count cannot). That is the exact
// conflation the error path 20 lines above exists to refuse -- it returns rather
// than guessing precisely because "could not tell" must never read as an answer,
// and a staged predicate would smuggle an unreadable row back in wearing a
// definite one's clothes. The count must be over EVERY owner row or it is not
// the question being asked. Same reasoning as countConceptRows
// (authoring_concept_retire.go), which reads storage under no actor for the
// same reason: a decision counter that scopes its own input answers "how many
// can I see" when the caller asked "how many are there". Note also that
// v1:identity:user is a CORE concept and cannot carry the staged marker at all
// -- the marker is only ever set by the durable promote path -- so gating here
// would be inert in production AND wrong in principle.
func (e *MemQLEngine) countOtherActiveOwners(ctx context.Context, excludeID string) (int, error) {
	db := e.database()
	if db == nil {
		return 0, fmt.Errorf("memory engine database not configured")
	}

	var candidates []string
	err := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Column("id").
		Where("concept = ?", conceptIdentityUser).
		Where("payload->>'role' = ?", "owner").
		Distinct().
		Scan(ctx, &candidates)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("scan owner candidates: %w", err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	var nodes []memorynodes.MemoryNode
	err = db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptIdentityUser).
		Where("id IN (?)", bun.In(candidates)).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("scan owner rows: %w", err)
	}

	// Compare BOTH id spellings. Stored ids are canonical
	// (`v1:identity:user:<shortId>`) and the mutation's own id is canonical at
	// this call site -- but a bare id reaching here would make this exclusion
	// silently miss, count the very row being deactivated as "another owner",
	// and PERMIT the deletion. That is failing open on the one guard whose
	// whole job is to fail closed, so the tolerance is cheap insurance rather
	// than defensive noise.
	exclude := strings.TrimSpace(excludeID)
	excludeCanonical := exclude
	if exclude != "" && !strings.HasPrefix(exclude, conceptIdentityUser+":") {
		excludeCanonical = conceptIdentityUser + ":" + exclude
	}
	excludeBare := strings.TrimPrefix(exclude, conceptIdentityUser+":")

	seen := make(map[string]struct{}, len(candidates))
	count := 0
	for i := range nodes {
		n := nodes[i]
		id := strings.TrimSpace(n.ID)
		if id == "" {
			continue
		}
		if id == exclude || id == excludeCanonical || strings.TrimPrefix(id, conceptIdentityUser+":") == excludeBare {
			continue
		}
		if _, done := seen[id]; done {
			continue // an older version of an id already decided
		}
		seen[id] = struct{}{}
		if latestRowIsActiveOwner(n) {
			count++
		}
	}
	return count, nil
}

func latestRowIsActiveOwner(n memorynodes.MemoryNode) bool {
	var payload map[string]any
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		// An unreadable row cannot be counted AS an owner. The caller's
		// fail-closed branch covers a query that errors outright; a single
		// malformed payload is not that, and counting it would be inventing an
		// owner that may not exist.
		return false
	}
	if role := strings.ToLower(strings.TrimSpace(stringFromAny(payload["role"]))); role != "owner" {
		// The LATEST version may have been demoted away from owner, which is
		// exactly why the id-level scan alone is not an answer.
		return false
	}
	// `active` defaults true on the concept, so an absent value is active.
	if raw, present := payload["active"]; present {
		return boolFromAny(raw)
	}
	return true
}
