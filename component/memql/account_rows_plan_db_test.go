package memql

import (
	"context"
	"database/sql"
	"encoding/json"
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

// The decision (accountDecisionSQL, behind AccountKindRowIDs, AccountRowCounts
// and the census) must find the account's versions through the account index's
// own condition and never evaluate payload->>'accountId' on a row-table heap
// tuple: on an installation the payload is stored out of line, and evaluating
// the expression fetches and unpacks it whole. So every chunk scan that names
// accountId names it as an Index Cond of the account index, and none carries a
// Filter or a Recheck Cond that names it (a bitmap scan rechecks lossy pages).
func TestAccountDecisionReadsTheAccountThroughTheIndexOnly(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "account decision plan", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("accountdecision_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	// Four daily chunks of one kind: 2,000 rows of three versions each, the
	// newest naming an account (acme for one row in ten, beta otherwise), the
	// older ones naming none, and payloads long enough to be stored out of line.
	const concept = "v1:todos:todo"
	stamp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + concept + `:r'||r,'` + concept + `',
           TIMESTAMPTZ '` + stamp + `'+(r%4)*interval '1 day'+v*interval '1 minute','fixture','{}',
           jsonb_strip_nulls(jsonb_build_object('accountId',CASE WHEN v=2 THEN CASE WHEN r%10=0 THEN 'acme' ELSE 'beta' END END,
             'body',(SELECT string_agg(md5(r||'-'||v||'-'||g),'') FROM generate_series(1,100) g)))
         FROM generate_series(1,2000) r CROSS JOIN generate_series(0,2) v`,
		`VACUUM ANALYZE "MemoryNodes"`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}

	// The copy's account index has a generated name; find it by its shape.
	var accountIndex string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = ? AND tablename = 'MemoryNodes'
	  AND indexdef LIKE '%(((payload ->> ''accountId''::text)), concept) WHERE%'`, schema).Scan(&accountIndex))

	ids, err := AccountKindRowIDs(ctx, db.DB, "acme", concept)
	require.NoError(t, err)
	require.Len(t, ids, 200)

	for _, shape := range []struct {
		name, sql string
	}{{"few", accountKindRowIDsFewSQL}, {"many", accountKindRowIDsManySQL}} {
		for _, account := range []string{"acme", "beta"} {
			account := shape.name + " " + account
			query := strings.NewReplacer("$1", "'"+strings.Fields(account)[1]+"'", "$2", "'v1:accounts:account:"+strings.Fields(account)[1]+"'", "$3::text[]", "ARRAY['"+concept+"']").Replace(shape.sql)
			// As the reader runs it: inside a transaction, through withIndexPlans.
			var raw []byte
			tx, err := db.DB.BeginTx(ctx, nil)
			require.NoError(t, err)
			require.NoError(t, withIndexPlans(ctx, tx, func() error {
				return tx.QueryRowContext(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) `+query).Scan(&raw)
			}))
			require.NoError(t, tx.Rollback())
			var plans []map[string]any
			require.NoError(t, json.Unmarshal(raw, &plans))
			viaIndex := 0
			var inspect func(plan map[string]any)
			inspect = func(plan map[string]any) {
				relation, _ := plan["Relation Name"].(string)
				index, _ := plan["Index Name"].(string)
				if strings.Contains(relation, "_chunk") || strings.Contains(index, "_chunk") {
					for _, key := range []string{"Filter", "Recheck Cond"} {
						cond, _ := plan[key].(string)
						require.NotContains(t, cond, "accountId", "%s evaluates the account on heap tuples (%s %s): %s", account, plan["Node Type"], key, raw)
					}
					if cond, _ := plan["Index Cond"].(string); strings.Contains(cond, "accountId") {
						require.Contains(t, index, accountIndex, "%s: the account is matched by an index that is not the account index: %s", account, raw)
						viaIndex++
					}
				}
				children, _ := plan["Plans"].([]any)
				for _, child := range children {
					inspect(child.(map[string]any))
				}
			}
			inspect(plans[0]["Plan"].(map[string]any))
			require.Equal(t, 4, viaIndex, "%s: every chunk's account versions are found through the account index: %s", account, raw)
		}
	}
}
