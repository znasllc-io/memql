package memql

import (
	"encoding/json"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
		{
			// resolvePlanFunctions expands FunctionCallExpression AFTER
			// planQuery has stripped directives, so a function whose
			// fn.Expr is shape(...) lands as plan.Root with the wrapper
			// still on top. The extractor needs to descend.
			name: "shape wrapper carries concept on its target",
			expr: &ShapeExpression{
				TemplateName: "magicLinkRequestFull",
				Target: &LogicalExpression{
					Op: LogicalAnd,
					Left: &ComparisonExpression{
						Field:    FieldReference{Parts: []string{"concept"}},
						Operator: OpEq,
						Value:    "v1:identity:magicLinkRequest",
					},
					Right: &ComparisonExpression{
						Field:    FieldReference{Parts: []string{"payload", "tokenHash"}},
						Operator: OpEq,
						Value:    "deadbeef",
					},
				},
			},
			expected: "v1:identity:magicLinkRequest",
		},
		{
			name: "sort wrapper carries concept on its target",
			expr: &SortExpression{
				Target: &ComparisonExpression{
					Field:    FieldReference{Parts: []string{"concept"}},
					Operator: OpEq,
					Value:    "v1:identity:user",
				},
			},
			expected: "v1:identity:user",
		},
		{
			name: "paginate -> sort -> filter chain still extracts",
			expr: &PaginateExpression{
				Target: &SortExpression{
					Target: &ComparisonExpression{
						Field:    FieldReference{Parts: []string{"concept"}},
						Operator: OpEq,
						Value:    "v1:identity:authCode",
					},
				},
			},
			expected: "v1:identity:authCode",
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

// TestNodeMatchesComparison_ProvenanceName is the #1670 regression test.
// The per-user seed dedup query contains a `provenance.name=="<seedName>"`
// term. Before the fix, nodeMatchesComparison recognised `provenance` as
// an intrinsic field (kind=intrinsicFieldProvenance) but had no case for it
// in the switch -- falling through to `default` and returning
// `field "provenance.name" is not supported in queries`. The combined-filter
// path compiled the SQL successfully (provenance->>'name' = ?) and the DB
// scan found the right row, but executeCombinedFilterQuery re-evaluated the
// full expression tree in-process on every candidate, hitting the same
// missing case and propagating the error, so lookupOrMintPerUserId always
// fell back to the deterministic id and logged 42× WARN per boot.
func TestNodeMatchesComparison_ProvenanceName(t *testing.T) {
	makeNode := func(provJSON string) memorynodes.MemoryNode {
		return memorynodes.MemoryNode{
			ID:         "v1:agents:agent:test-id",
			Concept:    "v1:agents:agent",
			Provenance: json.RawMessage(provJSON),
		}
	}

	cases := []struct {
		name      string
		nodeJSON  string
		leaf      string
		op        ComparisonOperator
		value     string
		wantMatch bool
		wantErr   bool
	}{
		{
			name:      "provenance.name matches seed name (the #1670 dedup case)",
			nodeJSON:  `{"kind":"seed","name":"trainerAgent"}`,
			leaf:      "name",
			op:        OpEq,
			value:     "trainerAgent",
			wantMatch: true,
		},
		{
			name:      "provenance.name does not match different seed",
			nodeJSON:  `{"kind":"seed","name":"assistant"}`,
			leaf:      "name",
			op:        OpEq,
			value:     "trainerAgent",
			wantMatch: false,
		},
		{
			name:      "provenance.name != match (ne operator, different name)",
			nodeJSON:  `{"kind":"seed","name":"assistant"}`,
			leaf:      "name",
			op:        OpNe,
			value:     "trainerAgent",
			wantMatch: true,
		},
		{
			name:      "provenance.kind matches",
			nodeJSON:  `{"kind":"seed","name":"plannerAgent"}`,
			leaf:      "kind",
			op:        OpEq,
			value:     "seed",
			wantMatch: true,
		},
		{
			name:      "provenance.kind does not match",
			nodeJSON:  `{"kind":"mutation","name":"mutationCreateAgent"}`,
			leaf:      "kind",
			op:        OpEq,
			value:     "seed",
			wantMatch: false,
		},
		{
			name:      "provenance.trigger matches",
			nodeJSON:  `{"kind":"automation","name":"autoProvision","trigger":"graph.node.created.v1:identity:user"}`,
			leaf:      "trigger",
			op:        OpEq,
			value:     "graph.node.created.v1:identity:user",
			wantMatch: true,
		},
		{
			name:      "provenance.via matches",
			nodeJSON:  `{"kind":"seed","name":"assistant","via":"mutationCreateAgent"}`,
			leaf:      "via",
			op:        OpEq,
			value:     "mutationCreateAgent",
			wantMatch: true,
		},
		{
			name:      "empty provenance JSON returns no-match (not error)",
			nodeJSON:  ``,
			leaf:      "name",
			op:        OpEq,
			value:     "trainerAgent",
			wantMatch: false,
		},
		{
			name:      "provenance leaf absent returns no-match",
			nodeJSON:  `{"kind":"seed","name":"assistant"}`,
			leaf:      "trigger",
			op:        OpEq,
			value:     "something",
			wantMatch: false,
		},
		{
			name:    "unsupported leaf returns error",
			nodeJSON: `{"kind":"seed","name":"assistant"}`,
			leaf:    "unknown",
			op:      OpEq,
			value:   "x",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := makeNode(tc.nodeJSON)
			cmp := &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"provenance", tc.leaf}, Raw: "provenance." + tc.leaf},
				Operator: tc.op,
				Value:    tc.value,
			}
			match, err := nodeMatchesComparison(node, cmp, map[string]map[string]any{})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantMatch, match)
		})
	}
}

// TestProvenanceLeafFromJSON pins the helper that extracts a single leaf
// from a raw provenance JSON blob.
func TestProvenanceLeafFromJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		leaf string
		want string
	}{
		{"present leaf", `{"kind":"seed","name":"assistant"}`, "name", "assistant"},
		{"present kind", `{"kind":"mutation","name":"mutationCreateAgent"}`, "kind", "mutation"},
		{"present via", `{"kind":"seed","name":"assistant","via":"mutationCreateAgent"}`, "via", "mutationCreateAgent"},
		{"absent leaf returns empty", `{"kind":"seed","name":"assistant"}`, "trigger", ""},
		{"empty JSON returns empty", ``, "name", ""},
		{"malformed JSON returns empty", `{bad}`, "name", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := provenanceLeafFromJSON(json.RawMessage(tc.raw), tc.leaf)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestCompileProvenanceComparison_SQLPath pins the SQL compile side of
// provenance filtering so both the SQL path and the post-filter path (above)
// stay in sync.
func TestCompileProvenanceComparison_SQLPath(t *testing.T) {
	cases := []struct {
		name    string
		leaf    string
		op      ComparisonOperator
		value   string
		wantSQL string
		wantErr bool
	}{
		{
			name:    "name eq",
			leaf:    "name",
			op:      OpEq,
			value:   "trainerAgent",
			wantSQL: "provenance->>'name' =",
		},
		{
			name:    "kind ne",
			leaf:    "kind",
			op:      OpNe,
			value:   "seed",
			wantSQL: "provenance->>'kind' <>",
		},
		{
			name:    "unsupported leaf errors",
			leaf:    "badleaf",
			op:      OpEq,
			value:   "x",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compileProvenanceComparison(tc.leaf, tc.op, tc.value)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Contains(t, got.sql, tc.wantSQL)
			require.Len(t, got.args, 1)
			require.Equal(t, tc.value, got.args[0])
		})
	}
}
