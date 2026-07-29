package memql

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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

// TestCond_TruthinessIsPinnedIndependently states cond's expected result per
// predicate value outright, rather than comparing it to another function.
//
// The first version of this test compared evalCollCond against
// RuntimeEvaluator.EvaluateIf and asserted nothing: EvaluateIf is
// `if isTruthy(cond) {...}` over the SAME package-level isTruthy that
// evalCollCond calls, so both sides move together. Review proved it -- making
// "" and 0 truthy left the comparison green while two other tests caught it.
//
// Its premise was wrong too. That comment claimed the multi-statement logic
// body runs cond through EvaluateIf; EvaluateIf has no production callers at
// all. The other evaluator is component/automations' evaluateCondLocally, over
// that package's own separate isTruthy -- unexported and unreachable from
// here, so the genuine cross-package divergence is filed rather than faked.
func TestCond_TruthinessIsPinnedIndependently(t *testing.T) {
	for _, tc := range []struct {
		name string
		pred any
		want any
	}{
		{"true", true, "yes"},
		{"false", false, "no"},
		{"nil is falsy", nil, "no"},
		{"empty string is falsy", "", "no"},
		{"non-empty string is truthy", "nonempty", "yes"},
		{"zero is falsy", 0, "no"},
		{"one is truthy", 1, "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
				"0": &LiteralValueNode{Value: tc.pred},
				"1": &LiteralValueNode{Value: "yes"},
				"2": &LiteralValueNode{Value: "no"},
			}}
			got, err := evalCollCond(node, map[string]any{}, nil)
			if err != nil {
				t.Fatalf("evalCollCond(pred=%#v): %v", tc.pred, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cond(%#v, \"yes\", \"no\") = %#v, want %#v", tc.pred, got, tc.want)
			}
		})
	}

	// A nil chosen branch must survive as nil rather than being treated as
	// absent -- cond is not coalesce.
	node := &FunctionCallExpression{Name: "cond", Args: map[string]any{
		"0": &LiteralValueNode{Value: true},
		"1": &LiteralValueNode{Value: nil},
		"2": &LiteralValueNode{Value: "no"},
	}}
	got, err := evalCollCond(node, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("evalCollCond with a nil then-branch: %v", err)
	}
	if got != nil {
		t.Errorf("cond(true, nil, \"no\") = %#v, want nil", got)
	}
}

// TestCond_EvaluatesThroughEngineExecute drives the engine, not the seam.
//
// The five tests above call evalCollScalar / evalCollCond directly, which is
// precisely the anti-pattern the sibling file records as a prior review
// finding: "every other test in this file reimplements the engine's dispatch
// instead of calling the engine. A test that re-derives the code path it is
// meant to protect cannot notice that path being deleted."
//
// TestPositionalBuiltinsEvaluateThroughEngineExecute closed that hole for
// coalesce only -- all three of its cases are deployGateGreen. cond was in the
// plan-root allowlist (engine.go) with nothing gating it: deleting `cond` from
// that allowlist left the entire tree green. memql#2915 asked for this
// explicitly, wanting cond driven "through resolvePlanFunctionsWithOrigin with
// real args, since no existing test does".
//
// A synthetic logic rather than a shipped one, because there is no shipped
// single-statement `return cond(args.X, ...)` construct -- that absence is why
// the defect survived. Upsert is the package's existing idiom for this
// (authoring_validate_test.go, server_only_gate_test.go).
func TestCond_EvaluatesThroughEngineExecute(t *testing.T) {
	engine := engineForSeamTest(t)
	upsert := func(t *testing.T, name string, expr ExpressionNode) {
		t.Helper()
		if err := engine.functions.Upsert(&Function{
			Name: name, FunctionKind: "logic", Enabled: true, Expr: expr,
		}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}
	argRef := func(p string) *ArgRefExpression { return &ArgRefExpression{Path: p} }
	lit := func(v any) *LiteralValueNode { return &LiteralValueNode{Value: v} }

	// The form docs/public/language/functions.md teaches verbatim:
	// cond(args.flag, "yes", "no").
	upsert(t, "condDocsForm", &FunctionCallExpression{Name: "cond", Args: map[string]any{
		"0": argRef("flag"), "1": lit("yes"), "2": lit("no"),
	}})
	// Every operand an arg ref -- all three resolved by the substitution.
	upsert(t, "condAllArgRefs", &FunctionCallExpression{Name: "cond", Args: map[string]any{
		"0": argRef("flag"), "1": argRef("a"), "2": argRef("b"),
	}})
	// cond nested inside coalesce: the outer builtin resolves the inner one.
	upsert(t, "condInsideCoalesce", &FunctionCallExpression{Name: "coalesce", Args: map[string]any{
		"0": &FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": argRef("flag"), "1": argRef("a"), "2": lit(nil),
		}},
		"1": lit("fallback"),
	}})

	ctx := auth.ContextWithInternalOrigin(context.Background())
	for _, tc := range []struct {
		name, query string
		want        any
	}{
		{"docs form, flag true", `logic condDocsForm(flag: true)`, "yes"},
		{"docs form, flag false", `logic condDocsForm(flag: false)`, "no"},
		{"docs form, flag absent", `logic condDocsForm()`, "no"},
		{"all arg refs, true", `logic condAllArgRefs(flag: true, a: "A", b: "B")`, "A"},
		{"all arg refs, false", `logic condAllArgRefs(flag: false, a: "A", b: "B")`, "B"},
		{"nested in coalesce, chosen", `logic condInsideCoalesce(flag: true, a: "picked")`, "picked"},
		{"nested in coalesce, falls back", `logic condInsideCoalesce(flag: false, a: "picked")`, "fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := engine.Execute(ctx, tc.query)
			if err != nil {
				t.Fatalf("Execute(%s): %v\n\nIf this is \"function ... was not expanded during "+
					"parsing\", the plan-root allowlist in engine.go no longer covers cond.\n"+
					"If it is \"cond() arg 0 is not an expression\", memql#2915 has regressed.",
					tc.query, err)
			}
			if got := seamResultValue(res); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Execute(%s) = %#v, want %#v", tc.query, got, tc.want)
			}
		})
	}
}

// TestCond_ArityAndOperandDiagnostics pins the messages an author sees when a
// cond is malformed. Review found the arity check gated by nothing: deleting
// it left the whole package green.
//
// The operand-failure messages are also asserted because memql#2915's fix
// changed their shape -- both now carry `cond() arg N: `, where a failing
// branch previously surfaced a bare `division by zero` with no indication of
// which construct produced it.
func TestCond_ArityAndOperandDiagnostics(t *testing.T) {
	lit := func(v any) *LiteralValueNode { return &LiteralValueNode{Value: v} }
	boom := &ArithmeticExpression{Left: lit(1), Op: "/", Right: lit(0)}

	t.Run("too few operands", func(t *testing.T) {
		_, err := evalCollCond(&FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": lit(true), "1": lit("a"),
		}}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "requires three arguments") {
			t.Fatalf("a two-operand cond must be rejected by the arity check; got %v", err)
		}
	})
	t.Run("too many operands", func(t *testing.T) {
		_, err := evalCollCond(&FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": lit(true), "1": lit("a"), "2": lit("b"), "3": lit("c"),
		}}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "requires three arguments") {
			t.Fatalf("a four-operand cond must be rejected by the arity check; got %v", err)
		}
	})
	t.Run("a failing predicate names the operand once", func(t *testing.T) {
		_, err := evalCollCond(&FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": boom, "1": lit("a"), "2": lit("b"),
		}}, nil, nil)
		if err == nil {
			t.Fatal("a predicate that cannot evaluate must error")
		}
		if got := strings.Count(err.Error(), "cond()"); got != 1 {
			t.Errorf("the message must name cond once, not stutter; got %d in %q", got, err.Error())
		}
	})
	t.Run("a failing branch names its index", func(t *testing.T) {
		_, err := evalCollCond(&FunctionCallExpression{Name: "cond", Args: map[string]any{
			"0": lit(true), "1": boom, "2": lit("b"),
		}}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "cond() arg 1") {
			t.Fatalf("a failing branch must say which operand failed; got %v", err)
		}
	})
}
