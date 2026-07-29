package memql

import (
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
