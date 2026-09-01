package memql

import (
	"context"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// probeArgLhsSrc is dsl/accounts' clientAccountsAll archive term reduced to the
// one shape it introduced: a caller-supplied flag that WIDENS a read, and
// therefore sits on the LEFT of a comparison because there is no row field to
// put there.
//
// The row half is written `payload.status` rather than the bare `status` the
// tree uses, because the bare-property rewrite (rewriteFilterFieldRefs) runs
// LATER in the plan pipeline than the argument expansion under test -- a bare
// field here would still be bare at the compile assertion below and fail it for
// a reason that has nothing to do with caller arguments.
const probeArgLhsSrc = `use accounts.concepts.{ account }

@enabled
@description("probe")
query account probeAccountsIncludeArchived {
  args {
    includeArchived  boolean
  }
  filter  payload.status=="active" || args.includeArchived==true
}
`

// An `args.<field>` on a comparison's LEFT-hand side must be BOUND by argument
// expansion, exactly as one on the right-hand side is (memql#4814).
//
// It was not, and the failure was invisible until the query first RAN: the
// parser only lowers `args.X` to an ArgRef when no comparison operator follows,
// so on the left it stayed a FieldReference; expansion had a branch for a
// comparison's VALUE and none for its FIELD; and the filter compiler, which
// knows `row.`, `payload.`, `provenance.` and `actor.` and nothing else, ended
// at `field "args.includeArchived" is not supported`. That error fails the
// WHOLE read, so every list of accounts in MemQL OS came back empty with a
// banner. clientAccountsAll is the only construct in the tree that writes the
// shape, and no load-time gate compiles a filter, so the build stayed green.
func TestFilterArgReferenceOnTheLeftHandSideIsBound(t *testing.T) {
	fn, err := tryParseNewFunctionSyntax("probeAccountsIncludeArchived", "query", probeArgLhsSrc, "probe", memorynodes.DefaultRegistry())
	if err != nil {
		t.Fatalf("the probe query must parse: %v", err)
	}

	// The three cases the DSL comment names, and the reason the term is a
	// disjunct rather than a `when(args.includeArchived)` guard: that guard
	// drops on ABSENCE, so a caller passing false -- which is what a checkbox
	// bound to a boolean sends -- would have widened the read.
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"passed true admits everything", map[string]any{"includeArchived": true}, true},
		{"passed false admits nothing extra", map[string]any{"includeArchived": false}, false},
		{"omitted admits nothing extra", map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newFunctionValidatorWithOrigin(nil, nil, 0)
			expanded, expErr := v.expandExpressionWithArgs(fn.Expr, tc.args)
			if expErr != nil {
				t.Fatalf("expanding with %v: %v", tc.args, expErr)
			}

			if leftover := argFieldComparisons(expanded); len(leftover) > 0 {
				t.Fatalf("expansion left %v as ROW FIELD references. The filter compiler has no "+
					"`args` namespace, so this query fails at execution with `field %q is not "+
					"supported` -- the whole read, not just the term.", leftover, leftover[0])
			}

			// The term must be FOLDED, not dropped: dropping it would make
			// `status=="active" || <gone>` a narrower read that still returns
			// rows, which is the failure a "no args. left" assertion alone
			// cannot see. Collected by a WALK rather than read off a fixed
			// position -- whether the concept conjunct wraps the disjunction or
			// sits beside it depends on whether the concept registry is
			// populated, which differs between running this test alone and
			// running the package.
			folded := constantBools(expanded)
			if len(folded) != 1 {
				t.Fatalf("expected exactly one folded caller-flag constant, got %d", len(folded))
			}
			if folded[0] != tc.want {
				t.Errorf("folded value = %v, want %v", folded[0], tc.want)
			}

			// And the folded tree must still compile as ONE query. A tree the
			// combined compiler refuses falls to the split evaluator, which
			// has no node-set for a constant and fails with "unsupported
			// expression node" -- a different opaque error in the same place.
			e := &MemQLEngine{}
			if _, compiled := e.tryCompileCombinedFilter(context.Background(), expanded, "v1:accounts:account"); !compiled {
				t.Errorf("the folded filter must compile to a single SQL query")
			}
		})
	}
}

// constantBools collects every folded query-time boolean in the tree.
func constantBools(expr ExpressionNode) []bool {
	var out []bool
	var walk func(ExpressionNode)
	walk = func(node ExpressionNode) {
		switch n := node.(type) {
		case *LogicalExpression:
			walk(n.Left)
			walk(n.Right)
		case *constantBoolExpression:
			out = append(out, n.value)
		}
	}
	walk(expr)
	return out
}

// argFieldComparisons collects every comparison in the tree whose LEFT-hand
// side is still an `args.` reference.
func argFieldComparisons(expr ExpressionNode) []string {
	var out []string
	var walk func(ExpressionNode)
	walk = func(node ExpressionNode) {
		switch n := node.(type) {
		case *LogicalExpression:
			walk(n.Left)
			walk(n.Right)
		case *ComparisonExpression:
			if strings.HasPrefix(strings.ToLower(n.Field.Raw), "args.") {
				out = append(out, n.Field.Raw)
			}
		}
	}
	walk(expr)
	return out
}

// The folded flag must reach the RESULT-CACHE KEY, or the widened and the
// narrowed read share one entry and each returns whichever ran first.
//
// This is not a hypothetical: it is what happened the first time the fold
// landed. `canonicalExpression`'s default arm renders an unknown node as "",
// so `clientAccountsAll(includeArchived: true)` and the same call with false
// -- which differ in NOTHING else once the flag is bound -- produced one
// signature. Caching is default-on with a 60s TTL, so the archived client
// appeared in the narrowed list and vanished from the widened one, decided by
// call order within the process.
func TestFoldedCallerFlagIsPartOfTheCacheSignature(t *testing.T) {
	yes := canonicalExpression(&constantBoolExpression{value: true})
	no := canonicalExpression(&constantBoolExpression{value: false})
	if yes == "" || no == "" {
		t.Fatalf("a folded caller flag renders as the empty string (true=%q false=%q), which is "+
			"what the default arm returns for a node it does not know", yes, no)
	}
	if yes == no {
		t.Fatalf("true and false render identically (%q), so a widened and a narrowed read share "+
			"one result-cache entry", yes)
	}
}
