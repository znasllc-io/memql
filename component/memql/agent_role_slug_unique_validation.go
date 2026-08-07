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
	"github.com/znasllc-io/memql/core/id"
)

// validateAgentRoleSlugUnique is the write-side guard that makes
// `v1:agents:agentRole.slug` mean what its own schema says it means
// (memql#3207, carrying memql#3114).
//
// # The gap it closes
//
// The field documents itself as "Canonical slug... stable, never renamed".
// "Canonical" describes a unique key, and there was no unique key: no index, no
// validator, and `createAgentRole` opens
//
//	id: args.agentRoleId ?? args.slug
//
// so a caller supplying an explicit `agentRoleId` alongside a slug a seeded row
// already owns minted a SECOND active row carrying that slug. Nothing refused
// it -- validateAgentRolePredefinedLock keys on the row ID and there is no
// prior row at the new id, and the merged payload's `predefined` is false, so
// by that guard's own contract it is an ordinary user-role write. Both rows
// then appear in `activeAgentRoles`, which is @public and unscoped, so a
// user-facing role picker shows a duplicate whose name the minter chose.
//
// memql#3066 made `findRoleBySlug` prefer the predefined row, which closed the
// branding / prompt / policy substitution at ONE lookup. It left the collision,
// and its preference lives inside that one function -- any other code path
// resolving a role by slug inherits none of it. This closes the collision, so
// that preference is now backed by the invariant rather than standing in for
// it. #3066's preference STAYS: defence in depth, not replaced.
//
// # The decision (the acceptance criterion asks for it to be recorded)
//
// Option 1 of the three the issue lists: a Go validator, wired into
// executeWrite beside its sibling. Not option 3 ("derive the id from the slug
// unconditionally"), for reasons read off the code rather than off taste:
//
//  1. Option 3 cannot remove the `agentRoleId` arg. buildArgsFromBody stamps
//     `args["<concept>Id"]` for every global seed with no opt-out, so the
//     seeder keeps sending it regardless of what the DSL declares -- leaving
//     either a declared-but-ignored arg (a compat shim) or an undeclared one
//     surviving only because the engine path is lenient about unknown args.
//  2. Option 3 is bypassable where this is not. The raw `insert(` short-circuit
//     skips the mutation template entirely, so an `id:` expression in the
//     mutation body never runs; a validator in executeWrite still fires. The
//     sibling predefined-lock guard records the same asymmetry for itself.
//  3. Option 3 does not REFUSE, which the criterion requires. A second write on
//     an existing slug would land on the same id and become a read-merge UPDATE
//     of that row -- silently accepted for a user slug, i.e. an overwrite
//     rather than a rejection.
//
// Option 2 (a load-time check over the seeded catalog) was not taken either: it
// catches seed-vs-seed collisions, and the threat here is a runtime, user-minted
// row.
//
// # What it does NOT buy -- do not let this drift
//
//   - Ownership. `concept agentRole` has no owner field and `createAgentRole`
//     stamps none. A caller supplying NO agentRoleId gets `id = slug`, which for
//     an existing USER role is that row's own id -- self, so uniqueness passes
//     and the write read-merges over someone else's custom role. That hole is
//     pre-existing and orthogonal to uniqueness; it needs an owner column, not a
//     unique key.
//   - Seed-vs-seed collisions. The system actor is carved out, mirroring the
//     sibling guard, so that the seeder's idempotent re-write of ~97 catalog
//     rows on every boot cannot be refused. Two authored seeds claiming one slug
//     is an authoring bug this guard deliberately does not police.
func (e *MemQLEngine) validateAgentRoleSlugUnique(ctx context.Context, payload map[string]any, mutationId, actor string) error {
	if e == nil || payload == nil {
		return nil
	}

	slug := strings.TrimSpace(stringFromAny(payload["slug"]))
	if slug == "" {
		// `slug` is `string!`; the concept schema rejects an empty one
		// downstream. Don't double-error on someone else's rule.
		return nil
	}

	// Only an ACTIVE row can contest the slug: every reader filters
	// isActiveRecord, so an inactive duplicate is invisible to the catalog and
	// a deactivated role must not burn its slug forever.
	//
	// `active` is always present on the named-mutation path (`active:
	// args.active ?? true` in the insert block). On the raw-insert path it may
	// be absent -- and a concept-field @default is NOT applied on insert
	// (memql#2960) -- so an absent value is read as ACTIVE here, which is the
	// fail-closed reading: it means the guard runs rather than being skipped.
	candidateActive := true
	if v, present := payload["active"]; present {
		candidateActive = boolFromAny(v)
	}
	if !candidateActive {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	holders, err := e.activeAgentRoleRowsForSlug(ctx, slug)
	if err != nil {
		// Fail CLOSED. An unavailable database must not be a way to mint the
		// shadow row -- the whole point of the guard is that "we could not
		// check" and "there is nothing to find" are different answers.
		return fmt.Errorf("v1:agents:agentRole: cannot verify slug uniqueness for %q: %w", slug, err)
	}

	conflictId, conflict := agentRoleSlugConflict(mutationId, candidateActive, holders)
	if !conflict {
		return nil
	}
	return fmt.Errorf(
		"v1:agents:agentRole: slug %q is already held by the active role %q -- a slug identifies "+
			"exactly one active role. The concept documents `slug` as canonical and never renamed, "+
			"and `activeAgentRoles` is unscoped, so a second row carrying this slug appears in every "+
			"catalog read alongside the first. Pick a different slug, or deactivate the existing role "+
			"first. See dsl/agents/concepts.memql:agentRole.slug (memql#3207).",
		slug, conflictId)
}

// agentRoleSlugRow is the minimum a uniqueness decision needs about a row that
// currently carries the slug.
type agentRoleSlugRow struct {
	Id     string
	Active bool
}

// canonicalAgentRoleStorageId converts either spelling of a row id into the
// stored, concept-qualified form.
//
// This is load-bearing rather than cosmetic: a mutation carries a BARE id
// (`family-doctor`) while storage holds the qualified one
// (`v1:agents:agentRole:family-doctor`). Compare the two raw and self-exclusion
// never matches, so the seeder's own idempotent re-write of every predefined
// role is refused on the first boot after this guard lands.
func canonicalAgentRoleStorageId(rowId string) string {
	trimmed := strings.TrimSpace(rowId)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, conceptAgentsAgentRole+":") {
		return trimmed
	}
	return id.BuildNodeId(conceptAgentsAgentRole, trimmed)
}

// agentRoleSlugConflict reports whether writing candidateId would put a second
// ACTIVE row under a slug that `holders` shows is already taken, and by whom.
//
// Pure so the rule can be tested without a database; the scan that produces
// `holders` is separate.
func agentRoleSlugConflict(candidateId string, candidateActive bool, holders []agentRoleSlugRow) (string, bool) {
	if !candidateActive {
		return "", false
	}
	self := canonicalAgentRoleStorageId(candidateId)
	for _, h := range holders {
		if !h.Active {
			continue
		}
		if canonicalAgentRoleStorageId(h.Id) == self {
			continue
		}
		return h.Id, true
	}
	return "", false
}

// activeAgentRoleRowsForSlug returns the rows whose LATEST version carries this
// slug, with that version's active flag.
//
// Two steps, deliberately. The store is time-series -- one logical row is many
// physical versions -- so a single `WHERE payload->>'slug' = ?` matches any
// version that EVER carried the slug, including one that has since been renamed
// away from it or deactivated. That would refuse writes on a slug nothing
// currently holds. So: scan the concept newest-first, keep the first version
// seen per id (that is the latest), and only then ask whether it carries the
// slug and is active.
func (e *MemQLEngine) activeAgentRoleRowsForSlug(ctx context.Context, slug string) ([]agentRoleSlugRow, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptAgentsAgentRole).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan agent roles: %w", err)
	}

	seen := make(map[string]struct{}, len(nodes))
	var out []agentRoleSlugRow
	for _, node := range nodes {
		if _, dup := seen[node.ID]; dup {
			continue // an older version of a row already resolved
		}
		seen[node.ID] = struct{}{}

		var payload map[string]any
		if jerr := json.Unmarshal(node.Payload, &payload); jerr != nil {
			continue
		}
		if strings.TrimSpace(stringFromAny(payload["slug"])) != slug {
			continue
		}
		active := true
		if v, present := payload["active"]; present {
			active = boolFromAny(v)
		}
		if !active {
			continue
		}
		out = append(out, agentRoleSlugRow{Id: node.ID, Active: true})
	}
	return out, nil
}
