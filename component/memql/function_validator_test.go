package memql

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
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
				{Name: "partitionId", Type: "string"},
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
		"partitionId":    "space-1",
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
		{"partitionId": "space-1", "questionNumber": 2, "unknown": true},                                      // top-level extra
		{"partitionId": "space-1", "questionNumber": 9},                                                       // max
		{"partitionId": "space-1", "questionNumber": 2, "status": "paused"},                                   // enum
		{"partitionId": "space-1", "questionNumber": 2, "scheduledAt": "not-a-date"},                          // format
		{"partitionId": "space-1", "questionNumber": 2, "payload": map[string]any{"tenantId": "t", "x": "y"}}, // nested extra
		{"partitionId": "space-1", "questionNumber": 2, "tags": []any{"ok", 123}},                             // array item type
	}
	for idx, tc := range invalidCases {
		if err := v.validateFunctionArgs(fn, tc); err == nil {
			t.Fatalf("expected validation error for case %d", idx)
		}
	}
}

// TestValidateFunctionArgs_MaxLength pins the @maxLength contract --
// strings longer than the cap reject; equal-or-under accept; non-
// string types are ignored (the cap only makes sense for strings).
// Drives memql-bff-copresent#27 free-text caps.
func TestValidateFunctionArgs_MaxLength(t *testing.T) {
	v := &functionValidator{}
	fn := &Function{
		Name: "createSomething",
		ArgsSchema: &ArgsSchemaConfig{
			Fields: []*FunctionArgsField{
				{Name: "subject", Type: "string", MaxLength: 10},
			},
		},
	}

	// At and under the cap -- accepted.
	for _, ok := range []string{"", "abc", "1234567890" /* exactly 10 */} {
		if err := v.validateFunctionArgs(fn, map[string]any{"subject": ok}); err != nil {
			t.Errorf("len %d: expected accept, got %v", len(ok), err)
		}
	}

	// One over -- rejected.
	if err := v.validateFunctionArgs(fn, map[string]any{"subject": "12345678901"}); err == nil {
		t.Errorf("len 11: expected reject, got nil")
	} else if !strings.Contains(err.Error(), "too long") {
		t.Errorf("expected 'too long' in error, got %v", err)
	}

	// Multi-byte UTF-8 -- caps are RUNE counts, not bytes. "あいうえお" is
	// 5 runes (15 bytes); cap is 10 -- accepted.
	if err := v.validateFunctionArgs(fn, map[string]any{"subject": "あいうえお"}); err != nil {
		t.Errorf("5-rune multi-byte string: expected accept, got %v", err)
	}

	// 11 runes -- rejected, regardless of byte count.
	if err := v.validateFunctionArgs(fn, map[string]any{"subject": "あいうえおあいうえおあ"}); err == nil {
		t.Errorf("11-rune multi-byte string: expected reject, got nil")
	}
}

// TestValidateFunctionArgs_Pattern pins the @pattern contract --
// strings matching the compiled regex accept; non-matching reject;
// nil patternRegex is a no-op. Drives memql-bff-copresent#28 ID
// format enforcement.
func TestValidateFunctionArgs_Pattern(t *testing.T) {
	v := &functionValidator{}
	conceptIdRE := regexp.MustCompile(`^v1:[a-z0-9]+:[a-z0-9_]+:[a-zA-Z0-9_-]+$`)
	fn := &Function{
		Name: "lookupConcept",
		ArgsSchema: &ArgsSchemaConfig{
			Fields: []*FunctionArgsField{
				{Name: "id", Type: "string", Pattern: conceptIdRE.String(), patternRegex: conceptIdRE},
			},
		},
	}

	wellFormed := []string{
		"v1:cognition:space:abc",
		"v1:agents:agent:40920a36-911a-4fb2-b69a-fadfc3919915",
		"v1:planner:plan:plan_2026_05_21",
	}
	for _, ok := range wellFormed {
		if err := v.validateFunctionArgs(fn, map[string]any{"id": ok}); err != nil {
			t.Errorf("well-formed id %q: expected accept, got %v", ok, err)
		}
	}

	pathTraversal := []string{
		"../../etc/passwd",
		"v1:cognition:space:../escape",
		"' OR 1=1 --",
		"v1:cognition:space:",
		"",
	}
	for _, bad := range pathTraversal {
		if err := v.validateFunctionArgs(fn, map[string]any{"id": bad}); err == nil {
			t.Errorf("malformed id %q: expected reject, got nil", bad)
		} else if !strings.Contains(err.Error(), "does not match pattern") {
			t.Errorf("expected 'does not match pattern' in error, got %v", err)
		}
	}
}

// TestConvertArgsField_InvalidPatternFailsLoad asserts the loader-
// side compile-on-load contract: an invalid regex in the DSL fails
// the function-loader pass, so the engine never sees a broken
// pattern at validation time.
func TestConvertArgsField_InvalidPatternFailsLoad(t *testing.T) {
	bad := &languageParser.ArgsField{Name: "id", Type: "string", Pattern: "[invalid"}
	if _, err := convertArgsField(bad); err == nil {
		t.Fatalf("expected error for invalid regex, got nil")
	}

	// Valid regex compiles + populates the cached patternRegex on the
	// returned field.
	good := &languageParser.ArgsField{Name: "id", Type: "string", Pattern: `^v1:`}
	got, err := convertArgsField(good)
	require.NoError(t, err)
	if got.patternRegex == nil {
		t.Errorf("convertArgsField did not populate patternRegex for valid pattern")
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
// a named shape (e.g., spaceUtterances, queryActiveSpaces).
func TestResolvePlanFunctions_PreservesNamedShapeReference(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "spaceUtterances",
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
			Name: "spaceUtterances",
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
