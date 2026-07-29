package memql

import (
	"reflect"
	"strings"
	"testing"
)

// memql#2915: cond() with arg-ref operands has never worked.
//
// Two steps disagree about what a cond operand is. expandExpressionWithArgs
// rewrites every arg through substituteArgRefValue, so an *ArgRefExpression
// operand becomes a plain Go value -- and evalCollCond then requires each
// positional operand to still be an ExpressionNode. Step one guarantees step
// three fails.
//
// Nothing shipped hits it today, which is exactly the #2870 profile: dead
// until someone writes the obvious thing, and there `return args.x ?? y` in
// root position turned out to have never run in three automations.
func TestCond_ArgRefOperandsSurviveExpansion(t *testing.T) {
	v := newFunctionValidatorWithOrigin(nil, nil, 0)
	args := map[string]any{"flag": true, "a": "yes", "b": "no"}
	node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
		"0": &ArgRefExpression{Path: "flag"},
		"1": &ArgRefExpression{Path: "a"},
		"2": &ArgRefExpression{Path: "b"},
	}}

	out, err := v.expandExpressionWithArgs(node, args)
	if err != nil {
		t.Fatalf("expanding cond with arg-ref operands: %v", err)
	}
	call, ok := out.(*FunctionCallExpression)
	if !ok {
		t.Fatalf("expansion must keep cond a call for the evaluator, got %T", out)
	}

	got, err := evalCollScalar(call, args, nil)
	if err != nil {
		t.Fatalf("`cond(args.flag, args.a, args.b)` must evaluate. Expansion resolves each "+
			"arg-ref operand to a plain value and evalCollCond then demands an ExpressionNode, "+
			"so the two ends disagree (memql#2915).\n  error: %v", err)
	}
	if got != "yes" {
		t.Errorf("a true predicate must select the `then` operand; got %#v", got)
	}
}

// The false branch, so a fix cannot pass by always taking operand 1.
func TestCond_ArgRefOperandsSelectTheElseBranch(t *testing.T) {
	v := newFunctionValidatorWithOrigin(nil, nil, 0)
	args := map[string]any{"flag": false, "a": "yes", "b": "no"}
	node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
		"0": &ArgRefExpression{Path: "flag"},
		"1": &ArgRefExpression{Path: "a"},
		"2": &ArgRefExpression{Path: "b"},
	}}
	out, err := v.expandExpressionWithArgs(node, args)
	if err != nil {
		t.Fatalf("expanding: %v", err)
	}
	got, err := evalCollScalar(out.(*FunctionCallExpression), args, nil)
	if err != nil {
		t.Fatalf("false-predicate cond must evaluate (memql#2915): %v", err)
	}
	if got != "no" {
		t.Errorf("a false predicate must select the `else` operand; got %#v", got)
	}
}

// The error string this issue reports, pinned so a regression is recognisable
// rather than merely red.
func TestCond_ArgRefOperandsDoNotProduceTheNotAnExpressionError(t *testing.T) {
	v := newFunctionValidatorWithOrigin(nil, nil, 0)
	args := map[string]any{"flag": true, "a": 1, "b": 2}
	node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
		"0": &ArgRefExpression{Path: "flag"},
		"1": &ArgRefExpression{Path: "a"},
		"2": &ArgRefExpression{Path: "b"},
	}}
	out, err := v.expandExpressionWithArgs(node, args)
	if err != nil {
		t.Fatalf("expanding: %v", err)
	}
	if _, err := evalCollScalar(out.(*FunctionCallExpression), args, nil); err != nil {
		if strings.Contains(err.Error(), "is not an expression") {
			t.Fatalf("memql#2915 verbatim: expansion resolved the operand and the evaluator "+
				"rejected the result.\n  error: %v", err)
		}
		t.Fatalf("cond with arg-ref operands failed: %v", err)
	}
}

// TestCond_ShortCircuitsTheUnchosenBranch is the guard on memql#2915's fix
// rather than on its defect.
//
// evalCollCond used to read both the predicate and the chosen branch through
// its own ExpressionNode-only closure; the fix routes both through
// evalCollPositionalOperand. Laziness is the property that rewrite could
// plausibly lose, and losing it is not hypothetical -- the sibling test
// TestCoalesceShortCircuits records that the first attempt at the #2870 fix
// broke cond() by folding eagerly. `cond(true, a, 1/0)` must return a.
func TestCond_ShortCircuitsTheUnchosenBranch(t *testing.T) {
	boom := &ArithmeticExpression{
		Left:  &LiteralValueNode{Value: 1},
		Op:    "/",
		Right: &LiteralValueNode{Value: 0},
	}
	t.Run("true predicate does not evaluate the else branch", func(t *testing.T) {
		node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": &LiteralValueNode{Value: true},
			"1": &LiteralValueNode{Value: "chosen"},
			"2": boom,
		}}
		got, err := evalCollCond(node, map[string]any{}, nil)
		if err != nil {
			t.Fatalf("the else branch must not be evaluated when the predicate is true; got %v", err)
		}
		if got != "chosen" {
			t.Errorf("cond = %#v, want %q", got, "chosen")
		}
	})
	t.Run("false predicate does not evaluate the then branch", func(t *testing.T) {
		node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": &LiteralValueNode{Value: false},
			"1": boom,
			"2": &LiteralValueNode{Value: "chosen"},
		}}
		got, err := evalCollCond(node, map[string]any{}, nil)
		if err != nil {
			t.Fatalf("the then branch must not be evaluated when the predicate is false; got %v", err)
		}
		if got != "chosen" {
			t.Errorf("cond = %#v, want %q", got, "chosen")
		}
	})
}

// TestCond_AgreesWithTheCanonicalEvaluator extends the concat/coalesce
// agreement check to the third positional builtin, which it did not cover.
//
// Same reason as its siblings: a logic body with an intermediate `x := ...`
// step runs cond through the LogicRunner and RuntimeEvaluator.EvaluateIf; a
// single-statement body runs it through fn.Expr and evalCollCond. Every
// shipped cond in the tree is the multi-statement form, so the two have never
// been compared -- and memql#2915 is a defect that only the untested half had.
func TestCond_AgreesWithTheCanonicalEvaluator(t *testing.T) {
	canonical := NewRuntimeEvaluator(nil)
	for _, ops := range [][3]any{
		{true, "yes", "no"},
		{false, "yes", "no"},
		{nil, "yes", "no"}, // nil predicate is falsy
		{"", "yes", "no"},  // empty string is falsy
		{"nonempty", "yes", "no"},
		{0, "yes", "no"},
		{1, "yes", "no"},
		{true, nil, "no"}, // a nil chosen branch must survive as nil
	} {
		node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": &LiteralValueNode{Value: ops[0]},
			"1": &LiteralValueNode{Value: ops[1]},
			"2": &LiteralValueNode{Value: ops[2]},
		}}
		got, err := evalCollCond(node, map[string]any{}, nil)
		if err != nil {
			t.Fatalf("evalCollCond(%#v): %v", ops, err)
		}
		want := canonical.EvaluateIf(ops[0], ops[1], ops[2])
		if !reflect.DeepEqual(got, want) {
			t.Errorf("cond%v: fn.Expr path = %#v, RuntimeEvaluator = %#v.\n\n"+
				"cond() must not mean two different things depending on whether the logic "+
				"body has one statement or two.", ops, got, want)
		}
	}
}
