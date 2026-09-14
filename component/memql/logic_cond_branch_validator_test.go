package memql

// logic_cond_branch_validator_test.go -- memql#2655 and #2693, in edition 2026.
//
// Before edition 2026 an identifier-led comparison in a VALUE position -- a
// cond branch, a coalesce or concat argument, a bare `return args.b == "y"` --
// was a trap: the multi-step string path returned the expression's own SOURCE
// TEXT (`args.b=="y"`) as the value, and the engine path mis-routed it as a
// store query, so the loader refused every such shape at load ("VALUE
// position"). Edition 2026 has one grammar with one precedence table (rule
// 11c): a comparison is a boolean value in any position, and every value a
// logic body returns or binds is evaluated by EvalExpr, on the LogicRunner or
// as a plan constant. The trap is gone rather than guarded, so these tests pin
// the new truth -- each former trap shape loads, and answers the boolean, not
// its source text -- and that the shapes that were always legal still load.

import (
	"context"
	"strings"
	"testing"
)

// loadValueProbe loads a logic body with one intermediate statement and the
// given return, and hands back the function.
func loadValueProbe(t *testing.T, ret string) (*Function, error) {
	t.Helper()
	src := strings.Join([]string{
		"@description(\"value position probe\")",
		"logic condBranchValProbe {",
		"  args {",
		"    a string @required",
		"    b string",
		"  }",
		"  body {",
		"    z := args.b ?? \"\"",
		"    return " + ret,
		"  }",
		"}",
	}, "\n")
	return tryParseNewFunctionSyntax("condBranchValProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
}

// TestLogicComparisonIsABooleanValue_InEveryValuePosition: every shape the
// pre-2026 gate refused as a comparison in a value position loads, and its
// value is the comparison's boolean.
func TestLogicComparisonIsABooleanValue_InEveryValuePosition(t *testing.T) {
	args := map[string]any{"a": "x", "b": "y"}
	for name, tc := range map[string]struct {
		ret  string
		want any
	}{
		"then-branch":             {`args.a == "x" ? args.b == "y" : "n"`, true},
		"else-branch":             {`args.a == "z" ? "y" : args.b == "n"`, false},
		"nested-then":             {`args.a == "x" ? (z == "y" ? args.b == "y" : "m") : "n"`, true},
		"coalesce-arg":            {`(args.a == "x" ? args.b == "y" : "n") ?? ""`, true},
		"nested-predicate":        {`(z == "q" ? true : args.b == "y") ? "1" : "2"`, "1"},
		"object-literal":          {`{v: args.a == "x" ? args.b == "y" : "n"}`, map[string]any{"v": true}},
		"coalesce-arg-comparison": {`args.b == "y" ?? "n"`, true},
		"bare terminal":           {`args.b == "y"`, true},
	} {
		t.Run(name, func(t *testing.T) {
			fn, err := loadValueProbe(t, tc.ret)
			if err != nil {
				t.Fatalf("%s must load in edition 2026: %v", tc.ret, err)
			}
			pc, ok := fn.Expr.(*PlanConstExpression)
			if !ok {
				t.Fatalf("fn.Expr = %T, want the returned expression as a *PlanConstExpression", fn.Expr)
			}
			// The runner binds the statement's local beside the arguments.
			got, err := EvalExpr(context.Background(), pc.Expr, MapScope{"args": args, "z": "y"}, EvalOptions{})
			if err != nil {
				t.Fatalf("%s: %v", tc.ret, err)
			}
			if !exprEqualForTest(got, tc.want) {
				t.Fatalf("%s = %#v, want %#v -- the comparison's value, never its source text", tc.ret, got, tc.want)
			}
		})
	}
}

// exprEqualForTest compares an EvalExpr result with an expected Go value,
// maps by their entries.
func exprEqualForTest(got, want any) bool {
	gm, gok := got.(map[string]any)
	wm, wok := want.(map[string]any)
	if gok || wok {
		if !gok || !wok || len(gm) != len(wm) {
			return false
		}
		for k, v := range wm {
			if gm[k] != v {
				return false
			}
		}
		return true
	}
	return got == want
}

// The bind-then-return idiom: a comparison as a statement's right-hand side,
// directly or under a conditional, loads -- the step's value is evaluated by
// the same EvalExpr as the return.
func TestLogicComparisonIsABooleanValue_AsAStatementValue(t *testing.T) {
	for name, rhs := range map[string]string{
		"direct":           `args.b == "y"`,
		"under a ternary":  `args.a == "x" ? args.b == "y" : "n"`,
		"coalesce-wrapped": `(args.a == "x" ? args.b == "y" : "n") ?? ""`,
		"nested ternary":   `args.a == "x" ? (z == "q" ? args.b == "y" : "m") : "n"`,
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Join([]string{
				"@description(\"statement value probe\")",
				"logic condBranchAssignProbe {",
				"  args {",
				"    a string @required",
				"    b string",
				"  }",
				"  body {",
				"    z := args.b ?? \"\"",
				"    w := " + rhs,
				"    return w",
				"  }",
				"}",
			}, "\n")
			fn, err := tryParseNewFunctionSyntax("condBranchAssignProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
			if err != nil {
				t.Fatalf("`w := %s` must load in edition 2026: %v", rhs, err)
			}
			if fn.LogicSteps == nil {
				t.Fatal("a body with statements before its return runs on the LogicRunner")
			}
		})
	}
}

// The pre-2026 #2542 arithmetic trap inside a lambda of a statement's call:
// `m.len - m.cap > 0` reads by precedence as `(m.len - m.cap) > 0`, so the
// statement loads.
func TestLogicStepArgs_ArithmeticReadsByPrecedence(t *testing.T) {
	src := strings.Join([]string{
		"@description(\"arithmetic in a lambda probe\")",
		"logic arithStepArgsProbe {",
		"  args {",
		"    b string",
		"  }",
		"  body {",
		"    w := args.b.split(\",\").where(m => m.len - m.cap > 0).count() > 0 ? \"y\" : \"n\"",
		"    return w",
		"  }",
		"}",
	}, "\n")
	if _, err := tryParseNewFunctionSyntax("arithStepArgsProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry()); err != nil {
		t.Fatalf("`m.len - m.cap > 0` is `(m.len - m.cap) > 0` and must load: %v", err)
	}
}

// TestLogicValuePosition_LegalShapesLoad pins the shapes that were always
// legal -- boolean-combinator returns (dsl/cluster/logic.memql), a
// conditional's predicate, lambda bodies, parenthesised comparisons, value
// operators with no comparison, named query calls, literals -- still load.
func TestLogicValuePosition_LegalShapesLoad(t *testing.T) {
	for name, ret := range map[string]string{
		"boolean AND return":     `z == "" && args.b == "bff"`,
		"boolean OR return":      `args.b == "x" || args.b == "y"`,
		"conditional predicate":  `args.b == "y" ? "1" : "2"`,
		"nested conditional":     `args.a == "x" ? (z == "q" ? "1" : "2") : "n"`,
		"parenthesised compare":  `(args.b ?? "") == "y"`,
		"lambda-body comparison": `args.b.split(",").where(m => m == "a" ? m == "b" : false).count()`,
		"coalesce non-compare":   `args.b ?? "default"`,
		"join non-compare":       `args.a + "-" + (args.b ?? "")`,
		"named query call":       `query listThings()`,
		"plain literal":          `"just a string"`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadValueProbe(t, ret); err != nil {
				t.Fatalf("%s must load: %v", ret, err)
			}
		})
	}
}
