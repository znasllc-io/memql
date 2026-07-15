package steps

import (
	"testing"
)

// expression_conformance_wave2_cond_projection_test.go extends the
// expression-evaluator conformance matrix (expression_conformance_test.go) with
// the #2542 items 2-3 positions -- cond over a collection chain and arithmetic
// in a groupBy-projection object-literal value. It lives in its own file, and
// runs through the SAME logicLocal engine (automations.EvaluateLocalExpr) and
// the shared conformanceCase / seedStep helpers, so it is part of the same
// matrix contract without editing the shared table literal (the wave-2 waves
// append their rows in separate files to stay merge-clean, per the wave-1
// pattern).
//
// These positions are LOGIC-TIME ONLY: the arg-time resolver neither drives
// collection chains nor parses infix arithmetic (TestArithmetic_ArgTimeStaysLiteral
// pins that boundary), so they run through logicLocal alone.
var wave2CondProjectionCases = []conformanceCase{
	{
		// Item 2: cond whose predicate is a bare boolean chain and whose chosen
		// branch is itself a chain aggregate.
		name:  "wave2_cond_over_chain_aggregate",
		setup: seedStep("rows", []any{map[string]any{"active": true}, map[string]any{"active": false}, map[string]any{"active": true}}, "success"),
		expr:  `cond(rows.any(r => r.active), rows.count(), 0)`,
		want:  3,
	},
	{
		// The lambda-carrying-chain terminal-return serializer gap: a
		// where(lambda).count() resolves locally instead of serializing to
		// <<unsupported expression *ast.LambdaExpr>>.
		name:  "wave2_lambda_chain_count",
		setup: seedStep("rows", []any{map[string]any{"active": true}, map[string]any{"active": false}, map[string]any{"active": true}}, "success"),
		expr:  `rows.where(r => r.active).count()`,
		want:  2,
	},
	{
		// Item 3: an arithmetic (percent) value inside a groupBy projection
		// object literal, aggregated to a scalar so the row is order-stable.
		name:  "wave2_projection_arithmetic_percent",
		setup: seedStep("scans", []any{map[string]any{"w": "a", "good": true}, map[string]any{"w": "a", "good": false}, map[string]any{"w": "b", "good": true}}, "success"),
		expr:  `scans.groupBy(s => s.w).select(g => {w: g.key, pct: g.items.where(i => i.good).count() * 100 / g.items.count()}).sum(g => g.pct)`,
		want:  150, // group a: 1*100/2=50 ; group b: 1*100/1=100 ; sum 150
	},
}

func TestExpressionEvaluators_Conformance_Wave2CondProjection(t *testing.T) {
	for _, c := range wave2CondProjectionCases {
		c := c
		t.Run(c.name+"/logicLocal", func(t *testing.T) {
			got, err := logicLocal(c.newSeededEvaluator(), c.expr)
			if err != nil {
				t.Fatalf("logicLocal(%q): unexpected error: %v", c.expr, err)
			}
			if !conformanceEqual(got, c.want) {
				t.Fatalf("logicLocal(%q) = %#v, want %#v", c.expr, got, c.want)
			}
		})
	}
}
