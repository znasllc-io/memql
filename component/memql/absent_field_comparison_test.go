package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#2783: how a comparison against an ABSENT payload field behaves.
//
// The direction here is deliberate, not accidental, and it has been
// refined twice -- but until now nothing pinned it, so a future change
// could flip it silently in either direction and re-break a shipped bug:
//
//   - #1685 chose IS DISTINCT FROM for `!=` so an absent field MATCHES.
//     The bug it fixed was the opposite: plain SQL `<>` yields NULL (not
//     true) when the field is missing, so `isNotDeleted`
//     (`deleted != true`) silently DROPPED every row that never had a
//     `deleted` key -- the concept @default is not always stamped.
//   - #1708/#1714 then carved out `!= ""`. An absent string field is
//     logically EQUAL to "" (both mean "not set"), and `!= ""` is the
//     canonical "is set" idiom across the DSL
//     (`deletionScheduledAt != ""`, `consumedAt != ""`), which under the
//     bare #1685 rule was matching every unset row.
//
// The cost of that choice is the fail-open direction #2783 documents: a
// MISSPELLED property in a `!=` predicate resolves to "absent" and
// therefore matches EVERY row. An `==` typo returns zero rows and is
// noticed immediately; a `!=` typo on an authorization- or
// deletion-scoped filter quietly serves rows that should be excluded.
//
// The semantics cannot fix that on their own, because they cannot tell
// declared-but-absent (null semantics are right) from undeclared
// entirely (an author error). That distinction belongs to field
// validation -- memql#2781. These tests pin the behaviour so that when
// #2781 lands, any change of direction is a loud, deliberate decision.

// absentFieldCase is one comparison against a payload that lacks the
// probed field.
type absentFieldCase struct {
	name      string
	op        ComparisonOperator
	value     any
	wantMatch bool
	why       string
}

func absentFieldCases() []absentFieldCase {
	return []absentFieldCase{
		{
			name:      "!= concrete value matches an absent field",
			op:        OpNe,
			value:     true,
			wantMatch: true,
			why:       "#1685: absent IS DISTINCT FROM a concrete value, so `deleted != true` keeps rows with no `deleted` key",
		},
		{
			name:      "!= non-empty string matches an absent field",
			op:        OpNe,
			value:     "revoked",
			wantMatch: true,
			why:       "#1685 applies to strings too, as long as the operand is not the empty string",
		},
		{
			name:      `!= "" does NOT match an absent field`,
			op:        OpNe,
			value:     "",
			wantMatch: false,
			why:       `#1708/#1714: an absent string field IS "" (not set), so the "is set" idiom must exclude it`,
		},
		{
			name:      "== concrete value does not match an absent field",
			op:        OpEq,
			value:     true,
			wantMatch: false,
			why:       "absent is correctly NOT equal to a concrete value -- an == typo returns zero rows, which is visible",
		},
		{
			name:      `== "" does not match an absent field`,
			op:        OpEq,
			value:     "",
			wantMatch: false,
			why:       "the post-filter treats absent as a non-match for every operator except the two carved out above",
		},
	}
}

// The in-process post-filter. executeCombinedFilterQuery re-evaluates the
// whole expression tree on every candidate after the DB scan, so this
// path decides the final row set just as much as the SQL does.
func TestAbsentPayloadField_PostFilterSemantics(t *testing.T) {
	// The payload deliberately carries a DIFFERENT key, so the probed
	// path is absent rather than the payload being empty -- that is the
	// typo shape (`deleted` vs a misspelled `delted`).
	node := memorynodes.MemoryNode{
		ID:      "v1:agents:agent:test-id",
		Concept: "v1:agents:agent",
		Payload: json.RawMessage(`{"active":true}`),
	}

	for _, tc := range absentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmp := &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "deleted"}, Raw: "payload.deleted"},
				Operator: tc.op,
				Value:    tc.value,
			}
			match, err := nodeMatchesComparison(node, cmp, map[string]map[string]any{})
			require.NoError(t, err)
			require.Equal(t, tc.wantMatch, match, tc.why)
		})
	}
}

// The SQL push-down must encode the same three rules. Asserting on the
// emitted fragment is what makes a silent flip impossible: swapping
// IS DISTINCT FROM back to `<>` would compile fine and pass every other
// test in the package.
func TestAbsentPayloadField_SQLPushdownSemantics(t *testing.T) {
	cases := []struct {
		name        string
		op          ComparisonOperator
		value       any
		wantFragile string
		why         string
	}{
		{
			name:        "!= concrete uses IS DISTINCT FROM so absent matches",
			op:          OpNe,
			value:       true,
			wantFragile: "IS DISTINCT FROM",
			why:         "#1685 -- plain `<>` yields NULL for an absent field and drops the row",
		},
		{
			name:        `!= "" coalesces so absent is excluded`,
			op:          OpNe,
			value:       "",
			wantFragile: "COALESCE",
			why:         `#1708/#1714 -- absent must read as "" for the "is set" idiom`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compilePayloadComparison([]string{"deleted"}, tc.op, tc.value)
			require.NoError(t, err)
			require.Contains(t, compiled.sql, tc.wantFragile, tc.why)
		})
	}

	// `==` must NOT be null-safe: absent is genuinely not equal.
	eq, err := compilePayloadComparison([]string{"deleted"}, OpEq, true)
	require.NoError(t, err)
	require.NotContains(t, eq.sql, "IS DISTINCT FROM",
		"== must stay plain equality; making it null-safe would match absent fields too")
}

// The invariant that actually protects correctness: a combined-filter
// query scans in SQL and then re-filters in process, so if the two paths
// disagree about an absent field the returned rows depend on which path
// ran. This asserts they agree on every case above.
func TestAbsentPayloadField_SQLAndPostFilterAgree(t *testing.T) {
	node := memorynodes.MemoryNode{
		ID:      "v1:agents:agent:test-id",
		Concept: "v1:agents:agent",
		Payload: json.RawMessage(`{"active":true}`),
	}

	for _, tc := range absentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			cmp := &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "deleted"}, Raw: "payload.deleted"},
				Operator: tc.op,
				Value:    tc.value,
			}
			post, err := nodeMatchesComparison(node, cmp, map[string]map[string]any{})
			require.NoError(t, err)

			compiled, err := compilePayloadComparison([]string{"deleted"}, tc.op, tc.value)
			require.NoError(t, err)

			// Model what the emitted SQL does to a NULL extraction:
			//   IS DISTINCT FROM <v>  -> NULL is distinct   -> matches
			//   COALESCE(x,'') <> ''  -> '' <> ''           -> excluded
			//   plain = / <>          -> NULL               -> excluded
			var sqlMatch bool
			switch {
			case strings.Contains(compiled.sql, "IS DISTINCT FROM"):
				sqlMatch = true
			case strings.Contains(compiled.sql, "COALESCE"):
				sqlMatch = false
			default:
				sqlMatch = false
			}

			require.Equal(t, post, sqlMatch,
				"SQL push-down and in-process post-filter disagree about an absent field for %s %v; "+
					"a combined-filter query would return different rows depending on which path ran",
				tc.op, tc.value)
		})
	}
}

// The consequence, stated as an executable claim rather than prose: a
// misspelled property in a `!=` predicate matches a row it was meant to
// exclude. This is the fail-open direction memql#2783 exists to record,
// and memql#2781 (field-existence validation) is its actual mitigation.
func TestAbsentPayloadField_MisspelledNotEqualsMatchesEveryRow(t *testing.T) {
	// A row that SHOULD be excluded by `deleted != true`.
	deleted := memorynodes.MemoryNode{
		ID:      "v1:agents:agent:deleted-row",
		Concept: "v1:agents:agent",
		Payload: json.RawMessage(`{"deleted":true}`),
	}

	correct := &ComparisonExpression{
		Field:    FieldReference{Parts: []string{"payload", "deleted"}, Raw: "payload.deleted"},
		Operator: OpNe,
		Value:    true,
	}
	match, err := nodeMatchesComparison(deleted, correct, map[string]map[string]any{})
	require.NoError(t, err)
	require.False(t, match, "the correctly-spelled predicate must exclude a deleted row")

	// One transposed letter, and the same row is now included.
	typo := &ComparisonExpression{
		Field:    FieldReference{Parts: []string{"payload", "delted"}, Raw: "payload.delted"},
		Operator: OpNe,
		Value:    true,
	}
	match, err = nodeMatchesComparison(deleted, typo, map[string]map[string]any{})
	require.NoError(t, err)
	require.True(t, match,
		"documented fail-open (memql#2783): a typo'd property is ABSENT, absent is DISTINCT FROM true, "+
			"so the predicate matches a row it was written to exclude. Mitigation is field-existence "+
			"validation at load (memql#2781), not a change of comparison semantics -- reversing this "+
			"would re-break #1685.")
}
