package database_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// The latest-per-id-within-a-concept read is the shape of EVERY concept query
// the executor emits:
//
//	SELECT DISTINCT ON (id) ... FROM "MemoryNodes"
//	WHERE concept = $1 ORDER BY id ASC, "createdAt" DESC
//
// With only (id, "createdAt" DESC) and (concept) indexed, TimescaleDB plans it
// as a SkipScan over the id index and filters concept per row -- which walks
// EVERY id in the table for every call (a production instance, 2026-09-13: a
// 274-node query read 1,006,847 rows and 6.9 GB of buffers, 178 s, and the
// pods issuing it on every heartbeat exhausted max_connections). The composite
// index below bounds the read to one concept's rows. It does not make the plan
// skip within a concept: TimescaleDB 2.29.2 does not SkipScan it (measured in
// latest_row_index_db_test.go, which also pins the incident plan it replaced).
const conceptIndexName = "memory_nodes_concept_id_created_at_desc_idx"

func conceptIndexDB(t *testing.T) *bun.DB {
	t.Helper()
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	if err != nil || !reachable {
		dbtest.Unreachable(t, "the MemoryNodes concept index", dbtest.DSN(), err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A name is not completeness (memql#5252): an interrupted transaction_per_chunk
// build leaves an invalid index under exactly this name, and CREATE INDEX IF
// NOT EXISTS reports success over it. So after the real migrations run, the
// index must be there by name AND complete -- valid on the table and, when
// MemoryNodes is a hypertable, carried by every chunk -- read both through the
// migration's own inspection and through the definition Postgres prints.
func TestMemoryNodesCarryTheConceptIdCreatedAtIndex(t *testing.T) {
	db := conceptIndexDB(t)
	ctx := context.Background()

	var indexdef string
	err := db.NewRaw(`SELECT indexdef FROM pg_indexes WHERE tablename = 'MemoryNodes' AND indexname = ?`, conceptIndexName).
		Scan(ctx, &indexdef)
	if err != nil {
		t.Fatalf("index %s is missing from MemoryNodes: %v", conceptIndexName, err)
	}
	for _, want := range []string{`(concept, id, "createdAt" DESC)`} {
		if !strings.Contains(indexdef, want) {
			t.Fatalf("index %s = %q, want it over %s", conceptIndexName, indexdef, want)
		}
	}

	report, err := database.InspectLatestRowIndex(ctx, db, "MemoryNodes", conceptIndexName)
	if err != nil {
		t.Fatalf("inspecting MemoryNodes: %v", err)
	}
	if report.Outcome != database.LatestRowIndexValid || report.Covering != conceptIndexName {
		t.Fatalf("MemoryNodes' %s is not complete after the migrations ran: %+v", conceptIndexName, report)
	}
	oracle := readOracle(t, db, lriTable{name: "MemoryNodes"}, conceptIndexName)
	if !oracle.complete() {
		t.Fatalf("MemoryNodes' %s is not complete by its printed definition: %+v", conceptIndexName, oracle)
	}
	t.Logf("MemoryNodes: hypertable=%v, %d chunks checked, every one covered", report.Hypertable, report.Chunks)
}

func TestLatestPerConceptReadPlansOverTheConceptIndex(t *testing.T) {
	db := conceptIndexDB(t)
	ctx := context.Background()

	// Two concepts, a few versions each, so the planner has something to
	// order and a concept predicate to push into the index.
	at := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		for _, concept := range []string{"v1:test:conceptIndexA", "v1:test:conceptIndexB"} {
			seedRetiredRow(t, ctx, db, concept, concept+":row-"+string(rune('a'+i)), at.Add(time.Duration(i)*time.Minute), map[string]any{"n": i})
		}
	}

	// A handful of rows never justify an index over a sequential scan, so the
	// planner is asked to choose among indexes only; what matters is WHICH
	// index answers the concept + latest-per-id shape.
	const plan = `SET LOCAL enable_seqscan = off;
EXPLAIN SELECT DISTINCT ON (id) id, "createdAt" FROM "MemoryNodes"
WHERE concept = 'v1:test:conceptIndexA' ORDER BY id ASC, "createdAt" DESC`
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.QueryContext(ctx, strings.TrimPrefix(plan, "SET LOCAL enable_seqscan = off;\n"))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, line)
	}
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, conceptIndexName) {
		t.Fatalf("the latest-per-concept read does not plan over %s:\n%s", conceptIndexName, got)
	}
}
