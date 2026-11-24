package memql

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// executor_filter_empty_membership_test.go -- `v in <empty list>` admits
// nothing, whichever field it tests and however the list arrived.
//
// A written `row.status in []` already lowers to const(false)
// (expr_lower_test.go), and `startsWith` treats an empty list the same way
// (executor_filter_startswith_test.go). A list that arrives as an ARGUMENT
// used to fail the whole read instead ("collection literal cannot be empty",
// "id list must include at least one value"): those refusals guarded against
// emitting SQL `IN ()`, which does not exist. The result was that a client
// with an honestly empty list -- Files' Materializer place before anything
// was made, its Desktop place's folder list when not searching -- got an
// error instead of an empty page. The legacy `not in` keeps its rule that an
// unset value is never a match, so on a field that can be unset it admits the
// set rows, and on a column that is never unset it admits every row.

// The form the call-site parser hands an argument list to the filter.
var emptyLists = []any{[]any{}}

func TestEmptyMembershipCompilesToAConstantOnColumns(t *testing.T) {
	for _, value := range emptyLists {
		for _, op := range []ComparisonOperator{OpIn, OpOut} {
			want := "FALSE"
			if op == OpOut {
				want = "TRUE"
			}
			id, err := compileIdComparison(op, value, "v1:library:artifact")
			require.NoError(t, err)
			require.Equal(t, want, id.sql, "id %s", op)
			require.Empty(t, id.args)

			concept, err := compileConceptComparisonIn(nil, op, value)
			require.NoError(t, err)
			require.Equal(t, want, concept.sql, "concept %s", op)

			typ, err := compileTypeComparison(op, value)
			require.NoError(t, err)
			require.Equal(t, want, typ.sql, "type %s", op)
		}
	}
}

func TestEmptyMembershipOnNullableColumnsKeepsNotInsUnsetRule(t *testing.T) {
	for _, value := range emptyLists {
		createdBy, err := compileCreatedByComparison(OpIn, value)
		require.NoError(t, err)
		require.Equal(t, "FALSE", createdBy.sql)
		createdBy, err = compileCreatedByComparison(OpOut, value)
		require.NoError(t, err)
		require.Equal(t, `("createdBy" IS NOT NULL)`, createdBy.sql)

		prov, err := compileProvenanceComparison("kind", OpIn, value)
		require.NoError(t, err)
		require.Equal(t, "FALSE", prov.sql)
		prov, err = compileProvenanceComparison("kind", OpOut, value)
		require.NoError(t, err)
		require.Equal(t, "(provenance->>'kind' IS NOT NULL)", prov.sql)
	}
}

func TestEmptyMembershipOnPayloadMatchesNothing(t *testing.T) {
	for _, value := range emptyLists {
		in, err := compilePayloadComparison([]string{"sourceConceptRef"}, OpIn, value)
		require.NoError(t, err)
		require.Equal(t, "FALSE", in.sql)
		require.Empty(t, in.args)

		out, err := compilePayloadComparison([]string{"sourceConceptRef"}, OpOut, value)
		require.NoError(t, err)
		require.Equal(t, compileUnsetSQL("payload #>> '{sourceConceptRef}'", false).sql, out.sql,
			"legacy not-in an empty list admits exactly the rows where the field is set")
	}
}

// The in-process evaluator re-checks every candidate the SQL returns, so it
// must give the same answers.
func TestEmptyMembershipInProcessAgreesWithSQL(t *testing.T) {
	set := memorynodes.MemoryNode{ID: "v1:library:artifact:a", Payload: json.RawMessage(`{"sourceConceptRef":"v1:library:file:x"}`)}
	unset := memorynodes.MemoryNode{ID: "v1:library:artifact:b", Payload: json.RawMessage(`{}`)}
	for _, value := range emptyLists {
		field := FieldReference{Parts: []string{"payload", "sourceConceptRef"}, Raw: "payload.sourceConceptRef"}
		for _, tc := range []struct {
			node memorynodes.MemoryNode
			op   ComparisonOperator
			want bool
		}{
			{set, OpIn, false},
			{unset, OpIn, false},
			{set, OpOut, true},
			{unset, OpOut, false},
		} {
			got, err := nodeMatchesComparison(tc.node, &ComparisonExpression{Field: field, Operator: tc.op, Value: value}, map[string]map[string]any{})
			require.NoError(t, err)
			require.Equal(t, tc.want, got, "payload %s on %s", tc.op, tc.node.ID)
		}
		idField := FieldReference{Parts: []string{"id"}, Raw: "id"}
		for op, want := range map[ComparisonOperator]bool{OpIn: false, OpOut: true} {
			got, err := nodeMatchesComparison(set, &ComparisonExpression{Field: idField, Operator: op, Value: value}, map[string]map[string]any{})
			require.NoError(t, err)
			require.Equal(t, want, got, "id %s", op)
		}
	}
}
