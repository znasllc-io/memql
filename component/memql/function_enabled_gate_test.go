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

// Builtins are EXEMPT from the gate until #2608: the converter never sets
// Enabled, so every builtin registers false regardless of @enabled. Gating
// them would reject every builtin call (the mcp-conformance lane caught
// exactly this: describeFunction -> help() -> "function \"help\" is
// disabled"). When #2608 makes the flag honest, this exemption and test
// are its to revisit.
func TestExpandFunctionCall_BuiltinExemptFromEnabledGate(t *testing.T) {
	reg := newFunctionRegistry()
	// The production builtin shape: Executor set, Expr nil (all 74 real
	// builtins). The exemption is deliberately narrowed to that shape -- a
	// builtin-typed function WITH a DSL body does not ride the exemption.
	require.NoError(t, reg.add(&Function{
		Name:     "help",
		Type:     FunctionTypeBuiltin,
		Enabled:  false, // the dead-flag reality for all builtins today
		Executor: "integration.meta.help",
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{Name: "help", Args: map[string]any{}},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil),
		"a builtin with the dead-false Enabled flag must keep expanding until #2608")
	_, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected the builtin dispatch branch, got %T", plan.Root)
}

// The exemption must hold against the REAL boot loader, not only a
// hand-registered fixture: if LoadUnifiedBuiltins stamped a different Type
// discriminator than the exemption checks, the gate would silently reject
// every builtin in production while unit fixtures stayed green -- exactly
// how the first cut of #2605 shipped green and broke mcp-conformance.
// Arg-validation errors are tolerated; only the gate's error fails the test,
// so this pin survives #2608 making the Enabled flag honest -- UNTIL a
// builtin is legitimately @disabled, at which point this test fails on that
// builtin by design: that failure is #2608's signal to scope the sweep to
// enabled builtins, not to silence the pin.
func TestExpandFunctionCall_RealBuiltinsExpandDespiteDeadFlag(t *testing.T) {
	registry := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	builtins := 0
	for name, fn := range registry.Snapshot() {
		if fn == nil || fn.Type != FunctionTypeBuiltin {
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
