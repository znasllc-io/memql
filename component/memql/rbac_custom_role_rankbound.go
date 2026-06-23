package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// validateRbacCustomRoleRankBound is the E1.4 (memql#2072) rank-bound guard
// for DATA-defined custom roles. A non-system actor that creates or edits a
// custom v1:rbac:role (predefined=false) may only set a rank STRICTLY BELOW
// their own rank -- a creator cannot mint (or re-rank) a role at or above
// their own privilege. This is what lets custom roles slot BETWEEN existing
// ranks (a developer can author a rank-250 "lead" between admin and developer)
// while preventing privilege escalation by self-minting a peer/superior role.
//
// Scope:
//   - predefined roles: out of scope here -- the base-role immutability guard
//     (validateRbacBaseRoleImmutable) already rejects every non-system write to
//     them. This guard returns nil for predefined rows so the two guards don't
//     double-handle the same write.
//   - system actors (the SeedMaterializer): exempt -- they author the base
//     catalog and may write any rank.
//   - the `rank` field absent from the merged payload: pass (an edit that
//     doesn't touch rank can't escalate it; the read-merge carries the prior
//     rank forward, so absence means "unchanged").
//
// The actor's own rank is resolved from the role catalog by the actor's role
// slug (auth context). When the actor's rank cannot be resolved (no catalog
// row for their slug -- e.g. a legacy reader/writer slug before E1.5 maps
// them), the guard FAILS CLOSED: an unresolved actor rank is treated as the
// lowest rank, so they can only mint a role below the lowest base rank, which
// in practice blocks custom-role authoring until their slug resolves. This is
// the safe default for a security gate.
//
// Mirrors auth.CanCreatePrincipal's rank semantics (strictly-below) but for the
// role resource rather than the principal resource. Wired into executeWrite
// beside validateRbacBaseRoleImmutable. Closes part of memql#2072.
func (e *MemQLEngine) validateRbacCustomRoleRankBound(ctx context.Context, payload map[string]any, priorPredefined bool, actor string) error {
	if payload == nil {
		return nil
	}
	// Predefined rows are handled by the immutability guard, not here.
	if boolFromAny(payload["predefined"]) || priorPredefined {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	// rank absent => the write doesn't set/change rank => nothing to bound.
	rawRank, present := payload["rank"]
	if !present {
		return nil
	}
	newRank, ok := intWithOkFromAny(rawRank)
	if !ok {
		// A non-numeric rank is a malformed write; the concept schema check
		// will reject it downstream. Don't double-error.
		return nil
	}

	actorRank, actorIsOwner := e.resolveActorRank(ctx, identity)

	if customRoleRankBoundOK(actorRank, newRank, actorIsOwner) {
		return nil
	}

	slug := strings.TrimSpace(stringFromAny(payload["slug"]))
	return fmt.Errorf(
		"v1:rbac:role: custom role %q rank %d is at or above the creator's own rank %d -- a role may only be created/edited at a rank STRICTLY BELOW the creator's (memql#2072). Choose a lower rank to slot the custom role beneath your own. See dsl/rbac/concepts.memql:role.",
		slug, newRank, actorRank,
	)
}

// customRoleRankBoundOK is the pure rank-bound decision: a custom role's rank
// must be strictly below the actor's own rank. An owner is exempt from the
// strict-below rule only in that they may author at any sub-owner rank; since
// owner holds the maximum base rank, `newRank < actorRank` already permits
// every sub-owner rank for an owner, so no separate owner branch is needed --
// but we keep the flag so a future custom rank authored ABOVE an owner (which
// the model forbids elsewhere) can never be minted by a non-owner.
func customRoleRankBoundOK(actorRank, newRank int, actorIsOwner bool) bool {
	_ = actorIsOwner
	return newRank < actorRank
}

// resolveActorRank resolves the acting principal's numeric rank from the role
// catalog by the actor's role slug, and whether that role is the owner. Fails
// closed: an unresolved slug yields rank 0 (below every base rank) so a
// security gate that bounds "strictly below the actor" denies by default.
func (e *MemQLEngine) resolveActorRank(ctx context.Context, identity auth.UserIdentity) (rank int, isOwner bool) {
	slug := strings.ToLower(strings.TrimSpace(identity.Role))
	if slug == "" {
		return 0, false
	}
	if slug == "owner" {
		isOwner = true
	}
	r, ok := e.lookupRoleRankBySlug(ctx, slug)
	if !ok {
		return 0, isOwner
	}
	return r, isOwner
}

// lookupRoleRankBySlug reads the latest v1:rbac:role row for a slug and returns
// its rank. Returns (0,false) when the catalog has no row for the slug or the
// DB is unavailable.
func (e *MemQLEngine) lookupRoleRankBySlug(ctx context.Context, slug string) (int, bool) {
	db := e.database()
	if db == nil {
		return 0, false
	}
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptRbacRole).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false
		}
		return 0, false
	}
	for _, n := range nodes {
		var p map[string]any
		if jerr := json.Unmarshal(n.Payload, &p); jerr != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(stringFromAny(p["slug"])), slug) {
			if rank, ok := intWithOkFromAny(p["rank"]); ok {
				return rank, true
			}
		}
	}
	return 0, false
}

// intWithOkFromAny coerces a JSON/DSL numeric to int, distinguishing absent /
// non-numeric (ok=false) from a real value -- the rank-bound guard needs that
// distinction to treat a rank-absent write as "unchanged" rather than rank 0.
// (The package's intFromAny returns 0 for both, which would conflate "rank not
// set" with "rank 0".)
func intWithOkFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case int32:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}
