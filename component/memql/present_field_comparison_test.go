package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The sibling of absent_field_comparison_test.go, for a field that is PRESENT
// -- and in particular present with a JSON type other than the literal's.
//
// Same invariant, same reason. executeCombinedFilterQuery scans in SQL and
// then re-evaluates the whole expression tree in process on every candidate,
// so BOTH paths decide the row set. latestMatchingNodes even RELOADS each
// scanned id to its true latest version before re-evaluating, a row the SQL
// scan may never have examined -- so if the two paths' comparison rules
// differ, "the latest row satisfies your predicate" quietly becomes "the
// latest row satisfies a DIFFERENT predicate than the one you wrote".
//
// # memql#3628, and what replaced it
//
// memql#3628 made the halves agree by having the post-filter copy what the
// SQL did: extract the stored value as TEXT via `#>>`, then CAST that text by
// the literal's type -- `::numeric` for a number literal, `::boolean` for a
// boolean one. So a stored string "5" equalled the number 5, and a stored 5
// equalled the string "5". It agreed, but it agreed on coercion, and it had a
// sharper edge: a stored "abc" compared with a number literal was a Postgres
// ERROR (`invalid input syntax for type numeric`) that failed the whole read.
//
// Edition 2026 (memql#5366) makes both halves TYPED instead: a literal
// compares only with a stored value of its own JSON type, which is the
// in-process evaluator's typed equality (`1 == "1"` is false). The SQL guards
// each comparison on jsonb_typeof, so a mistyped stored value is simply not
// equal, not ordered and not a member -- and never reaches a cast that could
// raise. The text comparison is still VERBATIM (nothing is trimmed), and
// numbers still compare numerically across int and float.

// presentFieldCase is one comparison against a payload whose field is present.
type presentFieldCase struct {
	name string

	// payloadJSON is the stored row payload written as JSON, so the STORED
	// TYPE is explicit on the page. A Go `any` would blur the one
	// distinction these cases exist to draw -- the number 5 against the
	// string "5".
	payloadJSON string

	// extracted is what `payload #>> '{ownerUserId}'` yields for that
	// payload.
	//
	// STATED BY HAND, NOT DERIVED. This is the test's own model of
	// Postgres, and computing it with the same helper the production path
	// uses would make the agreement assertion below a tautology -- the
	// exact trap memql#2783's review caught in sqlMatchesAbsentRow, where
	// an inferred answer silently agreed with itself. The db-gated test at
	// the bottom of this file checks the hand-written value against a real
	// server.
	extracted string

	op    ComparisonOperator
	value any

	// wantSQL is the EXACT fragment the push-down must emit, operator
	// included, for the same reason absentFieldCase asserts it exactly.
	wantSQL string

	// wantMatch is whether the row is returned. Both paths must agree.
	wantMatch bool

	// dbSkip, when non-empty, keeps the case out of the db-gated check and
	// says why. Only the collection shapes use it: their bound arguments go
	// through bun.In, which the one-off `SELECT <fragment>` harness below
	// does not reproduce faithfully enough to be evidence.
	dbSkip string

	why string
}

// The fragments every case below is built from, spelled once.
const (
	presentStrEq  = `(jsonb_typeof(payload->'ownerUserId') = 'string' AND payload #>> '{ownerUserId}' = ?)`
	presentNumEq  = `(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'number' THEN (payload #>> '{ownerUserId}')::numeric = ? ELSE FALSE END)`
	presentBoolEq = `(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'boolean' THEN (payload #>> '{ownerUserId}')::boolean = ? ELSE FALSE END)`
)

// presentNot is the two-valued negation compileNotSQL wraps a fragment in.
func presentNot(fragment string) string {
	return "(NOT COALESCE((" + fragment + "), FALSE))"
}

func presentFieldCases() []presentFieldCase {
	return []presentFieldCase{
		// ---- strings: verbatim, never trimmed ------------------------------
		{
			name:        "stored whitespace is NOT trimmed away before an == compare",
			payloadJSON: `{"ownerUserId":" u-1 "}`,
			extracted:   " u-1 ",
			op:          OpEq,
			value:       "u-1",
			wantSQL:     presentStrEq,
			wantMatch:   false,
			why:         "no `=` in Postgres trims its operands, and neither does the post-filter",
		},
		{
			name:        "stored whitespace matches a literal carrying the same whitespace",
			payloadJSON: `{"ownerUserId":" u-1 "}`,
			extracted:   " u-1 ",
			op:          OpEq,
			value:       " u-1 ",
			wantSQL:     presentStrEq,
			wantMatch:   true,
			why:         "the value is comparable, just verbatim -- this is the other half of not trimming",
		},
		{
			name:        "!= against stored whitespace is the mirror of the == case",
			payloadJSON: `{"ownerUserId":" u-1 "}`,
			extracted:   " u-1 ",
			op:          OpNe,
			value:       "u-1",
			wantSQL:     presentNot(presentStrEq),
			wantMatch:   true,
			why:         "`!=` is the exact negation of `==`; ' u-1 ' is not 'u-1'",
		},
		{
			name:        "byte order puts an uppercase E before a lowercase a",
			payloadJSON: `{"ownerUserId":"E"}`,
			extracted:   "E",
			op:          OpLt,
			value:       "a",
			wantSQL:     `(jsonb_typeof(payload->'ownerUserId') = 'string' AND (payload #>> '{ownerUserId}') COLLATE "C" < ?)`,
			wantMatch:   true,
			why:         "strings order by byte on both halves; COLLATE \"C\" is what makes the database agree with Go",
		},

		// ---- the one notion of unset, for a PRESENT value --------------------
		{
			name:        "a stored empty string == \"\"",
			payloadJSON: `{"ownerUserId":""}`,
			extracted:   "",
			op:          OpEq,
			value:       "",
			wantSQL:     `(COALESCE(payload #>> '{ownerUserId}', '') = '')`,
			wantMatch:   true,
			why:         "\"\" is unset, and unset equals unset",
		},
		{
			name:        "a stored single space is a value, not unset",
			payloadJSON: `{"ownerUserId":" "}`,
			extracted:   " ",
			op:          OpEq,
			value:       "",
			wantSQL:     `(COALESCE(payload #>> '{ownerUserId}', '') = '')`,
			wantMatch:   false,
			why:         "only the empty string is unset; whitespace is a value",
		},
		{
			name:        "a stored empty string == nil",
			payloadJSON: `{"ownerUserId":""}`,
			extracted:   "",
			op:          OpMissing,
			value:       nil,
			wantSQL:     `(COALESCE(payload #>> '{ownerUserId}', '') = '')`,
			wantMatch:   true,
			why:         "nil and \"\" are one value to == and !=",
		},
		{
			name:        "a stored number is set",
			payloadJSON: `{"ownerUserId":0}`,
			extracted:   "0",
			op:          OpNotMissing,
			value:       nil,
			wantSQL:     `(COALESCE(payload #>> '{ownerUserId}', '') <> '')`,
			wantMatch:   true,
			why:         "a number never extracts as the empty text, so it is never unset",
		},

		// ---- TYPED: a string literal against a stored number or boolean ------
		{
			name:        "a stored NUMBER is not equal to the matching string literal",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpEq,
			value:       "5",
			wantSQL:     presentStrEq,
			wantMatch:   false,
			why:         "typed: 5 == \"5\" is false (memql#3628 compared the extracted text and said true)",
		},
		{
			name:        "stored 1 vs \"1\"",
			payloadJSON: `{"ownerUserId":1}`,
			extracted:   "1",
			op:          OpEq,
			value:       "1",
			wantSQL:     presentStrEq,
			wantMatch:   false,
			why:         "typed equality, the coordinator's pinned case",
		},
		{
			name:        "a stored NUMBER is != the matching string literal",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpNe,
			value:       "5",
			wantSQL:     presentNot(presentStrEq),
			wantMatch:   true,
			why:         "the negation of a typed false",
		},
		{
			name:        "a stored BOOLEAN is not equal to the matching string literal",
			payloadJSON: `{"ownerUserId":true}`,
			extracted:   "true",
			op:          OpEq,
			value:       "true",
			wantSQL:     presentStrEq,
			wantMatch:   false,
			why:         "typed: a boolean is not the string \"true\"",
		},
		{
			name:        "a string ordering does not order a stored number",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpGt,
			value:       "4",
			wantSQL:     `(jsonb_typeof(payload->'ownerUserId') = 'string' AND (payload #>> '{ownerUserId}') COLLATE "C" > ?)`,
			wantMatch:   false,
			why:         "typed: a string literal orders strings only (memql#3628 ordered the text '5')",
		},

		// ---- TYPED: a number literal ----------------------------------------
		{
			name:        "a stored STRING of digits is not equal to a number literal",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpEq,
			value:       int64(5),
			wantSQL:     presentNumEq,
			wantMatch:   false,
			why:         "typed: \"5\" == 5 is false (memql#3628 cast the text to numeric and said true)",
		},
		{
			name:        "stored \"1\" vs 1",
			payloadJSON: `{"ownerUserId":"1"}`,
			extracted:   "1",
			op:          OpEq,
			value:       int64(1),
			wantSQL:     presentNumEq,
			wantMatch:   false,
			why:         "typed equality, the coordinator's pinned case",
		},
		{
			name:        "a stored string with surrounding whitespace is not a number either",
			payloadJSON: `{"ownerUserId":" 5 "}`,
			extracted:   " 5 ",
			op:          OpEq,
			value:       int64(5),
			wantSQL:     presentNumEq,
			wantMatch:   false,
			why:         "the typed guard never reaches the cast that used to skip the whitespace",
		},
		{
			name:        "a stored number equals a number literal",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpEq,
			value:       int64(5),
			wantSQL:     presentNumEq,
			wantMatch:   true,
			why:         "the typed positive",
		},
		{
			name:        "numbers compare numerically across int and float",
			payloadJSON: `{"ownerUserId":5.0}`,
			extracted:   "5.0",
			op:          OpEq,
			value:       int64(5),
			wantSQL:     presentNumEq,
			wantMatch:   true,
			why:         "5.0 is 5; the scale jsonb keeps in the text does not matter to ::numeric or to Go",
		},
		{
			name:        "a number ordering orders a stored number",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpGt,
			value:       int64(4),
			wantSQL:     `(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'number' THEN (payload #>> '{ownerUserId}')::numeric > ? ELSE FALSE END)`,
			wantMatch:   true,
			why:         "the typed positive for an ordering",
		},
		{
			name:        "a number ordering does not order a stored string of digits",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpGt,
			value:       int64(4),
			wantSQL:     `(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'number' THEN (payload #>> '{ownerUserId}')::numeric > ? ELSE FALSE END)`,
			wantMatch:   false,
			why:         "typed: a string is not ordered against a number",
		},
		{
			// Before memql#5366 this was not a false -- it was a Postgres
			// ERROR that failed the whole read. The db-gated check below is
			// what proves it no longer raises.
			name:        "a malformed stored value never reaches the numeric cast",
			payloadJSON: `{"ownerUserId":"abc"}`,
			extracted:   "abc",
			op:          OpGt,
			value:       int64(4),
			wantSQL:     `(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'number' THEN (payload #>> '{ownerUserId}')::numeric > ? ELSE FALSE END)`,
			wantMatch:   false,
			why:         "the CASE keeps the cast off a string, so 'abc' is simply not greater than 4",
		},

		// ---- TYPED: a boolean literal ---------------------------------------
		{
			name:        "stored \"true\" vs true",
			payloadJSON: `{"ownerUserId":"true"}`,
			extracted:   "true",
			op:          OpEq,
			value:       true,
			wantSQL:     presentBoolEq,
			wantMatch:   false,
			why:         "typed: the string \"true\" is not the boolean (memql#3628 cast it and said true)",
		},
		{
			name:        "stored \"true\" != true",
			payloadJSON: `{"ownerUserId":"true"}`,
			extracted:   "true",
			op:          OpNe,
			value:       true,
			wantSQL:     presentNot(presentBoolEq),
			wantMatch:   true,
			why:         "the negation of a typed false",
		},
		{
			name:        "Postgres boolean input would accept \"on\"; the typed rule does not cast it",
			payloadJSON: `{"ownerUserId":"on"}`,
			extracted:   "on",
			op:          OpEq,
			value:       true,
			wantSQL:     presentBoolEq,
			wantMatch:   false,
			why:         "boolin takes any unambiguous prefix of true/false/yes/no/on/off; the typed guard never gets there",
		},
		{
			name:        "a malformed stored value never reaches the boolean cast",
			payloadJSON: `{"ownerUserId":"maybe"}`,
			extracted:   "maybe",
			op:          OpEq,
			value:       true,
			wantSQL:     presentBoolEq,
			wantMatch:   false,
			why:         "'maybe'::boolean is a Postgres ERROR; behind the CASE it is not evaluated",
		},
		{
			name:        "a stored boolean equals a boolean literal",
			payloadJSON: `{"ownerUserId":true}`,
			extracted:   "true",
			op:          OpEq,
			value:       true,
			wantSQL:     presentBoolEq,
			wantMatch:   true,
			why:         "the typed positive",
		},

		// ---- the same rules under `in` and `startsWith` ----------------------
		{
			name:        "a stored NUMBER is not in a string collection",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpIn,
			value:       []any{"5", "6"},
			wantSQL:     `(jsonb_typeof(payload->'ownerUserId') = 'string' AND payload #>> '{ownerUserId}' IN (?))`,
			wantMatch:   false,
			dbSkip:      "bound args go through bun.In",
			why:         "`in` is `==` against each member, and == is typed",
		},
		{
			name:        "a stored STRING of digits is not in a number collection",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpIn,
			value:       []any{int64(5), int64(6)},
			wantSQL:     `(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'number' THEN (payload #>> '{ownerUserId}')::numeric IN (?) ELSE FALSE END)`,
			wantMatch:   false,
			dbSkip:      "bound args go through bun.In",
			why:         "typed membership",
		},
		{
			name:        "a stored string is in a string collection",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpIn,
			value:       []any{"5", "6"},
			wantSQL:     `(jsonb_typeof(payload->'ownerUserId') = 'string' AND payload #>> '{ownerUserId}' IN (?))`,
			wantMatch:   true,
			dbSkip:      "bound args go through bun.In",
			why:         "the typed positive",
		},
		{
			name:        "a stored number has no prefix",
			payloadJSON: `{"ownerUserId":42}`,
			extracted:   "42",
			op:          OpStartsWith,
			value:       "4",
			wantSQL:     `(jsonb_typeof(payload->'ownerUserId') = 'string' AND (payload #>> '{ownerUserId}') ^@ ANY(?::text[]))`,
			wantMatch:   false,
			why:         "a prefix test is a question about a string (memql#4208 read the number's digits)",
		},
		{
			name:        "a stored string has its prefix",
			payloadJSON: `{"ownerUserId":"42"}`,
			extracted:   "42",
			op:          OpStartsWith,
			value:       "4",
			wantSQL:     `(jsonb_typeof(payload->'ownerUserId') = 'string' AND (payload #>> '{ownerUserId}') ^@ ANY(?::text[]))`,
			wantMatch:   true,
			why:         "the typed positive",
		},
	}
}

// presentFieldNode builds the scanned candidate the post-filter sees.
func presentFieldNode(t *testing.T, payloadJSON string) memorynodes.MemoryNode {
	t.Helper()
	require.True(t, json.Valid([]byte(payloadJSON)), "payload fixture must be valid JSON")
	return memorynodes.MemoryNode{
		ID:      "v1:agents:agent:test-id",
		Concept: "v1:agents:agent",
		Payload: json.RawMessage(payloadJSON),
	}
}

func presentFieldComparison(op ComparisonOperator, value any) *ComparisonExpression {
	// `payload.ownerUserId` is the internal spelling both paths receive: an
	// authored bare `ownerUserId` is rewritten by filterFieldRef before
	// either sees it.
	return &ComparisonExpression{
		Field:    FieldReference{Parts: []string{"payload", "ownerUserId"}, Raw: "payload.ownerUserId"},
		Operator: op,
		Value:    value,
	}
}

// presentJSONType is jsonb_typeof, restated: the JSON type of the stored
// value, read off the fixture's own JSON text.
func presentJSONType(t *testing.T, payloadJSON string) string {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(payloadJSON), &payload))
	switch payload["ownerUserId"].(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	t.Fatalf("unexpected stored value in %s", payloadJSON)
	return ""
}

// sqlMatchesPresentRow computes whether an emitted fragment returns a row
// whose stored value has the given JSON type and TEXT extraction.
//
// It is an independent restatement of Postgres, written out here rather than
// borrowed from the production helpers, and it FAILS on any shape it does not
// recognise -- the same discipline sqlMatchesAbsentRow adopted after an
// inferring version turned the agreement check into a tautology.
func sqlMatchesPresentRow(t *testing.T, fragment, jsonType, extracted string, operand any) bool {
	t.Helper()
	if left, op, right, ok := splitBinaryFragment(fragment); ok {
		l := sqlMatchesPresentRow(t, left, jsonType, extracted, operand)
		r := sqlMatchesPresentRow(t, right, jsonType, extracted, operand)
		if op == "AND" {
			return l && r
		}
		return l || r
	}
	const text = "payload #>> '{ownerUserId}'"
	guard := func(prefix, want string) (string, bool) {
		if !strings.HasPrefix(fragment, prefix) {
			return "", false
		}
		return strings.TrimPrefix(fragment, prefix), jsonType == want
	}
	switch {
	case strings.HasPrefix(fragment, "(NOT COALESCE((") && strings.HasSuffix(fragment, "), FALSE))"):
		inner := strings.TrimSuffix(strings.TrimPrefix(fragment, "(NOT COALESCE(("), "), FALSE))")
		return !sqlMatchesPresentRow(t, inner, jsonType, extracted, operand)

	case fragment == "(COALESCE("+text+", '') = '')":
		return extracted == ""
	case fragment == "(COALESCE("+text+", '') <> '')":
		return extracted != ""
	}

	if rest, typeOK := guard("(jsonb_typeof(payload->'ownerUserId') = 'string' AND ", "string"); rest != "" {
		if !typeOK {
			return false
		}
		switch {
		case rest == text+" = ?)":
			return extracted == operand.(string)
		case rest == text+" IN (?))":
			return collectionHasText(operandStrings(t, operand), extracted)
		case rest == "("+text+") ^@ ANY(?::text[]))":
			for _, prefix := range operandPrefixes(t, operand) {
				if strings.HasPrefix(extracted, prefix) {
					return true
				}
			}
			return false
		case strings.HasPrefix(rest, "("+text+") COLLATE \"C\" "):
			return applyOrderedOperator(t, rest, extracted, operand.(string))
		}
		t.Fatalf("unrecognised string comparison %q", fragment)
	}
	if rest, typeOK := guard("(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'number' THEN ", "number"); rest != "" {
		if !typeOK {
			return false
		}
		n, err := strconv.ParseFloat(extracted, 64)
		require.NoError(t, err, "a stored number extracts as parseable digits")
		if strings.Contains(rest, "IN (?)") {
			for _, c := range operandFloats(t, operand) {
				if c == n {
					return true
				}
			}
			return false
		}
		return applyOrderedOperator(t, rest, n, operandFloat(t, operand))
	}
	if rest, typeOK := guard("(CASE WHEN jsonb_typeof(payload->'ownerUserId') = 'boolean' THEN ", "boolean"); rest != "" {
		if !typeOK {
			return false
		}
		want, ok := operand.(bool)
		require.True(t, ok, "a boolean comparison compares booleans; got %T", operand)
		return (extracted == "true") == want
	}
	t.Fatalf("unrecognised SQL shape %q -- extend this model deliberately", fragment)
	return false
}

// applyOrderedOperator reads the comparison operator out of a fragment and
// applies it. Ordered before equality so `>=` cannot be mistaken for `= ?`.
func applyOrderedOperator[T string | float64](t *testing.T, fragment string, actual, want T) bool {
	t.Helper()
	switch {
	case strings.Contains(fragment, ">= ?"):
		return actual >= want
	case strings.Contains(fragment, "<= ?"):
		return actual <= want
	case strings.Contains(fragment, "<> ?"):
		return actual != want
	case strings.Contains(fragment, "> ?"):
		return actual > want
	case strings.Contains(fragment, "< ?"):
		return actual < want
	case strings.Contains(fragment, "= ?"):
		return actual == want
	}
	t.Fatalf("unrecognised operator in fragment %q -- extend this model deliberately", fragment)
	return false
}

func operandStrings(t *testing.T, operand any) []string {
	t.Helper()
	items, ok := operand.([]any)
	require.True(t, ok, "a collection fragment compares a collection; got %T", operand)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		require.True(t, ok, "string collection element must be a string; got %T", item)
		out = append(out, s)
	}
	return out
}

func operandPrefixes(t *testing.T, operand any) []string {
	t.Helper()
	if s, ok := operand.(string); ok {
		return []string{s}
	}
	return operandStrings(t, operand)
}

func operandFloats(t *testing.T, operand any) []float64 {
	t.Helper()
	items, ok := operand.([]any)
	require.True(t, ok, "a collection fragment compares a collection; got %T", operand)
	out := make([]float64, 0, len(items))
	for _, item := range items {
		out = append(out, operandFloat(t, item))
	}
	return out
}

func operandFloat(t *testing.T, operand any) float64 {
	t.Helper()
	switch v := operand.(type) {
	case int64:
		return float64(v)
	case float64:
		return v
	}
	t.Fatalf("a ::numeric fragment compares numbers; got %T", operand)
	return 0
}

func collectionHasText(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// The push-down must emit the exact fragment recorded for each case.
func TestPresentPayloadField_SQLPushdownSemantics(t *testing.T) {
	for _, tc := range presentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compilePayloadComparison([]string{"ownerUserId"}, tc.op, tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.wantSQL, compiled.sql, tc.why)
		})
	}
}

// The invariant that protects correctness: a combined-filter query scans in
// SQL and then re-filters in process, so if the two paths disagree about a
// present value the rows you get depend on which path ran.
func TestPresentPayloadField_SQLAndPostFilterAgree(t *testing.T) {
	for _, tc := range presentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			node := presentFieldNode(t, tc.payloadJSON)

			post, err := nodeMatchesComparison(node, presentFieldComparison(tc.op, tc.value), map[string]map[string]any{})
			require.NoError(t, err)

			compiled, err := compilePayloadComparison([]string{"ownerUserId"}, tc.op, tc.value)
			require.NoError(t, err)
			sqlMatch := sqlMatchesPresentRow(t, compiled.sql, presentJSONType(t, tc.payloadJSON), tc.extracted, tc.value)

			require.Equal(t, sqlMatch, post,
				"SQL push-down and in-process post-filter disagree about %s %v %v; a combined-filter "+
					"query would return different rows depending on which path ran. SQL: %s",
				tc.payloadJSON, tc.op, tc.value, compiled.sql)
			require.Equal(t, tc.wantMatch, post, tc.why)
		})
	}
}

// Postgres-gated: the hand-written `extracted` values and the model above are
// this test file's only claim about the database, and both are checked here
// against a real server. Skips when no Postgres is reachable.
//
// The harness evaluates the emitted fragment directly over a synthetic payload
// rather than seeding rows: the fragment is the whole subject, and `IS TRUE`
// reproduces exactly what a WHERE clause does with a NULL result. The two
// malformed cases are the ones this used to answer with an ERROR.
func TestPresentPayloadField_SQLSemanticsMatchPostgres(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "present-field SQL/post-filter parity", dsn, err)
	}

	for _, tc := range presentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dbSkip != "" {
				t.Skipf("not evaluated against Postgres: %s", tc.dbSkip)
			}
			compiled, err := compilePayloadComparison([]string{"ownerUserId"}, tc.op, tc.value)
			require.NoError(t, err)

			// The extracted text first, so the hand-written fixture is
			// checked rather than trusted.
			var extracted string
			require.NoError(t,
				db.NewRaw(`SELECT CAST(? AS jsonb) #>> '{ownerUserId}'`, tc.payloadJSON).Scan(ctx, &extracted),
				"extract %s", tc.payloadJSON)
			require.Equal(t, tc.extracted, extracted,
				"the hand-written extraction for %s is wrong, which would invalidate the model above", tc.payloadJSON)

			// Then the fragment itself. Argument order follows the text:
			// the fragment's placeholders precede the payload's.
			args := append(append([]any{}, compiled.args...), tc.payloadJSON)
			var matched bool
			require.NoError(t,
				db.NewRaw(`SELECT (`+compiled.sql+`) IS TRUE FROM (SELECT CAST(? AS jsonb) AS payload) AS t`, args...).
					Scan(ctx, &matched),
				"evaluate %s", compiled.sql)

			require.Equal(t, tc.wantMatch, matched,
				"Postgres disagrees with the recorded expectation for %s %v %v. SQL: %s",
				tc.payloadJSON, tc.op, tc.value, compiled.sql)
		})
	}
}

// The intrinsic half of memql#3628. nodeMatchesComparison used to
// strings.TrimSpace BOTH sides of every intrinsic string comparison, while the
// compile functions normalise only the bound parameter and leave the column
// alone. Each case below pins the post-filter to its own compile counterpart.
func TestIntrinsicFields_PostFilterMatchesTheCompiledSQL(t *testing.T) {
	node := memorynodes.MemoryNode{
		ID:         "v1:agents:agent:test-id",
		Concept:    "v1:agents:agent",
		Type:       "agent",
		CreatedBy:  " system ",
		Payload:    json.RawMessage(`{"active":true}`),
		Provenance: json.RawMessage(`{"kind":"direct","name":"seed"}`),
	}

	for _, tc := range []struct {
		name      string
		field     []string
		op        ComparisonOperator
		value     any
		wantMatch bool
		why       string
	}{
		{
			name:      "a stored createdBy carrying whitespace does not match the trimmed literal",
			field:     []string{"createdBy"},
			op:        OpEq,
			value:     "system",
			wantMatch: false,
			why:       `compileCreatedByComparison binds "createdBy" = 'system' and the column holds ' system '`,
		},
		{
			name:      "the createdBy LITERAL is still trimmed, because the compile path trims it",
			field:     []string{"createdBy"},
			op:        OpEq,
			value:     " system ",
			wantMatch: false,
			why:       "compileCreatedByComparison trims the parameter to 'system', which the stored ' system ' is not",
		},
		{
			name:      "the id literal is trimmed, matching compileIdComparison",
			field:     []string{"id"},
			op:        OpEq,
			value:     "  v1:agents:agent:test-id  ",
			wantMatch: true,
			why:       "compileIdComparison trims the parameter before binding id = ?",
		},
		{
			name:      "the type literal is lowercased AND trimmed, matching compileTypeComparison",
			field:     []string{"type"},
			op:        OpEq,
			value:     " AGENT ",
			wantMatch: true,
			why:       "compileTypeComparison binds strings.ToLower(strings.TrimSpace(v)); the post-filter used to compare the literal verbatim",
		},
		{
			name:      "the concept literal is trimmed, matching compileConceptComparison",
			field:     []string{"concept"},
			op:        OpEq,
			value:     " v1:agents:agent ",
			wantMatch: true,
			why:       "compileConceptComparison trims the parameter before binding concept = ?",
		},
		{
			name:      "a provenance literal is NOT trimmed, because compileProvenanceComparison does not trim it",
			field:     []string{"provenance", "name"},
			op:        OpEq,
			value:     " seed ",
			wantMatch: false,
			why:       "compileProvenanceComparison binds the string untouched; the post-filter used to trim it and match",
		},
		{
			name:      "the same provenance leaf matches verbatim",
			field:     []string{"provenance", "name"},
			op:        OpEq,
			value:     "seed",
			wantMatch: true,
			why:       "the leaf is comparable, just verbatim",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmp := &ComparisonExpression{
				Field:    FieldReference{Parts: tc.field, Raw: strings.Join(tc.field, ".")},
				Operator: tc.op,
				Value:    tc.value,
			}
			match, err := nodeMatchesComparison(node, cmp, map[string]map[string]any{})
			require.NoError(t, err)
			require.Equal(t, tc.wantMatch, match, tc.why)
		})
	}
}

// The residual this change deliberately does NOT close, recorded as an
// executable claim so the next reader finds it as a known boundary rather than
// as a fresh discovery.
//
// compileIdComparison runs resolveFullId, which expands a BARE shortId to
// `{concept}:{shortId}` using the query's concept context. nodeMatchesComparison
// has no concept context to expand with, so a bare-id filter matches in SQL and
// misses in process. Fail-closed like the rest of memql#3628's class, but the
// fix is to thread conceptContext through nodeMatches -> nodeMatchesComparison,
// not to change a comparison rule -- which is why it is not in this change.
func TestIntrinsicId_BareShortIdStillDivergesFromTheSQLPath(t *testing.T) {
	node := memorynodes.MemoryNode{
		ID:      "v1:agents:agent:test-id",
		Concept: "v1:agents:agent",
	}

	compiled, err := compileIdComparison(OpEq, "test-id", "v1:agents:agent")
	require.NoError(t, err)
	require.Equal(t, []any{"v1:agents:agent:test-id"}, compiled.args,
		"the push-down expands a bare shortId against the concept context")

	cmp := &ComparisonExpression{
		Field:    FieldReference{Parts: []string{"id"}, Raw: "id"},
		Operator: OpEq,
		Value:    "test-id",
	}
	match, err := nodeMatchesComparison(node, cmp, map[string]map[string]any{})
	require.NoError(t, err)
	require.False(t, match,
		"documented residual: the post-filter cannot expand a bare shortId, so it drops a row the SQL scan "+
			"returned. Fail-closed. Closing it means threading conceptContext into the post-filter evaluator.")
}
