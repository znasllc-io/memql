package steps

// logic_arg_counter_roundtrip_test.go -- memql#2542 item 5: "arithmetic on a
// counter field that was passed as a logic arg and re-parsed mis-types it;
// `x - 10` won't parse inside a lambda".
//
// The item names two suspects on the serialize->re-parse boundary owned by
// this file's production code (function.go): renderMemQLValue's numeric
// rendering, and literal type inference on re-parse of object literals.
// The round-trip tests here PIN both clean: an int counter rendered by
// renderFunctionArgs re-parses as int64 -- standalone, nested in an object
// literal, and inside a row collection -- and stays comparable as a number
// through the full logic path (never bool).
//
// The remaining facet was a GRAMMAR gap, now CLOSED (#2542 item 5 residual):
// the langparser used to parse comparisons only identifier-led
// (parseIdentifierExpression), so a comparison whose LHS is arithmetic
// (`r.count - 10 > 0`) -- or any non-identifier-led comparison
// (`0 < r.count`) -- could not parse inside a lambda. A comparison precedence
// level above parseAdditive (parser.parseComparisonLevel) now produces an
// expression-led BinaryComparisonExpr, converted + evaluated in-memory
// wherever collection lambdas run. TestLambdaArithmeticUnderComparison_ParsesAndEvaluates
// asserts the now-working parse shape and the in-memory evaluation.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// TestLogicArgIntCounterRoundTrip_TypesPreserved drives the exact
// FunctionExecutor serialize path (resolveArgsRefs + renderFunctionArgs)
// and the engine's re-parse (langparser.ParseExpression, the sole runtime
// parser) for int counters in every arg position, asserting the values
// come back typed int64 -- ruling out the two #2542-item-5 suspects that
// live in this package.
func TestLogicArgIntCounterRoundTrip_TypesPreserved(t *testing.T) {
	cases := []struct {
		name   string
		args   map[string]any
		verify func(t *testing.T, got map[string]any)
	}{
		{
			name: "scalar int arg",
			args: map[string]any{"count": 3},
			verify: func(t *testing.T, got map[string]any) {
				if got["count"] != int64(3) {
					t.Errorf("count = %#v (%T), want int64(3)", got["count"], got["count"])
				}
			},
		},
		{
			name: "object-literal counter arg (the {count: 3} shape)",
			args: map[string]any{"counter": map[string]any{"count": 3}},
			verify: func(t *testing.T, got map[string]any) {
				counter, ok := got["counter"].(map[string]any)
				if !ok {
					t.Fatalf("counter = %#v (%T), want object", got["counter"], got["counter"])
				}
				if counter["count"] != int64(3) {
					t.Errorf("counter.count = %#v (%T), want int64(3)", counter["count"], counter["count"])
				}
			},
		},
		{
			name: "row collection with int counters",
			args: map[string]any{"rows": []any{
				map[string]any{"count": 3},
				map[string]any{"count": 30},
			}},
			verify: func(t *testing.T, got map[string]any) {
				rows, ok := got["rows"].([]any)
				if !ok || len(rows) != 2 {
					t.Fatalf("rows = %#v, want 2-row collection", got["rows"])
				}
				for i, r := range rows {
					row, _ := r.(map[string]any)
					if row == nil {
						t.Fatalf("rows[%d] = %#v, want object", i, r)
					}
					if _, isInt := row["count"].(int64); !isInt {
						t.Errorf("rows[%d].count = %#v (%T), want int64 (memql#2542 item 5: an int counter must never re-type)", i, row["count"], row["count"])
					}
				}
			},
		},
		{
			name: "float64-decoded counter narrows to int64",
			// JSON-decoded automation configs carry numbers as float64;
			// renderMemQLValue prints 3 and the re-parse types the bare
			// integer int64 (the documented int-first literal dance).
			args: map[string]any{"count": float64(3)},
			verify: func(t *testing.T, got map[string]any) {
				if got["count"] != int64(3) {
					t.Errorf("count = %#v (%T), want int64(3)", got["count"], got["count"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolveArgsRefs(tc.args, automations.NewEvaluator())
			if err != nil {
				t.Fatalf("resolveArgsRefs: %v", err)
			}
			query := "logicBumpProbe(" + renderFunctionArgs(resolved) + ")"
			parsed, err := langparser.ParseExpression(query)
			if err != nil {
				t.Fatalf("re-parse failed: %v\nquery: %s", err, query)
			}
			call, ok := parsed.(*langparser.FunctionCallExpr)
			if !ok {
				t.Fatalf("expected *FunctionCallExpr, got %T", parsed)
			}
			for k, v := range call.Args {
				if _, isBool := v.(bool); isBool {
					t.Errorf("arg %q re-typed to bool (%#v) -- the #2542 item-5 mis-typing", k, v)
				}
			}
			tc.verify(t, call.Args)
		})
	}
}

// TestLogicArgIntCounterComparison_FullPath runs the round-tripped counter
// rows through the real logic path: the receiving logic compares the
// counter numerically in a where() lambda. A bool-typed counter cannot
// satisfy `r.count > 10`, so the expected single match proves the value
// arrived as a number.
func TestLogicArgIntCounterComparison_FullPath(t *testing.T) {
	logicSrc := `@enabled
@description("memql#2542 item 5 counter probe")
logic logicCounterProbe {
  args {
    rows []object @required
  }
  body {
    big := args.rows.where(r => r.count > 10)
    return big.count()
  }
}
`
	body := parseLogicBodyForSteps(t, logicSrc)

	// Serialize + re-parse the arg exactly as the function step does.
	resolved, err := resolveArgsRefs(map[string]any{"rows": []any{
		map[string]any{"count": 3},
		map[string]any{"count": 30},
	}}, automations.NewEvaluator())
	if err != nil {
		t.Fatalf("resolveArgsRefs: %v", err)
	}
	query := "logicCounterProbe(" + renderFunctionArgs(resolved) + ")"
	parsed, err := langparser.ParseExpression(query)
	if err != nil {
		t.Fatalf("re-parse failed: %v\nquery: %s", err, query)
	}
	call := parsed.(*langparser.FunctionCallExpr)

	runner := automations.NewLogicRunner(&memql.MemQLEngine{}, &nullStepRegistry{}, nil)
	out, err := runner.RunLogic(context.Background(), "logicCounterProbe", body, call.Args)
	if err != nil {
		t.Fatalf("RunLogic on round-tripped counter rows: %v", err)
	}
	if !intEquals(out, 1) {
		t.Errorf("numeric comparison on the round-tripped counter: got %#v (%T), want 1 matching row (a bool-typed counter matches 0)", out, out)
	}
}

// TestLambdaArithmeticUnderComparison_ParsesAndEvaluates is the FLIP of the
// wave-1 pin TestLambdaArithmeticUnderComparison_ParseGap: the #2542 item-5
// grammar gap is closed. An expression-led comparison inside a lambda --
// arithmetic LHS (`r.count - 10 > 0`), parenthesised arithmetic LHS, or a
// literal-led comparison (`0 < r.count - 10`) -- now parses to a
// BinaryComparisonExpr and evaluates in-memory. The pure-arithmetic and
// identifier-led lambda bodies that already parsed keep working.
func TestLambdaArithmeticUnderComparison_ParsesAndEvaluates(t *testing.T) {
	// Previously-supported lambda shapes still parse.
	stillParses := []string{
		`args.rows.select(r => r.count - 10)`,
		`args.rows.select(r => r.count * 100 / 2)`,
		`args.rows.where(r => r.count > 10)`,
	}
	for _, src := range stillParses {
		if _, err := langparser.ParseExpression(src); err != nil {
			t.Errorf("expected %q to still parse; got: %v", src, err)
		}
	}

	// The wave-1 rejected forms now parse inside the lambda's where() arg list.
	nowParses := []string{
		`args.rows.where(r => r.count - 10 > 0)`,   // arithmetic LHS
		`args.rows.where(r => (r.count - 10) > 0)`, // parenthesised arithmetic LHS
		`args.rows.where(r => 0 < r.count - 10)`,   // literal-led comparison
	}
	for _, src := range nowParses {
		if _, err := langparser.ParseExpression(src); err != nil {
			t.Errorf("expected %q to parse (the #2542 item-5 grammar gap is closed); got: %v", src, err)
		}
	}

	// Parse shape: an expression-led comparison is a *BinaryComparisonExpr with
	// full-expression operands, distinct from the Field-led *ComparisonExpr an
	// identifier-led comparison produces.
	shapeCases := []struct {
		src      string
		wantOp   langparser.ComparisonOperator
		leftKind string // "arith" | "literal"
	}{
		{`r.count - 10 > 0`, langparser.OpGt, "arith"},
		{`(r.count - 10) > 0`, langparser.OpGt, "arith"},
		{`0 < r.count - 10`, langparser.OpLt, "literal"},
	}
	for _, sc := range shapeCases {
		parsed, err := langparser.ParseExpression(sc.src)
		if err != nil {
			t.Fatalf("ParseExpression(%q): %v", sc.src, err)
		}
		cmp, ok := parsed.(*langparser.BinaryComparisonExpr)
		if !ok {
			t.Fatalf("%q parsed to %T, want *BinaryComparisonExpr", sc.src, parsed)
		}
		if cmp.Operator != sc.wantOp {
			t.Errorf("%q operator = %q, want %q", sc.src, cmp.Operator, sc.wantOp)
		}
		switch sc.leftKind {
		case "arith":
			if _, ok := cmp.Left.(*langparser.ArithmeticExpr); !ok {
				t.Errorf("%q left = %T, want *ArithmeticExpr", sc.src, cmp.Left)
			}
		case "literal":
			if _, ok := cmp.Left.(*langparser.LiteralExpr); !ok {
				t.Errorf("%q left = %T, want *LiteralExpr", sc.src, cmp.Left)
			}
		}
	}

	// Identifier-led comparisons keep their Field-led *ComparisonExpr shape --
	// the new level is a no-op passthrough for them (back-compat).
	idLed, err := langparser.ParseExpression(`r.count > 10`)
	if err != nil {
		t.Fatalf("ParseExpression(r.count > 10): %v", err)
	}
	if _, ok := idLed.(*langparser.ComparisonExpr); !ok {
		t.Errorf("identifier-led `r.count > 10` = %T, want *ComparisonExpr (unchanged shape)", idLed)
	}

	// In-memory evaluation: a where() lambda with an arithmetic-LHS comparison
	// keeps exactly the rows whose count exceeds 10 (count - 10 > 0).
	logicSrc := `@enabled
@description("memql#2542 item 5 residual: expression-led comparison in a lambda")
logic logicArithCmpProbe {
  args {
    rows []object @required
  }
  body {
    big := args.rows.where(r => r.count - 10 > 0)
    return big.count()
  }
}
`
	body := parseLogicBodyForSteps(t, logicSrc)
	runner := automations.NewLogicRunner(&memql.MemQLEngine{}, &nullStepRegistry{}, nil)
	out, err := runner.RunLogic(context.Background(), "logicArithCmpProbe", body, map[string]any{
		"rows": []any{
			map[string]any{"count": int64(3)},
			map[string]any{"count": int64(30)},
			map[string]any{"count": int64(11)},
		},
	})
	if err != nil {
		t.Fatalf("RunLogic with arithmetic-LHS comparison lambda: %v", err)
	}
	if !intEquals(out, 2) {
		t.Errorf("where(r => r.count - 10 > 0) kept %#v rows, want 2 (count 30 and 11)", out)
	}
}
