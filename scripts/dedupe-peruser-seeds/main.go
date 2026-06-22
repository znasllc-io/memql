// dedupe-peruser-seeds is a one-shot data migration that cleans up
// the duplicate per-user seed rows created by pre-PR-#274 cluster
// boots, where the seed materializer minted fresh random UUIDs for
// every concurrent startup-sweep racer instead of the documented
// deterministic `<seedName>-<userId>` id.
//
// USAGE:
//
//	# inspect what would be deleted, no writes
//	go run ./scripts/dedupe-peruser-seeds --dry-run
//
//	# actually delete the doomed rows
//	go run ./scripts/dedupe-peruser-seeds --execute
//
// CONNECTION:
//
// Reads $MEMORY_NODES_DATABASE_DSN. The docker-compose stack stamps
// `postgres://memql:memql_dev@postgres:5432/memql?sslmode=disable`
// on every memql container; running this script against a host's
// docker desktop is `MEMORY_NODES_DATABASE_DSN=postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable go run ./scripts/dedupe-peruser-seeds --dry-run`.
//
// POST-RUN:
//
// After --execute, every affected (seed, owner) pair will have ZERO
// rows in the DB (none of the doomed UUIDs matched the canonical
// `<seedName>-<bareUserId>` id, since the bug being cleaned up here
// is that NO canonical row was ever written pre-#274). The
// materializer's startup sweep is what writes the canonical row;
// run a cluster restart (`docker compose -f docker/docker-compose.full.yml restart` or the cluster equivalent) and the bff / cognition / agent /
// planner / voice nodes will each call SeedMaterializer.Start(),
// which reads the now-empty (seed, owner) buckets and writes one
// row per pair at the canonical id. Concurrent racers across the
// nodes collapse to versions of one logical row via the fix from
// #274.
//
// CROSS-CLIENT:
//
// The cleanup is at the DB layer, so both copresent and memql-cockpit
// inherit the post-state automatically. No client-side change needed.
//
// SAFETY:
//
//   - Plan computation is pure (see plan.go + plan_test.go). The
//     SQL execution path runs in a single transaction; partial
//     failure rolls back.
//   - Only rows the planner classifies as doomed are touched.
//     User-created agents (non-seed provenance) are filtered out
//     by SQL upstream and re-filtered by ComputePlan as defense in
//     depth.
//   - Hard delete is intentional. memql's (id, createdAt) PK means
//     "soft delete" would still leave the doomed-id versions in the
//     time-series, defeating the cleanup. The duplicates were never
//     legitimate data.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/lib/pq"
)

func main() {
	var (
		dryRun  = flag.Bool("dry-run", false, "Print the plan without writing")
		execute = flag.Bool("execute", false, "Actually run the DELETEs (mutually exclusive with --dry-run)")
		dsn     = flag.String("dsn", "", "Postgres DSN; defaults to $MEMORY_NODES_DATABASE_DSN")
	)
	flag.Parse()

	if *dryRun == *execute {
		fmt.Fprintln(os.Stderr, "ERROR: exactly one of --dry-run / --execute is required")
		os.Exit(2)
	}

	resolvedDSN := *dsn
	if resolvedDSN == "" {
		resolvedDSN = os.Getenv("MEMORY_NODES_DATABASE_DSN")
	}
	if resolvedDSN == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --dsn or $MEMORY_NODES_DATABASE_DSN required")
		os.Exit(2)
	}

	if err := run(context.Background(), resolvedDSN, *execute); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string, execute bool) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	agents, err := loadSeedAgentRows(ctx, db)
	if err != nil {
		return fmt.Errorf("load seed agent rows: %w", err)
	}
	fmt.Printf("loaded %d distinct seed-materialized perUser rows\n", len(agents))

	participants, err := loadAIParticipantRows(ctx, db, agents)
	if err != nil {
		return fmt.Errorf("load SI participant rows: %w", err)
	}
	fmt.Printf("loaded %d distinct non-left SI participants pointing at seed agents\n", len(participants))

	plan := ComputePlan(agents, participants)
	printPlan(plan)

	// Read-only audit: scan other concepts that carry an agentId
	// reference (authorizations, delegations, audio/video overrides,
	// utterances, etc.) and surface counts so operators know what
	// else points at the cleaned-up agents. The main delete pass
	// below does not touch these rows; the audit is informational.
	if err := auditAgentReferences(ctx, db, plan); err != nil {
		return fmt.Errorf("agentId reference audit: %w", err)
	}

	if !execute {
		fmt.Println()
		fmt.Println("dry-run only; pass --execute to apply.")
		return nil
	}
	if len(plan.DoomedAgentIDs) == 0 && len(plan.DoomedParticipantIDs) == 0 {
		fmt.Println("nothing to do.")
		return nil
	}

	if err := executePlan(ctx, db, plan); err != nil {
		return fmt.Errorf("execute plan: %w", err)
	}
	fmt.Println()
	fmt.Printf("deleted %d agent rows (all versions) and %d participant rows (all versions).\n",
		len(plan.DoomedAgentIDs), len(plan.DoomedParticipantIDs))

	reseedCount := 0
	for _, g := range plan.Groups {
		if g.NeedsReseed {
			reseedCount++
		}
	}
	if reseedCount > 0 {
		fmt.Println()
		fmt.Printf("NEXT STEP: %d (seed, owner) pair(s) need re-seeding.\n", reseedCount)
		fmt.Println("Restart the memql cluster so the seed materializer's startup")
		fmt.Println("sweep re-writes the canonical `<seedName>-<bareUserId>` row.")
	}
	return nil
}

// loadSeedAgentRows pulls the latest version of every perUser
// seed-materialized agent row. The DISTINCT ON keeps one row per
// id; ORDER BY descending createdAt within the group puts the
// latest version first.
func loadSeedAgentRows(ctx context.Context, db *sql.DB) ([]AgentRow, error) {
	const q = `
SELECT DISTINCT ON (id)
  id,
  concept,
  payload->>'ownerUserId' AS owner,
  provenance->>'name'     AS seed_name
FROM "MemoryNodes"
WHERE provenance->>'kind' = 'seed'
  AND payload->>'ownerUserId' IS NOT NULL
  AND payload->>'ownerUserId' <> ''
ORDER BY id, "createdAt" DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentRow
	for rows.Next() {
		var r AgentRow
		if err := rows.Scan(&r.ID, &r.Concept, &r.OwnerUserID, &r.SeedName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadAIParticipantRows pulls the latest non-left SI participant
// row per id whose agentId points at any of the seed-materialized
// agents from loadSeedAgentRows. Scoping the lookup to known seed
// agents keeps the result small even on large clusters and means
// we don't accidentally touch participants tied to user-created
// agents.
func loadAIParticipantRows(ctx context.Context, db *sql.DB, agents []AgentRow) ([]ParticipantRow, error) {
	if len(agents) == 0 {
		return nil, nil
	}
	agentIDs := make([]string, 0, len(agents))
	for _, a := range agents {
		agentIDs = append(agentIDs, a.ID)
	}

	const q = `
SELECT DISTINCT ON (id)
  id,
  payload->>'agentId' AS agent_id
FROM "MemoryNodes"
WHERE concept = 'v1:cognition:participant'
  AND payload->>'participantType' = 'si'
  AND payload->>'agentId' = ANY($1)
  AND payload->>'status' <> 'left'
ORDER BY id, "createdAt" DESC`

	rows, err := db.QueryContext(ctx, q, pqArray(agentIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ParticipantRow
	for rows.Next() {
		var r ParticipantRow
		if err := rows.Scan(&r.ID, &r.AgentID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// executePlan runs both deletes inside one transaction. Either
// both succeed or neither does -- partial cleanup would leave the
// DB in a more confusing state than the starting condition.
func executePlan(ctx context.Context, db *sql.DB, plan Plan) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(plan.DoomedAgentIDs) > 0 {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM "MemoryNodes" WHERE id = ANY($1)`,
			pqArray(plan.DoomedAgentIDs))
		if err != nil {
			return fmt.Errorf("delete agent rows: %w", err)
		}
	}
	if len(plan.DoomedParticipantIDs) > 0 {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM "MemoryNodes" WHERE id = ANY($1)`,
			pqArray(plan.DoomedParticipantIDs))
		if err != nil {
			return fmt.Errorf("delete participant rows: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// pqArray returns the lib/pq array placeholder shape for a slice
// of strings. Avoiding the explicit pq.Array import keeps the
// dependency surface of this one-shot script as small as possible.
func pqArray(values []string) any {
	// pq accepts the comma-separated `{a,b,c}` literal for text[]
	// columns when the values don't contain quotes / commas /
	// backslashes. All our ids are canonical opaque strings, so
	// the simple form is safe. Encoding via json + a small post-
	// process is the lazy-but-correct way; pq.Array would also
	// work but pulls in another concrete type.
	quoted := make([]string, len(values))
	for i, v := range values {
		// Defensive escape for any rogue character even though
		// real ids never carry these.
		s := strings.ReplaceAll(v, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		quoted[i] = `"` + s + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

func printPlan(plan Plan) {
	fmt.Println()
	fmt.Println("--- migration plan ---")
	if len(plan.Groups) == 0 {
		fmt.Println("no perUser seed rows to consider; nothing to do.")
		return
	}

	for _, g := range plan.Groups {
		if len(g.Doomed) == 0 && !g.NeedsReseed {
			// Group is already clean. Skip from the noisy
			// summary but mention it in the totals below.
			continue
		}
		fmt.Println()
		fmt.Printf("seed=%s owner=%s\n", g.SeedName, g.OwnerUserID)
		fmt.Printf("  expected canonical id: %s\n", g.ExpectedID)
		if len(g.Keep) == 0 {
			fmt.Printf("  KEEP (canonical): <missing -- reseed required>\n")
		} else {
			fmt.Printf("  KEEP (canonical): %s\n", g.Keep[0])
		}
		fmt.Printf("  DOOM: %d row(s)\n", len(g.Doomed))
		for _, id := range g.Doomed {
			fmt.Printf("    - %s\n", id)
		}
	}

	fmt.Println()
	fmt.Printf("totals: %d groups, %d doomed agent rows, %d doomed participant rows\n",
		len(plan.Groups), len(plan.DoomedAgentIDs), len(plan.DoomedParticipantIDs))

	// One-line JSON for easy machine consumption when piped.
	summary := map[string]int{
		"groups":              len(plan.Groups),
		"doomedAgents":        len(plan.DoomedAgentIDs),
		"doomedParticipants":  len(plan.DoomedParticipantIDs),
	}
	if b, err := json.Marshal(summary); err == nil {
		fmt.Printf("summary-json: %s\n", string(b))
	}
}
