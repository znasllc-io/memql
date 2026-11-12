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

// accountPlanFixture is a schema of its own holding a hypertable of four daily
// chunks of one kind: 2,000 rows of three versions each, the newest naming an
// account (acme for one row in ten, beta otherwise), the older ones naming
// none, and payloads long enough to be stored out of line. It returns the
// connection, the kind, and the name the copy gave the account index.
func accountPlanFixture(t *testing.T, purpose string) (*bun.DB, string, string) {
	t.Helper()
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, purpose, dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("accountplan_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

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
	var accountIndex string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = ? AND tablename = 'MemoryNodes'
	  AND indexdef LIKE '%(((payload ->> ''accountId''::text)), concept) WHERE%'`, schema).Scan(&accountIndex))
	return db, concept, accountIndex
}

// explainAsTheReaderRuns is the plan of query, with the reader's parameters
// written in, inside a transaction through withIndexPlans, as the reader runs
// its statements.
func explainAsTheReaderRuns(t *testing.T, db *bun.DB, query string, args ...string) []byte {
	t.Helper()
	ctx := context.Background()
	pairs := make([]string, 0, 2*len(args))
	for n := len(args); n >= 1; n-- { // $10 before $1
		pairs = append(pairs, fmt.Sprintf("$%d", n), args[n-1])
	}
	query = strings.NewReplacer(pairs...).Replace(query)
	var raw []byte
	tx, err := db.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, withIndexPlans(ctx, tx, func() error {
		return tx.QueryRowContext(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) `+query).Scan(&raw)
	}))
	require.NoError(t, tx.Rollback())
	return raw
}

// requireAccountThroughTheIndex holds a plan to the reader's promise: no chunk
// scan carries a Filter or Recheck Cond naming accountId (an expression on a
// heap tuple, which unpacks an out-of-line payload; a bitmap rechecks lossy
// pages), and every chunk scan that names accountId names it as an Index Cond
// of the account index. It returns how many chunk scans did.
func requireAccountThroughTheIndex(t *testing.T, raw []byte, accountIndex, label string) int {
	t.Helper()
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
				require.NotContains(t, cond, "accountId", "%s evaluates the account on heap tuples (%s %s): %s", label, plan["Node Type"], key, raw)
			}
			if cond, _ := plan["Index Cond"].(string); strings.Contains(cond, "accountId") {
				require.Contains(t, index, accountIndex, "%s: the account is matched by an index that is not the account index: %s", label, raw)
				viaIndex++
			}
		}
		children, _ := plan["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any))
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	return viaIndex
}

// The decision (accountDecisionSQL, behind AccountKindRowIDs and
// AccountRowCounts) must find the account's versions through the account
// index's own condition and never evaluate payload->>'accountId' on a
// row-table heap tuple: on an installation the payload is stored out of line,
// and evaluating the expression fetches and unpacks it whole.
func TestAccountDecisionReadsTheAccountThroughTheIndexOnly(t *testing.T) {
	db, concept, accountIndex := accountPlanFixture(t, "account decision plan")
	ids, err := AccountKindRowIDs(context.Background(), db.DB, "acme", concept)
	require.NoError(t, err)
	require.Len(t, ids, 200)
	for _, shape := range []struct {
		name, sql string
	}{
		{"row ids, few", accountKindRowIDsFewSQL}, {"row ids, many", accountKindRowIDsManySQL},
		{"counts, few", accountRowCountsFewSQL}, {"counts, many", accountRowCountsManySQL},
	} {
		for _, account := range []string{"acme", "beta"} {
			label := shape.name + " " + account
			raw := explainAsTheReaderRuns(t, db, shape.sql, "'"+account+"'", "'v1:accounts:account:"+account+"'", "ARRAY['"+concept+"']")
			require.Equal(t, 4, requireAccountThroughTheIndex(t, raw, accountIndex, label), "%s: every chunk's account versions are found through the account index: %s", label, raw)
		}
	}
}

// The census reads the account's versions (named) and every version naming
// any account (anyNamed, through the index's leading column read as a range),
// both through the account index.
func TestAccountCensusReadsTheAccountThroughTheIndexOnly(t *testing.T) {
	db, concept, accountIndex := accountPlanFixture(t, "account census plan")
	for _, account := range []string{"acme", "beta"} {
		raw := explainAsTheReaderRuns(t, db, accountCensusSQL, "'"+account+"'", "'v1:accounts:account:"+account+"'", "ARRAY['"+concept+"']")
		require.Equal(t, 8, requireAccountThroughTheIndex(t, raw, accountIndex, "census "+account),
			"census %s: named and anyNamed each read every chunk through the account index: %s", account, raw)
	}
}

// The list of rows in no account decides from narrow columns: the versions
// naming any account through the account index, every chunk, and no payload of
// a versioned row evaluated on a heap tuple; no sequential scan of a chunk.
func TestUnplacedRowIDsReadTheAccountThroughTheIndexOnly(t *testing.T) {
	db, concept, accountIndex := accountPlanFixture(t, "unplaced rows plan")
	ids, err := UnplacedRowIDs(context.Background(), db.DB, concept, "", 50)
	require.NoError(t, err)
	require.Empty(t, ids, "fixture: every row's newest version names an account")
	for _, after := range []string{"''", "'" + concept + ":r1500'"} {
		raw := explainAsTheReaderRuns(t, db, unplacedRowIDsSQL, "'"+concept+"'", after, "50")
		require.Equal(t, 4, requireAccountThroughTheIndex(t, raw, accountIndex, "unplaced after "+after),
			"unplaced after %s: every chunk's named versions are found through the account index: %s", after, raw)
		requireNoChunkSeqScan(t, raw, "unplaced after "+after)
	}
}

// requireNoChunkSeqScan fails when the plan reads a chunk of the row table by a
// sequential scan.
func requireNoChunkSeqScan(t *testing.T, raw []byte, label string) {
	t.Helper()
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	var inspect func(plan map[string]any)
	inspect = func(plan map[string]any) {
		relation, _ := plan["Relation Name"].(string)
		if plan["Node Type"] == "Seq Scan" {
			require.NotContains(t, relation, "_chunk", "%s scans a chunk sequentially: %s", label, raw)
			require.NotEqual(t, "MemoryNodes", relation, "%s scans the row table sequentially: %s", label, raw)
		}
		children, _ := plan["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any))
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
}

// The probe that chooses the decision's shape counts the account's versions
// through the account index.
func TestAccountDecisionProbeReadsTheAccountThroughTheIndexOnly(t *testing.T) {
	db, concept, accountIndex := accountPlanFixture(t, "account probe plan")
	for _, account := range []string{"acme", "beta"} {
		raw := explainAsTheReaderRuns(t, db, accountDecisionProbeSQL, "'"+account+"'", "'v1:accounts:account:"+account+"'", "ARRAY['"+concept+"']", "2000")
		require.GreaterOrEqual(t, requireAccountThroughTheIndex(t, raw, accountIndex, "probe "+account), 1,
			"probe %s: the count reads the account index: %s", account, raw)
	}
}

// AccountRowHeads names no account; its plan must read each chunk through an
// index the id leads and read no version of another row (the hash join over
// the whole kind its comment records read every one).
func TestAccountRowHeadsReadOnlyTheirIds(t *testing.T) {
	db, concept, _ := accountPlanFixture(t, "account row heads plan")
	ids := make([]string, 0, 50)
	for r := 1; r <= 50; r++ {
		ids = append(ids, fmt.Sprintf("'%s:r%d'", concept, r*37))
	}
	// As AccountRowHeads runs it: on the pool, with the planner's own settings.
	plan := explainNewestByID(t, db, strings.NewReplacer("$2", "ARRAY["+strings.Join(ids, ",")+"]", "$1", "'"+concept+"'").Replace(accountRowHeadsSQL))
	require.Empty(t, plan.unprobed, "every chunk must be read through an index that finds the ids: %s", plan.raw)
	require.Zero(t, plan.discarded, "no version of another row may be read and thrown away: %s", plan.raw)
}
