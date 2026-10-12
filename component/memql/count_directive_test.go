package memql

// count_directive_test.go -- unit coverage for the `count` aggregate
// directive (memql#1730). These are DB-free: they exercise the plan-level
// peeling (applyDirectiveWrappers -> plan.Count), the canonical signature,
// and the AST conversion, which are the engine-side seams the count query
// rides through before reaching executeCountPlan.

import (
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func conceptEq(value string) *ComparisonExpression {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    value,
	}
}

// applyDirectiveWrappers must peel the outermost CountExpression off and set
// plan.Count, leaving the inner filter as the normalized root.
func TestApplyDirectiveWrappers_CountSetsPlanFlag(t *testing.T) {
	inner := conceptEq("v1:identity:user")
	plan := &QueryPlan{Root: &CountExpression{Target: inner}}

	root, err := applyDirectiveWrappers(plan)
	require.NoError(t, err)
	require.True(t, plan.Count, "count directive must set plan.Count")
	require.Equal(t, inner, root, "the inner filter must remain as the normalized root")
}

// Nested count() directives are a malformed query and must be rejected.
func TestApplyDirectiveWrappers_DoubleCountRejected(t *testing.T) {
	plan := &QueryPlan{Root: &CountExpression{Target: &CountExpression{Target: conceptEq("v1:identity:user")}}}
	_, err := applyDirectiveWrappers(plan)
	require.Error(t, err)
	require.Contains(t, err.Error(), "multiple count() directives")
}

// A stray CountExpression buried below a non-directive node must be rejected by
// the outermost-wrapper guard (ensureNoDirectiveNodes), same as the other
// directive wrappers.
func TestApplyDirectiveWrappers_CountMustBeOutermost(t *testing.T) {
	plan := &QueryPlan{Root: &LogicalExpression{
		Op:    LogicalAnd,
		Left:  conceptEq("v1:identity:user"),
		Right: &CountExpression{Target: conceptEq("v1:identity:user")},
	}}
	_, err := applyDirectiveWrappers(plan)
	require.Error(t, err, "count() must be the outermost wrapper")
}

// canonicalExpression must render the count wrapper so the cache signature is
// distinct from the same filter without the aggregate.
func TestCanonicalExpression_Count(t *testing.T) {
	inner := conceptEq("v1:identity:user")
	withCount := canonicalExpression(&CountExpression{Target: inner})
	withoutCount := canonicalExpression(inner)
	require.NotEqual(t, withCount, withoutCount, "count must change the canonical signature")
	require.Contains(t, withCount, "count(")
}

// The AST converter must translate the language-parser CountExpr into the
// engine CountExpression (a missing case errors in convertExpression's default).
func TestASTConverter_CountExpr(t *testing.T) {
	conv := NewASTConverter()
	out, err := conv.ConvertExpression(&languageParser.CountExpr{
		Target: &languageParser.ComparisonExpr{
			Field:    languageParser.FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: languageParser.ComparisonOperator("=="),
			Value:    "v1:identity:user",
		},
	})
	require.NoError(t, err)
	count, ok := out.(*CountExpression)
	require.True(t, ok, "expected *CountExpression, got %T", out)
	require.NotNil(t, count.Target)
}
