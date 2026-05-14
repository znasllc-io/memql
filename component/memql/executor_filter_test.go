package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompilePayloadComparisonInOperatorString(t *testing.T) {
	// Test in with string values (should generate array-aware SQL)
	result, err := compilePayloadComparison(
		[]string{"topics"},
		OpIn,
		[]any{"filters", "shape"},
	)
	require.NoError(t, err)

	// Should contain both array check (jsonb_typeof + jsonb_exists_any) and scalar check (IN)
	require.Contains(t, result.sql, "jsonb_typeof")
	require.Contains(t, result.sql, "jsonb_exists_any")
	require.Contains(t, result.sql, "?::text[]")
	require.Contains(t, result.sql, "IN")
	// Args: text[] array + bun.In for scalar IN clause
	require.Len(t, result.args, 2)
}

func TestCompilePayloadComparisonOutOperatorString(t *testing.T) {
	// Test not in with string values (should generate array-aware SQL)
	result, err := compilePayloadComparison(
		[]string{"tags"},
		OpOut,
		[]any{"admin", "system"},
	)
	require.NoError(t, err)

	// Should contain both array check with NOT and scalar NOT IN
	require.Contains(t, result.sql, "jsonb_typeof")
	require.Contains(t, result.sql, "NOT jsonb_exists_any")
	require.Contains(t, result.sql, "?::text[]")
	require.Contains(t, result.sql, "NOT IN")
	// Args: text[] array + bun.In for scalar NOT IN clause
	require.Len(t, result.args, 2)
}

func TestCompilePayloadComparisonInOperatorNumbers(t *testing.T) {
	// Test in with numeric values (should use simple IN, not array-aware)
	result, err := compilePayloadComparison(
		[]string{"score"},
		OpIn,
		[]any{int64(1), int64(2), int64(3)},
	)
	require.NoError(t, err)

	// Numbers don't support array matching, just scalar IN
	require.Contains(t, result.sql, "::numeric")
	require.Contains(t, result.sql, "IN")
	require.NotContains(t, result.sql, "jsonb_typeof") // No array check for numbers
	require.Len(t, result.args, 1)
}

func TestCompilePayloadComparisonNestedPath(t *testing.T) {
	// Test in with nested path like payload.profile.tags
	result, err := compilePayloadComparison(
		[]string{"profile", "tags"},
		OpIn,
		[]any{"vip", "beta"},
	)
	require.NoError(t, err)

	// Should build correct nested path expressions
	require.Contains(t, result.sql, "payload->'profile'->'tags'")   // JSONB path
	require.Contains(t, result.sql, "payload #>> '{profile,tags}'") // Text path
}

func TestCompilePayloadComparisonDoesNotInlineValues(t *testing.T) {
	// Ensures array values are parameterized instead of embedded directly in SQL.
	result, err := compilePayloadComparison(
		[]string{"name"},
		OpIn,
		[]any{"O'Brien", "test'value"},
	)
	require.NoError(t, err)

	require.NotContains(t, result.sql, "O''Brien")
	require.Contains(t, result.sql, "jsonb_exists_any")
}

func TestBuildJSONBPathExpression(t *testing.T) {
	tests := []struct {
		name     string
		path     []string
		expected string
		wantErr  bool
	}{
		{
			name:     "single segment",
			path:     []string{"topics"},
			expected: "payload->'topics'",
		},
		{
			name:     "nested path",
			path:     []string{"profile", "tags"},
			expected: "payload->'profile'->'tags'",
		},
		{
			name:     "deeply nested",
			path:     []string{"a", "b", "c", "d"},
			expected: "payload->'a'->'b'->'c'->'d'",
		},
		{
			name:    "empty path",
			path:    []string{},
			wantErr: true,
		},
		{
			name:    "invalid segment",
			path:    []string{"valid", "inva;lid"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := buildJSONBPathExpression(tc.path)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, result)
		})
	}
}

// Tests for ID comparison and resolution

func TestResolveFullId(t *testing.T) {
	tests := []struct {
		name           string
		id             string
		conceptContext string
		expected       string
		wantErr        bool
		errContains    string
	}{
		{
			name:           "full ID with concept prefix returns as-is",
			id:             "v1:examples:world:world-aurora",
			conceptContext: "",
			expected:       "v1:examples:world:world-aurora",
		},
		{
			name:           "full ID ignores concept context",
			id:             "v1:examples:world:world-aurora",
			conceptContext: "v1:other:concept",
			expected:       "v1:examples:world:world-aurora",
		},
		{
			name:           "short ID with concept context resolves correctly",
			id:             "world-aurora",
			conceptContext: "v1:examples:world",
			expected:       "v1:examples:world:world-aurora",
		},
		{
			name:           "short ID without concept context returns error",
			id:             "world-aurora",
			conceptContext: "",
			wantErr:        true,
			errContains:    "short ID",
		},
		{
			name:           "empty ID returns error",
			id:             "",
			conceptContext: "v1:examples:world",
			wantErr:        true,
			errContains:    "cannot be empty",
		},
		{
			name:           "ID with single colon treated as full ID",
			id:             "namespace:id",
			conceptContext: "",
			expected:       "namespace:id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := resolveFullId(tc.id, tc.conceptContext)
			if tc.wantErr {
				require.Error(t, err)
				if tc.errContains != "" {
					require.Contains(t, err.Error(), tc.errContains)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestCompileIdComparison(t *testing.T) {
	tests := []struct {
		name           string
		op             ComparisonOperator
		value          any
		conceptContext string
		wantErr        bool
		errContains    string
		sqlContains    string
	}{
		{
			name:           "equality with full ID",
			op:             OpEq,
			value:          "v1:concept:my-id",
			conceptContext: "",
			sqlContains:    "id = ?",
		},
		{
			name:           "equality with short ID and concept",
			op:             OpEq,
			value:          "my-id",
			conceptContext: "v1:concept",
			sqlContains:    "id = ?",
		},
		{
			name:           "equality with short ID without concept errors",
			op:             OpEq,
			value:          "my-id",
			conceptContext: "",
			wantErr:        true,
			errContains:    "short ID",
		},
		{
			name:           "not-equal with full ID",
			op:             OpNe,
			value:          "v1:concept:my-id",
			conceptContext: "",
			sqlContains:    "id <> ?",
		},
		{
			name:           "in operator with full IDs",
			op:             OpIn,
			value:          []any{"v1:concept:id1", "v1:concept:id2"},
			conceptContext: "",
			sqlContains:    "id IN",
		},
		{
			name:           "in operator with short IDs and concept",
			op:             OpIn,
			value:          []any{"id1", "id2"},
			conceptContext: "v1:concept",
			sqlContains:    "id IN",
		},
		{
			name:           "out operator with full IDs",
			op:             OpOut,
			value:          []any{"v1:concept:id1"},
			conceptContext: "",
			sqlContains:    "id NOT IN",
		},
		{
			name:           "unsupported operator",
			op:             OpGt,
			value:          "v1:concept:id",
			conceptContext: "",
			wantErr:        true,
			errContains:    "not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileIdComparison(tc.op, tc.value, tc.conceptContext)
			if tc.wantErr {
				require.Error(t, err)
				if tc.errContains != "" {
					require.Contains(t, err.Error(), tc.errContains)
				}
				return
			}
			require.NoError(t, err)
			if tc.sqlContains != "" {
				require.Contains(t, result.sql, tc.sqlContains)
			}
		})
	}
}

func TestExtractConceptFromExpression(t *testing.T) {
	tests := []struct {
		name     string
		expr     ExpressionNode
		expected string
	}{
		{
			name:     "nil expression",
			expr:     nil,
			expected: "",
		},
		{
			name: "concept equality expression",
			expr: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"concept"}},
				Operator: OpEq,
				Value:    "v1:examples:world",
			},
			expected: "v1:examples:world",
		},
		{
			name: "concept not-equal expression (should not extract)",
			expr: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"concept"}},
				Operator: OpNe,
				Value:    "v1:examples:world",
			},
			expected: "",
		},
		{
			name: "non-concept comparison",
			expr: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "name"}},
				Operator: OpEq,
				Value:    "test",
			},
			expected: "",
		},
		{
			name: "AND expression with concept on left",
			expr: &LogicalExpression{
				Op: LogicalAnd,
				Left: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"concept"}},
					Operator: OpEq,
					Value:    "v1:examples:world",
				},
				Right: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"id"}},
					Operator: OpEq,
					Value:    "world-aurora",
				},
			},
			expected: "v1:examples:world",
		},
		{
			name: "AND expression with concept on right",
			expr: &LogicalExpression{
				Op: LogicalAnd,
				Left: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"id"}},
					Operator: OpEq,
					Value:    "world-aurora",
				},
				Right: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"concept"}},
					Operator: OpEq,
					Value:    "v1:examples:world",
				},
			},
			expected: "v1:examples:world",
		},
		{
			name: "OR expression (should not extract - ambiguous)",
			expr: &LogicalExpression{
				Op: LogicalOr,
				Left: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"concept"}},
					Operator: OpEq,
					Value:    "v1:concept:a",
				},
				Right: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"concept"}},
					Operator: OpEq,
					Value:    "v1:concept:b",
				},
			},
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractConceptFromExpression(tc.expr)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestResolveComparisonForExecution(t *testing.T) {
	tests := []struct {
		name           string
		cmp            *ComparisonExpression
		conceptContext string
		expectedValue  any
		wantErr        bool
		errContains    string
	}{
		{
			name:           "nil comparison returns nil",
			cmp:            nil,
			conceptContext: "",
			expectedValue:  nil,
		},
		{
			name: "non-ID field returns unchanged",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"concept"}},
				Operator: OpEq,
				Value:    "v1:examples:world",
			},
			conceptContext: "v1:other:concept",
			expectedValue:  "v1:examples:world",
		},
		{
			name: "payload field returns unchanged",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "name"}},
				Operator: OpEq,
				Value:    "test",
			},
			conceptContext: "v1:examples:world",
			expectedValue:  "test",
		},
		{
			name: "ID with full ID returns unchanged",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpEq,
				Value:    "v1:examples:world:world-aurora",
			},
			conceptContext: "",
			expectedValue:  "v1:examples:world:world-aurora",
		},
		{
			name: "ID with short ID and concept context resolves",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpEq,
				Value:    "world-aurora",
			},
			conceptContext: "v1:examples:world",
			expectedValue:  "v1:examples:world:world-aurora",
		},
		{
			name: "ID with short ID without concept context errors",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpEq,
				Value:    "world-aurora",
			},
			conceptContext: "",
			wantErr:        true,
			errContains:    "short ID",
		},
		{
			name: "ID not-equal with short ID resolves",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpNe,
				Value:    "world-aurora",
			},
			conceptContext: "v1:examples:world",
			expectedValue:  "v1:examples:world:world-aurora",
		},
		{
			name: "ID in operator with short IDs resolves all",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpIn,
				Value:    []any{"world-aurora", "world-nebula"},
			},
			conceptContext: "v1:examples:world",
			expectedValue:  []any{"v1:examples:world:world-aurora", "v1:examples:world:world-nebula"},
		},
		{
			name: "ID out operator with mixed IDs resolves short ones",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpOut,
				Value:    []any{"v1:examples:world:world-aurora", "world-nebula"},
			},
			conceptContext: "v1:examples:world",
			expectedValue:  []any{"v1:examples:world:world-aurora", "v1:examples:world:world-nebula"},
		},
		{
			name: "ID missing operator returns unchanged",
			cmp: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"id"}},
				Operator: OpMissing,
				Value:    nil,
			},
			conceptContext: "v1:examples:world",
			expectedValue:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := resolveComparisonForExecution(tc.cmp, tc.conceptContext)
			if tc.wantErr {
				require.Error(t, err)
				if tc.errContains != "" {
					require.Contains(t, err.Error(), tc.errContains)
				}
				return
			}
			require.NoError(t, err)
			if tc.cmp == nil {
				require.Nil(t, result)
				return
			}
			require.NotNil(t, result)
			require.Equal(t, tc.expectedValue, result.Value)
		})
	}
}
