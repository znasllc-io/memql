package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestIncludesFoldSearchIsLiteralAndAgreesWithExpressionEvaluation(t *testing.T) {
	for _, tc := range []struct {
		title  any
		needle string
		want   bool
	}{
		{"Release ACME", "acme", true},
		{"Crème BRÛLÉE", "brûlée", true},
		{"Release 50%_off", "%_OFF", true},
		{"Release 500off", "%_off", false},
		{"Bob's machine", "BOB'S", true},
		{"any", "", false},
		{"any", "  ", false},
		{nil, "a", false},
		{123, "123", false},
	} {
		call := `row => row.title.includes(` + langparser.QuoteString(tc.needle) + `, true)`
		lam, err := langparser.ParseV1Lambda(call)
		require.NoError(t, err)
		env := lowerTestEnv(t)
		env.Param = "row"
		ir, err := Lower(lam.Body, env)
		require.NoError(t, err, call)
		actual, err := EvalExpr(context.Background(), lam.Body, MapScope{"row": map[string]any{"title": tc.title}}, EvalOptions{})
		require.NoError(t, err, call)
		require.Equal(t, tc.want, actual, "%s over %v", call, tc.title)
		compiled := irCompile(t, ir)
		if tc.needle == "" || tc.needle == "  " {
			require.Equal(t, "FALSE", compiled.sql)
		} else {
			require.Contains(t, compiled.sql, "strpos(lower(payload #>> '{title}'), lower(?)) > 0")
			require.Equal(t, []any{tc.needle}, compiled.args)
			match, err := compareScalarValues(tc.title, OpIncludesFold, tc.needle)
			require.NoError(t, err)
			require.Equal(t, tc.want, match)
		}
	}
}

func TestIncludesFoldOptionPreservesExistingCallsAndRejectsInvalidFlags(t *testing.T) {
	for _, suffix := range []string{"", ", false"} {
		ir, err := lowerSource(t, lowerTestEnv(t), `row => row.title.includes("Disk"`+suffix+`)`)
		require.NoError(t, err)
		require.Equal(t, OpIncludes, ir.(*ComparisonExpression).Operator)
		for _, tc := range []struct {
			title  any
			needle string
			want   bool
		}{
			{"Disk", "Disk", true},
			{"Disk", "disk", false},
			{"Crème BRÛLÉE", "brûlée", false},
			{"Release 50%_off", "%_off", true},
			{"Release 500off", "%_off", false},
			{"any", "", false},
			{"any", "  ", false},
			{nil, "a", false},
			{123, "123", false},
		} {
			call := `row => row.title.includes(` + langparser.QuoteString(tc.needle) + suffix + `)`
			lambda, err := langparser.ParseV1Lambda(call)
			require.NoError(t, err)
			actual, err := EvalExpr(context.Background(), lambda.Body, MapScope{"row": map[string]any{"title": tc.title}}, EvalOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.want, actual, "%s over %v", call, tc.title)
		}
	}
	for _, flag := range []string{`"true"`, `1`, `args.flag`} {
		_, err := lowerSource(t, lowerTestEnv(t), `row => row.title.includes("disk", `+flag+`)`)
		require.ErrorContains(t, err, "ignoreCase")
	}
	_, err := EvalExpr(context.Background(), xmeth(xlit("Disk"), "includes", xlit("disk"), xlit("true")), MapScope{}, EvalOptions{})
	var exprErr *ExprError
	require.ErrorAs(t, err, &exprErr)
	require.Contains(t, err.Error(), "ignoreCase must be a boolean")
}
