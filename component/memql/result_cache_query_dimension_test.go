package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The per-query cache dimension (memql#4532).
//
// WHY A NAME AND NOT THE OBSERVE RECORD. The task left the choice open
// between stamping `cached` into the observe invocation record and a per-FQN
// counter pair in component/metrics, asking for the reason to be recorded.
// It is the counter pair, for one decisive reason: observe's shipped default
// level is `off` on every node type (component/observe/CLAUDE.md), so a
// `cached` bool stamped into the invocation record answers nothing at all
// until an operator has already turned observe on for that FQN -- and the
// operator asking "is caching working for this query" is precisely the
// operator who has not. The metrics path is always-on, costs one counter
// increment on a path that has already done a cache lookup, and needs no
// TimescaleDB round trip. The observe record remains the right home for
// per-invocation DETAIL (args, duration, trace id); a hit ratio is an
// aggregate, which is what a counter is for.
//
// The name is bounded BY CONSTRUCTION: it is a registered query construct's
// name or nothing. See metrics.AdhocQueryLabel.
func TestQueryFunctionNameForPlanRoot_NamesTheQueryConstruct(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "spaceParticipants",
		FunctionKind: "query",
		Enabled:      true,
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:cognition:participant",
		},
	}))

	got := queryFunctionNameForPlanRoot(&FunctionCallExpression{Name: "spaceParticipants", Args: map[string]any{}}, reg)
	require.Equal(t, "spaceParticipants", got)
}

// The name has to survive the directive wrappers, because the real corpus is
// full of them -- sort / paginate / shape are on most reads. Resolving
// through them is what keeps a paginated query from reporting as ad-hoc.
func TestQueryFunctionNameForPlanRoot_ResolvesThroughDirectiveWrappers(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "sitesAll",
		FunctionKind: "query",
		Enabled:      true,
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:platform:site",
		},
	}))

	call := &FunctionCallExpression{Name: "sitesAll", Args: map[string]any{}}
	wrapped := &SortExpression{Target: &CountExpression{Target: call}}

	require.Equal(t, "sitesAll", queryFunctionNameForPlanRoot(wrapped, reg))
}

// A mutation call is not a cacheable read and must contribute no per-query
// series -- otherwise the denominator of every hit ratio is wrong.
func TestQueryFunctionNameForPlanRoot_IgnoresNonQueryKinds(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "createSpace",
		FunctionKind: "mutation",
		Enabled:      true,
	}))

	require.Equal(t, "", queryFunctionNameForPlanRoot(&FunctionCallExpression{Name: "createSpace", Args: map[string]any{}}, reg))
}

// An ad-hoc filter expression resolves to no name. The recorder folds "" into
// metrics.AdhocQueryLabel; what matters here is that nothing else -- least of
// all the query text -- is offered as a substitute.
func TestQueryFunctionNameForPlanRoot_AdhocExpressionHasNoName(t *testing.T) {
	reg := newFunctionRegistry()
	adhoc := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    "v1:cognition:space",
	}
	require.Equal(t, "", queryFunctionNameForPlanRoot(adhoc, reg))
	require.Equal(t, "", queryFunctionNameForPlanRoot(nil, reg))
	require.Equal(t, "", queryFunctionNameForPlanRoot(adhoc, nil))
}

// The stamp has to happen BEFORE expansion, for the same reason BoundConcept
// does: afterwards the plan root is the function's body and there is no name
// left to read. This pins the ordering rather than the mechanism.
func TestResolvePlanFunctions_StampsSourceFunctionBeforeExpansion(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "activeAgentRoles",
		FunctionKind: "query",
		Enabled:      true,
		BoundConcept: "v1:agents:agentRole",
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:agents:agentRole",
		},
	}))

	plan := &QueryPlan{Root: &FunctionCallExpression{Name: "activeAgentRoles", Args: map[string]any{}}}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))

	require.Equal(t, "activeAgentRoles", plan.SourceFunction,
		"the plan lost the query's name during expansion -- every read would report as ad-hoc")
	require.Equal(t, "v1:agents:agentRole", plan.BoundConcept,
		"the sibling stamp must be unaffected")
}
