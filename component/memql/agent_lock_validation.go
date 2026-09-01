package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/num"
)

// conceptAgentsAgent / conceptAgentsAgentRole are the canonical
// concept ids the validator queries against. Defined locally rather
// than in component/database/memory-nodes/concept_ids.go because the
// agent concept doesn't (yet) carry a Go-side constant -- one
// well-named local string keeps the validator self-contained and
// avoids broadening that file for a single validator's use.
const (
	conceptAgentsAgent     = "v1:agents:agent"
	conceptAgentsAgentRole = "v1:agents:agentRole"
)

// validateAgentLockedItems runs the lock-enforcement pre-insert guard
// on v1:agents:agent writes. The contract:
//
//   - If the proposed payload doesn't touch capabilities, no-op.
//     Updates that change name / personality / providerConfig only
//     can't violate locks; we skip the role lookup entirely.
//
//   - If capabilities IS being written, resolve the agent's roleSlug
//     (prefer payload.roleSlug for inserts; fall back to the latest
//     existing row for partial updates) and look up its
//     v1:agents:agentRole row. Every id in role.lockedDomainIds MUST
//     be present in proposed capabilities.domains; every slug in
//     role.lockedToolSlugs MUST be present in proposed capabilities.tools.
//
//   - Rejected writes return a typed error message naming the role,
//     the kind of lock violated, and the specific missing ids. The
//     caller (the cockpit, the product SPA, a tool extending an agent)
//     gets enough context to fix the payload.
//
// The guard runs on every agents:agent write -- inserts AND updates --
// because updateAgent's `update agent` shape lands in the
// same pre-insert path with the fully-merged payload. The role catalog
// is the single source of truth: bumping a role's lockedDomainIds at
// seed time means every existing agent of that role gets the new lock
// enforced on its NEXT mutation.
//
// User-created (non-predefined) roles can carry empty locked* lists
// and behave as fully-editable agents -- the validator's "no role
// found" / "empty locked lists" paths both succeed silently.
func (e *MemQLEngine) validateAgentLockedItems(ctx context.Context, payload map[string]any, mutationId, actor string) error {
	if payload == nil {
		return nil
	}

	// Short-circuit: no capabilities in the payload means no possible
	// lock violation. Inserts that don't carry capabilities (rare; the
	// agent CRUD always does) and updates that touch unrelated fields
	// (the common case) both exit here in O(1).
	rawCaps, capsPresent := payload["capabilities"]
	if !capsPresent {
		return nil
	}
	caps, ok := rawCaps.(map[string]any)
	if !ok {
		// A non-object capabilities is a payload error caught
		// elsewhere; lock enforcement has nothing to say about it.
		return nil
	}

	roleSlug, err := e.resolveAgentRoleSlug(ctx, payload, mutationId)
	if err != nil {
		return fmt.Errorf("v1:agents:agent: resolve roleSlug for lock check: %w", err)
	}
	if roleSlug == "" {
		// Legacy agents and pre-seed-migration rows may carry no
		// roleSlug. Without a role we have no locked set; pass through.
		return nil
	}

	role, err := e.fetchAgentRole(ctx, roleSlug)
	if err != nil {
		return fmt.Errorf("v1:agents:agent: fetch role %q: %w", roleSlug, err)
	}
	if role == nil {
		// User-created roles or stale predefined ids won't resolve;
		// silently allow the write. The alternative -- reject any
		// write whose roleSlug doesn't have a catalog row -- would
		// brick agents whose role row was retired but whose agent row
		// still exists, and that's a worse failure mode than a missing
		// lock check.
		return nil
	}

	proposedSkills := stringSliceFromCapabilitiesField(caps, "skillIds")
	proposedSkillSet := setFromSlice(proposedSkills)

	// Locked skills can't be stripped.
	if missing := missingFrom(proposedSkillSet, role.lockedSkillIds); len(missing) > 0 {
		return fmt.Errorf(
			"v1:agents:agent: role %q requires locked skills that the update would remove: %s. "+
				"Locked skills cannot be removed; add them back to capabilities.skillIds or change roleSlug.",
			roleSlug, strings.Join(missing, ", "),
		)
	}

	// Forbidden skills can never be granted -- hard denylist regardless
	// of source.
	if forbidden := overlap(proposedSkillSet, role.forbiddenSkillIds); len(forbidden) > 0 {
		return fmt.Errorf(
			"v1:agents:agent: role %q forbids these skills: %s. "+
				"Remove them from capabilities.skillIds.",
			roleSlug, strings.Join(forbidden, ", "),
		)
	}

	// Skill count cap: lower of agent.skillBudgetMax and role.maxSkills.
	effectiveCap := role.maxSkills
	if cap := intFromCapabilitiesField(caps, "skillBudgetMax"); cap > 0 && cap < effectiveCap {
		effectiveCap = cap
	}
	if effectiveCap > 0 && len(proposedSkills) > effectiveCap {
		return fmt.Errorf(
			"v1:agents:agent: role %q caps skillIds at %d (effective cap from "+
				"min(skillBudgetMax, role.maxSkills)); proposed payload carries %d.",
			roleSlug, effectiveCap, len(proposedSkills),
		)
	}
	return nil
}

// agentRoleLockSet is the minimal projection of a v1:agents:agentRole
// row needed by the lock validator. Unmarshaled from the row's
// payload column. Phase 2 (#158): the skill-id surface replaces the
// old per-domain / per-tool / per-liveKnowledge locked lists.
type agentRoleLockSet struct {
	lockedSkillIds    []string
	forbiddenSkillIds []string
	maxSkills         int
}

// resolveAgentRoleSlug returns the roleSlug the validator should look
// up. Preference order:
//
//  1. payload["roleSlug"] -- the caller is changing the role (rare;
//     valid for either insert or update).
//  2. The latest existing v1:agents:agent row at this mutation id --
//     the common case for updates where roleSlug is unchanged.
//
// Returns "" when neither path yields a value; the caller treats that
// as "no enforcement available, pass through."
// staged-data: MUST-NOT-GATE -- this resolves WHICH role lock to enforce, and
// gated it resolves to the wrong one (epic memql#3974, task memql#3984).
//
// A miss returns "" rather than an error, so a staged role row does not stop
// the validation -- it makes the validation run against no role at all, or
// against whatever the caller supplied. Read together with fetchAgentRole
// below: this one picks the lock, that one reads it, and both fail toward
// LESS enforcement.
func (e *MemQLEngine) resolveAgentRoleSlug(ctx context.Context, payload map[string]any, mutationId string) (string, error) {
	if slug, ok := payload["roleSlug"].(string); ok {
		if s := strings.TrimSpace(slug); s != "" {
			return s, nil
		}
	}
	if mutationId == "" {
		return "", nil
	}
	db := e.database()
	if db == nil {
		return "", nil
	}
	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("concept = ?", conceptAgentsAgent).
		Where("id = ?", strings.TrimSpace(mutationId)).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("scan agent %q: %w", mutationId, err)
	}
	var existing map[string]any
	if jerr := json.Unmarshal(node.Payload, &existing); jerr != nil {
		return "", fmt.Errorf("unmarshal agent payload: %w", jerr)
	}
	if slug, ok := existing["roleSlug"].(string); ok {
		return strings.TrimSpace(slug), nil
	}
	return "", nil
}

// fetchAgentRole reads the v1:agents:agentRole row keyed by slug and
// extracts its lockedDomainIds / lockedToolSlugs into a minimal struct.
// Returns (nil, nil) when no row exists; (nil, err) for hard scan
// failures. Optimization point: the catalog is read-mostly + seeded
// once at startup -- a small in-engine cache would cut this query
// out of the hot mutation path. Held as a follow-up; v1 keeps the
// validator self-contained.
// staged-data: MUST-NOT-GATE -- SECURITY, and it fails OPEN (epic memql#3974,
// task memql#3984).
//
// This reads a role's lockedDomainIds / lockedToolSlugs -- the set an agent
// bound to that role may NOT be given. Not-found returns (nil, nil), which the
// caller reads as "no lock", so gating this hands an agent bound to a staged
// role exactly the tools the lock exists to withhold. Hiding the ROLE row does
// not hide the agent; it removes the restriction on it.
//
// The general shape, worth stating once: an authorization input is not a read
// the tier is talking about. Nothing here reaches a reader -- the rows are
// consumed to make a decision -- so withholding them cannot protect anyone and
// can only corrupt the decision.
func (e *MemQLEngine) fetchAgentRole(ctx context.Context, slug string) (*agentRoleLockSet, error) {
	db := e.database()
	if db == nil {
		return nil, nil
	}
	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("concept = ?", conceptAgentsAgentRole).
		Where("id = ?", slug).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan agentRole %q: %w", slug, err)
	}
	var rolePayload map[string]any
	if jerr := json.Unmarshal(node.Payload, &rolePayload); jerr != nil {
		return nil, fmt.Errorf("unmarshal agentRole payload: %w", jerr)
	}
	return &agentRoleLockSet{
		lockedSkillIds:    stringSliceFromAny(rolePayload["lockedSkillIds"]),
		forbiddenSkillIds: stringSliceFromAny(rolePayload["forbiddenSkillIds"]),
		maxSkills:         intFromAny(rolePayload["maxSkills"]),
	}, nil
}

// stringSliceFromCapabilitiesField returns capabilities[key] as a
// []string, tolerant of nil / wrong shape (returns nil). Used by the
// skill-id lock validator to read the proposed capabilities.skillIds
// list with its element order preserved (the cap check counts unique
// elements; the lock check uses the set form below).
func stringSliceFromCapabilitiesField(caps map[string]any, key string) []string {
	raw, ok := caps[key]
	if !ok {
		return nil
	}
	return stringSliceFromAny(raw)
}

// intFromCapabilitiesField returns capabilities[key] as an int,
// tolerant of float64 (JSON unmarshal default) / int / int64 shapes.
// Returns 0 when missing or unparseable.
func intFromCapabilitiesField(caps map[string]any, key string) int {
	raw, ok := caps[key]
	if !ok {
		return 0
	}
	return intFromAny(raw)
}

// intFromAny coerces a JSON-unmarshaled numeric to int. JSON numbers
// land as float64; typed callers may pass int / int64. Returns 0 for
// missing / unparseable inputs.
// intFromAny reads a capability payload's numeric field.
//
// SATURATES out of range (memql#4779), and this is the exact twin of the site
// memql#4778 fixed: `maxSkills` on a role, plus `maxHumans` on a space. Both
// are CAPS read through `> 0` guards, so the two other answers each state
// something false -- zero says "this role may hold no skills at all", and a
// bare conversion inverts the ranking outright, presenting the least
// restrictive role in the catalog as the most.
func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return num.ClampFloat64(x)
	case int:
		return x
	case int64:
		return num.ClampInt64(x)
	case int32:
		return int(x)
	}
	return 0
}

// setFromSlice turns a slice into a presence set for O(1) membership
// checks. Skips empty / whitespace-only ids defensively.
func setFromSlice(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

// overlap returns the elements of forbidden that ARE present in have,
// in stable order, deduped. Inverse of missingFrom.
func overlap(have map[string]struct{}, forbidden []string) []string {
	if len(forbidden) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(forbidden))
	var out []string
	for _, id := range forbidden {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, present := have[id]; present {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// missingFrom returns the elements of required not present in have,
// in stable order, deduped. Used to render the "missing ids" portion
// of the rejection error so callers see exactly which locks tripped.
func missingFrom(have map[string]struct{}, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(required))
	var out []string
	for _, id := range required {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, present := have[id]; !present {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// stringSliceFromAny flattens the assorted shapes that []string can
// take after a JSON unmarshal (a []any, a []string when typed
// upstream, a single string degenerate case) into a []string. Used
// by both the agent payload reader and the role payload reader.
func stringSliceFromAny(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}
