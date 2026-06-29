package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// temporal_access_test.go proves the temporal-access (`asOf`) visibility
// rule from the core-builtins ADR §2.3 (story memql#2305):
//   - a query reading `asOf latest` is marked time-dependent on its
//     loaded contract (Function.LatestMode); `asOf <explicit timestamp>`
//     is deterministic and is NOT marked;
//   - `asOf` used outside a query (a spec body here) is a load error.

func temporalLoadRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cluster:node": {Name: "v1:cluster:node"},
	})
}

func loadTemporalQuery(t *testing.T, name, asOfClause string) *Function {
	t.Helper()
	src := "use cluster.concepts.{ node }\n\n" +
		"@enabled\n" +
		"query node " + name + " {\n" +
		"  " + asOfClause + "\n" +
		"  filter  payload.active == true\n" +
		"  shape   nodeCard\n" +
		"}"
	fn, err := tryParseNewFunctionSyntax(name, "query", src, "cluster.queries.memql", temporalLoadRegistry())
	require.NoError(t, err)
	require.NotNil(t, fn)
	return fn
}

// TestQueryAsOfLatestMarksContract: `asOf latest` -> the query's loaded
// metadata is flagged time-dependent.
func TestQueryAsOfLatestMarksContract(t *testing.T) {
	fn := loadTemporalQuery(t, "queryLiveNodes", "asOf    latest")
	require.True(t, fn.LatestMode, "query with `asOf latest` must be marked time-dependent (LatestMode)")
}

// TestQueryAsOfTimestampNotMarked: `asOf <explicit timestamp>` is
// deterministic (immutable historical state) -> NOT marked.
func TestQueryAsOfTimestampNotMarked(t *testing.T) {
	fn := loadTemporalQuery(t, "queryNodesAt", `asOf    "2026-01-01T00:00:00Z"`)
	require.False(t, fn.LatestMode, "query with `asOf <timestamp>` is deterministic and must NOT be marked")
}

// TestQueryNoAsOfNotMarked: a plain query (no temporal clause) is not
// time-dependent.
func TestQueryNoAsOfNotMarked(t *testing.T) {
	src := "use cluster.concepts.{ node }\n\n" +
		"@enabled\n" +
		"query node queryPlainNodes {\n" +
		"  filter  payload.active == true\n" +
		"  shape   nodeCard\n" +
		"}"
	fn, err := tryParseNewFunctionSyntax("queryPlainNodes", "query", src, "cluster.queries.memql", temporalLoadRegistry())
	require.NoError(t, err)
	require.False(t, fn.LatestMode)
}

// TestSpecRejectsAsOf: a spec body is an atomic boolean predicate, not a
// temporal read -- `asOf` is a load error.
func TestSpecRejectsAsOf(t *testing.T) {
	src := []byte(`@description("Boom: asOf in a spec body.")
spec specReadsAsOf {
  asOf(payload.active == true, latest)
}`)
	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected a load error for asOf in a spec body, got nil")
	}
	if !strings.Contains(err.Error(), "query-only") {
		t.Fatalf("expected query-only error, got: %v", err)
	}
}
