package memql

import (
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// executor_filter_startswith_test.go -- the engine half of the `startsWith`
// predicate (memql#4208): the SQL it compiles to, the in-process evaluation
// that must agree with it, and the two seams that would otherwise be silent.
//
// The SQL contract: `(<text path> ^@ ANY(?::text[]))`, one bound text[]
// parameter, never an inlined prefix. `^@` is Postgres starts_with() as an
// operator -- a plain byte-prefix test, so a LIKE metacharacter in a prefix is
// literal. ANY over an empty array is false, which is what makes "an empty
// list matches nothing" hold in the database as well as in Go.

func TestCompilePayloadComparison_StartsWith_String(t *testing.T) {
	result, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith, "integration.")
	require.NoError(t, err)
	require.Equal(t, "((payload #>> '{codeReference}') ^@ ANY(?::text[]))", result.sql)
	require.Len(t, result.args, 1)
	require.Equal(t, pq.Array([]string{"integration."}), result.args[0])
}

func TestCompilePayloadComparison_StartsWith_List(t *testing.T) {
	result, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith,
		[]any{"integration.email.", "integration.shopify."})
	require.NoError(t, err)
	require.Equal(t, "((payload #>> '{codeReference}') ^@ ANY(?::text[]))", result.sql)
	require.Len(t, result.args, 1)
	require.Equal(t, pq.Array([]string{"integration.email.", "integration.shopify."}), result.args[0])
}

// A typed []string (what a Go caller binds) is accepted alongside the []any
// the call-site parser produces.
func TestCompilePayloadComparison_StartsWith_TypedStringSlice(t *testing.T) {
	result, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith, []string{"a.", "b."})
	require.NoError(t, err)
	require.Equal(t, pq.Array([]string{"a.", "b."}), result.args[0])
}

// The guarantee the codeMetricsInWindow read leans on: an empty list is a
// predicate that admits nothing, not one that admits everything. It compiles
// to a constant FALSE rather than to `^@ ANY('{}')` so the intent is legible
// in the emitted SQL, and so no parameter is bound for it.
func TestCompilePayloadComparison_StartsWith_EmptyListMatchesNothing(t *testing.T) {
	for _, value := range []any{[]any{}, []string{}} {
		result, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith, value)
		require.NoError(t, err)
		require.Equal(t, "FALSE", result.sql)
		require.Empty(t, result.args)
	}
}

// A blank prefix is not a prefix. Every language's HasPrefix says "" matches
// everything, and that is exactly the fail-open a SELECTION must not have:
// `codeReference startsWith args.prefixes` with a caller-supplied [""] would
// otherwise read the whole table. Blanks are dropped; a list of nothing but
// blanks matches nothing.
func TestCompilePayloadComparison_StartsWith_BlankPrefixMatchesNothing(t *testing.T) {
	for _, value := range []any{"", "   ", []any{""}, []any{"", " "}} {
		result, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith, value)
		require.NoError(t, err, "%#v", value)
		require.Equal(t, "FALSE", result.sql, "%#v", value)
	}
	result, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith, []any{"", "integration."})
	require.NoError(t, err)
	require.Equal(t, pq.Array([]string{"integration."}), result.args[0],
		"a blank next to a real prefix is dropped, the real prefix survives")
}

func TestCompilePayloadComparison_StartsWith_NestedPath(t *testing.T) {
	result, err := compilePayloadComparison([]string{"source", "codeReference"}, OpStartsWith, "x")
	require.NoError(t, err)
	require.Equal(t, "((payload #>> '{source,codeReference}') ^@ ANY(?::text[]))", result.sql)
}

func TestCompilePayloadComparison_StartsWith_NeverInlinesThePrefix(t *testing.T) {
	result, err := compilePayloadComparison([]string{"name"}, OpStartsWith, []any{"O'Brien", "x%_y"})
	require.NoError(t, err)
	require.NotContains(t, result.sql, "O'Brien")
	require.NotContains(t, result.sql, "x%_y")
	require.NotContains(t, result.sql, "LIKE")
}

func TestCompilePayloadComparison_StartsWith_RejectsNonStrings(t *testing.T) {
	for _, value := range []any{int64(5), true, nil, []any{"a.", int64(5)}, map[string]any{"a": "b"}} {
		_, err := compilePayloadComparison([]string{"codeReference"}, OpStartsWith, value)
		require.Error(t, err, "%#v", value)
		require.Contains(t, err.Error(), "startsWith")
	}
}

// Row intrinsics are not in scope: `row.id startsWith` refuses at compile
// rather than silently compiling to something else.
func TestCompileIntrinsicComparison_StartsWith_Refused(t *testing.T) {
	_, err := compileIdComparison(OpStartsWith, "v1:x:", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not supported")
	_, err = compileCreatedByComparison(OpStartsWith, "u")
	require.Error(t, err)
	_, err = compileTypeComparison(OpStartsWith, "t")
	require.Error(t, err)
	_, err = compileProvenanceComparison("kind", OpStartsWith, "auto")
	require.Error(t, err)
	_, err = (&MemQLEngine{}).compileConceptComparison(OpStartsWith, "v1:")
	require.Error(t, err)
}

// The AST converter's operator switch defaults to OpEq for anything it does
// not know. That default is the one place a new operator could be accepted
// and silently turned into equality, so it is pinned here.
func TestConvertComparisonOperator_StartsWith(t *testing.T) {
	require.Equal(t, OpStartsWith, convertComparisonOperator(languageParser.OpStartsWith))
}

// The in-process evaluator runs on every candidate the SQL scan returns
// (executeCombinedFilterQuery post-filters the full tree), so it must agree
// with the SQL on every case above or the scan would find rows the
// post-filter then drops.
func TestCompareScalarValues_StartsWith(t *testing.T) {
	cases := []struct {
		name     string
		actual   any
		expected any
		want     bool
	}{
		{"string prefix hit", "integration.email.send", "integration.", true},
		{"string prefix miss", "method:github.com/x", "integration.", false},
		{"exact value is its own prefix", "integration.", "integration.", true},
		{"list any hit", "integration.shopify.sync", []any{"integration.email.", "integration.shopify."}, true},
		{"list all miss", "integration.shopify.sync", []any{"integration.email.", "method:"}, false},
		{"typed slice", "integration.shopify.sync", []string{"integration.shopify."}, true},
		{"empty list matches nothing", "integration.shopify.sync", []any{}, false},
		{"blank prefix matches nothing", "integration.shopify.sync", "", false},
		{"blank-only list matches nothing", "integration.shopify.sync", []any{"", " "}, false},
		{"blank beside a real prefix", "integration.shopify.sync", []any{"", "integration."}, true},
		{"metacharacters are literal, not wildcards", "integration.ax", "integration.%", false},
		{"literal metacharacter prefix matches its own text", "integration.%x", "integration.%", true},
		// `#>>` extracts the TEXT form of whatever the field holds, so SQL
		// prefix-tests a number by its digits; the in-process evaluator must
		// say the same or the post-filter drops rows the scan admitted.
		{"numeric field is tested by its text form, as #>> does", float64(42), "4", true},
		{"numeric field text form miss", float64(42), "5", false},
		{"absent field never matches", nil, "integration.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compareScalarValues(tc.actual, OpStartsWith, tc.expected)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	_, err := compareScalarValues("x", OpStartsWith, int64(5))
	require.Error(t, err, "a non-string prefix is a query error, not a non-match")
}

// Logic bodies and collection lambdas evaluate comparisons through their own
// resolvers; the predicate must mean the same thing there.
func TestEvalCollComparison_StartsWith(t *testing.T) {
	node := &ComparisonExpression{
		Field:    FieldReference{Raw: "m.ref", Parts: []string{"m", "ref"}},
		Operator: OpStartsWith,
		Value:    []any{"integration."},
	}
	locals := map[string]any{"m": map[string]any{"ref": "integration.email.send"}}
	got, err := evalCollComparison(node, nil, locals)
	require.NoError(t, err)
	require.Equal(t, true, got)

	locals["m"] = map[string]any{"ref": "method:x"}
	got, err = evalCollComparison(node, nil, locals)
	require.NoError(t, err)
	require.Equal(t, false, got)
}

func TestCompareValues_StartsWith(t *testing.T) {
	got, err := compareValues("integration.email.send", OpStartsWith, "integration.")
	require.NoError(t, err)
	require.True(t, got)
	got, err = compareValues("integration.email.send", OpStartsWith, []any{})
	require.NoError(t, err)
	require.False(t, got)
	got, err = compareValues(nil, OpStartsWith, "integration.")
	require.NoError(t, err)
	require.False(t, got)
}
