package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// newest_version_by_id_plan_db_test.go -- the reads a write makes for the
// newest version of one id: the read-merge prior and the unchanged-content
// check (bunStore.QueryMemoryNodes) and both row version fence reads
// (fenceWriteTarget, checkRowVersionFences).

type newestByIDCaptureKey struct{}

type newestByIDCapture struct {
	mu      sync.Mutex
	queries []string
}

func (h *newestByIDCapture) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *newestByIDCapture) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if ctx.Value(newestByIDCaptureKey{}) != true || event.Err != nil && !errors.Is(event.Err, sql.ErrNoRows) ||
		!strings.HasPrefix(event.Query, "SELECT") || !strings.Contains(event.Query, `"createdAt" DESC`) && !strings.Contains(event.Query, `"createdAt" ASC`) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queries = append(h.queries, event.Query)
}

func (h *newestByIDCapture) take(t *testing.T, read string) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.queries, 1, "%s must read its row's versions in one statement", read)
	query := h.queries[0]
	h.queries = nil
	return query
}

// A newest-version read of one id must be planned the same whether
// TimescaleDB's ordered append is on or off, and must read each chunk through
// an index that finds the id. With ordered append on, ORDER BY "createdAt"
// DESC LIMIT 1 becomes an ordered walk of the chunks, newest first, costed by
// its first chunk alone, so nothing stops the planner from giving an older
// chunk a scan of its createdAt index that filters on the id: the cheapest scan
// to start, and a read of the whole chunk when the id has no version there. A
// row about to be created has a version nowhere. On an installation that read
// ran past the statement timeout, and no pipeline run could be created until
// ordered append was turned off for the engine's database role. This session
// keeps it on.
func TestNewestVersionReadsOfOneIDDoNotDependOnOrderedAppend(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "newest version by id plan", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("newestbyid_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema, "timescaledb.enable_ordered_append": "on"}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	// Four full days of 40 rows, each written 250 times, under long ids as an
	// installation's are, so an id index is a taller tree than the createdAt
	// one. The hot row is written 100 times a day and once more on a quiet
	// newest day that holds a few rows.
	const concept = "v1:todos:todo"
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stamp := base.Format(time.RFC3339)
	hot := concept + ":hot-" + strings.Repeat("0123456789abcdef", 6)
	missing := concept + ":about-to-be-created-" + strings.Repeat("0123456789abcdef", 6)
	newestHot := base.Add(4*24*time.Hour + time.Hour)
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + concept + `:history-'||d||'-'||r||'-'||repeat(md5(d||'-'||r),3),'` + concept + `',
           TIMESTAMPTZ '` + stamp + `'+d*interval '1 day'+r*interval '30 minutes'+v*interval '2 seconds','fixture','{}',jsonb_build_object('title','v'||v)
         FROM generate_series(0,3) d CROSS JOIN generate_series(0,39) r CROSS JOIN generate_series(0,249) v`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + hot + `','` + concept + `',TIMESTAMPTZ '` + stamp + `'+d*interval '1 day'+interval '20 hours'+v*interval '1 second','fixture','{}',jsonb_build_object('title','v'||v)
         FROM generate_series(0,3) d CROSS JOIN generate_series(0,99) v`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + concept + `:quiet-'||r,'` + concept + `',TIMESTAMPTZ '` + stamp + `'+interval '4 days'+r*interval '1 minute','fixture','{}','{}'
         FROM generate_series(0,9) r`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	newest := memorynodes.MemoryNode{ID: hot, Concept: concept, CreatedAt: newestHot, CreatedBy: "fixture", Payload: json.RawMessage(`{"title":"newest"}`)}
	_, err = db.NewInsert().Model(&newest).Exec(ctx)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `VACUUM ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	var chunks int
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_schema = ? AND hypertable_name = 'MemoryNodes'`, schema).Scan(&chunks))
	require.Equal(t, 5, chunks)

	_, err = LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(eng, memorynodes.DefaultRegistry()))
	hook := &newestByIDCapture{}
	db.AddQueryHook(hook)
	readCtx := context.WithValue(ctx, newestByIDCaptureKey{}, true)
	store := &bunStore{db: db}

	reads := []struct {
		name string
		read func(t *testing.T)
	}{
		{"the read-merge prior of a row about to be created", func(t *testing.T) {
			nodes, err := store.QueryMemoryNodes(readCtx, memorynodes.QueryParams{IDs: []string{missing}, Concept: concept, Limit: 1})
			require.NoError(t, err)
			require.Empty(t, nodes)
		}},
		{"the read-merge prior of a row with a long history", func(t *testing.T) {
			nodes, err := store.QueryMemoryNodes(readCtx, memorynodes.QueryParams{IDs: []string{hot}, Concept: concept, Limit: 1})
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			require.True(t, nodes[0].CreatedAt.Equal(newestHot), "the newest version, not %v", nodes[0].CreatedAt)
		}},
		{"the oldest-first read of a row with a long history", func(t *testing.T) {
			nodes, err := store.QueryMemoryNodes(readCtx, memorynodes.QueryParams{IDs: []string{hot}, Concept: concept, Limit: 1, Order: memorynodes.QueryOrderCreatedAtAsc})
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			require.True(t, nodes[0].CreatedAt.Equal(base.Add(20*time.Hour)), "the oldest version, not %v", nodes[0].CreatedAt)
		}},
		{"the write target fence of a row about to be created", func(t *testing.T) {
			fenced, err := eng.fenceWriteTarget(readCtx, concept, missing)
			require.NoError(t, err)
			require.True(t, rowVersionFences(fenced)[0].version.IsZero())
		}},
		{"the write target fence of a row with a long history", func(t *testing.T) {
			fenced, err := eng.fenceWriteTarget(readCtx, concept, hot)
			require.NoError(t, err)
			require.True(t, rowVersionFences(fenced)[0].version.Equal(newestHot))
		}},
		{"the commit fence of a row about to be created", func(t *testing.T) {
			require.NoError(t, db.RunInTx(readCtx, nil, func(ctx context.Context, tx bun.Tx) error {
				return eng.checkRowVersionFences(ContextWithRowVersionFence(ctx, concept, missing, time.Time{}), tx)
			}))
		}},
		{"the commit fence of a row with a long history", func(t *testing.T) {
			require.NoError(t, db.RunInTx(readCtx, nil, func(ctx context.Context, tx bun.Tx) error {
				return eng.checkRowVersionFences(ContextWithRowVersionFence(ctx, concept, hot, newestHot), tx)
			}))
		}},
		{"a stale commit fence of a row with a long history", func(t *testing.T) {
			stale := db.RunInTx(readCtx, nil, func(ctx context.Context, tx bun.Tx) error {
				return eng.checkRowVersionFences(ContextWithRowVersionFence(ctx, concept, hot, newestHot.Add(-time.Hour)), tx)
			})
			require.ErrorIs(t, stale, ErrRowVersionChanged)
		}},
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			read.read(t)
			query := hook.take(t, read.name)
			require.Equal(t, newestByIDPlanShape(t, db, query, false), newestByIDPlanShape(t, db, query, true),
				"the plan must not change with ordered append: %s", query)
			plan := explainNewestByID(t, db, query)
			require.False(t, plan.orderedChunkWalk, "an ordered chunk walk costs only its first chunk: %s", plan.raw)
			require.Equal(t, chunks, plan.chunkScans, "every chunk is probed once: %s", plan.raw)
			require.Empty(t, plan.unprobed, "every chunk must be read through an index that finds the id: %s", plan.raw)
			require.Zero(t, plan.discarded, "no version of another row may be read and thrown away: %s", plan.raw)
		})
	}
}

// newestByIDPlan is what a plan of a newest-version read did. A chunk is
// counted at its scan, not at a TimescaleDB Custom Scan wrapping it.
type newestByIDPlan struct {
	orderedChunkWalk bool     // a ChunkAppend that walks the chunks in time order
	chunkScans       int      // chunk scans in the plan
	unprobed         []string // chunk scans whose index condition does not name the id
	discarded        int      // chunk rows a filter read and threw away
	raw              string
}

func explainNewestByID(t *testing.T, db *bun.DB, query string) newestByIDPlan {
	t.Helper()
	var raw []byte
	require.NoError(t, db.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE,FORMAT JSON) `+query).Scan(&raw))
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	out := newestByIDPlan{raw: string(raw)}
	var inspect func(plan map[string]any)
	inspect = func(plan map[string]any) {
		nodeType, _ := plan["Node Type"].(string)
		provider, _ := plan["Custom Plan Provider"].(string)
		order, _ := plan["Order"].([]any)
		if provider == "ChunkAppend" && len(order) > 0 {
			out.orderedChunkWalk = true
		}
		relation, _ := plan["Relation Name"].(string)
		if strings.Contains(relation, "_chunk") && nodeType != "Custom Scan" {
			out.chunkScans++
			cond, _ := plan["Index Cond"].(string)
			if recheck, ok := plan["Recheck Cond"].(string); ok {
				cond = recheck
			}
			if !strings.Contains(cond, "(id = ") {
				out.unprobed = append(out.unprobed, relation+": "+nodeType+" "+cond)
			}
			removed, _ := plan["Rows Removed by Filter"].(float64)
			loops, _ := plan["Actual Loops"].(float64)
			out.discarded += int(removed * loops)
		}
		children, _ := plan["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any))
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	return out
}

func newestByIDPlanShape(t *testing.T, db *bun.DB, query string, orderedAppend bool) string {
	t.Helper()
	setting := "off"
	if orderedAppend {
		setting = "on"
	}
	var lines []string
	require.NoError(t, db.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT set_config('timescaledb.enable_ordered_append', ?, true)`, setting); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `EXPLAIN (COSTS OFF) `+query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			lines = append(lines, line)
		}
		return rows.Err()
	}))
	return strings.Join(lines, "\n")
}
