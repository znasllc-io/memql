package steps

import (
	"testing"
)

// expression_conformance_wave3_cond_comparison_test.go extends the
// expression-evaluator conformance matrix with the wave-3 #2542 item-2 headline:
// a cond whose predicate is a COMPARISON over a collection-chain aggregate /
// step-method accessor (`cond(rows.where(...).count() > N, ...)`). The wave-3
// parseExpressionArg relational-comparison extension makes it authorable; the
// evaluateCondPredicate chain-comparison path resolves the aggregate operand
// through the local evaluator (not EvaluateCondition's EvaluateFilterValue,
// which left the aggregate text un-evaluated and degraded the ordering to a
// spurious letter-led lexicographic compare -- constant-true).
//
// A FALSE case is included deliberately: the constant-true bug the fix removes
// only manifested when the predicate SHOULD be false, so every threshold below
// keeps a false row. Logic-time only (logicLocal).
var wave3CondComparisonCases = []conformanceCase{
	{
		name:  "wave3_cond_chain_comparison_true",
		setup: seedStep("rows", []any{map[string]any{"active": true}, map[string]any{"active": false}, map[string]any{"active": true}}, "success"),
		expr:  `cond(rows.where(r => r.active).count() > 1, "many", "few")`,
		want:  "many", // 2 active > 1
	},
	{
		name:  "wave3_cond_chain_comparison_false",
		setup: seedStep("rows", []any{map[string]any{"active": true}, map[string]any{"active": false}, map[string]any{"active": true}}, "success"),
		expr:  `cond(rows.where(r => r.active).count() > 5, "many", "few")`,
		want:  "few", // 2 active is NOT > 5 -- would be spuriously "many" pre-fix
	},
	{
		name:  "wave3_cond_step_accessor_comparison_false",
		setup: seedStep("rows", []any{map[string]any{"active": true}, map[string]any{"active": true}}, "success"),
		expr:  `cond(rows.count() >= 5, "big", "small")`,
		want:  "small", // rows.count() is 2, not >= 5
	},
}

func TestExpressionEvaluators_Conformance_Wave3CondComparison(t *testing.T) {
	for _, c := range wave3CondComparisonCases {
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
