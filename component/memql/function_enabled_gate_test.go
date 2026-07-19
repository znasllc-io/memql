package memql

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// #2605: @disabled must gate query EXECUTION, not just discovery. Mutations
// and logic hard-fail on a disabled function (engine.go), but query expansion
// historically never consulted fn.Enabled -- a @disabled query was hidden
// from functions() and MCP yet still expanded and executed when called
// directly. Zero @disabled queries ship, so the gate lands breaking nobody.
func TestExpandFunctionCall_DisabledQueryRejected(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "retiredReport",
		FunctionKind: "query",
		Enabled:      false,
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:cognition:space",
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{Name: "retiredReport", Args: map[string]any{}},
	}
	err := resolvePlanFunctions(plan, reg, nil)
	require.Error(t, err, "a @disabled query must not expand")
	require.Contains(t, err.Error(), `function "retiredReport" is disabled`,
		"gate wording must match the mutation/logic gates in engine.go")
}

// The gate must also hold when the disabled query is reached through the
// body of an enabled one (nested expansion), not only at the plan root.
func TestExpandFunctionCall_DisabledQueryRejectedNested(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "retiredInner",
		FunctionKind: "query",
		Enabled:      false,
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:cognition:space",
		},
	}))
	require.NoError(t, reg.add(&Function{
		Name:         "outerCaller",
		FunctionKind: "query",
		Enabled:      true,
		Expr:         &FunctionCallExpression{Name: "retiredInner", Args: map[string]any{}},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{Name: "outerCaller", Args: map[string]any{}},
	}
	err := resolvePlanFunctions(plan, reg, nil)
	require.Error(t, err, "a disabled query reached via nested expansion must not expand")
	require.Contains(t, err.Error(), `function "retiredInner" is disabled`)
}

// #2608 made the builtin Enabled flag honest and removed the #2605 gate
// exemption: a @disabled builtin now rejects at expansion exactly like a
// disabled query, and an enabled one reaches the dispatch branch.
func TestExpandFunctionCall_BuiltinLifecycleGated(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:     "retiredHelper",
		Type:     FunctionTypeBuiltin,
		Enabled:  false, // honest @disabled post-#2608
		Executor: "integration.meta.retired",
	}))
	require.NoError(t, reg.add(&Function{
		Name:     "help",
		Type:     FunctionTypeBuiltin,
		Enabled:  true,
		Executor: "integration.meta.help",
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{Name: "retiredHelper", Args: map[string]any{}},
	}
	err := resolvePlanFunctions(plan, reg, nil)
	require.Error(t, err, "a @disabled builtin must not expand post-#2608")
	require.Contains(t, err.Error(), `function "retiredHelper" is disabled`)

	plan = &QueryPlan{
		Root: &FunctionCallExpression{Name: "help", Args: map[string]any{}},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))
	_, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected the builtin dispatch branch, got %T", plan.Root)
}

// Post-#2608 the flag is honest and the sweep is scoped to ENABLED
// builtins (a legitimately @disabled one is SUPPOSED to reject): every
// enabled builtin in the real boot registry must clear the gate. This is
// the pin that catches a Type-discriminator or loader drift that would
// mass-reject builtins in production while unit fixtures stay green --
// exactly how the first cut of #2605 shipped green and broke
// mcp-conformance. Arg-validation errors are tolerated; only the gate's
// error fails the test.
func TestExpandFunctionCall_EnabledBuiltinsClearTheGate(t *testing.T) {
	registry := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	builtins := 0
	for name, fn := range registry.Snapshot() {
		if fn == nil || fn.Type != FunctionTypeBuiltin || !fn.Enabled {
			continue
		}
		builtins++
		plan := &QueryPlan{Root: &FunctionCallExpression{Name: name, Args: map[string]any{}}}
		if err := resolvePlanFunctions(plan, registry, nil); err != nil && strings.Contains(err.Error(), "is disabled") {
			t.Errorf("builtin %q rejected by the enabled gate: %v", name, err)
		}
	}
	if builtins < 50 {
		t.Fatalf("expected the embedded tree to register its builtins (74 at the time of writing, guarded at 50), got %d -- loader path changed?", builtins)
	}
}

// Parity guard: an enabled query keeps expanding exactly as before.
func TestExpandFunctionCall_EnabledQueryUnaffected(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "activeReport",
		FunctionKind: "query",
		Enabled:      true,
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:cognition:space",
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{Name: "activeReport", Args: map[string]any{}},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))
	cmp, ok := plan.Root.(*ComparisonExpression)
	require.True(t, ok, "expected ComparisonExpression after expansion, got %T", plan.Root)
	require.Equal(t, "v1:cognition:space", cmp.Value)
}
