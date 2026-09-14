package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// expr_ir_nodes_test.go -- DB-free pins for the edition-2026 IR nodes
// (memql#5366): the EXACT SQL each one compiles to, the in-process twin's
// answers, the canonical signatures, and the refusals.
//
// Exact fragments, not token probes, for the reason absent_field_comparison_
// test.go gives: a `Contains(sql, "COALESCE")` still passes when the operator
// inside it is inverted. The db-gated agreement lane
// (expr_ir_agreement_db_test.go) is what checks these fragments against a real
// server; this file is what makes a change to one a visible diff.

// irPayloadField builds the internal spelling of a payload property.
func irPayloadField(path string) FieldReference {
	parts := append([]string{"payload"}, strings.Split(path, ".")...)
	return FieldReference{Raw: strings.Join(parts, "."), Parts: parts}
}

// irElementField builds a field under the collection element in scope; an
// empty path is the element itself.
func irElementField(path string) FieldReference {
	parts := []string{arrayElementRoot}
	if path != "" {
		parts = append(parts, strings.Split(path, ".")...)
	}
	return FieldReference{Raw: strings.Join(parts, "."), Parts: parts}
}

func irPayloadCmp(path string, op ComparisonOperator, value any) *ComparisonExpression {
	return &ComparisonExpression{Field: irPayloadField(path), Operator: op, Value: value}
}

func irElementCmp(path string, op ComparisonOperator, value any) *ComparisonExpression {
	return &ComparisonExpression{Field: irElementField(path), Operator: op, Value: value}
}

func irCompile(t *testing.T, expr ExpressionNode) compiledExpression {
	t.Helper()
	compiled, ok := (&MemQLEngine{}).tryCompileCombinedFilter(context.Background(), expr, "")
	require.True(t, ok, "%s must compile to SQL", canonicalExpression(expr))
	return compiled
}

// irRow builds a scanned candidate. The id is derived from the payload
// because the evaluator's payload cache is keyed by (id, createdAt): two
// fixtures sharing an id would share one decoded payload, and every case
// after the first would be evaluated against the first case's row.
func irRow(t *testing.T, payloadJSON string) memorynodes.MemoryNode {
	t.Helper()
	require.True(t, json.Valid([]byte(payloadJSON)), "fixture payload must be valid JSON: %s", payloadJSON)
	return memorynodes.MemoryNode{ID: "v1:agents:agent:" + payloadJSON, Concept: "v1:agents:agent", Payload: json.RawMessage(payloadJSON)}
}

// ---- NotExpression ---------------------------------------------------------

func TestNotExpression_CompilesToTwoValuedNegation(t *testing.T) {
	cases := []struct {
		name string
		expr ExpressionNode
		sql  string
		args []any
	}{
		{
			name: "a payload comparison",
			expr: &NotExpression{Target: irPayloadCmp("deleted", OpEq, true)},
			sql:  `(NOT COALESCE(((CASE WHEN jsonb_typeof(payload->'deleted') = 'boolean' THEN (payload #>> '{deleted}')::boolean = ? ELSE FALSE END)), FALSE))`,
			args: []any{true},
		},
		{
			name: "a conjunction",
			expr: &NotExpression{Target: &LogicalExpression{
				Op:    LogicalAnd,
				Left:  irPayloadCmp("status", OpEq, "archived"),
				Right: irPayloadCmp("n", OpGt, int64(2)),
			}},
			sql:  `(NOT COALESCE((((jsonb_typeof(payload->'status') = 'string' AND payload #>> '{status}' = ?) AND (CASE WHEN jsonb_typeof(payload->'n') = 'number' THEN (payload #>> '{n}')::numeric > ? ELSE FALSE END))), FALSE))`,
			args: []any{"archived", float64(2)},
		},
		{
			name: "a double negation keeps both layers",
			expr: &NotExpression{Target: &NotExpression{Target: irPayloadCmp("s", OpEq, "a")}},
			sql:  `(NOT COALESCE(((NOT COALESCE(((jsonb_typeof(payload->'s') = 'string' AND payload #>> '{s}' = ?)), FALSE))), FALSE))`,
			args: []any{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled := irCompile(t, tc.expr)
			require.Equal(t, tc.sql, compiled.sql)
			require.Equal(t, tc.args, compiled.args)
		})
	}
}

// The absence table's `!e` row, in process: `!(x == v)` is exactly `x != v`
// for an absent x (both true), which is the case a bare SQL NOT gets wrong.
func TestNotExpression_InProcessIsExactNegation(t *testing.T) {
	absent := irRow(t, `{"other":1}`)
	present := irRow(t, `{"status":"archived"}`)
	cache := map[string]map[string]any{}

	eq := irPayloadCmp("status", OpEq, "archived")
	for _, tc := range []struct {
		row  memorynodes.MemoryNode
		want bool
	}{
		{absent, true},
		{present, false},
	} {
		got, err := nodeMatchesExpression(tc.row, &NotExpression{Target: eq}, cache)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
		ne, err := nodeMatchesExpression(tc.row, irPayloadCmp("status", OpNe, "archived"), cache)
		require.NoError(t, err)
		require.Equal(t, ne, got, "!(x == v) must be exactly x != v")
	}

	_, err := nodeMatchesExpression(present, &NotExpression{}, cache)
	require.Error(t, err, "a NOT with no operand is malformed, not true")
}

func TestNotExpression_OverATraversalRefusesByName(t *testing.T) {
	eng := &MemQLEngine{}
	traversal := &NotExpression{Target: &RelationshipExpression{
		Function: RelChildOf,
		Target:   irPayloadCmp("status", OpEq, "done"),
	}}
	_, ok := eng.tryCompileCombinedFilter(context.Background(), traversal, "")
	require.False(t, ok, "a NOT over a traversal must not compile")

	_, err := eng.evaluateExpressionSetWithContext(context.Background(), traversal, nil, 0, nil, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "`!` over a relationship traversal does not lower")

	// Nested under a conjunction the refusal is the same: the conjunction
	// splits, the comparison side compiles, and the NOT side is refused
	// rather than complemented within whatever the scan read.
	mixed := &LogicalExpression{Op: LogicalAnd, Left: irPayloadCmp("a", OpEq, "b"), Right: traversal}
	_, ok = eng.tryCompileCombinedFilter(context.Background(), mixed, "")
	require.False(t, ok)

	// A non-traversal operand that does not compile is named for what it is.
	_, err = eng.evaluateExpressionSetWithContext(context.Background(),
		&NotExpression{Target: &BuiltinFunctionExpression{Name: "concepts"}}, nil, 0, nil, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "`!` over the builtin \"concepts\" does not lower")
}

// ---- the == "" rule and byte-order strings --------------------------------

func TestStringOrderingCompilesByteOrder(t *testing.T) {
	for _, tc := range []struct {
		op  ComparisonOperator
		sql string
	}{
		{OpLt, `(jsonb_typeof(payload->'s') = 'string' AND (payload #>> '{s}') COLLATE "C" < ?)`},
		{OpLe, `(jsonb_typeof(payload->'s') = 'string' AND (payload #>> '{s}') COLLATE "C" <= ?)`},
		{OpGt, `(jsonb_typeof(payload->'s') = 'string' AND (payload #>> '{s}') COLLATE "C" > ?)`},
		{OpGe, `(jsonb_typeof(payload->'s') = 'string' AND (payload #>> '{s}') COLLATE "C" >= ?)`},
		// Equality is NOT collated: a deterministic collation's `=` is byte
		// equality already. `!=` is the two-valued negation of `==`.
		{OpEq, `(jsonb_typeof(payload->'s') = 'string' AND payload #>> '{s}' = ?)`},
		{OpNe, `(NOT COALESCE(((jsonb_typeof(payload->'s') = 'string' AND payload #>> '{s}' = ?)), FALSE))`},
	} {
		compiled, err := compilePayloadComparison([]string{"s"}, tc.op, "a")
		require.NoError(t, err)
		require.Equal(t, tc.sql, compiled.sql, "string %s", tc.op)
	}

	// Only STRING literals are collated. A number casts to numeric, where a
	// collation would be an error -- behind a CASE, so the cast only ever
	// sees a stored number.
	compiled, err := compilePayloadComparison([]string{"n"}, OpLt, int64(3))
	require.NoError(t, err)
	require.Equal(t, `(CASE WHEN jsonb_typeof(payload->'n') = 'number' THEN (payload #>> '{n}')::numeric < ? ELSE FALSE END)`, compiled.sql)
}

func TestEqualsEmptyStringCoalesces(t *testing.T) {
	compiled, err := compilePayloadComparison([]string{"revokedAt"}, OpEq, "")
	require.NoError(t, err)
	require.Equal(t, `(COALESCE(payload #>> '{revokedAt}', '') = '')`, compiled.sql)
	require.Empty(t, compiled.args)

	// One notion of unset: `== nil` is the same question, and the same SQL.
	missing, err := compilePayloadComparison([]string{"revokedAt"}, OpMissing, nil)
	require.NoError(t, err)
	require.Equal(t, compiled.sql, missing.sql)

	cache := map[string]map[string]any{}
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{}`, true},                 // key missing
		{`{"revokedAt":null}`, true}, // JSON null
		{`{"revokedAt":""}`, true},
		{`{"revokedAt":" "}`, false},
		{`{"revokedAt":"2026-01-01T00:00:00Z"}`, false},
	} {
		got, err := nodeMatchesComparison(irRow(t, tc.payload), irPayloadCmp("revokedAt", OpEq, ""), cache)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "revokedAt == \"\" over %s", tc.payload)
	}
}

// A payload that decodes to no object has no fields, so every field is
// absent -- the same answer the SQL gives (`#>>` into a JSON-null payload is
// NULL). The arm used to answer false for every operator but `== nil`.
func TestJSONNullPayloadReadsAsAbsent(t *testing.T) {
	cache := map[string]map[string]any{}
	row := irRow(t, `null`)
	for _, tc := range []struct {
		op    ComparisonOperator
		value any
		want  bool
	}{
		{OpNe, true, true},
		{OpNe, "", false},
		{OpEq, "", true},
		{OpEq, "a", false},
		{OpMissing, nil, true},
		{OpNotMissing, nil, false},
		{OpLt, "a", false},
	} {
		got, err := nodeMatchesComparison(row, irPayloadCmp("f", tc.op, tc.value), cache)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "f %s %v over a JSON-null payload", tc.op, tc.value)
	}
}

// Membership is a question about an ARRAY on both halves: jsonb containment
// is also true between two equal scalars, which the guard rules out.
func TestHasIsGuardedToAnArray(t *testing.T) {
	compiled, err := compilePayloadComparison([]string{"tags"}, OpHas, "a")
	require.NoError(t, err)
	require.Equal(t, `(jsonb_typeof(payload->'tags') = 'array' AND payload->'tags' @> to_jsonb(?::text))`, compiled.sql)

	compiled, err = compilePayloadComparison([]string{"nums"}, OpHas, int64(1))
	require.NoError(t, err)
	require.Equal(t, `(jsonb_typeof(payload->'nums') = 'array' AND payload->'nums' @> to_jsonb(?::numeric))`, compiled.sql)

	got, err := nodeMatchesComparison(irRow(t, `{"tags":"a"}`), irPayloadCmp("tags", OpHas, "a"), map[string]map[string]any{})
	require.NoError(t, err)
	require.False(t, got, "a scalar is not an array, so it contains nothing")

	// An UNSET needle is == to an unset element: a "" or a null one.
	compiled, err = compilePayloadComparison([]string{"tags"}, OpHas, nil)
	require.NoError(t, err)
	require.Equal(t, `(jsonb_typeof(payload->'tags') = 'array' AND (payload->'tags' @> '[""]'::jsonb OR payload->'tags' @> '[null]'::jsonb))`, compiled.sql)
	for payload, want := range map[string]bool{`{"tags":[null]}`: true, `{"tags":[""]}`: true, `{"tags":["a"]}`: false, `{"tags":[]}`: false} {
		got, err := nodeMatchesComparison(irRow(t, payload), irPayloadCmp("tags", OpHas, nil), map[string]map[string]any{})
		require.NoError(t, err)
		require.Equal(t, want, got, "nil in tags over %s", payload)
	}
}

// ---- ArrayPredicateExpression: SQL ------------------------------------------

func TestArrayPredicate_CompilesToJSONBArraySQL(t *testing.T) {
	cases := []struct {
		name string
		expr *ArrayPredicateExpression
		sql  string
		args []any
	}{
		{
			name: "any over the element itself",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Param: "t",
				Pred: irElementCmp("", OpEq, "a")},
			sql:  `(CASE WHEN jsonb_typeof(payload->'tags') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'tags') AS e(v) WHERE COALESCE(((jsonb_typeof(e.v) = 'string' AND e.v #>> '{}' = ?)), FALSE)) ELSE FALSE END)`,
			args: []any{"a"},
		},
		{
			name: "all over an element field, with the absent-is-vacuous ELSE",
			expr: &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAll, Param: "i",
				Pred: irElementCmp("qty", OpGt, int64(0))},
			sql:  `(CASE WHEN jsonb_typeof(payload->'items') = 'array' THEN NOT EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'items') AS e(v) WHERE NOT COALESCE(((CASE WHEN jsonb_typeof(e.v->'qty') = 'number' THEN (e.v #>> '{qty}')::numeric > ? ELSE FALSE END)), FALSE)) ELSE (payload #>> '{items}') IS NULL END)`,
			args: []any{float64(0)},
		},
		{
			name: "count, with the non-array guard inside the length",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpGt, CountValue: int64(2)},
			sql:  `(COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(payload->'tags') = 'array' THEN payload->'tags' END), 0) > ?)`,
			args: []any{float64(2)},
		},
		{
			name: "an element ordering is byte order too",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny,
				Pred: irElementCmp("", OpLt, "b")},
			sql:  `(CASE WHEN jsonb_typeof(payload->'tags') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'tags') AS e(v) WHERE COALESCE(((jsonb_typeof(e.v) = 'string' AND (e.v #>> '{}') COLLATE "C" < ?)), FALSE)) ELSE FALSE END)`,
			args: []any{"b"},
		},
		{
			name: "an element == \"\" gets the unset rule",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny,
				Pred: irElementCmp("", OpEq, "")},
			sql:  `(CASE WHEN jsonb_typeof(payload->'tags') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'tags') AS e(v) WHERE COALESCE(((COALESCE(e.v #>> '{}', '') = '')), FALSE)) ELSE FALSE END)`,
			args: nil,
		},
		{
			name: "a row comparison inside the element predicate correlates to the outer row",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny,
				Pred: &LogicalExpression{Op: LogicalAnd, Left: irElementCmp("", OpEq, "a"), Right: irPayloadCmp("active", OpEq, true)}},
			sql:  `(CASE WHEN jsonb_typeof(payload->'tags') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'tags') AS e(v) WHERE COALESCE((((jsonb_typeof(e.v) = 'string' AND e.v #>> '{}' = ?) AND (CASE WHEN jsonb_typeof(payload->'active') = 'boolean' THEN (payload #>> '{active}')::boolean = ? ELSE FALSE END))), FALSE)) ELSE FALSE END)`,
			args: []any{"a", true},
		},
		{
			name: "a nested predicate reads its array under the enclosing element and aliases its own",
			expr: &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAny,
				Pred: &ArrayPredicateExpression{Field: irElementField("tags"), Method: ArrayMethodAny,
					Pred: irElementCmp("", OpEq, "x")}},
			sql:  `(CASE WHEN jsonb_typeof(payload->'items') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'items') AS e(v) WHERE COALESCE(((CASE WHEN jsonb_typeof(e.v->'tags') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(e.v->'tags') AS e1(v) WHERE COALESCE(((jsonb_typeof(e1.v) = 'string' AND e1.v #>> '{}' = ?)), FALSE)) ELSE FALSE END)), FALSE)) ELSE FALSE END)`,
			args: []any{"x"},
		},
		{
			name: "a negated element predicate",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny,
				Pred: &NotExpression{Target: irElementCmp("", OpEq, "a")}},
			sql:  `(CASE WHEN jsonb_typeof(payload->'tags') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(payload->'tags') AS e(v) WHERE COALESCE(((NOT COALESCE(((jsonb_typeof(e.v) = 'string' AND e.v #>> '{}' = ?)), FALSE))), FALSE)) ELSE FALSE END)`,
			args: []any{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled := irCompile(t, tc.expr)
			require.Equal(t, tc.sql, compiled.sql)
			require.Equal(t, tc.args, compiled.args)
		})
	}
}

func TestArrayPredicate_RefusesShapesThatDoNotLower(t *testing.T) {
	eng := &MemQLEngine{}
	for _, tc := range []struct {
		name string
		expr ExpressionNode
		want string
	}{
		{
			name: "an array that is not a payload field",
			expr: &ArrayPredicateExpression{Field: FieldReference{Raw: "actor.groups", Parts: []string{"actor", "groups"}},
				Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "a")},
			want: "lowers over a payload array field",
		},
		{
			name: "an element outside any collection predicate",
			expr: &ArrayPredicateExpression{Field: irElementField("tags"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "a")},
			want: "names a collection element outside a collection predicate",
		},
		{
			name: "any with no element predicate",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny},
			want: "needs an element predicate",
		},
		{
			name: "an unknown method",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: "where", Pred: irElementCmp("", OpEq, "a")},
			want: "is not one of any, all, count",
		},
		{
			name: "a count compared with a string",
			expr: &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpEq, CountValue: "3"},
			want: "count() compares with a number",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := eng.tryCompileCombinedFilter(context.Background(), tc.expr, "")
			require.False(t, ok)
			_, err := eng.evaluateExpressionSetWithContext(context.Background(), tc.expr, nil, 0, nil, "")
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}

	// An element comparison at row level is refused by the comparison arm.
	_, ok := eng.tryCompileCombinedFilter(context.Background(), irElementCmp("", OpEq, "a"), "")
	require.False(t, ok)
	_, err := nodeMatchesExpression(irRow(t, `{}`), irElementCmp("", OpEq, "a"), map[string]map[string]any{})
	require.Error(t, err)
}

// ---- ArrayPredicateExpression: the in-process rules ------------------------

// The rules table from expr_collection_sql.go, row by row, in process. The
// db-gated lane holds the SQL to the same table.
func TestArrayPredicate_InProcessRules(t *testing.T) {
	anyA := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "a")}
	allA := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAll, Pred: irElementCmp("", OpEq, "a")}
	countZero := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpEq, CountValue: int64(0)}
	countTwo := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpGe, CountValue: int64(2)}
	anyNil := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Pred: irElementCmp("", OpMissing, nil)}
	anyBlank := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "")}
	allSet := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAll, Pred: irElementCmp("", OpNe, "")}

	type want struct{ anyA, allA, countZero, countTwo, anyNil, anyBlank, allSet bool }
	cases := []struct {
		name    string
		payload string
		want    want
	}{
		{"absent", `{}`, want{false, true, true, false, false, false, true}},
		{"JSON null", `{"tags":null}`, want{false, true, true, false, false, false, true}},
		{"a present scalar", `{"tags":"a"}`, want{false, false, true, false, false, false, false}},
		{"empty", `{"tags":[]}`, want{false, true, true, false, false, false, true}},
		{"just a", `{"tags":["a"]}`, want{true, true, false, false, false, false, true}},
		{"a and b", `{"tags":["a","b"]}`, want{true, false, false, true, false, false, true}},
		{"a null element", `{"tags":[null]}`, want{false, false, false, false, true, true, false}},
		// One notion of unset: a "" element IS == nil.
		{"a blank element", `{"tags":[""]}`, want{false, false, false, false, true, true, false}},
		// Typed: a number element is not the string "a".
		{"a mixed array", `{"tags":[1,"b"]}`, want{false, false, false, true, false, false, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := irRow(t, tc.payload)
			cache := map[string]map[string]any{}
			check := func(label string, expr ExpressionNode, want bool) {
				t.Helper()
				got, err := nodeMatchesExpression(row, expr, cache)
				require.NoError(t, err, label)
				require.Equal(t, want, got, "%s over %s", label, tc.payload)
				negated, err := nodeMatchesExpression(row, &NotExpression{Target: expr}, cache)
				require.NoError(t, err, label)
				require.Equal(t, !want, negated, "!%s over %s", label, tc.payload)
			}
			check("any(t => t == \"a\")", anyA, tc.want.anyA)
			check("all(t => t == \"a\")", allA, tc.want.allA)
			check("count() == 0", countZero, tc.want.countZero)
			check("count() >= 2", countTwo, tc.want.countTwo)
			check("any(t => t == nil)", anyNil, tc.want.anyNil)
			check("any(t => t == \"\")", anyBlank, tc.want.anyBlank)
			check("all(t => t != \"\")", allSet, tc.want.allSet)
		})
	}
}

func TestArrayPredicate_InProcessElementFieldsAndNesting(t *testing.T) {
	cache := map[string]map[string]any{}
	qtyPositive := &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAny, Pred: irElementCmp("qty", OpGt, int64(0))}
	allPositive := &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAll, Pred: irElementCmp("qty", OpGt, int64(0))}
	nested := &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAny,
		Pred: &ArrayPredicateExpression{Field: irElementField("tags"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "x")}}
	rowTerm := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny,
		Pred: &LogicalExpression{Op: LogicalAnd, Left: irElementCmp("", OpEq, "a"), Right: irPayloadCmp("active", OpEq, true)}}

	for _, tc := range []struct {
		payload string
		expr    ExpressionNode
		want    bool
	}{
		{`{"items":[{"qty":1},{"qty":0}]}`, qtyPositive, true},
		{`{"items":[{"qty":1},{"qty":0}]}`, allPositive, false},
		{`{"items":[{"qty":2}]}`, allPositive, true},
		{`{"items":[{"k":"x"}]}`, qtyPositive, false}, // element field absent
		{`{"items":[{"k":"x"}]}`, allPositive, false}, // ... and an absent element field fails all()
		{`{"items":[{"qty":null}]}`, allPositive, false},
		{`{"items":["scalar"]}`, qtyPositive, false}, // a path through a scalar is absent
		{`{"items":[{"tags":["x"]},{"tags":[]}]}`, nested, true},
		{`{"items":[{"tags":["y"]}]}`, nested, false},
		{`{"items":[{"tags":"x"}]}`, nested, false}, // the inner array is a scalar
		{`{"tags":["a"],"active":true}`, rowTerm, true},
		{`{"tags":["a"],"active":false}`, rowTerm, false},
	} {
		got, err := nodeMatchesExpression(irRow(t, tc.payload), tc.expr, cache)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "%s over %s", canonicalExpression(tc.expr), tc.payload)
	}
}

// ---- PlanConstExpression: never executed unevaluated -----------------------

func TestPlanConstExpression_UnevaluatedIsRefusedEverywhere(t *testing.T) {
	pc := &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}
	eng := &MemQLEngine{}
	row := irRow(t, `{"expiresAt":"2026-01-01T00:00:00Z"}`)

	_, ok := eng.tryCompileCombinedFilter(context.Background(), pc, "")
	require.False(t, ok, "a plan constant has no value to bind")

	_, err := nodeMatchesExpression(row, pc, map[string]map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reached the executor unevaluated")

	_, err = eng.evaluateExpressionSetWithContext(context.Background(), pc, nil, 0, nil, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "reached the executor unevaluated")

	// In a comparison's value position the compiler names it too, rather
	// than reporting "unsupported literal type *memql.PlanConstExpression".
	_, err = compilePayloadComparison([]string{"expiresAt"}, OpLt, pc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "plan constant (now) reached the executor unevaluated")
}

// ---- canonical signatures ---------------------------------------------------

func TestCanonicalExpression_EditionTwentySixNodes(t *testing.T) {
	eq := irPayloadCmp("deleted", OpEq, true)
	require.Equal(t, `!(payload.deleted==true)`, canonicalExpression(&NotExpression{Target: eq}))
	require.NotEqual(t, canonicalExpression(eq), canonicalExpression(&NotExpression{Target: eq}),
		"a negation must not share its operand's result-cache signature")

	anyT := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Param: "t", Pred: irElementCmp("", OpEq, "a")}
	anyX := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Param: "x", Pred: irElementCmp("", OpEq, "a")}
	require.Equal(t, `payload.tags.any($elem=="a")`, canonicalExpression(anyT))
	require.Equal(t, canonicalExpression(anyT), canonicalExpression(anyX), "the lambda's parameter name is not part of the identity")

	allT := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAll, Pred: irElementCmp("", OpEq, "a")}
	require.Equal(t, `payload.tags.all($elem=="a")`, canonicalExpression(allT))

	count := &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpGt, CountValue: int64(2)}
	require.Equal(t, `payload.tags.count()>2`, canonicalExpression(count))

	pc := &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}
	require.Equal(t, `planConst(now)`, canonicalExpression(pc))
	require.Equal(t, `payload.expiresat<planConst(now)`, canonicalExpression(irPayloadCmp("expiresAt", OpLt, pc)),
		"a plan constant in value position renders by source, never by pointer")
}
