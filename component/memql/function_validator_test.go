package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateFunctionArgs_RichSchemas(t *testing.T) {
	v := &functionValidator{}
	noExtra := false
	min := 1.0
	max := 5.0

	fn := &Function{
		Name: "recordSurveyAnswer",
		ArgsSchema: &ArgsSchemaConfig{
			AdditionalProperties: &noExtra,
			Fields: []*FunctionArgsField{
				{Name: "spaceId", Type: "string"},
				{Name: "questionNumber", Type: "number", Minimum: &min, Maximum: &max},
				{Name: "status", Type: "string", Optional: true, Enum: []any{"active", "closed"}},
				{Name: "scheduledAt", Type: "string", Optional: true, Format: "date-time"},
				{
					Name:     "payload",
					Type:     "object",
					Optional: true,
					Nested: []*FunctionArgsField{
						{Name: "tenantId", Type: "string"},
					},
					AdditionalProperties: &noExtra,
				},
				{
					Name:     "tags",
					Type:     "array",
					Optional: true,
					Items:    &FunctionArgsField{Type: "string"},
				},
			},
		},
	}

	valid := map[string]any{
		"spaceId":        "space-1",
		"questionNumber": 4,
		"status":         "active",
		"scheduledAt":    "2026-01-01T12:00:00Z",
		"payload": map[string]any{
			"tenantId": "t-1",
		},
		"tags": []any{"a", "b"},
	}
	if err := v.validateFunctionArgs(fn, valid); err != nil {
		t.Fatalf("expected valid args, got %v", err)
	}

	invalidCases := []map[string]any{
		{"spaceId": "space-1", "questionNumber": 2, "unknown": true},                                      // top-level extra
		{"spaceId": "space-1", "questionNumber": 9},                                                       // max
		{"spaceId": "space-1", "questionNumber": 2, "status": "paused"},                                   // enum
		{"spaceId": "space-1", "questionNumber": 2, "scheduledAt": "not-a-date"},                          // format
		{"spaceId": "space-1", "questionNumber": 2, "payload": map[string]any{"tenantId": "t", "x": "y"}}, // nested extra
		{"spaceId": "space-1", "questionNumber": 2, "tags": []any{"ok", 123}},                             // array item type
	}
	for idx, tc := range invalidCases {
		if err := v.validateFunctionArgs(fn, tc); err == nil {
			t.Fatalf("expected validation error for case %d", idx)
		}
	}
}

// TestResolvePlanFunctions_PreservesNamedShapeReference covers the case
// where a query function body contains shape(filter, "namedShape"). Function
// expansion must preserve ShapeExpression.TemplateName all the way through
// clone + arg substitution, otherwise the named shape lookup fails silently
// at execution time and the response payload ships as an empty `data` array
// while the bundle still populates.
//
// Regression for: frontend-reported empty `data` on every query*() that uses
// a named shape (e.g., querySpaceUtterances, queryActiveSpaces).
func TestResolvePlanFunctions_PreservesNamedShapeReference(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "querySpaceUtterances",
		FunctionKind: "query",
		Enabled:      true,
		Expr: &ShapeExpression{
			Target: &ComparisonExpression{
				Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
				Operator: OpEq,
				Value:    "v1:cognition:utterance",
			},
			TemplateName: "utteranceFull",
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "querySpaceUtterances",
			Args: map[string]any{},
		},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))

	shape, ok := plan.Root.(*ShapeExpression)
	require.True(t, ok, "expected ShapeExpression after function expansion, got %T", plan.Root)
	require.Equal(t, "utteranceFull", shape.TemplateName, "TemplateName must survive clone+expand")
	require.NotNil(t, shape.Target, "Target should be preserved through expansion")
}

// TestExpandExpressionWithArgs_ComparisonPreservesAuxFields covers the
// same-shape "reconstructed struct loses fields" trap that bit ShapeExpression.
// When an arg() substitutes into a ComparisonExpression, CacheHintSeconds and
// FieldSelections must ride along -- else any per-comparison cache hint or
// field projection declared in a function body is silently lost at expansion.
// No shipped .memql hits this today, which is exactly why a test belongs
// here rather than "we'd have noticed."
func TestExpandExpressionWithArgs_ComparisonPreservesAuxFields(t *testing.T) {
	hint := 42
	original := &ComparisonExpression{
		Field:            FieldReference{Raw: "payload.status", Parts: []string{"payload", "status"}},
		Operator:         OpEq,
		Value:            &ArgReference{Path: "status"},
		CacheHintSeconds: &hint,
		FieldSelections: []FieldReference{
			{Raw: "payload.title", Parts: []string{"payload", "title"}},
		},
	}

	v := newFunctionValidator(nil, nil)
	expanded, err := v.expandExpressionWithArgs(original, map[string]any{"status": "open"})
	require.NoError(t, err)

	cmp, ok := expanded.(*ComparisonExpression)
	require.True(t, ok)
	require.Equal(t, "open", cmp.Value, "Value should be the substituted arg")
	require.NotNil(t, cmp.CacheHintSeconds, "CacheHintSeconds must survive substitution")
	require.Equal(t, 42, *cmp.CacheHintSeconds)
	require.Len(t, cmp.FieldSelections, 1, "FieldSelections must survive substitution")
	require.Equal(t, "payload.title", cmp.FieldSelections[0].Raw)
}

// TestCloneExpressionNode_PreservesShapeTemplateName guards the clone path
// directly. expandFunctionCall clones fn.Expr before arg substitution, so
// cloneExpressionNode must round-trip TemplateName.
func TestCloneExpressionNode_PreservesShapeTemplateName(t *testing.T) {
	original := &ShapeExpression{
		Target: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:cognition:space",
		},
		TemplateName:  "spaceFull",
		IncludeBundle: true,
	}

	cloned, ok := cloneExpressionNode(original).(*ShapeExpression)
	require.True(t, ok)
	require.Equal(t, "spaceFull", cloned.TemplateName)
	require.True(t, cloned.IncludeBundle)
}
