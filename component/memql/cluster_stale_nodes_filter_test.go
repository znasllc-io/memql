package memql

// cluster_stale_nodes_filter_test.go -- regression coverage for memql#1642:
// staleClusterNodes must push its `olderThan` arg down onto a
// `payload.lastSeen < olderThan` filter comparison. Before the fix the query
// took no args and applied no lastSeen predicate, so it returned the identical
// node set for every threshold (e.g. olderThan=2026 and olderThan=2020 both
// returned all 47 nodes).
//
// Parsing the named query runs resolvePlanFunctions, which binds the call args
// into the loaded filter tree (the exact path engine.Execute takes before SQL
// compile) and unwraps the `when(args.olderThan) { ... }` guard. We then walk
// the comparison tree to prove the lastSeen<olderThan predicate is present when
// olderThan is supplied -- and absent when it is not (the prune cron calls it
// arg-free, so that path must stay an unfiltered latest-per-id scan).

import (
	"io"
	"log/slog"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/stretchr/testify/require"
)

func loadedStaleNodesEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := memoryNodes.DefaultRegistry()
	require.NotNil(t, registry)
	eng, err := New(nil)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	return eng
}

// TestQueryStaleClusterNodes_OlderThanPushdown: with olderThan supplied, the
// parsed query carries a `payload.lastSeen < <olderThan>` comparison bound to
// the passed value.
func TestQueryStaleClusterNodes_OlderThanPushdown(t *testing.T) {
	eng := loadedStaleNodesEngine(t)

	const cutoff = "2020-01-01T00:00:00Z"
	plan, err := eng.Parse(`staleClusterNodes(olderThan: "` + cutoff + `")`)
	require.NoError(t, err)
	require.NotNil(t, plan.Root)

	var lastSeenCmp *ComparisonExpression
	walkComparisonExpressions(plan.Root, func(c *ComparisonExpression) {
		if c.Field.Raw == "payload.lastSeen" {
			lastSeenCmp = c
		}
	})

	require.NotNil(t, lastSeenCmp,
		"staleClusterNodes must apply a payload.lastSeen predicate when olderThan is supplied (issue #1642)")
	require.Equal(t, OpLt, lastSeenCmp.Operator,
		"the lastSeen predicate must be a strict '<' comparison against olderThan")
	require.Equal(t, cutoff, lastSeenCmp.Value,
		"the lastSeen comparison must bind to the supplied olderThan value")
}

// TestQueryStaleClusterNodes_NoOlderThan_DropsGuard: called arg-free (the prune
// cron path), the when-guard drops, so NO lastSeen predicate remains -- the
// query stays an unfiltered latest-per-id scan.
func TestQueryStaleClusterNodes_NoOlderThan_DropsGuard(t *testing.T) {
	eng := loadedStaleNodesEngine(t)

	plan, err := eng.Parse(`staleClusterNodes()`)
	require.NoError(t, err)
	require.NotNil(t, plan.Root)

	walkComparisonExpressions(plan.Root, func(c *ComparisonExpression) {
		if c.Field.Raw == "payload.lastSeen" {
			t.Errorf("arg-free staleClusterNodes must not carry a lastSeen predicate (when-guard should drop), got %#v", c)
		}
	})
}
