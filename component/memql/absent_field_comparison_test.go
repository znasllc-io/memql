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
// The direction is deliberate, not accidental, and it has been refined
// twice -- but nothing pinned it, so a future change could flip it
// silently in either direction and re-break a shipped bug:
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
// The cost of that choice is the fail-open direction #2783 records: a
// MISSPELLED property resolves to "absent" and therefore matches EVERY
// row. An `==` typo returns zero rows and is noticed immediately; a `!=`
// typo on an authorization- or deletion-scoped filter quietly serves
// rows that should have been excluded.
//
// The semantics cannot fix that alone -- they cannot tell
// declared-but-absent (null semantics correct) from undeclared entirely
// (an author error). That distinction belongs to field validation,
// memql#2781. These tests pin the behaviour so that when #2781 lands,
// any change of direction is a loud, deliberate decision.

// absentFieldCase is one comparison against a payload lacking the field.
type absentFieldCase struct {
	name  string
	op    ComparisonOperator
	value any

	// wantSQL is the EXACT fragment the push-down must emit.
	//
	// Asserting the whole fragment -- operator included -- rather than
	// probing for a token is the point. A `require.Contains(sql,
	// "COALESCE")` still passes when the operator inside it is inverted
	// from `<>` to `=`, which turns the "is set" idiom into "is NOT set"
	// and re-breaks #1708 verbatim, with the package green (memql#2783
	// review, finding 1).
	wantSQL string

	// wantMatch is whether a row LACKING the field is returned. Both the
	// post-filter and the emitted SQL must agree on this.
	wantMatch bool

	why string
}

func absentFieldCases() []absentFieldCase {
	return []absentFieldCase{
		{
			name:      "!= concrete value matches an absent field",
			op:        OpNe,
			value:     true,
			wantSQL:   `((payload #>> '{deleted}')::boolean IS DISTINCT FROM ?)`,
			wantMatch: true,
			why:       "#1685: absent IS DISTINCT FROM a concrete value, so deleted != true keeps rows with no deleted key",
		},
		{
			name:      "!= non-empty string matches an absent field",
			op:        OpNe,
			value:     "revoked",
			wantSQL:   `(payload #>> '{deleted}' IS DISTINCT FROM ?)`,
			wantMatch: true,
			why:       "#1685 applies to strings too, as long as the operand is not the empty string",
		},
		{
			name:      `!= "" does NOT match an absent field`,
			op:        OpNe,
			value:     "",
			wantSQL:   `(COALESCE(payload #>> '{deleted}', '') <> ?)`,
			wantMatch: false,
			why:       `#1708/#1714: an absent string field IS "" (not set), so the "is set" idiom must exclude it`,
		},
		{
			name:      "== concrete value does not match an absent field",
			op:        OpEq,
			value:     true,
			wantSQL:   `((payload #>> '{deleted}')::boolean = ?)`,
			wantMatch: false,
			why:       "absent is correctly NOT equal to a concrete value -- an == typo returns zero rows, which is visible",
		},
		{
			name:      `== "" does not match an absent field`,
			op:        OpEq,
			value:     "",
			wantSQL:   `(payload #>> '{deleted}' = ?)`,
			wantMatch: false,
			why:       "NULL = '' is NULL, not true, so absent is excluded; == must never be made null-safe",
		},
	}
}

func absentFieldNode() memorynodes.MemoryNode {
	// The payload deliberately carries a DIFFERENT key, so the probed
	// path is absent rather than the payload being empty -- that is the
	// typo shape (`deleted` vs a misspelled `delted`).
	return memorynodes.MemoryNode{
		ID:      "v1:agents:agent:test-id",
		Concept: "v1:agents:agent",
		Payload: json.RawMessage(`{"active":true}`),
	}
}

func absentFieldComparison(op ComparisonOperator, value any) *ComparisonExpression {
	// `payload.deleted` is what both paths receive in practice: an
	// authored bare `deleted` is rewritten to `payload.deleted` by
	// filterFieldRef before either path sees it. The bare spelling is the
	// authoring form; this is the internal one.
	return &ComparisonExpression{
		Field:    FieldReference{Parts: []string{"payload", "deleted"}, Raw: "payload.deleted"},
		Operator: op,
		Value:    value,
	}
}

// The in-process post-filter. executeCombinedFilterQuery re-evaluates the
// whole expression tree on every candidate after the DB scan, so this
// path decides the final row set just as much as the SQL does.
func TestAbsentPayloadField_PostFilterSemantics(t *testing.T) {
	node := absentFieldNode()
	for _, tc := range absentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			match, err := nodeMatchesComparison(node, absentFieldComparison(tc.op, tc.value), map[string]map[string]any{})
			require.NoError(t, err)
			require.Equal(t, tc.wantMatch, match, tc.why)
		})
	}
}

// The SQL push-down must encode the same rules, asserted as the exact
// emitted fragment so an inverted operator cannot hide inside a retained
// token.
func TestAbsentPayloadField_SQLPushdownSemantics(t *testing.T) {
	for _, tc := range absentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compilePayloadComparison([]string{"deleted"}, tc.op, tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.wantSQL, compiled.sql, tc.why)
		})
	}
}

// sqlMatchesAbsentRow computes whether an emitted fragment matches a row
// whose extraction is SQL NULL.
//
// It recognises exactly the shapes the push-down emits and FAILS on
// anything else, deliberately: an earlier version inferred the answer
// from a substring and fell back to "does not match" for anything
// unrecognised, which made the agreement check below a tautology -- a
// `COALESCE(x, ”) = ?` fragment (matching a NULL row in real Postgres)
// was scored as non-matching and the divergence went unnoticed
// (memql#2783 review, finding 2).
func sqlMatchesAbsentRow(t *testing.T, fragment string, operand any) bool {
	t.Helper()
	switch {
	// COALESCE FIRST. It rewrites the NULL away, so the operator that
	// follows applies to '' rather than to NULL -- which means this arm
	// must win even when the fragment also contains IS DISTINCT FROM.
	// `COALESCE(NULL,'') IS DISTINCT FROM ''` is FALSE, the opposite of
	// the bare IS DISTINCT FROM answer, so checking that arm first would
	// report a divergence that does not exist.
	case strings.HasPrefix(fragment, "(COALESCE("):
		// COALESCE(NULL, '') -> ''. Apply the fragment's own operator.
		s, ok := operand.(string)
		require.True(t, ok, "a COALESCE fragment compares strings; got %T", operand)
		switch {
		case strings.Contains(fragment, "<> ?"):
			return s != ""
		case strings.Contains(fragment, "IS DISTINCT FROM ?"):
			return s != ""
		case strings.Contains(fragment, "= ?") && !strings.Contains(fragment, ">= ?") && !strings.Contains(fragment, "<= ?"):
			return s == ""
		}
		t.Fatalf("unrecognised operator in COALESCE fragment %q", fragment)

	case strings.Contains(fragment, "IS DISTINCT FROM ?"):
		// NULL IS DISTINCT FROM <non-null operand> -> TRUE.
		return true

	// Every remaining shape compares NULL directly, which yields NULL --
	// never true -- so the row is not returned. Guarding `= ?` against
	// the compound operators keeps `>=` / `<=` from being read as
	// equality even though the answer happens to coincide.
	case strings.Contains(fragment, "= ?"),
		strings.Contains(fragment, "> ?"),
		strings.Contains(fragment, "< ?"),
		strings.Contains(fragment, "IN (?)"):
		return false
	}
	t.Fatalf("unrecognised SQL shape %q -- extend sqlMatchesAbsentRow deliberately "+
		"rather than letting a new shape fall into a default", fragment)
	return false
}

// The invariant that actually protects correctness: a combined-filter
// query scans in SQL and then re-filters in process, so if the two paths
// disagree about an absent field the rows you get depend on which path
// ran.
func TestAbsentPayloadField_SQLAndPostFilterAgree(t *testing.T) {
	node := absentFieldNode()
	for _, tc := range absentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			post, err := nodeMatchesComparison(node, absentFieldComparison(tc.op, tc.value), map[string]map[string]any{})
			require.NoError(t, err)

			compiled, err := compilePayloadComparison([]string{"deleted"}, tc.op, tc.value)
			require.NoError(t, err)
			sqlMatch := sqlMatchesAbsentRow(t, compiled.sql, tc.value)

			require.Equal(t, post, sqlMatch,
				"SQL push-down and in-process post-filter disagree about an absent field for %v %v; "+
					"a combined-filter query would return different rows depending on which path ran. SQL: %s",
				tc.op, tc.value, compiled.sql)
			require.Equal(t, tc.wantMatch, post, tc.why)
		})
	}
}

// The null-safe rule is specific to `!=`. `not in` and the ordered
// comparisons treat an absent field as a NON-match, so
// `deleted not in [true]` does NOT behave like `deleted != true` -- an
// asymmetry an author can reasonably get wrong, and one the authoring
// rules now state, so it is pinned here too.
//
// SQL agrees by construction: NOT IN / IN / `>` against a NULL
// extraction all yield NULL, which does not return the row.
func TestAbsentPayloadField_OnlyNotEqualsIsNullSafe(t *testing.T) {
	node := absentFieldNode()
	for _, tc := range []struct {
		name    string
		op      ComparisonOperator
		value   any
		wantSQL string
	}{
		{"not in", OpOut, []any{true}, `((payload #>> '{deleted}')::boolean NOT IN (?))`},
		{"in", OpIn, []any{true}, `((payload #>> '{deleted}')::boolean IN (?))`},
		{"greater than", OpGt, 5, `((payload #>> '{deleted}')::numeric > ?)`},
		// A STRING collection compiles to an entirely different shape
		// (jsonb_typeof / jsonb_exists_any, to support array-valued
		// payload fields). It must reach the same answer for an absent
		// field: jsonb_typeof(NULL) is NULL, so neither disjunct holds.
		{
			"in, string collection", OpIn, []any{"a", "b"},
			`((jsonb_typeof(payload->'deleted') = 'array' AND jsonb_exists_any(payload->'deleted', ?::text[])) OR (payload #>> '{deleted}' IN (?)))`,
		},
		{
			"not in, string collection", OpOut, []any{"a", "b"},
			`((jsonb_typeof(payload->'deleted') = 'array' AND NOT jsonb_exists_any(payload->'deleted', ?::text[])) OR (jsonb_typeof(payload->'deleted') != 'array' AND payload #>> '{deleted}' NOT IN (?)))`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, err := nodeMatchesComparison(node, absentFieldComparison(tc.op, tc.value), map[string]map[string]any{})
			require.NoError(t, err)
			require.False(t, match,
				"only != is null-safe; %s must treat an absent field as a non-match", tc.name)

			compiled, err := compilePayloadComparison([]string{"deleted"}, tc.op, tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.wantSQL, compiled.sql)
			require.NotContains(t, compiled.sql, "IS DISTINCT FROM",
				"%s must not acquire null-safe semantics", tc.name)
			require.NotContains(t, compiled.sql, "COALESCE",
				"%s must not acquire the empty-string carve-out", tc.name)
		})
	}
}

// The consequence, as an executable claim rather than prose: a
// misspelled property in a `!=` predicate matches a row it was written
// to exclude. This is the fail-open direction memql#2783 records, and
// memql#2781 (field-existence validation) is its actual mitigation.
func TestAbsentPayloadField_MisspelledNotEqualsMatchesEveryRow(t *testing.T) {
	// A row that SHOULD be excluded by `deleted != true`.
	deleted := memorynodes.MemoryNode{
		ID:      "v1:agents:agent:deleted-row",
		Concept: "v1:agents:agent",
		Payload: json.RawMessage(`{"deleted":true}`),
	}

	match, err := nodeMatchesComparison(deleted, absentFieldComparison(OpNe, true), map[string]map[string]any{})
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
			"validation (memql#2781), not a change of comparison semantics -- reversing this would "+
			"re-break #1685.")
}
