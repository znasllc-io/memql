package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

func collectionLoadRegistry(t *testing.T) memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:common:thing": fixtureConcept(t, "v1:common:thing", "concept thing {\n  tags  []string\n}\n"),
	})
}

// TestLogicCollectionMethodLoads proves a logic returning the Story 4 (#2302 /
// ADR §2.2) collection surface loads end-to-end through the real function
// loader. Its compiled body is the return, whose value the LogicRunner parses
// and evaluates with EvalExpr -- and that value answers the count the
// collection surface promises.
func TestLogicCollectionMethodLoads(t *testing.T) {
	src := strings.Join([]string{
		"@enabled",
		"@description(\"count active members\")",
		"logic logicCountActiveMembers {",
		"  args {",
		"    members object @required",
		"  }",
		"  return args.members.where(m => m.active).count()",
		"}",
	}, "\n")

	fn, err := tryParseNewFunctionSyntax("logicCountActiveMembers", "logic", src, "common.logic.memql", collectionLoadRegistry(t))
	require.NoError(t, err, "load logic with collection method")
	ret := statementReturnExpr(t, fn)
	require.Equal(t, "args.members.where(m => m.active).count()", ast.FormatExpr(ret))

	members := []any{map[string]any{"active": true}, map[string]any{"active": false}, map[string]any{"active": true}}
	got, err := EvalExpr(context.Background(), ret, MapScope{"args": map[string]any{"members": members}}, EvalOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(2), got)
}

// TestQueryFilterCollectionMethodScope proves the scope rule end-to-end, as
// the edition-2026 tier manifest draws it: a query filter pushes down, so a
// collection method that runs in process over the ROW's list is refused at
// load, naming the pushdown spelling -- while the methods that lower over a
// row array (any, all, count) load, and a method over an argument is a plan
// constant, evaluated once per call. (Before the flip every collection
// method was refused in a filter, whatever it read.)
func TestQueryFilterCollectionMethodScope(t *testing.T) {
	load := func(args, filter string) error {
		lines := []string{"use common.concepts.{ thing }", "", "@enabled", "@description(\"probe\")", "query thing queryCollectionScope {"}
		if args != "" {
			lines = append(lines, "  args {", "    "+args, "  }")
		}
		lines = append(lines, "  filter "+filter, "  shape thing", "}")
		_, err := tryParseNewFunctionSyntax("queryCollectionScope", "query", strings.Join(lines, "\n"), "common.queries.memql", collectionLoadRegistry(t))
		return err
	}

	err := load("", `row => row.tags.where(t => t == "x").count() > 0`)
	require.Error(t, err, "an in-process collection method over the row must not load in a filter")
	require.Contains(t, err.Error(), "does not lower in a query filter")
	require.Contains(t, err.Error(), "runs in process")

	require.NoError(t, load("", `row => row.tags.any(t => t == "x")`), "any() over a row array lowers to SQL")
	require.NoError(t, load("members  []object", `row => args.members.any(m => m.active)`),
		"a method over an argument is a plan constant, evaluated once per call")
}
