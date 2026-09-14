package steps

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// exprLeaf is a compiled value leaf holding the expression src, parsed.
func exprLeaf(t *testing.T, src string) *automations.ExprLeaf {
	t.Helper()
	node, err := langparser.ParseV1Expression(src)
	require.NoErrorf(t, err, "parse %s", src)
	return &automations.ExprLeaf{Src: src, Node: node}
}

// A date call in a function argument resolves to its value before the call is
// rendered -- never to its source text, which a date comparison downstream
// would read as a string greater than every ISO timestamp.
func TestFunctionDateArgumentsResolveBeforeRendering(t *testing.T) {
	evaluator := automations.NewEvaluator()
	evaluator.SetCustom("timestamp", "2026-09-09T06:00:00Z")
	evaluator.SetCustom("args", map[string]any{})
	for _, tc := range []struct {
		name, expr string
		want       any
	}{
		{"prune default", `addDuration(now, "PT-" + (args.window ?? "30") + "M")`, "2026-09-09T05:30:00Z"},
		{"days between", `daysBetween("2026-09-01", "2026-09-03")`, "2"},
		{"nested date", `"cutoff=" + addDuration(now, "-PT30M")`, "cutoff=2026-09-09T05:30:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluator.ResolveV1Map(context.Background(), map[string]any{"x": exprLeaf(t, tc.expr)})
			require.NoError(t, err)
			require.Equal(t, tc.want, fmt.Sprint(got["x"]))
		})
	}
}

// An invalid date in a function argument fails the step before dispatch: an
// invalid optional cutoff must not become nil (removing its guard and turning
// a sweep into a full scan) or source text.
func TestFunctionInvalidDateArgumentFailsBeforeDispatch(t *testing.T) {
	evaluator := automations.NewEvaluator()
	for _, expression := range []string{
		`addDuration("invalid-date", "-PT30M")`,
		`"" + addDuration("invalid-date", "-PT30M")`,
		`[addDuration("invalid-date", "-PT30M")]`,
	} {
		t.Run(expression, func(t *testing.T) {
			args := map[string]any{"olderThan": exprLeaf(t, expression)}
			_, err := evaluator.ResolveV1Map(context.Background(), args)
			require.Error(t, err, "an invalid optional cutoff must not become nil or source text")
			result, err := (&FunctionExecutor{}).Execute(context.Background(), &automations.Step{
				ID: "stale", Function: &automations.FunctionStepConfig{Name: "staleClusterNodes", Args: args},
			}, &Context{Engine: &memql.MemQLEngine{}, Evaluator: evaluator})
			require.Error(t, err)
			require.Contains(t, err.Error(), "argument")
			require.Equal(t, "failed", result.Status)
		})
	}
}
