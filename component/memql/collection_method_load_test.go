package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func collectionLoadRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:common:thing": {Name: "v1:common:thing"},
	})
}

// TestLogicCollectionMethodLoads proves a single-statement logic body using
// the Story 4 (#2302 / ADR §2.2) collection surface loads end-to-end
// through the real function loader (struct rewriter -> langparser -> AST
// converter) into a CollectionMethodExpression fn.Expr.
func TestLogicCollectionMethodLoads(t *testing.T) {
	src := strings.Join([]string{
		"@enabled",
		"@description(\"count active members\")",
		"logic logicCountActiveMembers {",
		"  args {",
		"    members object @required",
		"  }",
		"  body {",
		"    return args.members.where(m => m.active).count()",
		"  }",
		"}",
	}, "\n")

	fn, err := tryParseNewFunctionSyntax("logicCountActiveMembers", "logic", src, "common.logic.memql", collectionLoadRegistry())
	if err != nil {
		t.Fatalf("load logic with collection method: %v", err)
	}
	if fn == nil || fn.Expr == nil {
		t.Fatalf("expected fn.Expr to be set")
	}
	if _, ok := fn.Expr.(*CollectionMethodExpression); !ok {
		t.Fatalf("fn.Expr = %T, want *CollectionMethodExpression", fn.Expr)
	}
}

// TestQueryFilterCollectionMethodRejected proves the scope rule end-to-end:
// a query filter using a collection method fails to load with the scope
// error (queries go through the default AST converter, which rejects the
// surface).
func TestQueryFilterCollectionMethodRejected(t *testing.T) {
	src := strings.Join([]string{
		"use common.concepts.{ thing }",
		"",
		"@enabled",
		"@description(\"bad query\")",
		"query thing queryBadCollection {",
		"  filter args.members.any(m => m.active)",
		"  shape thing",
		"}",
	}, "\n")

	_, err := tryParseNewFunctionSyntax("queryBadCollection", "query", src, "common.queries.memql", collectionLoadRegistry())
	if err == nil {
		t.Fatalf("expected query filter with collection method to be rejected")
	}
	if !strings.Contains(err.Error(), "not allowed in specs or query filters") &&
		!strings.Contains(err.Error(), "collection methods") {
		t.Fatalf("unexpected error %q; want scope rejection", err.Error())
	}
}
