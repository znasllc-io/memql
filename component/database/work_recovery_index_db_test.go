package database_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestWorkRecoveryIndexBuildsAndRepairsChunkCoverage(t *testing.T) {
	testWorkIndex(t, "memory_nodes_work_recovery_idx", database.EnsureWorkRecoveryIndex)
}
func TestWorkJournalLookupIndexBuildsAndRepairsChunkCoverage(t *testing.T) {
	testWorkIndex(t, "memory_nodes_work_journal_lookup_idx", database.EnsureWorkJournalLookupIndex)
}
func testWorkIndex(t *testing.T, indexName string, build func(context.Context, *bun.DB, *slog.Logger, string) error) {
	admin := conceptIndexDB(t)
	ctx := context.Background()
	for _, hypertable := range []bool{false, true} {
		t.Run(fmt.Sprint("hypertable=", hypertable), func(t *testing.T) {
			schema := fmt.Sprintf("recoveryidx_%d", time.Now().UnixNano())
			if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
			db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(ctx, `CREATE TABLE "MemoryNodes" (id text, concept text, "createdAt" timestamptz NOT NULL, payload jsonb)`); err != nil {
				t.Fatal(err)
			}
			if hypertable {
				var installed bool
				if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&installed); err != nil {
					t.Fatal(err)
				}
				if !installed {
					t.Skip("TimescaleDB not installed")
				}
				if _, err := db.ExecContext(ctx, `SELECT public.create_hypertable('"MemoryNodes"', 'createdAt', chunk_time_interval => interval '1 day')`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" SELECT 'run-'||i, 'v1:work:run', now()-i*interval '1 day', '{"status":"running"}'::jsonb FROM generate_series(1,3) i`); err != nil {
				t.Fatal(err)
			}
			ensure := func() {
				t.Helper()
				if err := build(ctx, db, nil, "MemoryNodes"); err != nil {
					t.Fatal(err)
				}
			}
			ensure()
			ensure() // repeated migration must preserve a complete build
			var root string
			if err := db.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=? AND indexname=?`, schema, indexName).Scan(&root); err != nil || root == "" {
				t.Fatalf("root index: %q %v", root, err)
			}
			if hypertable {
				var victim string
				if err := db.QueryRowContext(ctx, `SELECT quote_ident(n.nspname)||'.'||quote_ident(c.relname) FROM pg_inherits h JOIN pg_index i ON i.indrelid=h.inhrelid JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE h.inhparent='"MemoryNodes"'::regclass AND c.relname LIKE ? LIMIT 1`, "%"+strings.TrimPrefix(indexName, "memory_nodes_")).Scan(&victim); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `DROP INDEX `+victim); err != nil {
					t.Fatal(err)
				}
				ensure() // a valid root alone must not hide a missing chunk index
				var missing int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_inherits h WHERE h.inhparent='"MemoryNodes"'::regclass AND NOT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=h.inhrelid AND i.indisvalid AND c.relname LIKE ?)`, "%"+strings.TrimPrefix(indexName, "memory_nodes_")).Scan(&missing); err != nil || missing != 0 {
					t.Fatalf("missing chunk indexes=%d: %v", missing, err)
				}
			}
		})
	}
}
