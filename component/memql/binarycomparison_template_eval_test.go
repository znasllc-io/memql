package memql

// binarycomparison_template_eval_test.go -- the mutation-template evaluator's
// half of the memql#2654 shape normalization. Template values carry cond(...)
// predicates parsed by the restricted builtin-arg grammar; with ==/!=
// normalized to *BinaryComparisonExpr (the shape the relationals already
// emit), evalParserExpression must evaluate that shape directly. Without the
// case, the node hits the typed-node rejection ("unsupported expression in
// mutation template") -- a loud error, which is also what relational template
// predicates did on main before this case existed.

import (
	"context"
	"testing"
	"time"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func TestEvalParserExpression_BinaryComparison(t *testing.T) {
	e := &mutationTemplateEvaluator{args: map[string]any{}, now: time.Now()}
	lit := func(v any) languageParser.ExpressionNode {
		return &languageParser.LiteralExpr{Value: v}
	}
	for _, tc := range []struct {
		op          languageParser.ComparisonOperator
		left, right any
		want        bool
	}{
		{languageParser.OpEq, "active", "active", true},
		{languageParser.OpEq, "active", "archived", false},
		{languageParser.OpNe, "active", "archived", true},
		{languageParser.OpNe, "active", "active", false},
		{languageParser.OpLt, int64(1), int64(2), true},
		{languageParser.OpLt, int64(2), int64(1), false},
		{languageParser.OpLe, int64(2), int64(2), true},
		{languageParser.OpGt, int64(3), int64(2), true},
		{languageParser.OpGe, int64(2), int64(3), false},
	} {
		got, err := e.evalParserExpression(context.Background(), &languageParser.BinaryComparisonExpr{
			Left: lit(tc.left), Operator: tc.op, Right: lit(tc.right),
		})
		if err != nil {
			t.Fatalf("%v %s %v: %v", tc.left, tc.op, tc.right, err)
		}
		if got != tc.want {
			t.Errorf("%v %s %v = %v, want %v", tc.left, tc.op, tc.right, got, tc.want)
		}
	}
}

// A cond() whose predicate is the normalized comparison shape evaluates the
// correct branch end to end through the template evaluator -- the exact node
// the arg grammar now produces for `cond(coalesce(args.x, "") == "y", ...)`.
func TestEvalParserExpression_CondOverBinaryComparison(t *testing.T) {
	e := &mutationTemplateEvaluator{args: map[string]any{}, now: time.Now()}
	pred := &languageParser.BinaryComparisonExpr{
		Left:     &languageParser.LiteralExpr{Value: "y"},
		Operator: languageParser.OpEq,
		Right:    &languageParser.LiteralExpr{Value: "y"},
	}
	got, err := e.evalParserExpression(context.Background(), &languageParser.CondExpr{
		Condition: pred,
		Then:      &languageParser.LiteralExpr{Value: "match"},
		Else:      &languageParser.LiteralExpr{Value: "miss"},
	})
	if err != nil {
		t.Fatalf("cond over BinaryComparisonExpr: %v", err)
	}
	if got != "match" {
		t.Errorf("cond took the wrong branch: got %v, want match", got)
	}
}
