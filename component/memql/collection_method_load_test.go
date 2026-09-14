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

// TestQueryFilterCollectionMethodRejected proves the scope rule end-to-end,
// as edition 2026 draws it. The legacy converter refused the Story 4
// collection surface in a query filter outright. Edition 2026 splits it by
// what the method reads (memql#5366): over the ROW's list, a method with no
// SQL form runs in process, and a filter that would run it per row is refused
// at load, naming the lowering that does exist; over an ARGUMENT it reads no
// row, so it is a plan constant, evaluated once per call, and loads.
func TestQueryFilterCollectionMethodRejected(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:common:thing": declaredConcept(t, "v1:common:thing", "  tags  []string"),
	})
	query := func(filter string) string {
		return strings.Join([]string{
			"use common.concepts.{ thing }",
			"",
			"@enabled",
			"@description(\"collection query\")",
			"query thing queryCollection {",
			// `members` is declared so the SCOPE rule is what this fixture
			// exercises. Undeclared, it trips the used-requires-declared args
			// check first (memql#3626) and the test would pass on the wrong
			// rejection; declared and unread, it trips the other half.
			"  args {",
			"    members []object",
			"  }",
			"  filter " + filter,
			"  paginate 20",
			"}",
		}, "\n")
	}

	_, err := tryParseNewFunctionSyntax("queryCollection", "query",
		query(`row => row.tags.where(t => t == "urgent").count() > 0 && args.members != nil`), "common.queries.memql", registry)
	if err == nil {
		t.Fatalf("expected a query filter running a collection method over the row to be rejected")
	}
	for _, want := range []string{
		"does not lower in a query filter",
		"`.where()` runs in process over the row's list",
		"`row.tags.any(t => t == \"urgent\")`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("unexpected error %q; want the lowering refusal naming %q", err.Error(), want)
		}
	}

	fn, err := tryParseNewFunctionSyntax("queryCollection", "query",
		query(`row => args.members.any(m => m.active) && row.tags.any(t => t == "urgent")`), "common.queries.memql", registry)
	if err != nil {
		t.Fatalf("a collection method over an argument is a plan constant and must load: %v", err)
	}
	if !treeHasPlanConstant(fn.Expr) {
		t.Fatalf("the argument's collection method did not lower to a plan constant: %s", canonicalExpression(unwrapToFilter(fn.Expr)))
	}
}
