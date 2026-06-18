package parser

import (
	"testing"
)

// TestNullLiteralComparisonLowersToMissing locks in #1631: a comparison
// against the `null` identifier (which parseValue lowers to a plain Go nil,
// NOT the string "null") must lower `==` / `!=` to OpMissing / OpNotMissing
// -- the same as the `nil` keyword. Before the fix the `null` identifier
// slipped past the conversion (which only matched *NilExpr and the literal
// string "null"), leaving an OpNe with a bound nil value that the executor's
// literal normalizer rejected with "unsupported literal type <nil>"
// (queryDueRefreshDomains's `payload.refreshCadenceDays != null`).
func TestNullLiteralComparisonLowersToMissing(t *testing.T) {
	cases := []struct {
		src    string
		wantOp ComparisonOperator
	}{
		// `null` identifier form (the one that regressed).
		{`payload.refreshCadenceDays != null`, OpNotMissing},
		{`payload.refreshCadenceDays == null`, OpMissing},
		// `nil` keyword form (must keep working).
		{`payload.refreshCadenceDays != nil`, OpNotMissing},
		{`payload.refreshCadenceDays == nil`, OpMissing},
	}

	for _, tc := range cases {
		expr := parseWhenExpr(t, tc.src)
		cmp, ok := expr.(*ComparisonExpr)
		if !ok {
			t.Fatalf("%q: expected *ComparisonExpr, got %T", tc.src, expr)
		}
		if cmp.Operator != tc.wantOp {
			t.Fatalf("%q: expected operator %v, got %v", tc.src, tc.wantOp, cmp.Operator)
		}
		if cmp.Value != nil {
			t.Fatalf("%q: expected nil value after lowering, got %#v", tc.src, cmp.Value)
		}
	}
}
