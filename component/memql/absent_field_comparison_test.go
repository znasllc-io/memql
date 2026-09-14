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
// three times -- but nothing pinned it, so a future change could flip it
// silently in either direction and re-break a shipped bug:
//
//   - #1685 chose null-safe inequality for `!=` so an absent field MATCHES.
//     The bug it fixed was the opposite: plain SQL `<>` yields NULL (not
//     true) when the field is missing, so `isNotDeleted`
//     (`deleted != true`) silently DROPPED every row that never had a
//     `deleted` key -- the concept @default is not always stamped.
//   - #1708/#1714 then carved out `!= ""`. An absent string field is
//     logically EQUAL to "" (both mean "not set"), and `!= ""` is the
//     canonical "is set" idiom across the DSL
//     (`deletionScheduledAt != ""`, `consumedAt != ""`), which under the
//     bare #1685 rule was matching every unset row.
//   - Edition 2026 (memql#5366) makes both carve-outs one rule: ONE NOTION
//     OF UNSET. An absent key, JSON null, `nil` and "" are one value to
//     `==` and `!=`, so `== ""` and `== nil` both MATCH an absent field
//     (the "is NOT set" idiom now agrees with the "is set" one), and the
//     two spellings compile to one fragment. Before, `revokedAt == ""`
//     excluded a row that had never been stamped with the key -- the
//     never-stamped row #1685 was about, excluded by the other operator.
//     Comparisons are also TYPED now (a literal compares only with a stored
//     value of its own JSON type), and `!=` is the exact two-valued negation
//     of `==`, which is what keeps #1685's direction.
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
			wantSQL:   `(NOT COALESCE(((CASE WHEN jsonb_typeof(payload->'deleted') = 'boolean' THEN (payload #>> '{deleted}')::boolean = ? ELSE FALSE END)), FALSE))`,
			wantMatch: true,
			why:       "#1685: absent is not equal to a concrete value, so deleted != true keeps rows with no deleted key -- `!=` is the two-valued negation of `==`",
		},
		{
			name:      "!= non-empty string matches an absent field",
			op:        OpNe,
			value:     "revoked",
			wantSQL:   `(NOT COALESCE(((jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' = ?)), FALSE))`,
			wantMatch: true,
			why:       "#1685 applies to strings too, as long as the operand is not the empty string",
		},
		{
			name:      `!= "" does NOT match an absent field`,
			op:        OpNe,
			value:     "",
			wantSQL:   `(COALESCE(payload #>> '{deleted}', '') <> '')`,
			wantMatch: false,
			why:       `#1708/#1714: an absent string field IS "" (not set), so the "is set" idiom must exclude it`,
		},
		{
			name:      "== concrete value does not match an absent field",
			op:        OpEq,
			value:     true,
			wantSQL:   `(CASE WHEN jsonb_typeof(payload->'deleted') = 'boolean' THEN (payload #>> '{deleted}')::boolean = ? ELSE FALSE END)`,
			wantMatch: false,
			why:       "absent is correctly NOT equal to a concrete value -- an == typo returns zero rows, which is visible",
		},
		{
			// THE REVERSAL (memql#5366). This row used to read `== "" does
			// not match an absent field`, with the SQL a plain `=` and the
			// reason "NULL = '' is NULL, not true". That reasoning was about
			// SQL, not about the language: it made the "is NOT set" idiom
			// disagree with the "is set" one above about the very rows both
			// are written for.
			name:      `== "" MATCHES an absent field`,
			op:        OpEq,
			value:     "",
			wantSQL:   `(COALESCE(payload #>> '{deleted}', '') = '')`,
			wantMatch: true,
			why:       `one notion of unset: an absent string field IS "" (not set), so the "is not set" idiom must include it`,
		},
		{
			// The unset rule is for the EMPTY string only. A whitespace-only
			// string is a value, and absent is not equal to a value.
			name:      `== " " (not empty) does not match an absent field`,
			op:        OpEq,
			value:     " ",
			wantSQL:   `(jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' = ?)`,
			wantMatch: false,
			why:       `only "" is unset; a single space is a value`,
		},
		{
			name:      "== nil matches an absent field, with the SAME fragment as == \"\"",
			op:        OpMissing,
			value:     nil,
			wantSQL:   `(COALESCE(payload #>> '{deleted}', '') = '')`,
			wantMatch: true,
			why:       "one notion of unset: `== nil` and `== \"\"` are one question",
		},
		{
			name:      "!= nil does not match an absent field, with the SAME fragment as != \"\"",
			op:        OpNotMissing,
			value:     nil,
			wantSQL:   `(COALESCE(payload #>> '{deleted}', '') <> '')`,
			wantMatch: false,
			why:       "one notion of unset: `!= nil` and `!= \"\"` are one question",
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

// splitBinaryFragment splits `((A) OP (B))` -- a fragment whose body is two
// parenthesised operands joined by AND or OR -- into its parts. ok is false
// for any other shape. The fragments this file models carry no parentheses
// inside quoted text, so counting parentheses is exact.
func splitBinaryFragment(fragment string) (string, string, string, bool) {
	if !strings.HasPrefix(fragment, "((") || !strings.HasSuffix(fragment, ")") {
		return "", "", "", false
	}
	body := fragment[1 : len(fragment)-1]
	depth := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				rest := body[i+1:]
				for _, op := range []string{" AND ", " OR "} {
					if strings.HasPrefix(rest, op) {
						return body[:i+1], strings.TrimSpace(op), rest[len(op):], true
					}
				}
				return "", "", "", false
			}
		}
	}
	return "", "", "", false
}

// sqlMatchesAbsentRow computes whether an emitted fragment matches a row
// whose extraction is SQL NULL (jsonb_typeof of it is NULL too).
//
// It recognises exactly the shapes the push-down emits and FAILS on
// anything else, deliberately: an earlier version inferred the answer
// from a substring and fell back to "does not match" for anything
// unrecognised, which made the agreement check below a tautology -- a
// `COALESCE(x, ”) = ?` fragment (matching a NULL row in real Postgres)
// was scored as non-matching and the divergence went unnoticed
// (memql#2783 review, finding 2).
func sqlMatchesAbsentRow(t *testing.T, fragment string) bool {
	t.Helper()
	if left, op, right, ok := splitBinaryFragment(fragment); ok {
		l, r := sqlMatchesAbsentRow(t, left), sqlMatchesAbsentRow(t, right)
		if op == "AND" {
			return l && r
		}
		return l || r
	}
	switch {
	// The two-valued negation: the operand's verdict for the absent row,
	// COALESCEd to false and negated. This is what makes `!=` null-safe.
	case strings.HasPrefix(fragment, "(NOT COALESCE((") && strings.HasSuffix(fragment, "), FALSE))"):
		inner := strings.TrimSuffix(strings.TrimPrefix(fragment, "(NOT COALESCE(("), "), FALSE))")
		return !sqlMatchesAbsentRow(t, inner)

	// The unset test: COALESCE(NULL, '') is the empty text.
	case strings.HasPrefix(fragment, "(COALESCE(") && strings.HasSuffix(fragment, " <> '')"):
		return false
	case strings.HasPrefix(fragment, "(COALESCE(") && strings.HasSuffix(fragment, " = '')"):
		return true

	// A typed comparison: jsonb_typeof(NULL) = '<type>' is NULL, so a CASE
	// takes its ELSE FALSE and an AND is NULL -- neither returns the row.
	case strings.HasPrefix(fragment, "(CASE WHEN jsonb_typeof("),
		strings.HasPrefix(fragment, "(jsonb_typeof("):
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
			sqlMatch := sqlMatchesAbsentRow(t, compiled.sql)

			require.Equal(t, post, sqlMatch,
				"SQL push-down and in-process post-filter disagree about an absent field for %v %v; "+
					"a combined-filter query would return different rows depending on which path ran. SQL: %s",
				tc.op, tc.value, compiled.sql)
			require.Equal(t, tc.wantMatch, post, tc.why)
		})
	}
}

// The null-safe rule is specific to `!=`. `not in`, `in` and the ordered
// comparisons treat an absent field as a NON-match, so
// `deleted not in [true]` does NOT behave like `deleted != true` -- an
// asymmetry an author can reasonably get wrong, and one the authoring
// rules state, so it is pinned here too.
//
// The fragments are asserted exactly and then JUDGED by the model rather
// than probed for tokens. The old version of this test asserted that no
// fragment here contained COALESCE; edition 2026's `not in` carries the
// unset test on purpose -- the COALESCE "is set" conjunct is precisely what
// EXCLUDES the absent row -- so the token was never the property.
func TestAbsentPayloadField_OnlyNotEqualsIsNullSafe(t *testing.T) {
	node := absentFieldNode()
	for _, tc := range []struct {
		name    string
		op      ComparisonOperator
		value   any
		wantSQL string
	}{
		{"not in", OpOut, []any{true},
			`((COALESCE(payload #>> '{deleted}', '') <> '') AND (NOT COALESCE(((CASE WHEN jsonb_typeof(payload->'deleted') = 'boolean' THEN (payload #>> '{deleted}')::boolean IN (?) ELSE FALSE END)), FALSE)))`},
		{"in", OpIn, []any{true},
			`(CASE WHEN jsonb_typeof(payload->'deleted') = 'boolean' THEN (payload #>> '{deleted}')::boolean IN (?) ELSE FALSE END)`},
		{"greater than", OpGt, 5,
			`(CASE WHEN jsonb_typeof(payload->'deleted') = 'number' THEN (payload #>> '{deleted}')::numeric > ? ELSE FALSE END)`},
		{"in, string collection", OpIn, []any{"a", "b"},
			`(jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' IN (?))`},
		{"not in, string collection", OpOut, []any{"a", "b"},
			`((COALESCE(payload #>> '{deleted}', '') <> '') AND (NOT COALESCE(((jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' IN (?))), FALSE)))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, err := nodeMatchesComparison(node, absentFieldComparison(tc.op, tc.value), map[string]map[string]any{})
			require.NoError(t, err)
			require.False(t, match,
				"only != is null-safe; %s must treat an absent field as a non-match", tc.name)

			compiled, err := compilePayloadComparison([]string{"deleted"}, tc.op, tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.wantSQL, compiled.sql)
			require.False(t, sqlMatchesAbsentRow(t, compiled.sql),
				"%s must not return the absent row in SQL either", tc.name)
		})
	}
}

// `in` is `==` against each member (memql#5366), so a list that names an
// UNSET member -- "" or nil -- admits the unset rows, and a list that does not
// never does.
func TestAbsentPayloadField_InAListWithAnUnsetMember(t *testing.T) {
	node := absentFieldNode()
	for _, tc := range []struct {
		name    string
		value   any
		wantSQL string
		want    bool
	}{
		{"an empty-string member", []any{"", "a"},
			`((COALESCE(payload #>> '{deleted}', '') = '') OR (jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' IN (?)))`, true},
		{"a nil member", []any{nil, "a"},
			`((COALESCE(payload #>> '{deleted}', '') = '') OR (jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' IN (?)))`, true},
		{"only unset members", []any{nil, ""},
			`(COALESCE(payload #>> '{deleted}', '') = '')`, true},
		{"no unset member", []any{"a"},
			`(jsonb_typeof(payload->'deleted') = 'string' AND payload #>> '{deleted}' IN (?))`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compilePayloadComparison([]string{"deleted"}, OpIn, tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.wantSQL, compiled.sql)
			require.Equal(t, tc.want, sqlMatchesAbsentRow(t, compiled.sql))
			match, err := nodeMatchesComparison(node, absentFieldComparison(OpIn, tc.value), map[string]map[string]any{})
			require.NoError(t, err)
			require.Equal(t, tc.want, match)
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
		"documented fail-open (memql#2783): a typo'd property is ABSENT, absent is not equal to true, "+
			"so the predicate matches a row it was written to exclude. Mitigation is field-existence "+
			"validation (memql#2781), not a change of comparison semantics -- reversing this would "+
			"re-break #1685.")
}
