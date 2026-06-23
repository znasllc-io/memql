package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// extractMintRows is the planner's local row-extractor for the
// mint-skill flow. Wraps memql.MaterializeRows so the handler doesn't
// have to thread the same boilerplate through every Execute caller.
func extractMintRows(res any) []map[string]any {
	return memql.MaterializeRows(res)
}

// handleMintSkill is the Phase 3 (memql#159) dispatch path for the
// planner agent's mintSkill action. Three steps:
//
//  1. Catalog-search-before-mint: walk the active skill catalog and
//     reject the mint if any existing row already covers >60% of the
//     requested bundle. Mint is the last resort; near-matches should
//     have come back as extendSpecialist on a prior iteration. When
//     this fires we escalate with a feedbackReason that names the
//     near-match so the operator can re-aim the planner.
//
//  2. Authority gate: load the planner's agentAuthorization rows for
//     the Plan's requestedBy user. The mint is in-envelope when a row
//     carries action='mintSkill' AND (skillTierAllowlist empty OR the
//     requested tier is in the allowlist) AND the row is active +
//     unexpired. Empty allowlist defaults to ["A"] -- B/C require
//     explicit opt-in per the issue body.
//
//     3a. In-envelope: invoke mintSkill with the payload + the
//     Plan / planner provenance. The next planner iteration will
//     see the new catalog row and can dispatch via the standard
//     extendSpecialist path.
//
//     3b. Out-of-envelope: write a v1:copresent:canvasState row of
//     kind='card' / data.variant='mintSkillApprovalRequested',
//     then transition the Plan to awaitingFeedback with
//     feedbackReason='mint_skill_approval_required'. The next turn
//     resumes once the user clicks Approve / Approve-and-allow /
//     Reject on the card and the canvas-action handler stamps the
//     resulting feedbackResponse.
func (l *PlannerAgentLoop) handleMintSkill(ctx context.Context, planId string, d plannerDecision) error {
	if err := validateMintPayload(d); err != nil {
		return l.markPlanFailed(ctx, planId, err.Error())
	}

	plan, err := l.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("mintSkill: load plan %s: %w", planId, err)
	}
	requestedBy := getString(plan, "requestedBy")
	ownerAgentId := getString(plan, "ownerAgentId")
	if requestedBy == "" {
		return l.markPlanFailed(ctx, planId,
			"mintSkill: plan missing requestedBy; cannot resolve mint authority")
	}

	// Step 1: catalog-search heuristic.
	if hit, overlap, err := l.findCatalogOverlap(ctx, d); err != nil {
		// Best-effort: if the lookup fails, log + proceed (the mutation's
		// own validation still gates). The heuristic is a planner-facing
		// quality check, not a hard correctness rule.
		l.logger.Warn("mintSkill: catalog-overlap check failed; proceeding without it",
			"planId", planId, "error", err)
	} else if hit != "" {
		return l.escalateAwaitingFeedback(ctx, planId, "mint_skill_redundant",
			fmt.Sprintf("mintSkill rejected: existing skill %q already covers %.0f%% of the requested bundle. Prefer extendSpecialist with skillId=%q on the next iteration.", hit, overlap*100, hit))
	}

	// Step 2: authority gate.
	allowed, reason, err := l.checkMintAuthority(ctx, ownerAgentId, requestedBy, d.Tier)
	if err != nil {
		return fmt.Errorf("mintSkill: authority check: %w", err)
	}
	if !allowed {
		// Step 3b: surface canvas card + park the Plan.
		if cardErr := l.writeMintApprovalCard(ctx, plan, d); cardErr != nil {
			l.logger.Warn("mintSkill: failed to write approval card",
				"planId", planId, "error", cardErr)
		}
		return l.escalateAwaitingFeedback(ctx, planId, "mint_skill_approval_required",
			fmt.Sprintf("Planner requested mintSkill outside the standing envelope (%s). Approval card surfaced; awaiting user action.", reason))
	}

	// Step 3a: in-envelope -- execute the mutation.
	skillId, err := l.executeMintSkill(ctx, d, planId, ownerAgentId)
	if err != nil {
		return fmt.Errorf("mintSkill: execute mutation: %w", err)
	}
	l.logger.Info("mintSkill: in-envelope mint executed",
		"planId", planId, "skillId", skillId, "tier", d.Tier)

	// Loop back into the planner so it can pick up the new skill on
	// the next iteration via extendSpecialist.
	return l.invokeAndDispatch(ctx, planId)
}

// validateMintPayload checks the mint decision has the minimum fields
// the mutation requires. Catches malformed planner output before any
// authority / catalog lookups run.
func validateMintPayload(d plannerDecision) error {
	missing := []string{}
	if strings.TrimSpace(d.Slug) == "" {
		missing = append(missing, "slug")
	}
	if strings.TrimSpace(d.Name) == "" {
		missing = append(missing, "name")
	}
	if strings.TrimSpace(d.Tier) == "" {
		missing = append(missing, "tier")
	}
	if strings.TrimSpace(d.Justification) == "" {
		missing = append(missing, "justification")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mintSkill decision missing required fields: %s", strings.Join(missing, ", "))
	}
	switch d.Tier {
	case "A", "B", "C":
	default:
		return fmt.Errorf("mintSkill: invalid tier %q (must be A, B, or C)", d.Tier)
	}
	return nil
}

// findCatalogOverlap returns the slug of the first active skill whose
// (domainIds ∪ toolSlugs ∪ liveSourceIds) overlaps >60% with the
// proposed mint, plus the overlap fraction. Returns ("", 0, nil) on
// no hit. Per the planner prompt + issue body: prefer
// extendSpecialist with the matching skill over minting a duplicate.
func (l *PlannerAgentLoop) findCatalogOverlap(ctx context.Context, d plannerDecision) (string, float64, error) {
	proposed := unionStringSets(d.DomainIds, d.ToolSlugs, d.LiveSourceIds)
	if len(proposed) == 0 {
		// Nothing to compare against; no overlap possible.
		return "", 0, nil
	}

	res, err := l.engine.Execute(systemActorContext(ctx), `activeSkillsFull({})`)
	if err != nil {
		return "", 0, fmt.Errorf("activeSkillsFull: %w", err)
	}
	for _, row := range extractMintRows(res) {
		existing := unionStringSets(
			stringSliceFromAny(row["domainIds"]),
			stringSliceFromAny(row["toolSlugs"]),
			stringSliceFromAny(row["liveSourceIds"]),
		)
		if len(existing) == 0 {
			continue
		}
		overlap := jaccardOverlap(proposed, existing)
		if overlap > 0.60 {
			slug := getString(row, "slug")
			if slug == "" {
				slug = getString(row, "id")
			}
			return slug, overlap, nil
		}
	}
	return "", 0, nil
}

// checkMintAuthority loads the planner agent's standing
// agentAuthorization grants for the requesting user and returns
// (allowed, humanReadableReason, err). A grant counts when:
//   - action='mintSkill'
//   - active==true
//   - expiresAt unset OR in the future
//   - skillTierAllowlist empty (defaults to ["A"]) OR contains the
//     requested tier
//
// Empty ownerAgentId (rare; a Plan without an assigned planner) means
// no grant possible -> out-of-envelope.
func (l *PlannerAgentLoop) checkMintAuthority(ctx context.Context, ownerAgentId, requestedBy, tier string) (bool, string, error) {
	if ownerAgentId == "" {
		return false, "plan has no ownerAgentId; cannot resolve planner grant", nil
	}
	call := fmt.Sprintf(`agentAuthorizationsForUser({userId:%q})`, requestedBy)
	res, err := l.engine.Execute(systemActorContext(ctx), call)
	if err != nil {
		return false, "", fmt.Errorf("agentAuthorizationsForUser: %w", err)
	}
	now := time.Now().UTC()
	for _, row := range extractMintRows(res) {
		if getString(row, "agentId") != ownerAgentId {
			continue
		}
		if getString(row, "action") != "mintSkill" {
			continue
		}
		if active, ok := row["active"].(bool); ok && !active {
			continue
		}
		if expiresAt := getString(row, "expiresAt"); expiresAt != "" {
			if t, err := time.Parse(time.RFC3339, expiresAt); err == nil && t.Before(now) {
				continue
			}
		}
		// Tier allowlist check. Empty defaults to ["A"] per the issue.
		allowlist := stringSliceFromAny(row["skillTierAllowlist"])
		if len(allowlist) == 0 {
			allowlist = []string{"A"}
		}
		if !contains(allowlist, tier) {
			return false, fmt.Sprintf("tier %q not in skillTierAllowlist %v", tier, allowlist), nil
		}
		return true, "", nil
	}
	return false, "no active mintSkill grant on planner agent", nil
}

// executeMintSkill invokes mintSkill with the decision
// payload + the Phase 3 provenance triad. Returns the new skill's id
// on success.
func (l *PlannerAgentLoop) executeMintSkill(ctx context.Context, d plannerDecision, planId, ownerAgentId string) (string, error) {
	skillId := strings.TrimSpace(d.Slug)
	if skillId == "" {
		skillId = id.NewShortId()
	}
	args := map[string]any{
		"skillId":           skillId,
		"slug":              d.Slug,
		"name":              d.Name,
		"description":       d.Description,
		"category":          d.Category,
		"tags":              d.Tags,
		"domainIds":         d.DomainIds,
		"toolSlugs":         d.ToolSlugs,
		"liveSourceIds":     d.LiveSourceIds,
		"tier":              d.Tier,
		"originatingPlanId": planId,
		"mintedByAgentId":   ownerAgentId,
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal mint args: %w", err)
	}
	call := fmt.Sprintf(`mintSkill(%s)`, string(payload))
	if _, err := l.engine.Execute(systemActorContext(ctx), call); err != nil {
		return "", err
	}
	return skillId, nil
}

// writeMintApprovalCard inserts a v1:copresent:canvasState row of
// kind='card' carrying data.variant='mintSkillApprovalRequested'.
// The frontend renderer (CoPresent consumer issue) reads variant +
// data fields and surfaces Approve / Approve-and-allow / Reject
// buttons. We attach the card to the Plan's space so the user sees
// it inline with the conversation that triggered the mint request.
func (l *PlannerAgentLoop) writeMintApprovalCard(ctx context.Context, plan map[string]any, d plannerDecision) error {
	partitionId := getString(plan, "partitionId")
	if partitionId == "" {
		// No space binding; nothing to render against. The
		// awaitingFeedback escalation still parks the Plan -- the
		// missing card just means the operator gets the bare
		// feedbackReason instead of the rich approval surface.
		return fmt.Errorf("plan has no partitionId; skipping mintSkillApprovalRequested card")
	}
	planId := getString(plan, "id")
	ownerAgentId := getString(plan, "ownerAgentId")

	cardData := map[string]any{
		"variant": "mintSkillApprovalRequested",
		"skill": map[string]any{
			"slug":          d.Slug,
			"name":          d.Name,
			"description":   d.Description,
			"category":      d.Category,
			"tier":          d.Tier,
			"tags":          d.Tags,
			"domainIds":     d.DomainIds,
			"toolSlugs":     d.ToolSlugs,
			"liveSourceIds": d.LiveSourceIds,
		},
		"justification": d.Justification,
		"planId":        planId,
		"plannerAgent":  ownerAgentId,
	}

	args := map[string]any{
		"canvasStateId": fmt.Sprintf("mint-skill-approval-%s-%s", planId, d.Slug),
		"space":         partitionId,
		"kind":          "card",
		"data":          cardData,
		"visibility":    "private",
		"forUserId":     getString(plan, "requestedBy"),
		"importance":    "notify",
		"actor": map[string]any{
			"kind":    "agent",
			"agentId": ownerAgentId,
		},
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal card args: %w", err)
	}
	call := fmt.Sprintf(`mutationCreateCanvasState(%s)`, string(payload))
	_, err = l.engine.Execute(systemActorContext(ctx), call)
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// unionStringSets returns a presence set of every non-empty string
// across the supplied slices.
func unionStringSets(slices ...[]string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range slices {
		for _, item := range s {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			out[item] = struct{}{}
		}
	}
	return out
}

// jaccardOverlap returns |a ∩ b| / |a ∪ b|. Empty sets return 0.
// Used by findCatalogOverlap to score candidate near-matches.
func jaccardOverlap(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersect := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersect++
		}
	}
	union := len(a) + len(b) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

// contains is the trivial slice-containment check. Used so the
// authority gate stays terse.
func contains(s []string, v string) bool {
	for _, item := range s {
		if item == v {
			return true
		}
	}
	return false
}

// stringSliceFromAny mirrors the agents-package helper of the same
// name. Local to the planner package so the dependency surface
// stays narrow.
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
	}
	return nil
}
