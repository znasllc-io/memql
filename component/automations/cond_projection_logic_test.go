package automations

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// cond_projection_logic_test.go drives #2542 items 2 (cond over a collection
// chain) and 3 (arithmetic in a groupBy-projection object-literal value), plus
// the lambda-carrying-chain terminal-return serializer gap, END-TO-END through
// the REAL logic path: parseLogicBody -> compileBodyToAutomation (serialize) ->
// RunLogic (re-parse + evaluate). A stub step registry stands in for the DB;
// every case below resolves locally, so nothing is dispatched -- the assertions
// pin both that the serializer emits re-parseable source and that the runtime
// re-parse evaluates it.

func runProjectionLogic(t *testing.T, src, fn string, args map[string]any) (any, []string) {
	t.Helper()
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)
	out, err := r.RunLogic(context.Background(), fn, body, args)
	if err != nil {
		t.Fatalf("RunLogic(%s): %v", fn, err)
	}
	return out, registry.dispatched
}

// --- Item 2: cond over a collection chain (terminal return) ---

func TestRunLogic_CondOverChain_TerminalReturn(t *testing.T) {
	src := `@enabled
@description("cond over a collection-chain aggregate (#2542 item 2)")
logic condReturn {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    return cond(active.any(), active.count(), 0)
  }
}
`
	out, dispatched := runProjectionLogic(t, src, "condReturn", map[string]any{
		"members": []any{
			map[string]any{"name": "a", "active": true},
			map[string]any{"name": "b", "active": false},
			map[string]any{"name": "c", "active": true},
		},
	})
	if !numericEquals(out, 2) {
		t.Errorf("return = %#v, want 2 (active count via cond then-branch)", out)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want none (cond + chain resolve locally)", dispatched)
	}
}

// The else branch is selected (and is itself a chain) when the predicate is
// false.
func TestRunLogic_CondOverChain_ElseBranch(t *testing.T) {
	src := `@enabled
@description("cond else-branch chain")
logic condElse {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    return cond(active.any(), "some-active", "none-active")
  }
}
`
	out, _ := runProjectionLogic(t, src, "condElse", map[string]any{
		"members": []any{
			map[string]any{"name": "a", "active": false},
		},
	})
	if out != "none-active" {
		t.Errorf("return = %#v, want \"none-active\"", out)
	}
}

// --- Item 2: cond as a STEP value ---

func TestRunLogic_CondOverChain_StepValue(t *testing.T) {
	src := `@enabled
@description("cond as a step value over a chain (#2542 item 2)")
logic condStep {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    label := cond(active.any(), "has-active", "no-active")
    return label
  }
}
`
	out, dispatched := runProjectionLogic(t, src, "condStep", map[string]any{
		"members": []any{
			map[string]any{"name": "a", "active": true},
		},
	})
	if out != "has-active" {
		t.Errorf("return = %#v, want \"has-active\"", out)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want none (cond step resolves locally, not via FunctionExecutor)", dispatched)
	}
}

// A cond predicate that is a comparison over a step scalar (parseable today)
// evaluates through the condition machinery.
func TestRunLogic_CondScalarComparisonPredicate(t *testing.T) {
	src := `@enabled
@description("cond with a scalar-comparison predicate")
logic condScalar {
  args {
    revenue int @required
  }
  body {
    r := coalesce(args.revenue, 0)
    return cond(r > 50, "high", "low")
  }
}
`
	if out, _ := runProjectionLogic(t, src, "condScalar", map[string]any{"revenue": 100}); out != "high" {
		t.Errorf("revenue 100 -> %#v, want \"high\"", out)
	}
	if out, _ := runProjectionLogic(t, src, "condScalar", map[string]any{"revenue": 10}); out != "low" {
		t.Errorf("revenue 10 -> %#v, want \"low\"", out)
	}
}

// TestRunLogic_CondComparisonOverChain_TerminalReturn is the #2542 item-2
// headline through the REAL source path (parse -> compile/serialize -> re-parse
// -> RunLogic): a cond whose predicate is a comparison over a collection-chain
// aggregate. Wave 3's parseExpressionArg relational-comparison extension makes
// the predicate parse; the compiler serializes the CondExpr with its
// BinaryComparisonExpr predicate, and evaluateCondPredicate routes the
// serialized comparison string through the condition evaluator. Resolves
// locally -- no step dispatch.
func TestRunLogic_CondComparisonOverChain_TerminalReturn(t *testing.T) {
	src := `@enabled
@description("cond over a comparison of a collection-chain aggregate (#2542 item 2)")
logic condChainCmp {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    return cond(active.count() > 1, "many", "few")
  }
}
`
	members := map[string]any{"members": []any{
		map[string]any{"name": "a", "active": true},
		map[string]any{"name": "b", "active": false},
		map[string]any{"name": "c", "active": true},
	}}
	out, dispatched := runProjectionLogic(t, src, "condChainCmp", members)
	if out != "many" {
		t.Errorf("return = %#v, want \"many\" (2 active > 1)", out)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want none (cond comparison-over-chain resolves locally)", dispatched)
	}

	// Predicate false -> else branch.
	fewMembers := map[string]any{"members": []any{
		map[string]any{"name": "a", "active": true},
		map[string]any{"name": "b", "active": false},
	}}
	if out, _ := runProjectionLogic(t, src, "condChainCmp", fewMembers); out != "few" {
		t.Errorf("return = %#v, want \"few\" (1 active, not > 1)", out)
	}
}

// The comparison-over-chain cond predicate also works as a STEP value (bound to
// an intermediate `:=` step, then returned) -- reached through
// reconstructPositionalBuiltinCall rather than the terminal-return path.
func TestRunLogic_CondComparisonOverChain_StepValue(t *testing.T) {
	src := `@enabled
@description("cond comparison-over-chain as a step value (#2542 item 2)")
logic condChainCmpStep {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    label := cond(active.count() >= 2, "quorum", "short")
    return label
  }
}
`
	members := map[string]any{"members": []any{
		map[string]any{"name": "a", "active": true},
		map[string]any{"name": "b", "active": true},
		map[string]any{"name": "c", "active": false},
	}}
	out, dispatched := runProjectionLogic(t, src, "condChainCmpStep", members)
	if out != "quorum" {
		t.Errorf("return = %#v, want \"quorum\" (2 active >= 2)", out)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want none (cond step resolves locally)", dispatched)
	}
}

// TestEvaluateLocalExpr_CondComparisonOverChainString drives the item-2
// headline shape -- a cond whose predicate is a comparison over a
// collection-chain aggregate -- at the EvaluateLocalExpr altitude with the
// serialized string form the compiler emits. Wave 3 landed the AUTHORING parse
// (parseExpressionArg now consumes ONE relational comparison; the wave-2 pin
// memql.TestCondComparisonOverChain_ParseGap was flipped to
// TestCondComparisonOverChain_ParsesAndEvaluates), so the end-to-end source
// path is exercised by TestRunLogic_CondComparisonOverChain_TerminalReturn
// below; this unit test keeps the direct string-form coverage.
func TestEvaluateLocalExpr_CondComparisonOverChainString(t *testing.T) {
	eval := NewEvaluator()
	eval.SetStepResult("rows", &StepResult{StepId: "rows", Status: "success", Result: []any{
		map[string]any{"active": true},
		map[string]any{"active": false},
		map[string]any{"active": true},
	}})
	out, handled, err := EvaluateLocalExpr(`cond(rows.where(r => r.active).count() > 0, rows.count(), 0)`, eval)
	if err != nil {
		t.Fatalf("EvaluateLocalExpr: %v", err)
	}
	if !handled {
		t.Fatalf("cond over a chain-comparison predicate was not handled locally")
	}
	if !numericEquals(out, 3) {
		t.Errorf("out = %#v, want 3 (predicate true -> rows.count())", out)
	}

	// FALSE case: the aggregate must be RESOLVED, not left as raw text -- 2
	// active is NOT > 5, so the else branch (0) is chosen. This pins the fix for
	// the spurious-lexicographic bug: EvaluateFilterValue left `rows.where(...)`
	// un-evaluated so the ordering degraded to `"rows..." > "5"` (letter-led
	// string, constant-true), which would have wrongly returned rows.count().
	falseOut, handled, err := EvaluateLocalExpr(`cond(rows.where(r => r.active).count() > 5, rows.count(), 0)`, eval)
	if err != nil {
		t.Fatalf("EvaluateLocalExpr false case: %v", err)
	}
	if !handled {
		t.Fatalf("false chain-comparison predicate was not handled locally")
	}
	if !numericEquals(falseOut, 0) {
		t.Errorf("false predicate out = %#v, want 0 (2 active is not > 5 -> else branch)", falseOut)
	}
}

// --- Lambda-carrying chain in TERMINAL RETURN (serializer gap) ---

func TestRunLogic_LambdaChainTerminalReturn(t *testing.T) {
	src := `@enabled
@description("lambda-carrying chain in terminal return (serializer gap)")
logic lambdaReturn {
  args {
    members []object @required
  }
  body {
    rows := args.members.where(m => m.active)
    return rows.where(m => m.vip).count()
  }
}
`
	out, dispatched := runProjectionLogic(t, src, "lambdaReturn", map[string]any{
		"members": []any{
			map[string]any{"active": true, "vip": true},
			map[string]any{"active": true, "vip": false},
			map[string]any{"active": true, "vip": true},
		},
	})
	if !numericEquals(out, 2) {
		t.Errorf("return = %#v, want 2 (vip count)", out)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want none (lambda chain resolves locally)", dispatched)
	}
}

// --- Item 3: arithmetic in a groupBy-projection object-literal value ---

func TestRunLogic_GroupByProjection_MethodCallValue(t *testing.T) {
	src := `@enabled
@description("groupBy projection with a method-call value (#2542 item 3)")
logic projCount {
  args {
    scans []object @required
  }
  body {
    rows := args.scans.where(s => s.done)
    return rows.groupBy(s => s.worker).select(g => {worker: g.key, n: g.items.count()})
  }
}
`
	out, dispatched := runProjectionLogic(t, src, "projCount", map[string]any{
		"scans": []any{
			map[string]any{"worker": "w1", "done": true},
			map[string]any{"worker": "w1", "done": true},
			map[string]any{"worker": "w2", "done": true},
		},
	})
	rows, ok := out.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("return = %#v, want 2 group rows", out)
	}
	if r0 := rows[0].(map[string]any); r0["worker"] != "w1" || !numericEquals(r0["n"], 2) {
		t.Errorf("group0 = %#v, want {worker:w1, n:2}", r0)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want none", dispatched)
	}
}

// The per-group accuracy PERCENT (`good * 100 / total`) is the idiomatic
// integer-arithmetic projection ratio -- it round-trips cleanly through the
// compile/re-parse boundary (all operands stay integers, so no float-literal
// precision is lost) and is exactly the repeat-rate / first-pass-yield percent
// shape the issue calls out. The true fractional (float) ratio via a `* 1.0`
// operand is pinned END-TO-END through the same boundary by
// TestRunLogic_GroupByProjection_FloatRatio below.
func TestRunLogic_GroupByProjection_ArithmeticRatioPercent(t *testing.T) {
	src := `@enabled
@description("per-group accuracy percent in a projection (#2542 item 3)")
logic accuracy {
  args {
    scans []object @required
  }
  body {
    rows := args.scans.where(s => s.done)
    return rows.groupBy(s => s.worker).select(g => {worker: g.key, pct: g.items.where(i => i.good).count() * 100 / g.items.count()})
  }
}
`
	out, _ := runProjectionLogic(t, src, "accuracy", map[string]any{
		"scans": []any{
			map[string]any{"worker": "w1", "done": true, "good": true},
			map[string]any{"worker": "w1", "done": true, "good": false},
			map[string]any{"worker": "w1", "done": true, "good": true},
			map[string]any{"worker": "w2", "done": true, "good": true},
		},
	})
	rows := out.([]any)
	w1 := rows[0].(map[string]any)
	if !numericEquals(w1["pct"], 66) {
		t.Errorf("w1 pct = %#v, want 66 (2 good * 100 / 3)", w1["pct"])
	}
	w2 := rows[1].(map[string]any)
	if !numericEquals(w2["pct"], 100) {
		t.Errorf("w2 pct = %#v, want 100 (1 good * 100 / 1)", w2["pct"])
	}
}

// TestRunLogic_GroupByProjection_FloatRatio pins the FRACTIONAL (float) ratio
// idiom -- `good / (total * 1.0)` -- END-TO-END through the compile/serialize/
// re-parse boundary. The `* 1.0` operand is a whole-valued float64 literal; the
// serializer must keep its decimal marker (`* 1.0`, not `* 1`), else it
// re-parses as int64 and the division silently collapses to INTEGER division,
// yielding 0 instead of ~0.6667 -- the memqllint-green/runtime-wrong class #2542
// eliminates. Regression guard for the float-literal serialization fix.
func TestRunLogic_GroupByProjection_FloatRatio(t *testing.T) {
	src := `@enabled
@description("per-group fractional accuracy ratio via a float operand (#2542 item 3)")
logic accuracyRatio {
  args {
    scans []object @required
  }
  body {
    rows := args.scans.where(s => s.done)
    return rows.groupBy(s => s.worker).select(g => {worker: g.key, acc: g.items.where(i => i.good).count() / (g.items.count() * 1.0)})
  }
}
`
	out, _ := runProjectionLogic(t, src, "accuracyRatio", map[string]any{
		"scans": []any{
			map[string]any{"worker": "w1", "done": true, "good": true},
			map[string]any{"worker": "w1", "done": true, "good": false},
			map[string]any{"worker": "w1", "done": true, "good": true},
			map[string]any{"worker": "w2", "done": true, "good": true},
		},
	})
	rows := out.([]any)
	w1 := rows[0].(map[string]any)
	acc, ok := w1["acc"].(float64)
	if !ok {
		t.Fatalf("w1 acc = %#v (%T), want float64 (integer division would yield int 0)", w1["acc"], w1["acc"])
	}
	if acc < 0.6666 || acc > 0.6667 {
		t.Errorf("w1 acc = %v, want ~0.6667 (2 good / (3 total * 1.0))", acc)
	}
	w2 := rows[1].(map[string]any)
	if acc2, ok := w2["acc"].(float64); !ok || acc2 != 1.0 {
		t.Errorf("w2 acc = %#v, want 1.0 (1 good / (1 total * 1.0))", w2["acc"])
	}
}

// Division by a zero group aggregate surfaces cleanly through the full logic
// path (never a panic).
func TestRunLogic_GroupByProjection_DivisionByZero(t *testing.T) {
	src := `@enabled
@description("projection division by a zero group aggregate")
logic ratioZero {
  args {
    scans []object @required
  }
  body {
    rows := args.scans.where(s => s.done)
    return rows.groupBy(s => s.worker).select(g => {worker: g.key, r: g.items.count() / g.items.where(i => i.bad).count()})
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)
	_, err := r.RunLogic(context.Background(), "ratioZero", body, map[string]any{
		"scans": []any{map[string]any{"worker": "w1", "done": true, "bad": false}},
	})
	if err == nil {
		t.Fatalf("projection division by zero must surface an error")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("error = %q, want it to name division by zero", err.Error())
	}
}

// TestRunLogic_CompileRoundTrip_CondAndProjection pins the SERIALIZER half:
// every #2542 item-2/3 return compiles to re-parseable MemQL, never an
// `<<unsupported expression ...>>` placeholder (the lambda-serializer gap this
// wave closes). The arrow-form lambda + parenthesized-arithmetic projection
// body must round-trip.
func TestRunLogic_CompileRoundTrip_CondAndProjection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // substrings the serialized _return must contain
	}{
		{
			name: "cond_over_chain",
			body: "active := args.members.where(m => m.active)\n    return cond(active.any(), active.count(), 0)",
			want: []string{"cond(active.any(), active.count(), 0)"},
		},
		{
			name: "lambda_chain_return",
			body: "rows := args.members.where(m => m.active)\n    return rows.where(m => m.vip).count()",
			want: []string{"rows.where(m => m.vip).count()"},
		},
		{
			name: "projection_arithmetic",
			body: "rows := args.scans.where(s => s.done)\n    return rows.groupBy(s => s.worker).select(g => {worker: g.key, n: (g.items.count() / g.items.count())})",
			want: []string{"groupBy(s => s.worker)", "select(g =>", "(g.items.count() / g.items.count())"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			src := "@enabled\nlogic probe {\n  args {\n    members []object @required\n    scans []object @required\n  }\n  body {\n    " + tc.body + "\n  }\n}\n"
			body := parseLogicBody(t, src)
			r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)
			auto, err := r.compileBodyToAutomation("probe", body)
			if err != nil {
				t.Fatalf("compileBodyToAutomation: %v", err)
			}
			var retStr string
			for _, s := range auto.Steps {
				if s != nil && s.ID == "_return" && s.Query != nil {
					retStr = s.Query.Query
				}
			}
			if retStr == "" {
				t.Fatalf("no _return step compiled")
			}
			if strings.Contains(retStr, "<<unsupported") {
				t.Fatalf("_return carries an unsupported-expression placeholder: %q", retStr)
			}
			for _, want := range tc.want {
				if !strings.Contains(retStr, want) {
					t.Errorf("_return = %q, want it to contain %q", retStr, want)
				}
			}
		})
	}
}
