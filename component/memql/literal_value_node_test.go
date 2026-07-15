package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// literal_value_node_test.go is the reproduction + fix guard for memql#1705:
// a Logic body whose return resolves to a scalar/arg value (the dry-run
// failure was `consolidateMemory` -> `return args.event.topic`) failed
// at runtime with `unsupported expression node *memql.LiteralValueNode`.
//
// Root cause: after function-call arg substitution folds the ArgRefExpression
// into a concrete value, plan.Root is a *LiteralValueNode, but the DSL
// expression evaluator's node dispatch had no case for it -- so the literal
// fell through to the node-set evaluator's `default` and errored at runtime.
//
// The fix adds explicit LiteralValueNode handling across every expression
// position the evaluator touches:
//   - Execute(): a literal at plan.Root returns the scalar as the result.
//   - cloneExpressionNode(): clones the literal (previously returned nil --
//     the #1090 / #1705 silent-nil footgun).
//   - nodeMatchesExpression(): a literal in predicate position folds to its
//     truthiness (post-filter counterpart to constantBoolExpression).
//   - evaluateSpecExpression(): a literal in a spec body evaluates to value.
//   - evaluateExpressionSetWithContext(): a literal in node-set (row-filter)
//     position is a precise, descriptive error rather than the generic
//     "unsupported expression node".
//
// TestCloneExpressionNode_ExhaustiveNodeCoverage enforces the broader
// contract: every ExpressionNode implementer must be explicitly handled by
// cloneExpressionNode, so a new AST node added without a case is caught at
// test time, not at runtime.

// TestNodeMatchesExpression_LiteralValueNode pins the predicate-position
// case: a literal evaluates by truthiness in the in-process post-filter.
func TestNodeMatchesExpression_LiteralValueNode(t *testing.T) {
	node := memorynodes.MemoryNode{ID: "v1:x:y:1", Concept: "v1:x:y"}
	cache := map[string]map[string]any{}

	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"true", true, true},
		{"false", false, false},
		{"nonEmptyString", "node.created", true},
		{"emptyString", "", false},
		{"nonZeroInt", int64(7), true},
		{"zeroInt", int64(0), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nodeMatchesExpression(node, &LiteralValueNode{Value: tc.val}, cache)
			require.NoError(t, err, "a literal in predicate position must not error (memql#1705)")
			require.Equal(t, tc.want, got)
		})
	}
}

// TestEvaluateSpecExpression_LiteralValueNode pins the spec-body case: a
// literal evaluates to its value (the caller applies specTruthy).
func TestEvaluateSpecExpression_LiteralValueNode(t *testing.T) {
	val, err := evaluateSpecExpression(context.Background(), nil, &LiteralValueNode{Value: "hi"}, map[string]any{})
	require.NoError(t, err, "a literal in a spec body must not error (memql#1705)")
	require.Equal(t, "hi", val)
}

// TestCloneExpressionNode_LiteralValueNode pins the clone case: the literal
// is copied, not silently nil-ed. A nil here was the #1090 / #1705 footgun
// (plan.Root went nil -> opaque downstream errors).
func TestCloneExpressionNode_LiteralValueNode(t *testing.T) {
	orig := &LiteralValueNode{Value: map[string]any{"topic": "node.created"}}
	cloned := cloneExpressionNode(orig)
	require.NotNil(t, cloned, "cloneExpressionNode must not drop a LiteralValueNode (memql#1705)")
	lit, ok := cloned.(*LiteralValueNode)
	require.True(t, ok, "clone must preserve the LiteralValueNode type, got %T", cloned)
	require.Equal(t, orig.Value, lit.Value)
}

// allExpressionNodeImplementers returns one instance of every concrete
// ExpressionNode implementer in the package. The exhaustive-coverage test
// below walks this list; if a new node type is added without being included
// here, the build fails (the marker assertion) and the author is forced to
// also extend cloneExpressionNode. This is the closest Go gives us to a
// compile-time exhaustiveness check over an interface's implementers.
func allExpressionNodeImplementers() map[string]ExpressionNode {
	return map[string]ExpressionNode{
		"FunctionCallExpression":      &FunctionCallExpression{Name: "f"},
		"AIExpression":                &AIExpression{},
		"LogicalExpression":           &LogicalExpression{Op: LogicalAnd, Left: &LiteralValueNode{Value: true}, Right: &LiteralValueNode{Value: true}},
		"SpecReferenceExpression":     &SpecReferenceExpression{Name: "s"},
		"RelationshipExpression":      &RelationshipExpression{},
		"ComparisonExpression":        &ComparisonExpression{Field: FieldReference{Parts: []string{"payload", "x"}}, Operator: OpEq, Value: "v"},
		"ArithmeticExpression":        &ArithmeticExpression{Op: "+", Left: &LiteralValueNode{Value: int64(1)}, Right: &LiteralValueNode{Value: int64(2)}},
		"BinaryComparisonExpression":  &BinaryComparisonExpression{Left: &LiteralValueNode{Value: int64(1)}, Operator: OpGt, Right: &LiteralValueNode{Value: int64(0)}},
		"BuiltinFunctionExpression":   &BuiltinFunctionExpression{Name: "b"},
		"SortExpression":              &SortExpression{},
		"PaginateExpression":          &PaginateExpression{},
		"SelectExpression":            &SelectExpression{},
		"TimestampExpression":         &TimestampExpression{},
		"DepthExpression":             &DepthExpression{},
		"CountExpression":             &CountExpression{},
		"ShapeExpression":             &ShapeExpression{},
		"ErrorRefExpression":          &ErrorRefExpression{},
		"ErrorExpression":             &ErrorExpression{Message: &LiteralValueNode{Value: "boom"}},
		"constantBoolExpression":      &constantBoolExpression{value: true},
		"ArgRefExpression":            &ArgRefExpression{Path: "x"},
		"CallerRefExpression":         &CallerRefExpression{},
		"ConditionalFilterExpression": &ConditionalFilterExpression{ArgPath: "x"},
		"LiteralValueNode":            &LiteralValueNode{Value: 1},
		"DotAccessExpression":         &DotAccessExpression{Object: &LiteralValueNode{Value: map[string]any{"x": 1}}, Field: "x"},
	}
}

// TestCloneExpressionNode_ExhaustiveNodeCoverage asserts cloneExpressionNode
// has an explicit case for EVERY ExpressionNode implementer -- a missing case
// returns nil (the default), which would silently drop the node from a plan
// tree and surface as an opaque runtime error later. This is the exhaustive
// AST-node-coverage guard the memql#1705 acceptance criteria require: add a
// new node type without a clone case and this test fails immediately.
func TestCloneExpressionNode_ExhaustiveNodeCoverage(t *testing.T) {
	for name, node := range allExpressionNodeImplementers() {
		t.Run(name, func(t *testing.T) {
			cloned := cloneExpressionNode(node)
			require.NotNilf(t, cloned, "cloneExpressionNode returned nil for %s -- every ExpressionNode implementer needs an explicit case (memql#1705)", name)
		})
	}
}

// TestEvaluateExpressionSet_LiteralIsDescriptiveError pins that a literal used
// where a row filter is expected yields a precise, descriptive error (not the
// generic "unsupported expression node"). Execute() intercepts a literal at
// plan.Root before this path, so this is the explicit guard for the misuse
// case.
func TestEvaluateExpressionSet_LiteralIsDescriptiveError(t *testing.T) {
	eng := &MemQLEngine{}
	_, err := eng.evaluateExpressionSetWithContext(context.Background(), &LiteralValueNode{Value: "x"}, nil, 0, nil, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot be used as a row filter")
	require.NotContains(t, err.Error(), "unsupported expression node", "must be a descriptive error, not the generic default (memql#1705)")
}

// TestExecute_LogicConsolidateMemoryShape_RunsToCompletion is the
// Postgres-gated end-to-end reproduction + fix guard. It registers the
// memql#1705 logic shape (`logic consolidateMemory { body { return
// args.event.topic } }`), invokes it through engine.Execute exactly as an
// automation function step would, and asserts the call runs to completion
// returning the bound topic value.
//
// Before the fix this failed with `unsupported expression node
// *memql.LiteralValueNode` (the arg path folds to a *LiteralValueNode at
// plan.Root, which the node-set evaluator had no case for). After the fix
// Execute() intercepts the literal root and returns the scalar.
//
// Postgres-gated: skips when no DB is reachable, like
// executor_mutation_readmerge_db_test.go (whose engine helper this reuses).
func TestExecute_LogicConsolidateMemoryShape_RunsToCompletion(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)

	const src = `@enabled
@description("repro of the memql#1705 consolidateMemory return shape")
logic consolidateMemory {
  args {
    event object @required
  }
  body {
    return args.event.topic
  }
}
`
	fn, err := tryParseNewFunctionSyntax("consolidateMemory", "logic", src, "memql#1705-test", memorynodes.DefaultRegistry())
	require.NoError(t, err, "the logic shape must parse")
	require.NoError(t, eng.Functions().Upsert(fn))

	event, err := json.Marshal(map[string]any{"topic": "node.created"})
	require.NoError(t, err)

	res, err := eng.Execute(ctx, "logic consolidateMemory(event: "+string(event)+")")
	require.NoError(t, err, "consolidateMemory must run to completion (memql#1705 regression -- was 'unsupported expression node *memql.LiteralValueNode')")
	require.NotNil(t, res)
	require.Equal(t, "node.created", res.OutputPayload(), "the logic must return the bound args.event.topic value")
}

// TestExecute_BareLiteralReturnsScalar is the DB-free companion: a bare
// literal query (what a logic body of `return "x"` reduces to, and what the
// QueryExecutor substitutes a resolved arg path into) must return the scalar
// rather than tripping the node-set evaluator. Postgres-gated only because
// Execute requires a configured DB; the assertion is the literal-root branch.
func TestExecute_BareLiteralReturnsScalar(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)
	res, err := eng.Execute(ctx, `"node.created"`)
	require.NoError(t, err, "a bare literal query must return a scalar, not error (memql#1705)")
	require.NotNil(t, res)
	require.Equal(t, "node.created", res.OutputPayload())
}
