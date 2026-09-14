package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// expr_field_operand_test.go -- the two comparison forms Lower emits that the
// executor did not have before memql#5366: a comparison between two fields of
// one row (FieldOperand), and the substring test (OpIncludes). The SQL text is
// pinned here; that it agrees with the in-process twin on real rows is the
// agreement matrix's job (expr_lower_agreement_db_test.go).

func TestFieldOperand_CompilesTypedAndNullSafe(t *testing.T) {
	t.Run("equality is both-unset or jsonb equal, two-valued", func(t *testing.T) {
		got := irCompile(t, &ComparisonExpression{Field: irPayloadField("overage"), Operator: OpEq, Value: &FieldOperand{Field: irPayloadField("reported")}})
		require.Equal(t,
			"((COALESCE(payload #>> '{overage}', '') = '' AND COALESCE(payload #>> '{reported}', '') = '') OR COALESCE(payload->'overage' = payload->'reported', FALSE))",
			got.sql)
		require.Empty(t, got.args, "a field comparison binds nothing: both sides are the row's")
	})
	t.Run("inequality is its two-valued negation", func(t *testing.T) {
		got := irCompile(t, &ComparisonExpression{Field: irPayloadField("overage"), Operator: OpNe, Value: &FieldOperand{Field: irPayloadField("reported")}})
		require.Equal(t,
			"(NOT COALESCE((((COALESCE(payload #>> '{overage}', '') = '' AND COALESCE(payload #>> '{reported}', '') = '') OR COALESCE(payload->'overage' = payload->'reported', FALSE))), FALSE))",
			got.sql)
	})
	t.Run("an ordering casts only two numbers, and orders two strings by byte", func(t *testing.T) {
		got := irCompile(t, &ComparisonExpression{Field: irPayloadField("overage"), Operator: OpGt, Value: &FieldOperand{Field: irPayloadField("reported")}})
		require.Equal(t,
			"(CASE WHEN jsonb_typeof(payload->'overage') = 'number' AND jsonb_typeof(payload->'reported') = 'number' THEN (payload #>> '{overage}')::numeric > (payload #>> '{reported}')::numeric "+
				"WHEN jsonb_typeof(payload->'overage') = 'string' AND jsonb_typeof(payload->'reported') = 'string' THEN (payload #>> '{overage}') COLLATE \"C\" > (payload #>> '{reported}') "+
				"ELSE FALSE END)",
			got.sql)
	})
	t.Run("both sides may be the element in scope", func(t *testing.T) {
		pred := &ArrayPredicateExpression{Field: irPayloadField("items"), Method: ArrayMethodAny, Param: "i",
			Pred: &ComparisonExpression{Field: irElementField("qty"), Operator: OpGe, Value: &FieldOperand{Field: irElementField("min")}}}
		got := irCompile(t, pred)
		require.Contains(t, got.sql, "jsonb_typeof(e.v->'qty') = 'number' AND jsonb_typeof(e.v->'min') = 'number' THEN (e.v #>> '{qty}')::numeric >= (e.v #>> '{min}')::numeric")
	})
	t.Run("a column is never a field operand", func(t *testing.T) {
		_, err := compileFieldOperandComparison(&ComparisonExpression{Field: FieldReference{Raw: "createdAt", Parts: []string{"createdAt"}}, Operator: OpEq},
			&FieldOperand{Field: irPayloadField("dueAt")}, nil)
		require.ErrorContains(t, err, "a field comparison reads two payload fields")
	})
}

func TestFieldOperand_InProcessIsEvalExprsEqualityAndOrder(t *testing.T) {
	cmp := func(op ComparisonOperator) *ComparisonExpression {
		return &ComparisonExpression{Field: irPayloadField("a"), Operator: op, Value: &FieldOperand{Field: irPayloadField("b")}}
	}
	cases := []struct {
		payload string
		op      ComparisonOperator
		want    bool
	}{
		{`{"a":1,"b":2}`, OpLt, true},
		{`{"a":2,"b":2.0}`, OpEq, true},
		{`{"a":"x","b":"x"}`, OpEq, true},
		{`{"a":"1","b":1}`, OpEq, false},
		{`{"a":"","b":null}`, OpEq, true},
		{`{}`, OpEq, true},
		{`{"a":1}`, OpEq, false},
		{`{"a":1}`, OpNe, true},
		{`{"a":"E","b":"a"}`, OpLt, true},
		{`{"a":1,"b":"2"}`, OpLt, false},
		{`{"a":true,"b":false}`, OpGt, false},
		{`{"a":[1,{"k":"v"}],"b":[1.0,{"k":"v"}]}`, OpEq, true},
		{`{"a":{},"b":{}}`, OpLt, false},
	}
	for _, tc := range cases {
		match, err := nodeMatchesExpression(irRow(t, tc.payload), cmp(tc.op), map[string]map[string]any{})
		require.NoError(t, err)
		require.Equal(t, tc.want, match, "%s over %s", tc.op, tc.payload)
	}
}

func TestIncludes_CompilesTypedAndRefusesABlankNeedle(t *testing.T) {
	got := irCompile(t, irPayloadCmp("title", OpIncludes, "50%_off"))
	require.Equal(t, "(jsonb_typeof(payload->'title') = 'string' AND strpos(payload #>> '{title}', ?) > 0)", got.sql)
	require.Equal(t, []any{"50%_off"}, got.args, "the needle is bound verbatim: strpos has no pattern language to escape")

	for _, blank := range []any{"", "   ", nil} {
		got := irCompile(t, irPayloadCmp("title", OpIncludes, blank))
		require.Equal(t, "FALSE", got.sql, "a blank or absent needle (%q) matches nothing", blank)
	}

	_, err := compilePayloadComparison([]string{"title"}, OpIncludes, int64(5))
	require.ErrorContains(t, err, "includes() takes a string")
}

func TestIncludes_InProcessMirrorsTheSQL(t *testing.T) {
	cases := []struct {
		payload string
		needle  any
		want    bool
	}{
		{`{"title":"half off today"}`, "off", true},
		{`{"title":"half off today"}`, "Off", false},
		{`{"title":"50%_off"}`, "%_", true},
		{`{"title":"x"}`, "", false},
		{`{"title":"x"}`, " ", false},
		{`{"title":"x"}`, nil, false},
		{`{"title":5}`, "5", false},
		{`{}`, "x", false},
		{`{"title":["off"]}`, "off", false},
	}
	for _, tc := range cases {
		match, err := nodeMatchesExpression(irRow(t, tc.payload), irPayloadCmp("title", OpIncludes, tc.needle), map[string]map[string]any{})
		require.NoError(t, err)
		require.Equal(t, tc.want, match, "%v in %s", tc.needle, tc.payload)
	}
}
