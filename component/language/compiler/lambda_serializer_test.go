package compiler

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// lambda_serializer_test.go pins the LambdaExpr serializer case (#2542): a
// lambda-carrying collection chain in a logic terminal-return position reaches
// the per-node expressionToString reconstruction path, where the missing case
// emitted `<<unsupported expression *ast.LambdaExpr>>` and the automation failed
// at runtime even though memqllint passed. The emitted arrow form must
// re-parse to an equivalent chain.
func TestExpressionToString_LambdaArrowForm(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "single_param_field_body",
			src:  `rows.where(m => m.active).count()`,
			want: `rows.where(m => m.active).count()`,
		},
		{
			name: "single_param_comparison_body",
			src:  `rows.where(m => m.role == "admin").count()`,
			want: `rows.where(m => m.role=="admin").count()`,
		},
		{
			name: "multi_param_reduce",
			src:  `nums.reduce(0, (acc, n) => acc + n)`,
			want: `nums.reduce(0, (acc, n) => (acc + n))`,
		},
		{
			name: "groupby_select_projection_arithmetic",
			src:  `rows.groupBy(g => g.w).select(g => {worker: g.key, pct: g.a.count() * 100 / g.b.count()})`,
			// key order in the emitted object literal is sorted (valueToString).
			want: `rows.groupBy(g => g.w).select(g => {pct: ((g.a.count() * 100) / g.b.count()), worker: g.key})`,
		},
	}
	c := New(Config{})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			expr, err := parser.ParseExpression(tc.src)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.src, err)
			}
			got := c.expressionToString(expr)
			if strings.Contains(got, "<<unsupported") {
				t.Fatalf("serialized %q to an unsupported-expression placeholder: %q", tc.src, got)
			}
			if got != tc.want {
				t.Errorf("expressionToString(%q) = %q, want %q", tc.src, got, tc.want)
			}
			// Round-trip: the emitted form must re-parse cleanly.
			if _, err := parser.ParseExpression(got); err != nil {
				t.Errorf("re-parse of emitted %q failed: %v", got, err)
			}
		})
	}
}
