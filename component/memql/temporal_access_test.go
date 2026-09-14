package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// temporal_access_test.go proves the temporal-access (`asOf`) visibility
// rule from the core-builtins ADR §2.3 (story memql#2305):
//   - a query reading `asOf latest` is marked time-dependent on its
//     loaded contract (Function.LatestMode); `asOf <explicit timestamp>`
//     is deterministic and is NOT marked;
//   - `asOf` used outside a query (a spec body here) is a load error.

func temporalLoadRegistry(t *testing.T) memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cluster:node": fixtureConcept(t, "v1:cluster:node", "concept node {\n  active  bool\n}\n"),
	})
}

func loadTemporalQuery(t *testing.T, name, asOfClause string) *Function {
	t.Helper()
	// An `asOf args.asOf ?? latest` clause READS an argument, so the fixture
	// has to declare it -- exactly as the live query carrying that clause does
	// (deploymentsForCluster: `args { clusterId string!  asOf datetime }`).
	// Undeclared, it is refused at load (memql#3626).
	argsBlock := ""
	if strings.Contains(asOfClause, "args.asOf") {
		argsBlock = "  args {\n    asOf  datetime\n  }\n"
	}
	src := "use cluster.concepts.{ node }\n\n" +
		"@enabled\n" +
		"query node " + name + " {\n" +
		argsBlock +
		"  " + asOfClause + "\n" +
		"  filter  row => row.active == true\n" +
		"  shape   nodeCard\n" +
		"}"
	fn, err := tryParseNewFunctionSyntax(name, "query", src, "cluster.queries.memql", temporalLoadRegistry(t))
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
		"  filter  row => row.active == true\n" +
		"  shape   nodeCard\n" +
		"}"
	fn, err := tryParseNewFunctionSyntax("queryPlainNodes", "query", src, "cluster.queries.memql", temporalLoadRegistry(t))
	require.NoError(t, err)
	require.False(t, fn.LatestMode)
}

// TestSpecRejectsAsOf: a spec body is an atomic boolean predicate, not a
// temporal read -- `asOf` is a load error. The path every v1 spec loads
// through, the shared parser, refuses it with the query-only message a logic
// body gets, at the author's `asOf` (memql#5364). Lower at the spec-body
// position stays the backstop for a lambda no declaration parsed -- the
// context-free parse this test builds one with -- and names asOf for what it
// is there too, a query clause and not a function, rather than refusing it as
// a predicate of the wrong arity with `asOf(row)` as the fix.
func TestSpecRejectsAsOf(t *testing.T) {
	src := `@description("Boom: asOf in a spec body.")
spec thing specReadsAsOf = row => asOf(row.active == true, latest)`
	_, err := languageParser.ParseSpecDecl(src)
	require.Error(t, err, "asOf in a spec body must not parse")
	require.Contains(t, err.Error(), "`asOf` is a query-only clause and cannot appear in a spec body")
	require.Contains(t, err.Error(), "line 2, column 35:")

	lam, err := languageParser.ParseV1Lambda("row => asOf(row.active == true, latest)")
	require.NoError(t, err)
	_, err = Lower(lam.Body, LowerEnv{
		Position:  tiers.PositionSpecBody,
		Param:     lam.Params[0],
		Predicate: (&MemQLEngine{specs: newSpecRegistry()}).predicateLookup(),
	})
	require.Error(t, err, "asOf in a spec body must not lower")
	require.Contains(t, err.Error(), "does not lower in a spec or trait body")
	require.Contains(t, err.Error(), "asOf(")
	require.Contains(t, err.Error(), "`asOf` is a query clause, not a function")
	require.Contains(t, err.Error(), "`asOf args.at`", "the fix is the query's own clause")
}

// The ruling that authorised `asOf args.X ?? latest` rested on one property:
// omit the argument and behaviour is byte-identical to `asOf latest`, so the
// six queries carrying that clause adopt the form with no migration and no
// behaviour change (memql#2992).
//
// LatestMode is part of that behaviour. It is the consumer-facing statement
// that a result is clock-dependent and NOT reproducible, and it is computed at
// LOAD time from the UNEXPANDED node -- long before resolveAsOfArg decides
// which branch a given call takes. So it must describe what the query MAY do.
// Reading only UseLatest flipped deploymentsForCluster's marker from
// time-dependent to deterministic while it still read the live tip for every
// caller that omits the argument, which is all of them today.
//
// This pins the equivalence directly, so the remaining five queries can adopt
// the form without each silently dropping its contract marker.
func TestAsOfArgWithLatestFallbackKeepsTheLatestContract(t *testing.T) {
	literal := loadTemporalQuery(t, "qLiteralLatest", "asOf    latest")
	fallback := loadTemporalQuery(t, "qArgFallbackLatest", "asOf    args.asOf ?? latest")

	if !literal.LatestMode {
		t.Fatal("baseline broken: `asOf latest` must mark the contract time-dependent")
	}
	if fallback.LatestMode != literal.LatestMode {
		t.Errorf("`asOf args.asOf ?? latest` has LatestMode=%v but `asOf latest` has %v.\n"+
			"With the argument omitted the two are the same read -- the live tip -- so the "+
			"contract marker must agree. It is computed at load from the unexpanded node, so it "+
			"has to describe what the query MAY do, not what one call did. A consumer reading "+
			"the loaded contract would be told this result is reproducible when it is not "+
			"(memql#2992).", fallback.LatestMode, literal.LatestMode)
	}

	// ...and an explicit instant is still deterministic, so the fix cannot be
	// satisfied by marking everything time-dependent.
	pinned := loadTemporalQuery(t, "qPinnedInstant", `asOf    "2026-07-28T12:00:00Z"`)
	if pinned.LatestMode {
		t.Error("`asOf <explicit timestamp>` is reproducible and must NOT be marked time-dependent")
	}
}
