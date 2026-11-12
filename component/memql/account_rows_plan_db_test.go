package memql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// account_rows_plan_db_test.go -- the plan of newestAccountSQL, the account
// reader's probe of one row's newest version (account_rows.go), held to what
// newest_version_by_id_plan_db_test.go holds the engine's own newest-version
// reads to: the same plan whether TimescaleDB's ordered append is on or off,
// no ordered walk of the chunks, every chunk probed once through an index that
// finds the id, and no version of another row read and thrown away. With
// "createdAt" alone in the ORDER BY of a LIMIT, an ordered walk is costed by
// its first chunk and may scan an older chunk whole for an id it does not
// hold; on an installation that read a multi-gigabyte chunk for one id.
func TestNewestAccountProbeDoesNotDependOnOrderedAppend(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "newest account probe plan", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("newestaccount_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema, "timescaledb.enable_ordered_append": "on"}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	// Four days of 40 rows written 100 times each, under long ids, and a hot
	// row written on every day and once more on a quiet newest day. The
	// probe asks for the hot row and for one that has no version anywhere.
	const concept = "v1:todos:todo"
	stamp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	hot := concept + ":hot-" + strings.Repeat("0123456789abcdef", 6)
	missing := concept + ":never-written-" + strings.Repeat("0123456789abcdef", 6)
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + concept + `:history-'||d||'-'||r||'-'||repeat(md5(d||'-'||r),3),'` + concept + `',
           TIMESTAMPTZ '` + stamp + `'+d*interval '1 day'+r*interval '30 minutes'+v*interval '2 seconds','fixture','{}',jsonb_build_object('accountId','acme')
         FROM generate_series(0,3) d CROSS JOIN generate_series(0,39) r CROSS JOIN generate_series(0,99) v`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + hot + `','` + concept + `',TIMESTAMPTZ '` + stamp + `'+d*interval '1 day'+interval '20 hours'+v*interval '1 second','fixture','{}',jsonb_build_object('accountId','acme')
         FROM generate_series(0,3) d CROSS JOIN generate_series(0,49) v`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         VALUES ('` + hot + `','` + concept + `',TIMESTAMPTZ '` + stamp + `'+interval '4 days 1 hour','fixture','{}','{"accountId":"acme"}')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + concept + `:quiet-'||r,'` + concept + `',TIMESTAMPTZ '` + stamp + `'+interval '4 days'+r*interval '1 minute','fixture','{}','{}'
         FROM generate_series(0,9) r`,
		`VACUUM ANALYZE "MemoryNodes"`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	var chunks int
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_schema = ? AND hypertable_name = 'MemoryNodes'`, schema).Scan(&chunks))
	require.Equal(t, 5, chunks)

	// The candidates come from a VALUES list, so c.id is a column the
	// planner cannot fold into a constant, as in the reader's statements.
	query := `SELECT c.id, ` + newestAccountSQL + ` AS account
	  FROM (VALUES ('` + concept + `', '` + hot + `'), ('` + concept + `', '` + missing + `')) AS c(concept, id)`
	var got []string
	rows, err := db.QueryContext(ctx, query)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		var account sql.NullString
		require.NoError(t, rows.Scan(&id, &account))
		got = append(got, fmt.Sprintf("%t=%s", id == hot, account.String))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, []string{"true=acme", "false="}, got)

	require.Equal(t, newestByIDPlanShape(t, db, query, false), newestByIDPlanShape(t, db, query, true),
		"the plan must not change with ordered append: %s", query)
	plan := explainNewestByID(t, db, query)
	require.False(t, plan.orderedChunkWalk, "an ordered chunk walk costs only its first chunk: %s", plan.raw)
	require.Equal(t, chunks, plan.chunkScans, "every chunk is probed once: %s", plan.raw)
	require.Empty(t, plan.unprobed, "every chunk must be read through an index that finds the id: %s", plan.raw)
	require.Zero(t, plan.discarded, "no version of another row may be read and thrown away: %s", plan.raw)
}
