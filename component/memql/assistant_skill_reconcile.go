package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/provenance"
)

// assistant_skill_reconcile.go -- boot-time, system-managed reconcile of
// the per-user Assistant's capabilities.skillIds (memql#1443).
//
// WHY THIS EXISTS
//
// createAgent is create-only: the SeedMaterializer's per-user
// sweep inserts `assistant-<userId>` once and is a no-op on every later
// boot (it dedups on the deterministic id). So editing the assistant
// seed's skillIds in dsl/agents/assistant.memql only affects users
// provisioned AFTER the change -- every EXISTING assistant row (incl.
// the owner's staging assistant) keeps the skill set it was minted with.
// Adding the todos/notes/calendar skills (#1443) would therefore never
// reach existing users without a backfill.
//
// This reconcile is that backfill, modeled as an idempotent, self-healing
// boot-time step rather than a one-shot env-gated migration (cf.
// MigrateAgentsToSkills): it runs on every cluster boot AND on the
// runtime user-created hook, so the canonical skill set converges with
// no operator action. The canonical set is read from the live assistant
// SeedDefinition (single source of truth) -- this file never hardcodes a
// skill list, so future seed edits flow through automatically.
//
// MERGE, NOT CLOBBER
//
// The reconcile UNIONS the canonical skillIds into each row's existing
// capabilities.skillIds; it never removes a skill the user (or planner)
// added, and it touches nothing else on the row -- name, avatar,
// personality, providerConfig and any other user-editable field are
// preserved (the write is a partial updateAgent carrying only
// {capabilities: {...}}). A row already carrying every canonical skill
// is skipped (no redundant version written).
//
// LOCK CAP INTERACTION
//
// validateAgentLockedItems (agent_lock_validation.go) enforces
// assistantRole.maxSkills on the update path. The canonical set is 10
// skillIds; the role cap was bumped to 12 (dsl/agents/roles/
// professional.memql) so both the seed create and this reconcile are
// admitted. If a row already carries user-added skills that, unioned
// with the canonical set, would exceed the cap, the update is rejected
// server-side and recorded as an error in the report -- the reconcile
// does not silently drop the row's existing skills to fit.

// AssistantSkillReconcileReport summarises one reconcile pass.
type AssistantSkillReconcileReport struct {
	Scanned   int      // assistant rows examined (latest version per id)
	Updated   int      // rows that gained at least one canonical skill
	AlreadyOK int      // rows already carrying the full canonical set
	Skipped   int      // rows not eligible (e.g. missing capabilities)
	Errors    []string // per-row failures (does not abort the pass)
}

// reconcileAssistantSkills walks every v1:agents:agent row whose roleSlug
// is "assistant" (the per-user Assistant, id pattern `assistant-<userId>`)
// and merges the canonical skillIds from the live `assistant` seed into
// each row's capabilities.skillIds. Idempotent and merge-only; see the
// file header for the full contract. Best-effort: a per-row failure is
// logged + recorded and the pass continues.
func (m *SeedMaterializer) reconcileAssistantSkills(ctx context.Context) (AssistantSkillReconcileReport, error) {
	report := AssistantSkillReconcileReport{}
	if m == nil || m.engine == nil {
		return report, fmt.Errorf("assistant skill reconcile: nil engine")
	}
	logger := m.engine.Logger

	canonical := m.canonicalAssistantSkillIds()
	if len(canonical) == 0 {
		// No assistant seed (or it carries no skillIds) -- nothing to
		// reconcile against. Not an error: a DSL tree without the
		// assistant seed is a valid (if unusual) configuration.
		if logger != nil {
			logger.Info("assistant skill reconcile: no canonical skillIds from the assistant seed; skipping")
		}
		return report, nil
	}

	db := m.engine.database()
	if db == nil {
		return report, fmt.Errorf("assistant skill reconcile: no database")
	}

	var rows []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&rows).
		Where("concept = ?", conceptAgentsAgent).
		OrderExpr(`"id" ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return report, fmt.Errorf("assistant skill reconcile: scan agents: %w", err)
	}

	seen := map[string]struct{}{}
	for _, node := range rows {
		if _, dup := seen[node.ID]; dup {
			continue // keep only the latest version per agent id
		}
		seen[node.ID] = struct{}{}

		var payload map[string]any
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			report.Errors = append(report.Errors,
				fmt.Sprintf("agent %s: payload decode: %v", node.ID, err))
			continue
		}
		if !isAssistantAgentPayload(payload) {
			continue // not a per-user Assistant row
		}
		report.Scanned++

		caps, ok := payload["capabilities"].(map[string]any)
		if !ok || caps == nil {
			// An assistant row with no capabilities block is malformed;
			// skip rather than synthesize one (a create path bug we
			// don't want to mask here).
			report.Skipped++
			continue
		}

		existing := stringSliceFromAny(caps["skillIds"])
		merged, added := unionPreservingOrder(existing, canonical)
		if len(added) == 0 {
			report.AlreadyOK++
			continue
		}

		// Build the FULL capabilities object with only skillIds changed.
		// updateAgent has no @mergeFields, so its `update`
		// contract is top-level REPLACE: a partial {capabilities:
		// {skillIds}} would wipe every sibling capability flag (avatar /
		// lipSync / vision / voiceToVoice / tools / domains / keywords).
		// We send the prior caps verbatim with skillIds overwritten, so
		// the replace is a no-op for every field except skillIds. Other
		// top-level fields (name, avatar persona, personality, ...) are
		// left out of the payload entirely and inherit from the prior
		// row. Copying into a fresh map keeps the decoded row untouched.
		newCaps := make(map[string]any, len(caps))
		for k, v := range caps {
			newCaps[k] = v
		}
		newCaps["skillIds"] = toAnySlice(merged)

		if err := m.writeReconciledAssistantSkills(ctx, node.ID, newCaps); err != nil {
			report.Errors = append(report.Errors,
				fmt.Sprintf("agent %s: update: %v", node.ID, err))
			continue
		}
		report.Updated++
		if logger != nil {
			logger.Info("assistant skill reconcile: backfilled skillIds",
				"agentId", node.ID, "added", added, "total", len(merged))
		}
	}

	if logger != nil {
		logger.Info("assistant skill reconcile: complete",
			"scanned", report.Scanned,
			"updated", report.Updated,
			"alreadyOK", report.AlreadyOK,
			"skipped", report.Skipped,
			"errors", len(report.Errors),
			"canonical", canonical)
	}
	return report, nil
}

// canonicalAssistantSkillIds reads capabilities.skillIds off the live
// `assistant` SeedDefinition -- the single source of truth. Returns nil
// when the seed (or its skillIds) is absent. The seed body is rendered
// through the same buildArgsFromBody path the materializer uses for the
// create, so the shape here matches exactly what a fresh assistant
// carries.
func (m *SeedMaterializer) canonicalAssistantSkillIds() []string {
	if m == nil || m.registry == nil {
		return nil
	}
	def, ok := m.registry.Get("assistant")
	if !ok || def == nil {
		return nil
	}
	// idVal/ownerUserId are irrelevant to skillIds extraction; pass
	// placeholders so the shared builder runs.
	args := buildArgsFromBody(def.Body, def.UseConcept, "assistant-canonical", "canonical")
	caps, ok := args["capabilities"].(map[string]any)
	if !ok || caps == nil {
		return nil
	}
	return stringSliceFromAny(caps["skillIds"])
}

// writeReconciledAssistantSkills issues updateAgent carrying the
// FULL capabilities object (with skillIds merged) as the only changed
// top-level field. Because updateAgent does top-level replace
// (no @mergeFields), the caller MUST pass the complete capabilities map
// -- a partial would wipe sibling flags. Every OTHER top-level field is
// omitted and inherits from the prior row. The system-actor + reconcile
// provenance mirror the migration write path; the lock validator runs
// server-side (the maxSkills cap + locked-skill checks all apply).
func (m *SeedMaterializer) writeReconciledAssistantSkills(ctx context.Context, agentId string, capabilities map[string]any) error {
	args := map[string]any{
		"agentId": agentId,
		"payload": map[string]any{
			"capabilities": capabilities,
		},
	}
	payloadJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	call := fmt.Sprintf(`updateAgent(%s)`, string(payloadJSON))
	ctx = provenance.ContextWithProvenance(systemActorContext(ctx), provenance.System("reconcile:assistant-skillids"))
	_, err = m.engine.Execute(ctx, call)
	return err
}

// isAssistantAgentPayload reports whether a v1:agents:agent payload is a
// per-user Assistant row. The roleSlug "assistant" is the canonical
// classifier; we also accept the legacy `role` field as a fallback for
// rows minted before roleSlug was universally stamped. We deliberately
// do NOT key off the `assistant-` id prefix alone -- a user-renamed
// specialist could in principle collide -- but the id prefix is a cheap
// corroborating signal so we accept either a matching slug/role OR the
// canonical id shape with kind "assistant".
func isAssistantAgentPayload(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	// Exclude soft-deleted rows.
	if deleted, ok := payload["deleted"].(bool); ok && deleted {
		return false
	}
	if slug := strings.TrimSpace(stringFromAny(payload["roleSlug"])); slug == "assistant" {
		return true
	}
	if role := strings.TrimSpace(stringFromAny(payload["role"])); role == "assistant" {
		return true
	}
	// Fallback: canonical id shape + kind. Covers rows that predate
	// roleSlug stamping but still carry kind="assistant".
	id := strings.TrimSpace(stringFromAny(payload["id"]))
	kind := strings.TrimSpace(stringFromAny(payload["kind"]))
	return strings.HasPrefix(id, "assistant-") && kind == "assistant"
}

// unionPreservingOrder returns existing ++ (canonical not already in
// existing), so user-added skills keep their position and the freshly
// added canonical ids append in canonical order. `added` lists only the
// canonical ids that were missing (empty => no write needed). Duplicates
// within existing are de-duped.
func unionPreservingOrder(existing, canonical []string) (merged, added []string) {
	have := map[string]struct{}{}
	merged = make([]string, 0, len(existing)+len(canonical))
	for _, s := range existing {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := have[s]; ok {
			continue
		}
		have[s] = struct{}{}
		merged = append(merged, s)
	}
	for _, s := range canonical {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := have[s]; ok {
			continue
		}
		have[s] = struct{}{}
		merged = append(merged, s)
		added = append(added, s)
	}
	return merged, added
}

// toAnySlice converts a []string into a []any so it round-trips through
// json.Marshal as a JSON array (and lands in the payload as a string
// array rather than a typed slice the downstream decoder might mishandle).
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
