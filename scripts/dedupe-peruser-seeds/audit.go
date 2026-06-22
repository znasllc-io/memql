package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// agentReferenceConcepts enumerates the concepts that carry an
// agentId field on their payload (and where the script's hard-
// delete pass does NOT touch them). Each entry tells the audit how
// to find the reference:
//
//   - Concept = the canonical concept id used at write time
//   - Path = the JSON path to the agentId field within payload.
//     "agentId" reads payload.agentId; "source.agentId" reads
//     payload.source.agentId, etc.
//   - Why = an operator-facing one-liner explaining what state
//     this concept tracks, so the audit output stands alone
//     without crossreferencing the DSL tree.
//
// agentIds[] (array) references and many-to-many shapes are NOT
// covered today -- they're surfaced as a CAVEAT in the audit
// summary so operators know the report isn't exhaustive. Adding
// array-field auditing means a JSONB containment query per
// concept; pinned to a follow-up if the pre-#274 backlog ever
// turns up an `agentIds[]` reference that matters.
var agentReferenceConcepts = []agentRefConcept{
	{
		Concept: "v1:agents:agentAuthorization",
		Path:    "agentId",
		Why:     "Standing per-(agent, planKind, spaceScope) autonomy grants.",
	},
	{
		Concept: "v1:identity:delegation",
		Path:    "agentId",
		Why:     "Identity delegations granting an agent bounded authority.",
	},
	{
		Concept: "v1:cognition:audioOverride",
		Path:    "agentId",
		Why:     "Per-(space, agent) TTS audio overrides.",
	},
	{
		Concept: "v1:cognition:videoOverride",
		Path:    "agentId",
		Why:     "Per-(space, agent) avatar video overrides.",
	},
	{
		Concept: "v1:cognition:clientToolRequest",
		Path:    "agentId",
		Why:     "Cross-node client-tool request records (audit-only field).",
	},
	{
		Concept: "v1:cognition:utterance",
		Path:    "source.agentId",
		Why:     "Agent-spoken utterances tag the source agent on payload.source.agentId.",
	},
}

type agentRefConcept struct {
	Concept string
	Path    string
	Why     string
}

// auditAgentReferences scans every non-participant concept that
// carries an agentId on its payload (see agentReferenceConcepts)
// and reports the row counts pointing at any of the doomed agent
// ids in plan. Read-only -- no rows are written. Output prints
// per-concept counts plus the doomed ids each one references.
//
// Why this is informational only:
//
//   - Participants are reconciled by the main script's hard-delete
//     pass (the doomed-participant set is then re-created by the
//     autoJoinAI automation on next cluster boot at the canonical
//     agent id, per memql#273 / PR #274).
//   - Utterances are historical chat content; rewriting / deleting
//     them would lose user data. The audit surfaces the count so
//     operators know how much history points at the cleaned-up
//     agents and can decide whether to rewrite payload.source.agentId
//     downstream (out of scope for this one-shot migration).
//   - Authorizations / delegations / overrides are configuration
//     state. If they're pointing at a doomed agent id post-cleanup
//     the agent is effectively un-configured; the user re-authorizes
//     / re-overrides at the canonical id when they next interact.
//     Rewriting would also work but isn't strictly required for the
//     system to converge.
//
// Output shape (per concept with any hits):
//
//	concept=v1:cognition:audioOverride path=agentId hits=3
//	  - 2 row(s) reference agent v1:agents:agent:abc...
//	  - 1 row(s) reference agent v1:agents:agent:def...
//
// And a trailing summary listing concepts with zero hits + the
// total reference count.
func auditAgentReferences(ctx context.Context, db *sql.DB, plan Plan) error {
	doomedIDs := plan.DoomedAgentIDs
	if len(doomedIDs) == 0 {
		fmt.Println()
		fmt.Println("--- agentId reference audit ---")
		fmt.Println("no doomed agent ids; skipping reference audit.")
		return nil
	}

	fmt.Println()
	fmt.Println("--- agentId reference audit (read-only) ---")
	fmt.Printf("auditing %d doomed agent ids across %d concept(s)\n",
		len(doomedIDs), len(agentReferenceConcepts))
	fmt.Println()

	zeroHitConcepts := []string{}
	totalHits := 0

	for _, ref := range agentReferenceConcepts {
		counts, err := countReferencesByAgentID(ctx, db, ref, doomedIDs)
		if err != nil {
			return fmt.Errorf("%s audit: %w", ref.Concept, err)
		}
		hits := 0
		for _, n := range counts {
			hits += n
		}
		totalHits += hits
		if hits == 0 {
			zeroHitConcepts = append(zeroHitConcepts, ref.Concept)
			continue
		}
		fmt.Printf("concept=%s path=%s hits=%d\n", ref.Concept, ref.Path, hits)
		fmt.Printf("  why: %s\n", ref.Why)
		ids := make([]string, 0, len(counts))
		for id := range counts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("  - %d row(s) reference agent %s\n", counts[id], id)
		}
		fmt.Println()
	}

	if len(zeroHitConcepts) > 0 {
		fmt.Printf("zero-hit concepts: %s\n", strings.Join(zeroHitConcepts, ", "))
	}
	fmt.Printf("total references to doomed agents across audited concepts: %d\n", totalHits)
	if totalHits > 0 {
		fmt.Println()
		fmt.Println("AUDIT IS INFORMATIONAL ONLY. The main delete pass touches only")
		fmt.Println("agent + participant rows. Other state above keeps pointing at the")
		fmt.Println("doomed ids until users re-authorize / re-override / re-chat at the")
		fmt.Println("canonical agent id, OR a follow-up rewrite pass is run.")
	}
	fmt.Println()
	fmt.Println("CAVEAT: this audit covers payload.<path>=string references only;")
	fmt.Println("array-valued references (e.g. payload.agentIds[] on groups) are not")
	fmt.Println("scanned. If the pre-#274 cluster booted agents that ended up in")
	fmt.Println("group rosters, re-audit manually.")
	return nil
}

// countReferencesByAgentID returns a map of agentId -> row count
// for the given concept + JSON path, scoped to the doomedIDs set.
// DISTINCT ON (id) collapses multi-version rows to one per id so
// the count is a "row count" not a "version count".
//
// The path is read from JSONB via the postgres `->>` operator
// chain. Nested paths (`source.agentId`) translate to
// `payload->'source'->>'agentId'`.
func countReferencesByAgentID(ctx context.Context, db *sql.DB, ref agentRefConcept, doomedIDs []string) (map[string]int, error) {
	expr, err := jsonPathExpr("payload", ref.Path)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
SELECT agent_id, COUNT(*) FROM (
  SELECT DISTINCT ON (id) id, %s AS agent_id
  FROM "MemoryNodes"
  WHERE concept = $1
    AND %s = ANY($2)
  ORDER BY id, "createdAt" DESC
) latest
GROUP BY agent_id`, expr, expr)

	rows, err := db.QueryContext(ctx, q, ref.Concept, pqArray(doomedIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var agentID string
		var n int
		if err := rows.Scan(&agentID, &n); err != nil {
			return nil, err
		}
		out[agentID] = n
	}
	return out, rows.Err()
}

// jsonPathExpr translates a dotted path (`source.agentId`) into the
// Postgres JSONB navigation expression rooted at `base` (`payload`).
// A single-segment path uses the text-extraction operator `->>`; a
// multi-segment path uses `->` for the intermediate objects and
// `->>` for the final scalar.
//
// Rejects empty / leading-dot paths so the audit can't accidentally
// blow up the entire payload column.
func jsonPathExpr(base, path string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("jsonPathExpr: base is empty")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("jsonPathExpr: path is empty")
	}
	segments := strings.Split(path, ".")
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("jsonPathExpr: empty segment in %q", path)
		}
	}
	var sb strings.Builder
	sb.WriteString(base)
	for i, s := range segments {
		if i == len(segments)-1 {
			sb.WriteString("->>'")
		} else {
			sb.WriteString("->'")
		}
		sb.WriteString(s)
		sb.WriteString("'")
	}
	return sb.String(), nil
}
