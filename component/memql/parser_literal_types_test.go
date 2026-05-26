package memql

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseLiteralTypes_BothPathsEquivalent is the #255 acceptance
// test: for every literal category (bool / int / float / string /
// null / identifier / array / map), the memql parser path and the
// opt-in langparser path produce reflect.DeepEqual-equal Value
// fields on the resulting *ComparisonExpression.
//
// The existing TestParseLiteralTypes (parser_test.go) covers the
// memql path's expected types in isolation; this test locks the
// cross-parser equivalence the #249 default-flip needs. Without it,
// a future change to either parser's literal construction (e.g. an
// integer literal silently becoming float64) wouldn't surface until
// the flip itself.
//
// Test design notes:
//
//   - Each row's `want` field is the EXACT Go type+value the BOTH
//     paths must produce. int64 for bare integers (mirroring the
//     memql parser's parseNumberLiteral + the langparser's
//     parseAttribute bare-numeric path); float64 for decimals;
//     reflect.DeepEqual handles slices / maps elementwise.
//
//   - Array and map cases use mixed element types to exercise the
//     recursive parseValue/parseFunctionArgValue paths (a regression
//     where parseValue alone gets fixed but parseArray's element
//     dispatch still emits float64 would show here).
//
//   - The langparser path is reached via parseViaLangparser even
//     though the engine default is still OFF (#249 / #258); the
//     test reads the langparser AST directly so it doesn't depend
//     on the runtime flag state.
//
//   - Bare identifier `pending` (no quotes) is handled differently
//     by the two parsers: the memql parser's parseLiteralValue
//     returns the string "pending"; the langparser's parsePrimary
//     emits an ArgRefExpr / function reference for bare
//     identifiers, which the converter resolves to an ArgReference
//     -- NOT a literal. That divergence is a pre-existing
//     identifier-classification issue, tracked separately under
//     epic #218; this test focuses on the literal categories the
//     #255 issue body enumerated (integer / float / string / bool /
//     null / array / map) and skips the bare-identifier case here.
func TestParseLiteralTypes_BothPathsEquivalent(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  any
	}{
		{"bool true", `payload.active==true`, true},
		{"bool false", `payload.active==false`, false},
		{"bare integer", `payload.count==42`, int64(42)},
		{"zero integer", `payload.count==0`, int64(0)},
		{"decimal float", `payload.ratio==3.14`, 3.14},
		{"quoted string", `payload.status=="pending"`, "pending"},
		{"empty string", `payload.note==""`, ""},
		{"null", `payload.note==null`, nil},
		{"int list", `payload.tags in [1, 2, 3]`, []any{int64(1), int64(2), int64(3)}},
		{"float list", `payload.tags in [1.5, 2.5]`, []any{1.5, 2.5}},
		{"string list", `payload.tags in ["a", "b"]`, []any{"a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// memql path
			memqlPlan := mustParse(t, tc.query)
			memqlComp := assertComparison(t, memqlPlan.Root)
			require.True(t, reflect.DeepEqual(tc.want, memqlComp.Value),
				"memql path: want %#v (%T), got %#v (%T)",
				tc.want, tc.want, memqlComp.Value, memqlComp.Value)

			// langparser path -- read the AST directly so the test
			// doesn't depend on whether the engine default is
			// flipped (still OFF in #258).
			langNode, err := parseViaLangparser(tc.query)
			require.NoError(t, err, "langparser path: parse failed")
			langComp := assertComparison(t, langNode)
			require.True(t, reflect.DeepEqual(tc.want, langComp.Value),
				"langparser path: want %#v (%T), got %#v (%T)",
				tc.want, tc.want, langComp.Value, langComp.Value)

			// Cross-parser equivalence -- the actual #255 acceptance.
			require.True(t, reflect.DeepEqual(memqlComp.Value, langComp.Value),
				"cross-parser divergence on %q: memql=%#v (%T) vs langparser=%#v (%T)",
				tc.query, memqlComp.Value, memqlComp.Value, langComp.Value, langComp.Value)
		})
	}
}

// TestParseLiteralTypes_NumericLiteralHelper locks the contract on
// the langparser-side parseNumericLiteral helper introduced for
// #255: integer-shaped strings produce int64, decimal/scientific
// strings produce float64. Single source of truth for every
// TokenNumber emitted by the langparser; if this drifts, the
// equivalence test above goes red too, but pinning the contract
// here points at the specific function rather than a parser path.
//
// Lives in component/memql/ because that's where the cross-parser
// equivalence story is told end-to-end (the helper itself is
// langparser-package-private; the assertion is the engine-facing
// behavior).
func TestParseLiteralTypes_NumericLiteralHelper(t *testing.T) {
	cases := []struct {
		query    string
		wantType string
		wantVal  any
	}{
		{`payload.count==0`, "int64", int64(0)},
		{`payload.count==1`, "int64", int64(1)},
		{`payload.count==9223372036854775807`, "int64", int64(9223372036854775807)},
		{`payload.ratio==1.0`, "float64", 1.0},
		{`payload.ratio==0.0`, "float64", 0.0},
		{`payload.ratio==1e3`, "float64", 1000.0},
		{`payload.ratio==2.5e-3`, "float64", 0.0025},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			node, err := parseViaLangparser(tc.query)
			require.NoError(t, err)
			comp := assertComparison(t, node)
			require.Equalf(t, tc.wantType, reflectTypeName(comp.Value),
				"want %s, got %T (value=%v)", tc.wantType, comp.Value, comp.Value)
			require.Equal(t, tc.wantVal, comp.Value)
		})
	}
}

func reflectTypeName(v any) string {
	if v == nil {
		return "nil"
	}
	return reflect.TypeOf(v).String()
}
