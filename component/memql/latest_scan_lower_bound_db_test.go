package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// latest_scan_lower_bound_db_test.go -- the latest-key enumeration narrowed by
// a row.createdAt lower bound (latestScanLowerBounds, latest_scan_keys.go).

func irCreatedAt(op ComparisonOperator, value any) *ComparisonExpression {
	return &ComparisonExpression{Field: FieldReference{Raw: "createdAt", Parts: []string{"createdAt"}}, Operator: op, Value: value}
}

func irAnd(left, right ExpressionNode) ExpressionNode {
	return &LogicalExpression{Op: LogicalAnd, Left: left, Right: right}
}

// The bound is pushed for exactly one shape: a top-level AND conjunct
// `createdAt >= <plan constant>`. Every other shape -- under OR or NOT, inside
// a wrapper, another operator, a value that is not a constant instant --
// enumerates exactly the keys it did before, byte for byte.
func TestLatestScanKeysPushesOnlyATopLevelCreatedAtLowerBound(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	concept := irConceptEq(keysetConcept)
	const since = "2026-11-01T00:00:00Z"
	const bound = `("createdAt" >= '2026-11-01 00:00:00+00:00')`
	plain := eng.latestScanKeys(concept, nil)
	require.NotNil(t, plain)
	require.NotContains(t, plain.String(), `"createdAt" >=`)

	// The email statistics walk's shape: the concept, the bound, and a row
	// predicate beside them, the bound written in either order.
	for name, expr := range map[string]ExpressionNode{
		"the walk's shape":       irAnd(irAnd(concept, irCreatedAt(OpGe, since)), irPayloadCmp("status", OpEq, "active")),
		"the bound written last": irAnd(concept, irAnd(irPayloadCmp("status", OpEq, "active"), irCreatedAt(OpGe, since))),
	} {
		t.Run(name, func(t *testing.T) {
			keys := eng.latestScanKeys(expr, nil)
			require.NotNil(t, keys)
			require.Contains(t, keys.String(), bound)
			require.NotContains(t, keys.String(), "payload", "the enumeration still reads no historical JSON")
		})
	}
	asOf := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	keys := eng.latestScanKeys(irAnd(concept, irCreatedAt(OpGe, since)), &asOf)
	require.Contains(t, keys.String(), bound)
	require.Contains(t, keys.String(), `"createdAt" <= '2026-11-05 00:00:00+00:00'`, "an asOf read keeps its own bound")

	for name, expr := range map[string]ExpressionNode{
		"under an OR":             irAnd(concept, &LogicalExpression{Op: LogicalOr, Left: irCreatedAt(OpGe, since), Right: irPayloadCmp("status", OpEq, "active")}),
		"under a NOT":             irAnd(concept, irNot(irCreatedAt(OpGe, since))),
		"inside a relationship":   irAnd(concept, &RelationshipExpression{Function: RelOwns, Target: irCreatedAt(OpGe, since)}),
		"strictly after":          irAnd(concept, irCreatedAt(OpGt, since)),
		"an upper bound":          irAnd(concept, irCreatedAt(OpLe, since)),
		"before":                  irAnd(concept, irCreatedAt(OpLt, since)),
		"an equality":             irAnd(concept, irCreatedAt(OpEq, since)),
		"against another field":   irAnd(concept, irCreatedAt(OpGe, &FieldOperand{Field: irPayloadField("since")})),
		"an unevaluated constant": irAnd(concept, irCreatedAt(OpGe, irPlanConstReading("args"))),
		"a number":                irAnd(concept, irCreatedAt(OpGe, 5)),
		"text that is no instant": irAnd(concept, irCreatedAt(OpGe, "yesterday")),
		"a payload field":         irAnd(concept, irPayloadCmp("createdAt", OpGe, since)),
	} {
		t.Run(name, func(t *testing.T) {
			keys := eng.latestScanKeys(expr, nil)
			require.NotNil(t, keys)
			require.Equal(t, plain.String(), keys.String(), "a shape other than the one bound must enumerate as it did")
		})
	}
}

// At production's planner settings, over ten one-day chunks, a walk since a
// watermark collapses only the versions at or after it: the chunks before the
// bound are excluded, the enumeration reads only the index entries the bound
// keeps -- and sorts no more than those, if the planner ranges the createdAt
// index rather than reading the covering one in order -- and a payload is read
// only for the keys the bound keeps.
// It returns exactly the rows the full collapse returns -- an id whose latest
// version is after the bound though older ones are before it, an id whose
// latest is AT the bound, never an id whose versions are all before it -- and
// the bound inside an OR is not pushed, so an id the OR's other arm keeps is
// still found.
//
// Production's settings, because a plan decided at the timescaledb-ha default
// of random_page_cost 1.1 over a few chunks is not production's
// (latest_scan_walk.go, "THE QUERY'S SHAPE IS WHAT MAKES THE PLANNER WALK").
func TestLatestScanLowerBoundReadsOnlyTheVersionsAfterIt(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "latest scan lower bound plan", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("lowerbound_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema,
			"random_page_cost": "4", "effective_cache_size": "3GB", "work_mem": "8MB"}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	// 2,000 ids of five versions a minute apart, one id every seven minutes:
	// 10,000 versions over ten days, each carrying a 1,280-character detail as
	// a payload worth not reading does. The bound is half a day before the
	// last version: the last 46 ids have their latest version at or after it,
	// and id 1955 straddles it, two versions before, one exactly at it, two
	// after. Four more ids pin the edges the bound could get wrong.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	boundAt := base.Add(9*24*time.Hour + 12*time.Hour)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }
	const prefix = keysetConcept + ":"
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT '` + prefix + `bulk-'||n,'` + keysetConcept + `',TIMESTAMPTZ '` + stamp(base) + `'+(n-1)*interval '7 minutes'+v*interval '1 minute','fixture','{}',
           jsonb_build_object('seq',n,'flag',false,'detail',repeat(md5(n||'-'||v),40))
         FROM generate_series(1,2000) n CROSS JOIN generate_series(0,4) v`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	edges := []struct {
		id       string
		versions []time.Duration // from the bound, oldest first
		flag     bool            // the latest version's flag
	}{
		{"edge-equal", []time.Duration{-2 * time.Hour, 0}, false},
		{"edge-straddle", []time.Duration{-3 * time.Hour, -time.Hour, time.Hour}, false},
		{"edge-before", []time.Duration{-3 * time.Hour, -time.Hour}, false},
		{"edge-flag", []time.Duration{-2 * time.Hour, -time.Hour}, true},
	}
	var nodes []memorynodes.MemoryNode
	for _, edge := range edges {
		for i, offset := range edge.versions {
			payload, err := json.Marshal(map[string]any{"seq": edge.id, "flag": edge.flag && i == len(edge.versions)-1})
			require.NoError(t, err)
			nodes = append(nodes, memorynodes.MemoryNode{ID: prefix + edge.id, Concept: keysetConcept, CreatedAt: boundAt.Add(offset),
				CreatedBy: "fixture", Payload: payload})
		}
	}
	_, err = db.NewInsert().Model(&nodes).Exec(ctx)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `VACUUM ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	var chunks int
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_schema = ? AND hypertable_name = 'MemoryNodes'`, schema).Scan(&chunks))
	require.GreaterOrEqual(t, chunks, 10, "the history must span production's ten chunks")

	_, err = LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(eng, memorynodes.DefaultRegistry()))
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	readCtx := context.WithValue(clusterOwnerCtx("lower-bound-owner"), latestPlanCaptureKey{}, true)

	// What the full collapse returns: every id whose LATEST version is at or
	// after the bound, at that version.
	want := map[string]time.Time{
		prefix + "edge-equal":    boundAt,
		prefix + "edge-straddle": boundAt.Add(time.Hour),
	}
	for n := 1; n <= 2000; n++ {
		latest := base.Add(time.Duration(n-1)*7*time.Minute + 4*time.Minute)
		if !latest.Before(boundAt) {
			want[fmt.Sprintf("%sbulk-%d", prefix, n)] = latest
		}
	}
	require.Len(t, want, 48)
	read := func(query string) (map[string]time.Time, string) {
		hook.mu.Lock()
		hook.queries = nil
		hook.mu.Unlock()
		res, err := eng.Execute(readCtx, query)
		require.NoError(t, err)
		got := map[string]time.Time{}
		for _, node := range res.Bundle.GetNodes() {
			got[node.GetId()] = node.GetCreatedAt().AsTime()
		}
		hook.mu.Lock()
		defer hook.mu.Unlock()
		for _, q := range hook.queries {
			if strings.Contains(q, "latest_keys") {
				return got, q
			}
		}
		t.Fatalf("no latest-key scan was captured for %s", query)
		return nil, ""
	}
	quote := func(at time.Time) string { return `"` + at.Format(time.RFC3339) + `"` }
	// Each read names its page. An unmarked read takes the engine's default
	// list cap, which the environment configures, and a cap below the 49
	// rows here would cut the oldest of them off the page.

	got, scan := read(fmt.Sprintf(`paginate(concept==%s && createdAt>=%s, 100)`, keysetConcept, quote(boundAt)))
	require.Equal(t, lowerBoundIDs(want), lowerBoundIDs(got), "the bound must return exactly the full collapse's rows")
	for id, at := range want {
		require.True(t, got[id].Equal(at), "%s must be read at its latest version %v, not %v", id, at, got[id])
	}
	keys := scan[:strings.Index(scan, "AS latest_keys")]
	require.Contains(t, keys, `"createdAt" >= '2026-01-10 12:00:00+00:00'`, "the bound must narrow the key enumeration")

	plan := explainLatestScan(t, db, scan)
	require.LessOrEqual(t, plan.keyChunks, 2, "the chunks wholly before the bound must be excluded: %s", plan.raw)
	require.LessOrEqual(t, plan.keyRows, 300, "the enumeration must read only the versions at or after the bound (230), not the history: %s", plan.raw)
	require.LessOrEqual(t, plan.keySortRows, 300, "a sort under the collapse may order only what the bound kept: %s", plan.raw)
	require.LessOrEqual(t, plan.payloadProbes, 60, "a payload must be read only for the keys the bound keeps (48): %s", plan.raw)

	// The same bound under an OR is not required, so it narrows nothing, and
	// the id whose latest version the OR's other arm keeps is found.
	got, scan = read(fmt.Sprintf(`paginate(concept==%s && (createdAt>=%s || payload.flag==true), 100)`, keysetConcept, quote(boundAt)))
	orWant := map[string]time.Time{prefix + "edge-flag": boundAt.Add(-time.Hour)}
	for id, at := range want {
		orWant[id] = at
	}
	require.Equal(t, lowerBoundIDs(orWant), lowerBoundIDs(got))
	require.NotContains(t, scan[:strings.Index(scan, "AS latest_keys")], `"createdAt" >=`, "a bound under an OR must not narrow the enumeration")
	plan = explainLatestScan(t, db, scan)
	require.GreaterOrEqual(t, plan.keyChunks, 10, "without the bound the enumeration reads every chunk: %s", plan.raw)
	require.GreaterOrEqual(t, plan.payloadProbes, 2000, "without the bound a payload is read for every id of the history: %s", plan.raw)
}

func lowerBoundIDs(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// latestScanPlan is what a plan of the latest-key scan read. The key
// enumeration is the subtree under the collapse (the Unique node). A chunk is
// counted at its scan, not at a TimescaleDB Custom Scan wrapping it (SkipScan,
// ChunkAppend), which names the same relation.
type latestScanPlan struct {
	keyChunks     int // chunks the key enumeration scanned
	keyRows       int // index entries its chunk scans returned
	keySortRows   int // the most rows a sort under the collapse ordered
	payloadProbes int // payload reads, summed over loops
	raw           string
}

func explainLatestScan(t *testing.T, db *bun.DB, query string) latestScanPlan {
	t.Helper()
	var raw []byte
	require.NoError(t, db.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE,VERBOSE,BUFFERS,FORMAT JSON) `+query).Scan(&raw))
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	out := latestScanPlan{raw: string(raw)}
	var inspect func(plan map[string]any, underUnique bool)
	inspect = func(plan map[string]any, underUnique bool) {
		relation, _ := plan["Relation Name"].(string)
		nodeType, _ := plan["Node Type"].(string)
		rows, _ := plan["Actual Rows"].(float64)
		loops, _ := plan["Actual Loops"].(float64)
		if strings.Contains(relation, "_chunk") && loops > 0 && nodeType != "Custom Scan" {
			switch {
			case latestPlanOutputsPayload(plan):
				out.payloadProbes += int(loops)
			case underUnique:
				out.keyChunks++
				out.keyRows += int(rows * loops)
			}
		}
		if underUnique && nodeType == "Sort" && int(rows) > out.keySortRows {
			out.keySortRows = int(rows)
		}
		children, _ := plan["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any), underUnique || nodeType == "Unique")
		}
	}
	inspect(plans[0]["Plan"].(map[string]any), false)
	return out
}
