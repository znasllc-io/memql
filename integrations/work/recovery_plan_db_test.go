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

// Recovery must read only the unfinished current heads and bounded canonical
// payloads, even with 100,000 completed histories across real Timescale chunks.
func TestRecoveryPlanReadsOnlyCurrentUnfinishedHeadsAcrossChunks(t *testing.T) {
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
		`INSERT INTO "MemoryNodes" SELECT 'run-'||n,'v1:work:run',TIMESTAMPTZ '2026-01-01'+v*interval '1 day',jsonb_build_object('status',CASE WHEN v=2 AND n>20 THEN 'succeeded' ELSE 'waiting' END,'detail',repeat('x',400)) FROM generate_series(1,100000) n CROSS JOIN generate_series(0,2) v`,
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
	installWorkHeadProjection(t, db)
	catchUpWorkHeads(t, db)
	if _, err := db.ExecContext(ctx, `VACUUM ANALYZE work_run_heads`); err != nil {
		t.Fatal(err)
	}
	for _, pageSize := range []int{5, sweepPageSize} {
		var raw []byte
		if err := db.QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+runsInFlightSQL, pageSize, runConcept).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []map[string]any
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		root := plans[0]["Plan"].(map[string]any)
		if root["Actual Rows"] != float64(min(pageSize, 20)) {
			t.Fatalf("bounded unfinished rows: %s", raw)
		}
		headIndex := false
		var inspect func(map[string]any)
		inspect = func(plan map[string]any) {
			name, _ := plan["Index Name"].(string)
			relation, _ := plan["Relation Name"].(string)
			if strings.Contains(name, "work_run_heads_in_flight") {
				headIndex = true
				if plan["Actual Rows"].(float64) > float64(pageSize) {
					t.Errorf("scanned terminal heads: %v", plan)
				}
			}
			if strings.Contains(relation, "_chunk") && plan["Actual Loops"].(float64) > 0 {
				if !strings.Contains(plan["Node Type"].(string), "Index") || plan["Actual Rows"].(float64) > 1 {
					t.Errorf("historical chunk scan instead of one payload probe: %v", plan)
				}
			}
			children, _ := plan["Plans"].([]any)
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
		inspect(root)
		if !headIndex {
			t.Fatalf("missing bounded in-flight head index: %s", raw)
		}

	}
	// Exercise transition capture on a newly created Timescale chunk. Bulk
	// retention must emit one dirty ID, not one event per historical version.
	if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" SELECT 'bulk-retired','v1:work:run',TIMESTAMPTZ '2026-01-10'+v*interval '1 second','{"status":"succeeded"}'::jsonb FROM generate_series(1,100) v`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id='bulk-retired'`); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_run_head_events WHERE id='bulk-retired'`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("bulk/new-chunk capture: %d %v", events, err)
	}
	catchUpWorkHeads(t, db)
	var heads int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_run_heads WHERE id='bulk-retired'`).Scan(&heads); err != nil || heads != 0 {
		t.Fatalf("retired history resurrected: %d %v", heads, err)
	}

}
