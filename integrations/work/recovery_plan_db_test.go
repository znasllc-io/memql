package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// A plain temporary table hid the production Timescale planner regression:
// correlating n.concept made candidate scans read historical heap tuples. Check
// the real chunk plan with wide completed histories, without planner overrides.
func TestRecoveryPlanUsesIndexOnlyCandidatesAcrossChunks(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work recovery plan", dbtest.DSN(), err)
	}
	var installed bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Skip("TimescaleDB is required for the chunk planner regression")
	}
	schema := fmt.Sprintf("recoveryplan_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (id text, concept text, "createdAt" timestamptz NOT NULL, payload jsonb)`,
		`SELECT public.create_hypertable('"MemoryNodes"', 'createdAt', chunk_time_interval => interval '1 day')`,
		`INSERT INTO "MemoryNodes" SELECT 'run-'||n,'v1:work:run',TIMESTAMPTZ '2026-01-01'+v*interval '1 day',jsonb_build_object('status',CASE WHEN v=2 AND n>20 THEN 'succeeded' ELSE 'waiting' END,'detail',repeat('x',1200)) FROM generate_series(1,10000) n CROSS JOIN generate_series(0,2) v`,
		`CREATE INDEX latest_keys ON "MemoryNodes" (concept,id,"createdAt" DESC)`,
		`CREATE INDEX memory_nodes_work_recovery_idx ON "MemoryNodes" ("createdAt",id) WHERE concept='v1:work:run' AND COALESCE(payload->>'status','') NOT IN ('succeeded','failed','cancelled','abandoned')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `VACUUM ANALYZE "MemoryNodes"`); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+runsInFlightSQL, runConcept, runConcept, 5, runConcept).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []map[string]any
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	root := plans[0]["Plan"].(map[string]any)
	if root["Actual Rows"] != float64(5) {
		t.Fatalf("bounded unfinished rows: %s", raw)
	}
	indexOnly := 0
	var inspect func(map[string]any)
	inspect = func(plan map[string]any) {
		if name, _ := plan["Index Name"].(string); strings.Contains(name, "work_recovery_idx") {
			if plan["Node Type"] != "Index Only Scan" || plan["Heap Fetches"] != float64(0) {
				t.Errorf("candidate reads historical heap: %v", plan)
			}
			indexOnly++
		}
		children, _ := plan["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any))
		}
	}
	inspect(root)
	if indexOnly < 3 {
		t.Fatalf("expected recovery index on all three chunks, got %d: %s", indexOnly, raw)
	}
}
