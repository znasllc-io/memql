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

// memql#3628: the sibling of absent_field_comparison_test.go, for a field that
// is PRESENT but not stored as the type the predicate's literal is.
//
// Same invariant, same reason. executeCombinedFilterQuery scans in SQL and
// then re-evaluates the whole expression tree in process on every candidate,
// so BOTH paths decide the row set. memql#2783 pinned their agreement for an
// ABSENT field and stopped there; for a present, differently-typed value the
// two used to disagree in five distinct ways:
//
//	payload                        predicate    SQL     post-filter (before)
//	{"ownerUserId":" u-1 "}        == "u-1"     false   TRUE
//	{"ownerUserId":5}              == "5"       true    FALSE
//	{"ownerUserId":"5"}            == 5         true    FALSE
//	{"ownerUserId":"true"}         == true      true    FALSE
//	{"ownerUserId":"true"}         != true      false   TRUE
//
// The divergence was always fail-CLOSED -- the paths compose as an
// intersection -- so this was never an authorization hole. Its symptom was a
// query silently returning nothing for data stored slightly off-type, with no
// error to explain why.
//
// "Intersection" undersells why the agreement matters, though. latestMatchingNodes
// RELOADS each scanned id to its true latest version and re-evaluates the
// predicate on THAT row -- which the SQL scan may never have examined, since
// the scan returned the newest version matching the filter rather than the
// newest version. So the post-filter is a second, independent decision over
// possibly-different bytes. If its comparison rules differ from the
// push-down's, "the latest row satisfies your predicate" quietly becomes "the
// latest row satisfies a DIFFERENT predicate than the one you wrote".
//
// THE DIRECTION IS DELIBERATE: the post-filter was changed to match the
// DATABASE, not the other way round. The push-down is the primary filter and
// it runs against the stored bytes. Changing the SQL instead would have taken
// rows AWAY from queries that work today.
//
// What the database actually does, and therefore what payloadText /
// payloadNumeric / payloadBool now model: extract the stored value as TEXT via
// `#>>`, then cast that text by the LITERAL's type. So the text comparison is
// verbatim (nothing is trimmed), while the numeric and boolean casts DO ignore
// surrounding whitespace, because numeric_in and boolin do. Trimming is not
// banned; it belongs to the cast rather than to the comparison, which is
// exactly the distinction the old `toString` collapsed.

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
	// through pq.Array / bun.In, which the one-off `SELECT <fragment>`
	// harness below does not reproduce faithfully enough to be evidence.
	dbSkip string

	why string
}

func presentFieldCases() []presentFieldCase {
	return []presentFieldCase{
		// ---- the trimming asymmetry -------------------------------------
		{
			name:        "stored whitespace is NOT trimmed away before an == compare",
			payloadJSON: `{"ownerUserId":" u-1 "}`,
			extracted:   " u-1 ",
			op:          OpEq,
			value:       "u-1",
			wantSQL:     `(payload #>> '{ownerUserId}' = ?)`,
			wantMatch:   false,
			why:         "no `=` in Postgres trims its operands; the old toString ran TrimSpace over the STORED value and matched",
		},
		{
			name:        "stored whitespace matches a literal carrying the same whitespace",
			payloadJSON: `{"ownerUserId":" u-1 "}`,
			extracted:   " u-1 ",
			op:          OpEq,
			value:       " u-1 ",
			wantSQL:     `(payload #>> '{ownerUserId}' = ?)`,
			wantMatch:   true,
			why:         "the value is comparable, just verbatim -- this is the other half of not trimming",
		},
		{
			name:        "!= against stored whitespace is the mirror of the == case",
			payloadJSON: `{"ownerUserId":" u-1 "}`,
			extracted:   " u-1 ",
			op:          OpNe,
			value:       "u-1",
			wantSQL:     `(payload #>> '{ownerUserId}' IS DISTINCT FROM ?)`,
			wantMatch:   true,
			why:         "IS DISTINCT FROM on two non-null texts is plain inequality; ' u-1 ' is not 'u-1'",
		},

		// ---- stored number vs string literal ------------------------------
		{
			name:        "a stored NUMBER compares equal to the matching string literal",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpEq,
			value:       "5",
			wantSQL:     `(payload #>> '{ownerUserId}' = ?)`,
			wantMatch:   true,
			why:         "#>> extracts JSON as TEXT, so the stored 5 is the text '5'; the old toString refused every non-string outright",
		},
		{
			name:        "a stored NUMBER is not != the matching string literal",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpNe,
			value:       "5",
			wantSQL:     `(payload #>> '{ownerUserId}' IS DISTINCT FROM ?)`,
			wantMatch:   false,
			why:         "the != direction of the same coercion; the old path returned true and handed back a row SQL excluded",
		},

		// ---- stored string vs number literal ------------------------------
		{
			name:        "a stored STRING of digits compares equal to a number literal",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpEq,
			value:       int64(5),
			wantSQL:     `((payload #>> '{ownerUserId}')::numeric = ?)`,
			wantMatch:   true,
			why:         "a number literal makes the push-down cast the extracted text to numeric, and '5'::numeric is 5",
		},
		{
			name:        "the numeric cast ignores surrounding whitespace",
			payloadJSON: `{"ownerUserId":" 5 "}`,
			extracted:   " 5 ",
			op:          OpEq,
			value:       int64(5),
			wantSQL:     `((payload #>> '{ownerUserId}')::numeric = ?)`,
			wantMatch:   true,
			why:         "numeric_in skips leading/trailing whitespace -- trimming belongs to the CAST, not to the comparison",
		},
		{
			name:        "ordered comparison casts a stored string of digits too",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpGt,
			value:       int64(4),
			wantSQL:     `((payload #>> '{ownerUserId}')::numeric > ?)`,
			wantMatch:   true,
			why:         "the cast is selected by the literal's type for every ordered operator, not just ==",
		},
		{
			name:        "ordered comparison against a string literal stays lexicographic over the extracted text",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpGt,
			value:       "4",
			wantSQL:     `(payload #>> '{ownerUserId}' > ?)`,
			wantMatch:   true,
			why:         "a string literal means no cast at all, so the stored number is compared as its text",
		},

		// ---- stored string vs boolean literal -----------------------------
		{
			name:        `a stored STRING "true" compares equal to a boolean literal`,
			payloadJSON: `{"ownerUserId":"true"}`,
			extracted:   "true",
			op:          OpEq,
			value:       true,
			wantSQL:     `((payload #>> '{ownerUserId}')::boolean = ?)`,
			wantMatch:   true,
			why:         "a boolean literal makes the push-down cast to boolean, and 'true'::boolean is true",
		},
		{
			name:        `a stored STRING "true" is not != a boolean literal`,
			payloadJSON: `{"ownerUserId":"true"}`,
			extracted:   "true",
			op:          OpNe,
			value:       true,
			wantSQL:     `((payload #>> '{ownerUserId}')::boolean IS DISTINCT FROM ?)`,
			wantMatch:   false,
			why:         "the fifth row of the issue's matrix: the old path returned true and included a row SQL excluded",
		},
		{
			name:        `Postgres boolean input accepts "on", so the post-filter must too`,
			payloadJSON: `{"ownerUserId":"on"}`,
			extracted:   "on",
			op:          OpEq,
			value:       true,
			wantSQL:     `((payload #>> '{ownerUserId}')::boolean = ?)`,
			wantMatch:   true,
			why:         "boolin takes any unambiguous prefix of true/false/yes/no/on/off plus 1/0; a reader narrowed to 'true'/'false' would re-open the divergence for the rest",
		},
		{
			name:        "a stored BOOLEAN compares equal to the matching string literal",
			payloadJSON: `{"ownerUserId":true}`,
			extracted:   "true",
			op:          OpEq,
			value:       "true",
			wantSQL:     `(payload #>> '{ownerUserId}' = ?)`,
			wantMatch:   true,
			why:         "the reverse direction: #>> renders a stored boolean as the text 'true'",
		},

		// ---- the same coercions under `in` --------------------------------
		{
			name:        "a stored NUMBER is found in a string collection",
			payloadJSON: `{"ownerUserId":5}`,
			extracted:   "5",
			op:          OpIn,
			value:       []any{"5", "6"},
			wantSQL:     `((jsonb_typeof(payload->'ownerUserId') = 'array' AND jsonb_exists_any(payload->'ownerUserId', ?::text[])) OR (payload #>> '{ownerUserId}' IN (?)))`,
			wantMatch:   true,
			dbSkip:      "bound args go through pq.Array + bun.In",
			why:         "valueInCollection reads the same extracted text the scalar == path does",
		},
		{
			name:        "a stored STRING of digits is found in a number collection",
			payloadJSON: `{"ownerUserId":"5"}`,
			extracted:   "5",
			op:          OpIn,
			value:       []any{int64(5), int64(6)},
			wantSQL:     `((payload #>> '{ownerUserId}')::numeric IN (?))`,
			wantMatch:   true,
			dbSkip:      "bound args go through bun.In",
			why:         "a number collection casts to numeric exactly as a number literal does",
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

// sqlMatchesPresentRow computes whether an emitted fragment returns a row
// whose extraction is the given TEXT.
//
// It is an independent restatement of Postgres, written out here rather than
// borrowed from the production helpers, and it FAILS on any shape it does not
// recognise -- the same discipline sqlMatchesAbsentRow adopted after an
// inferring version turned the agreement check into a tautology.
func sqlMatchesPresentRow(t *testing.T, fragment, extracted string, operand any) bool {
	t.Helper()

	// Which cast the push-down chose is visible in the fragment, and the
	// cast decides what the operands ARE before any operator runs.
	switch {
	case strings.HasPrefix(fragment, "((jsonb_typeof("):
		// String-collection membership. The first disjunct only fires for
		// an array-valued field; every fixture here stores a scalar, so
		// jsonb_typeof is 'string'/'number'/'boolean' and that disjunct is
		// false. Guarded rather than assumed.
		require.False(t, strings.HasPrefix(strings.TrimSpace(extracted), "["),
			"an array-valued fixture would take the jsonb_exists_any disjunct; extend this model deliberately")
		items := operandStrings(t, operand)
		in := collectionHasText(items, extracted)
		if strings.Contains(fragment, "NOT jsonb_exists_any") {
			return !in
		}
		return in

	case strings.Contains(fragment, ")::numeric"):
		// numeric_in ignores surrounding whitespace and rejects anything
		// that is not a number (a Postgres ERROR, which this models as a
		// non-match -- see payloadNumeric's comment).
		n, err := strconv.ParseFloat(strings.TrimSpace(extracted), 64)
		if err != nil {
			return false
		}
		if strings.Contains(fragment, "IN (?)") {
			nums := operandFloats(t, operand)
			found := false
			for _, c := range nums {
				if c == n {
					found = true
					break
				}
			}
			if strings.Contains(fragment, "NOT IN (?)") {
				return !found
			}
			return found
		}
		return applyOrderedOperator(t, fragment, n, operandFloat(t, operand))

	case strings.Contains(fragment, ")::boolean"):
		// boolin: any unambiguous prefix of true/false/yes/no/on/off, plus
		// 1/0, case-insensitive, whitespace ignored. Spelled out only for
		// the spellings the fixtures use; anything else is a deliberate
		// extension rather than a guess.
		var b bool
		switch strings.ToLower(strings.TrimSpace(extracted)) {
		case "true", "t", "yes", "y", "on", "1":
			b = true
		case "false", "f", "no", "n", "off", "0":
			b = false
		default:
			t.Fatalf("extraction %q is not a boolean literal Postgres accepts; extend this model deliberately", extracted)
		}
		want, ok := operand.(bool)
		require.True(t, ok, "a ::boolean fragment compares booleans; got %T", operand)
		if strings.Contains(fragment, "IS DISTINCT FROM ?") {
			return b != want
		}
		require.Contains(t, fragment, "= ?", "unrecognised operator in boolean fragment %q", fragment)
		return b == want

	default:
		// No cast: a plain text comparison over the extracted value.
		if strings.Contains(fragment, "IN (?)") {
			items := operandStrings(t, operand)
			found := collectionHasText(items, extracted)
			if strings.Contains(fragment, "NOT IN (?)") {
				return !found
			}
			return found
		}
		want, ok := operand.(string)
		require.True(t, ok, "an uncast fragment compares text; got %T", operand)
		if strings.Contains(fragment, "IS DISTINCT FROM ?") {
			// Both sides are non-null here, so this is plain inequality.
			return extracted != want
		}
		return applyOrderedOperator(t, fragment, extracted, want)
	}
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
// present-but-differently-typed value the rows you get depend on which path
// ran. Every row of memql#3628's matrix fails this test against the old
// post-filter.
func TestPresentPayloadField_SQLAndPostFilterAgree(t *testing.T) {
	for _, tc := range presentFieldCases() {
		t.Run(tc.name, func(t *testing.T) {
			node := presentFieldNode(t, tc.payloadJSON)

			post, err := nodeMatchesComparison(node, presentFieldComparison(tc.op, tc.value), map[string]map[string]any{})
			require.NoError(t, err)

			compiled, err := compilePayloadComparison([]string{"ownerUserId"}, tc.op, tc.value)
			require.NoError(t, err)
			sqlMatch := sqlMatchesPresentRow(t, compiled.sql, tc.extracted, tc.value)

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
// reproduces exactly what a WHERE clause does with a NULL result.
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
