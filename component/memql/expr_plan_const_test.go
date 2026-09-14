package memql

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/ast"
)

// expr_plan_const_test.go -- the replacement rules for PlanConstExpression
// (memql#5366): where argument expansion replaces a plan constant, with what,
// and what the logical operators around it fold to.
//
// The real evaluator (EvalExpr) lands on a sibling branch, so these tests
// install fakePlanConstantEvaluator: a deliberately tiny stand-in that
// evaluates exactly the node kinds built here and FAILS on anything else, so
// no test can pass by the fake guessing. What is under test is the executor
// side -- the bindings it hands over and what it does with the answer -- not
// the evaluation.

// fakePlanConstantEvaluator evaluates literals, nil, names from bindings,
// member access (absence propagates), `!`, `==` / `!=` (nil-aware) and
// `&&` / `||`. Nothing else.
func fakePlanConstantEvaluator(ctx context.Context, n ast.ExpressionNode, bindings map[string]any) (any, error) {
	switch e := n.(type) {
	case *ast.LiteralExpr:
		return e.Value, nil
	case *ast.NilExpr:
		return nil, nil
	case *ast.ParenExpr:
		return fakePlanConstantEvaluator(ctx, e.Inner, bindings)
	case *ast.IdentExpr:
		value, ok := bindings[e.Name]
		if !ok {
			return nil, fmt.Errorf("unknown name %q", e.Name)
		}
		return value, nil
	case *ast.MemberExpr:
		object, err := fakePlanConstantEvaluator(ctx, e.Object, bindings)
		if err != nil {
			return nil, err
		}
		m, ok := object.(map[string]any)
		if !ok {
			return nil, nil
		}
		return m[e.Field], nil
	case *ast.UnaryExpr:
		if e.Op != "!" {
			break
		}
		operand, err := fakePlanConstantEvaluator(ctx, e.Operand, bindings)
		if err != nil {
			return nil, err
		}
		b, ok := operand.(bool)
		if !ok {
			return nil, fmt.Errorf("fake evaluator: ! over %T", operand)
		}
		return !b, nil
	case *ast.BinaryExpr:
		left, err := fakePlanConstantEvaluator(ctx, e.Left, bindings)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case "&&", "||":
			lb, ok := left.(bool)
			if !ok {
				return nil, fmt.Errorf("fake evaluator: %s over %T", e.Op, left)
			}
			if (e.Op == "&&" && !lb) || (e.Op == "||" && lb) {
				return lb, nil
			}
			right, err := fakePlanConstantEvaluator(ctx, e.Right, bindings)
			if err != nil {
				return nil, err
			}
			rb, ok := right.(bool)
			if !ok {
				return nil, fmt.Errorf("fake evaluator: %s over %T", e.Op, right)
			}
			return rb, nil
		case "==", "!=":
			right, err := fakePlanConstantEvaluator(ctx, e.Right, bindings)
			if err != nil {
				return nil, err
			}
			equal := (left == nil && right == nil) || (left != nil && right != nil && reflect.DeepEqual(left, right))
			if e.Op == "!=" {
				return !equal, nil
			}
			return equal, nil
		}
	}
	return nil, fmt.Errorf("fake evaluator: unsupported node %T", n)
}

// planConstantCalls records every evaluation the hook received.
type planConstantCalls struct {
	sources  []string
	bindings []map[string]any
}

// withFakePlanConstantEvaluator installs the fake for one test and restores
// whatever was there. Tests using it must not call t.Parallel: the hook is a
// package variable.
func withFakePlanConstantEvaluator(t *testing.T) *planConstantCalls {
	t.Helper()
	calls := &planConstantCalls{}
	previous := planConstantEvaluator
	planConstantEvaluator = func(ctx context.Context, n ast.ExpressionNode, bindings map[string]any) (any, error) {
		calls.sources = append(calls.sources, ast.FormatExpr(n))
		calls.bindings = append(calls.bindings, bindings)
		return fakePlanConstantEvaluator(ctx, n, bindings)
	}
	t.Cleanup(func() { planConstantEvaluator = previous })
	return calls
}

// pcLiteral is a plan constant that evaluates to v.
func pcLiteral(v any) *PlanConstExpression {
	if v == nil {
		return &PlanConstExpression{Expr: &ast.NilExpr{}}
	}
	return &PlanConstExpression{Expr: &ast.LiteralExpr{Value: v}}
}

// pcArgIsNil is `args.<name> == nil`, the left half of the edition-2026
// optional-argument guard.
func pcArgIsNil(name string) *PlanConstExpression {
	return &PlanConstExpression{Expr: &ast.BinaryExpr{
		Op:    "==",
		Left:  &ast.MemberExpr{Object: &ast.IdentExpr{Name: "args"}, Field: name},
		Right: &ast.NilExpr{},
	}}
}

// pcArg is `args.<name>` as a value.
func pcArg(name string) *PlanConstExpression {
	return &PlanConstExpression{Expr: &ast.MemberExpr{Object: &ast.IdentExpr{Name: "args"}, Field: name}}
}

// testPlanConstAmbient is an ambient envelope carrying every root.
func testPlanConstAmbient() map[string]any {
	return map[string]any{
		"actor":     map[string]any{"userId": "u-1", "role": "writer"},
		"config":    map[string]any{"demoMode": true},
		"now":       "2026-09-13T12:00:00Z",
		"partition": "",
	}
}

func planConstValidator(specs *SpecRegistry) *functionValidator {
	return newFunctionValidatorWithAmbient(nil, specs, auth.OriginInternal, testPlanConstAmbient())
}

// failIfExpanded is a comparison whose expansion ERRORS: its value is an
// argument reference to an argument nobody passes. A test that finds this
// node's error was expanded when it must not have been.
func failIfExpanded() *ComparisonExpression {
	return &ComparisonExpression{Field: irPayloadField("f"), Operator: OpEq, Value: &ArgReference{Path: "neverPassed"}}
}

func TestPlanConstant_UnwiredEvaluatorRefuses(t *testing.T) {
	previous := planConstantEvaluator
	planConstantEvaluator = nil
	t.Cleanup(func() { planConstantEvaluator = previous })

	v := planConstValidator(nil)
	_, err := v.expandExpressionWithArgs(pcLiteral(true), map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "plan constants need the in-process evaluator")

	_, err = v.expandExpressionWithArgs(irPayloadCmp("f", OpEq, pcLiteral("x")), map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "plan constants need the in-process evaluator")
}

func TestPlanConstant_PredicatePositionMustBeBoolean(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)

	for _, want := range []bool{true, false} {
		got, err := v.expandExpressionWithArgs(pcLiteral(want), map[string]any{})
		require.NoError(t, err)
		c, ok := got.(*constantBoolExpression)
		require.True(t, ok, "a boolean plan constant becomes a constant, got %T", got)
		require.Equal(t, want, c.value)
		require.True(t, c.planConstant, "and is marked as a plan constant, so the operators around it fold")
	}

	for _, tc := range []struct {
		value any
		kind  string
	}{
		{"yes", "string"},
		{int64(1), "number"},
		{nil, "nil"},
		{[]any{true}, "list"},
	} {
		_, err := v.expandExpressionWithArgs(pcLiteral(tc.value), map[string]any{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "a plan constant in a condition must be boolean, got "+tc.kind)
	}
}

func TestPlanConstant_ValuePositionBecomesTheComparisonValue(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)
	stamp := time.Date(2026, 9, 13, 12, 0, 0, 123, time.UTC)

	for _, tc := range []struct {
		name  string
		in    *ComparisonExpression
		op    ComparisonOperator
		value any
	}{
		{"a string", irPayloadCmp("status", OpEq, pcLiteral("open")), OpEq, "open"},
		{"an int widened to int64", irPayloadCmp("n", OpGt, pcLiteral(3)), OpGt, int64(3)},
		{"a float32 widened to float64", irPayloadCmp("n", OpGt, pcLiteral(float32(1.5))), OpGt, float64(1.5)},
		{"a time rendered as now is", irPayloadCmp("expiresAt", OpLt, pcLiteral(stamp)), OpLt, "2026-09-13T12:00:00.000000123Z"},
		{"an argument", irPayloadCmp("owner", OpEq, pcArg("owner")), OpEq, "u-9"},
		{"a list keeps its members", irPayloadCmp("status", OpIn, pcLiteral([]any{"a", "b"})), OpIn, []any{"a", "b"}},
		// One notion of unset: a nil member is an UNSET member, which admits
		// the unset rows, so it is kept for the compilers.
		{"a list keeps its unset members", irPayloadCmp("status", OpIn, pcLiteral([]any{"a", nil})), OpIn, []any{"a", nil}},
		{"a []string becomes []any", irPayloadCmp("status", OpIn, pcLiteral([]string{"a"})), OpIn, []any{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.expandExpressionWithArgs(tc.in, map[string]any{"owner": "u-9"})
			require.NoError(t, err)
			cmp, ok := got.(*ComparisonExpression)
			require.True(t, ok, "got %T", got)
			require.Equal(t, tc.op, cmp.Operator)
			require.Equal(t, tc.value, cmp.Value)
			_, stillConst := tc.in.Value.(*PlanConstExpression)
			require.True(t, stillConst, "expansion must not mutate the registered tree")
		})
	}

	_, err := v.expandExpressionWithArgs(irPayloadCmp("status", OpEq, pcLiteral(map[string]any{"a": 1})), map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "evaluated to a map")
}

// An absent value is decided by the one notion of unset, never handed to the
// compilers as nil.
func TestPlanConstant_AbsentValueFollowsTheAbsenceTable(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)

	rewritten := func(t *testing.T, in ExpressionNode) ExpressionNode {
		t.Helper()
		got, err := v.expandExpressionWithArgs(in, map[string]any{})
		require.NoError(t, err)
		return got
	}

	for _, tc := range []struct {
		name   string
		in     ExpressionNode
		wantOp ComparisonOperator // "" when the answer is a constant
		want   bool
	}{
		{"== absent is == nil", irPayloadCmp("f", OpEq, pcArg("x")), OpMissing, false},
		{"!= absent is != nil", irPayloadCmp("f", OpNe, pcArg("x")), OpNotMissing, false},
		{"an element == absent is == nil too", irElementCmp("qty", OpEq, pcArg("x")), OpMissing, false},
		{"an ordering against absent is false", irPayloadCmp("f", OpLt, pcArg("x")), "", false},
		{"in absent is false", irPayloadCmp("f", OpIn, pcArg("x")), "", false},
		{"not in an absent list is != nil", irPayloadCmp("f", OpOut, pcArg("x")), OpNotMissing, false},
		{"an absent needle looks for an unset element", irPayloadCmp("tags", OpHas, pcArg("x")), OpHas, false},
		{"startsWith absent is false", irPayloadCmp("f", OpStartsWith, pcArg("x")), "", false},
		{"an empty list decides in", irPayloadCmp("f", OpIn, pcLiteral([]any{})), "", false},
		{"not in an empty list is != nil", irPayloadCmp("f", OpOut, pcLiteral([]any{})), OpNotMissing, false},
		// A NOT NULL column is never absent.
		{"a column == absent is false", &ComparisonExpression{Field: FieldReference{Raw: "createdBy", Parts: []string{"createdBy"}}, Operator: OpEq, Value: pcArg("x")}, "", false},
		{"a column != absent is true", &ComparisonExpression{Field: FieldReference{Raw: "row.createdBy", Parts: []string{"row", "createdBy"}}, Operator: OpNe, Value: pcArg("x")}, "", true},
		// A bare field is a payload property the bare-access rewrite has not
		// reached yet (expansion runs first).
		{"a bare field is a payload field", &ComparisonExpression{Field: FieldReference{Raw: "status", Parts: []string{"status"}}, Operator: OpEq, Value: pcArg("x")}, OpMissing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rewritten(t, tc.in)
			if tc.wantOp != "" {
				cmp, ok := got.(*ComparisonExpression)
				require.True(t, ok, "got %T", got)
				require.Equal(t, tc.wantOp, cmp.Operator)
				require.Nil(t, cmp.Value)
				return
			}
			c, ok := got.(*constantBoolExpression)
			require.True(t, ok, "got %T (%s)", got, canonicalExpression(got))
			require.Equal(t, tc.want, c.value)
			require.True(t, c.planConstant)
		})
	}

	_, err := v.expandExpressionWithArgs(&ComparisonExpression{
		Field: FieldReference{Raw: "row.provenance.kind", Parts: []string{"row", "provenance", "kind"}}, Operator: OpEq, Value: pcArg("x"),
	}, map[string]any{})
	require.Error(t, err, "a provenance leaf can be absent and has no absent comparison in the pushdown")
	require.Contains(t, err.Error(), "provenance leaf has no absent comparison")
}

// The four identities, and the short-circuit that makes the optional-argument
// guard safe: a deciding LEFT operand leaves the right one unexpanded.
func TestPlanConstant_LogicalOperatorsFold(t *testing.T) {
	calls := withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)
	row := irPayloadCmp("status", OpEq, "open")

	expand := func(t *testing.T, in ExpressionNode) ExpressionNode {
		t.Helper()
		got, err := v.expandExpressionWithArgs(in, map[string]any{})
		require.NoError(t, err)
		return got
	}
	isConst := func(t *testing.T, got ExpressionNode, want bool) {
		t.Helper()
		c, ok := got.(*constantBoolExpression)
		require.True(t, ok, "want a constant, got %T (%s)", got, canonicalExpression(got))
		require.Equal(t, want, c.value)
		require.True(t, c.planConstant)
	}
	isRow := func(t *testing.T, got ExpressionNode) {
		t.Helper()
		require.Equal(t, canonicalExpression(row), canonicalExpression(got))
	}

	isRow(t, expand(t, &LogicalExpression{Op: LogicalAnd, Left: pcLiteral(true), Right: row}))
	isRow(t, expand(t, &LogicalExpression{Op: LogicalOr, Left: pcLiteral(false), Right: row}))
	isRow(t, expand(t, &LogicalExpression{Op: LogicalAnd, Left: row, Right: pcLiteral(true)}))
	isRow(t, expand(t, &LogicalExpression{Op: LogicalOr, Left: row, Right: pcLiteral(false)}))
	isConst(t, expand(t, &LogicalExpression{Op: LogicalAnd, Left: row, Right: pcLiteral(false)}), false)
	isConst(t, expand(t, &LogicalExpression{Op: LogicalOr, Left: row, Right: pcLiteral(true)}), true)
	isConst(t, expand(t, &NotExpression{Target: pcLiteral(true)}), false)
	isConst(t, expand(t, &NotExpression{Target: pcLiteral(false)}), true)

	// The short-circuit. failIfExpanded() would error if expanded, and the
	// plan constant inside the right operand must not even be evaluated.
	before := len(calls.sources)
	isConst(t, expand(t, &LogicalExpression{Op: LogicalAnd, Left: pcLiteral(false), Right: failIfExpanded()}), false)
	isConst(t, expand(t, &LogicalExpression{Op: LogicalOr, Left: pcLiteral(true),
		Right: &LogicalExpression{Op: LogicalAnd, Left: failIfExpanded(), Right: pcLiteral("never evaluated")}}), true)
	require.Equal(t, before+2, len(calls.sources), "only the two deciding left operands are evaluated")

	// Control: the same right operand DOES error when the left operand does
	// not decide -- which is what makes the assertions above evidence that it
	// was skipped rather than harmless.
	_, err := v.expandExpressionWithArgs(&LogicalExpression{Op: LogicalOr, Left: pcLiteral(false), Right: failIfExpanded()}, map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), `required argument "neverPassed" not provided`)
}

// The older caller-flag constants (memql#4814) keep their shape; only plan
// constants fold.
func TestPlanConstant_OnlyPlanConstantsFold(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)
	flag := &ComparisonExpression{Field: FieldReference{Raw: "args.includeArchived", Parts: []string{"args", "includeArchived"}}, Operator: OpEq, Value: true}
	got, err := v.expandExpressionWithArgs(&LogicalExpression{Op: LogicalOr, Left: irPayloadCmp("status", OpEq, "active"), Right: flag},
		map[string]any{"includeArchived": false})
	require.NoError(t, err)
	logical, ok := got.(*LogicalExpression)
	require.True(t, ok, "the caller-flag disjunction keeps its shape, got %T", got)
	c, ok := logical.Right.(*constantBoolExpression)
	require.True(t, ok)
	require.False(t, c.value)
	require.False(t, c.planConstant)
}

// The motivating shape: `args.x == nil || row.f == args.x` reaches SQL as TRUE
// or as the single comparison, and never errors on the absent argument.
func TestPlanConstant_OptionalArgumentGuardFoldsBeforeSQL(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)
	eng := &MemQLEngine{}

	orGuard := &LogicalExpression{Op: LogicalOr, Left: pcArgIsNil("x"),
		Right: &ComparisonExpression{Field: irPayloadField("f"), Operator: OpEq, Value: &ArgReference{Path: "x"}}}
	andGuard := &LogicalExpression{Op: LogicalAnd, Left: &NotExpression{Target: pcArgIsNil("x")},
		Right: &ComparisonExpression{Field: irPayloadField("f"), Operator: OpEq, Value: &ArgReference{Path: "x"}}}

	for _, tc := range []struct {
		name  string
		guard ExpressionNode
		args  map[string]any
		sql   string
	}{
		{"|| guard, argument absent", orGuard, map[string]any{}, "TRUE"},
		{"|| guard, argument present", orGuard, map[string]any{"x": "v"}, "(jsonb_typeof(payload->'f') = 'string' AND payload #>> '{f}' = ?)"},
		{"&& guard, argument absent", andGuard, map[string]any{}, "FALSE"},
		{"&& guard, argument present", andGuard, map[string]any{"x": "v"}, "(jsonb_typeof(payload->'f') = 'string' AND payload #>> '{f}' = ?)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.expandExpressionWithArgs(tc.guard, tc.args)
			require.NoError(t, err)
			compiled, ok := eng.tryCompileCombinedFilter(context.Background(), got, "")
			require.True(t, ok)
			require.Equal(t, tc.sql, compiled.sql)
		})
	}
}

func TestPlanConstant_NotOfADroppedOperandIsDropped(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)
	dropped := &NotExpression{Target: &ConditionalFilterExpression{ArgPath: "x", Filter: irPayloadCmp("f", OpEq, "a")}}

	got, err := v.expandExpressionWithArgs(dropped, map[string]any{})
	require.NoError(t, err)
	require.Nil(t, got, "a negation of nothing is nothing, not true")

	got, err = v.expandExpressionWithArgs(&LogicalExpression{Op: LogicalAnd, Left: irPayloadCmp("g", OpEq, "b"), Right: dropped}, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, canonicalExpression(irPayloadCmp("g", OpEq, "b")), canonicalExpression(got))

	got, err = v.expandExpressionWithArgs(dropped, map[string]any{"x": "present"})
	require.NoError(t, err)
	require.Equal(t, `!(payload.f=="a")`, canonicalExpression(got))
}

func TestPlanConstant_InsideACollectionPredicate(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)

	anyQty := &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAny,
		Pred: irElementCmp("qty", OpGt, pcArg("min"))}
	got, err := v.expandExpressionWithArgs(anyQty, map[string]any{"min": 2})
	require.NoError(t, err)
	require.Equal(t, `payload.items.any($elem.qty>2)`, canonicalExpression(got))

	got, err = v.expandExpressionWithArgs(anyQty, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, `payload.items.any(const(false))`, canonicalExpression(got),
		"an ordering against an absent value is false, inside an element predicate as outside")

	count := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpGt, CountValue: pcArg("n")}
	got, err = v.expandExpressionWithArgs(count, map[string]any{"n": 1})
	require.NoError(t, err)
	require.Equal(t, `payload.tags.count()>1`, canonicalExpression(got))

	got, err = v.expandExpressionWithArgs(count, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, `const(false)`, canonicalExpression(got), "a count is never absent, so > absent is false")

	notEqual := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpNe, CountValue: pcArg("n")}
	got, err = v.expandExpressionWithArgs(notEqual, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, `const(true)`, canonicalExpression(got))
}

func TestPlanConstant_BindingsAreTheCallAndTheEnvelope(t *testing.T) {
	calls := withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)

	_, err := v.expandExpressionWithArgs(pcLiteral(true), map[string]any{"a": 1})
	require.NoError(t, err)
	require.Len(t, calls.bindings, 1)
	got := calls.bindings[0]
	require.Equal(t, map[string]any{"a": 1}, got["args"])
	require.Equal(t, testPlanConstAmbient()["actor"], got["actor"])
	require.Equal(t, testPlanConstAmbient()["config"], got["config"])
	require.Equal(t, "2026-09-13T12:00:00Z", got["now"])
	_, hasPartition := got["partition"]
	require.False(t, hasPartition, "partition is a retired dimension, not an edition-2026 root")

	// Outside a call there are no arguments to bind, and a plan constant
	// reading them is told so by the evaluator rather than handed an empty map.
	_, err = v.expandExpressionWithArgs(pcArg("a"), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown name "args"`)
}

// A spec whose body carries a plan constant is inlined, per call, at
// expansion; every other spec stays a reference for the executor.
func TestPlanConstant_SpecBodyIsExpandedPerCall(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	specs := newSpecRegistry()
	require.NoError(t, specs.add(&Spec{
		Name:   "isFresh",
		Origin: "planconst/specs.memql",
		Expr:   irPayloadCmp("expiresAt", OpGt, &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}),
	}))
	require.NoError(t, specs.add(&Spec{
		Name:   "isFreshAndOpen",
		Origin: "planconst/specs.memql",
		Expr:   &LogicalExpression{Op: LogicalAnd, Left: &SpecReferenceExpression{Name: "isFresh"}, Right: irPayloadCmp("status", OpEq, "open")},
	}))
	require.NoError(t, specs.add(&Spec{
		Name:   "isOpen",
		Origin: "planconst/specs.memql",
		Expr:   irPayloadCmp("status", OpEq, "open"),
	}))
	v := planConstValidator(specs)

	got, err := v.expandExpressionWithArgs(&SpecReferenceExpression{Name: "isFresh"}, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, `payload.expiresat>"2026-09-13T12:00:00Z"`, canonicalExpression(got))

	got, err = v.expandExpressionWithArgs(&SpecReferenceExpression{Name: "isFreshAndOpen"}, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, `AND(payload.expiresat>"2026-09-13T12:00:00Z",payload.status=="open")`, canonicalExpression(got),
		"a spec that only REFERENCES a plan-constant spec needs the same per-call expansion")

	got, err = v.expandExpressionWithArgs(&SpecReferenceExpression{Name: "isOpen"}, map[string]any{})
	require.NoError(t, err)
	require.IsType(t, &SpecReferenceExpression{}, got, "a spec with no plan constant stays a reference")

	// The call-syntax spelling reaches expandFunctionCall, which returned
	// the registered body verbatim.
	got, err = v.expandExpressionWithArgs(&FunctionCallExpression{Name: "isFresh", Args: map[string]any{}}, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, `payload.expiresat>"2026-09-13T12:00:00Z"`, canonicalExpression(got))
}

// Two calls whose plan constants evaluate differently must not share a
// result-cache entry: the folded value is in the tree the signature reads.
func TestPlanConstant_FoldedValueIsPartOfTheSignature(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	v := planConstValidator(nil)
	body := irPayloadCmp("owner", OpEq, pcArg("owner"))

	one, err := v.expandExpressionWithArgs(body, map[string]any{"owner": "u-1"})
	require.NoError(t, err)
	two, err := v.expandExpressionWithArgs(body, map[string]any{"owner": "u-2"})
	require.NoError(t, err)
	again, err := v.expandExpressionWithArgs(body, map[string]any{"owner": "u-1"})
	require.NoError(t, err)
	require.NotEqual(t, canonicalExpression(one), canonicalExpression(two))
	require.Equal(t, canonicalExpression(one), canonicalExpression(again))
}

// A context-spec evaluates per call already, so its plan constants are
// evaluated in place against the spec's envelope.
func TestPlanConstant_ContextSpecEvaluatesInPlace(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	envelope := testPlanConstAmbient()

	got, err := evaluateSpecExpression(context.Background(), nil, pcLiteral(true), envelope)
	require.NoError(t, err)
	require.Equal(t, true, got)

	_, err = evaluateSpecExpression(context.Background(), nil, pcLiteral("x"), envelope)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be boolean")

	role := &ComparisonExpression{
		Field:    FieldReference{Raw: "actor.role", Parts: []string{"actor", "role"}},
		Operator: OpEq,
		Value:    &PlanConstExpression{Expr: &ast.LiteralExpr{Value: "writer"}},
	}
	got, err = evaluateSpecExpression(context.Background(), nil, role, envelope)
	require.NoError(t, err)
	require.Equal(t, true, got)

	got, err = evaluateSpecExpression(context.Background(), nil, &NotExpression{Target: role}, envelope)
	require.NoError(t, err)
	require.Equal(t, false, got)
}
