package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

func payloadCmp(field string, op ComparisonOperator, value any) *ComparisonExpression {
	raw := "payload." + field
	return &ComparisonExpression{
		Field:    FieldReference{Raw: raw, Parts: strings.Split(raw, ".")},
		Operator: op,
		Value:    value,
	}
}

func actorCmp(field string, op ComparisonOperator, value any) *ComparisonExpression {
	raw := "actor." + field
	return &ComparisonExpression{
		Field:    FieldReference{Raw: raw, Parts: strings.Split(raw, ".")},
		Operator: op,
		Value:    value,
	}
}

func adminCtx() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-admin", Role: auth.RoleAdmin})
}

// TestCompileActorFieldComparison locks the actor-as-field SQL binding (#974):
// `actor.role == "admin"` is a query-time constant, so both operands bind as
// parameters and the DB evaluates the constant comparison.
func TestCompileActorFieldComparison(t *testing.T) {
	node := actorCmp("role", OpEq, "admin")
	require.True(t, isActorFieldComparison(node))

	result, err := compileActorFieldComparison(adminCtx(), node)
	require.NoError(t, err)
	require.Equal(t, "? = ?", strings.TrimSpace(result.sql))
	require.Equal(t, []any{"admin", "admin"}, result.args) // resolved actor role, then literal
}

// TestTryCompileCombinedFilter_MixedRowAndActorOr is the headline #974 case:
// a mixed `payload.stage=="won" || actor.role=="admin"` filter pushes down to
// a single SQL query with OR + parenthesization, instead of the old split.
func TestTryCompileCombinedFilter_MixedRowAndActorOr(t *testing.T) {
	e := &MemQLEngine{}
	expr := &LogicalExpression{
		Op:    LogicalOr,
		Left:  payloadCmp("stage", OpEq, "won"),
		Right: actorCmp("role", OpEq, "admin"),
	}

	result, ok := e.tryCompileCombinedFilter(adminCtx(), expr, "")
	require.True(t, ok, "mixed row || actor filter should compile to a single SQL fragment")
	require.Contains(t, result.sql, " OR ")
	require.True(t, strings.HasPrefix(strings.TrimSpace(result.sql), "("), "OR fragment must be parenthesized")
	require.Contains(t, result.sql, "? = ?") // the actor-field bound comparison
	// args carry the payload value plus the resolved actor role + literal.
	require.Contains(t, result.args, "admin")
}

// TestCanvasStateTwoQueryCollapse proves the representative workaround named in
// the issue collapses to one query: the CoPresent canvasState public/private
// merge -- two queries today purely because the filter parser had no OR --
// becomes a single
//
//	payload.space==<id> && (payload.visibility=="public" || payload.forUserId==actor.userId)
//
// filter. The actor term here is actor-as-value (resolved by
// resolveActorReferences before SQL compile); the OR + parens are #972/#974.
func TestCanvasStateTwoQueryCollapse(t *testing.T) {
	e := &MemQLEngine{}
	expr := &LogicalExpression{
		Op:   LogicalAnd,
		Left: payloadCmp("space", OpEq, "v1:cognition:space:s-1"),
		Right: &LogicalExpression{
			Op:   LogicalOr,
			Left: payloadCmp("visibility", OpEq, "public"),
			Right: &ComparisonExpression{
				Field:    FieldReference{Raw: "payload.forUserId", Parts: []string{"payload", "forUserId"}},
				Operator: OpEq,
				Value:    &ActorReference{Path: "userId"},
			},
		},
	}

	// Resolve actor-as-value references, exactly as the executor does before
	// SQL compilation.
	resolved, err := resolveActorReferences(adminCtx(), expr)
	require.NoError(t, err)

	result, ok := e.tryCompileCombinedFilter(adminCtx(), resolved, "")
	require.True(t, ok, "the canvasState public/private OR filter should compile to one SQL query")
	require.Contains(t, result.sql, " OR ")
	require.Contains(t, result.sql, " AND ")
	require.Contains(t, result.args, "u-admin") // the resolved actor.userId bound as a param
}

// TestTryCompileCombinedFilter_NestedParensPrecedence proves parens + AND/OR
// nesting compile with the structure preserved.
func TestTryCompileCombinedFilter_NestedParensPrecedence(t *testing.T) {
	e := &MemQLEngine{}
	// payload.a=="x" && (payload.b=="y" || actor.role=="admin")
	expr := &LogicalExpression{
		Op:   LogicalAnd,
		Left: payloadCmp("a", OpEq, "x"),
		Right: &LogicalExpression{
			Op:    LogicalOr,
			Left:  payloadCmp("b", OpEq, "y"),
			Right: actorCmp("role", OpEq, "admin"),
		},
	}

	result, ok := e.tryCompileCombinedFilter(adminCtx(), expr, "")
	require.True(t, ok)
	require.Contains(t, result.sql, " AND ")
	require.Contains(t, result.sql, " OR ")
}
