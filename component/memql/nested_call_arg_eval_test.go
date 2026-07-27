package memql

import (
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// nested_call_arg_eval_test.go -- memql#2870.
//
// An expression passed as an argument to a NESTED construct call used to reach
// argument validation as a raw AST node instead of a value:
//
//	function "runningPlansForUser": argument validation failed:
//	argument "userId": expected string, got *ast.CoalesceExpr
//
// Which meant `killSwitchSuspendsRunningPlans` -- the computer-use kill switch
// -- failed at its decide step and NEVER suspended a plan. Two more live
// automations failed the same way, and one corrupted its argument silently.
//
// The bug is NOT about coalesce. `magicLinkExpirySweep` passes a bare `now` and
// fails identically, which is what proves the shape is "any expression in a
// nested call argument". Coalesce is just the spelling most of the tree happens
// to use.
//
// WHY ONLY EIGHT OF THIRTY-THREE LOGICS WERE AFFECTED. A logic body with an
// intermediate `name := <call>` step gets `fn.LogicSteps` set
// (function_loader.go nonReturnStepCount > 0) and runs through the LogicRunner,
// whose automation-step evaluator folds arguments to values before the call. A
// SINGLE-STATEMENT body leaves LogicSteps nil and runs through fn.Expr ->
// ASTConverter -> functionValidator, which had no value evaluator in argument
// position at all. Adding a throwaway second statement to the DSL would have
// "fixed" the kill switch, which is the clearest statement of the defect.

// loadTreeForNestedArgTest loads the shipped tree the way bff boots it and
// returns the registries the resolver needs.
func loadTreeForNestedArgTest(t *testing.T) (*FunctionRegistry, *SpecRegistry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()

	schemaIdx, err := buildSchemaIndex(concepts)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	specRegistry, err := loadEmbeddedSpecs(logger, schemaIdx)
	if err != nil {
		t.Fatalf("loadEmbeddedSpecs: %v", err)
	}
	if _, err := LoadUnifiedSpecs(logger, specRegistry); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}

	functionRegistry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functionRegistry, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if _, err := LoadUnifiedBuiltins(logger, functionRegistry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	return functionRegistry, specRegistry
}

// TestNestedCallArgumentsResolveToValues drives the five shipped constructs that
// pass an expression into a nested call through the real resolver, and asserts
// each one resolves rather than dying on a raw AST node.
//
// Each case is the production entry point: resolvePlanFunctionsWithOrigin is
// what the engine calls, and the failure it produced is byte-for-byte the error
// on issue #2870.
func TestNestedCallArgumentsResolveToValues(t *testing.T) {
	functions, specs := loadTreeForNestedArgTest(t)

	cases := []struct {
		name string
		// call is the logic construct the automation invokes.
		call string
		// args is the event payload the automation binds.
		args map[string]any
		// wantRoot is the expected value when the construct's body leaves a
		// positional builtin at plan.Root for in-memory evaluation.
		wantRoot any
		// why records what breaks in production when this fails.
		why string
	}{
		{
			name: "killSwitchSuspendsRunningPlans",
			call: "killSwitchSuspendsRunningPlans",
			args: map[string]any{
				"event": map[string]any{"node": map[string]any{"id": "v1:identity:user:abc"}},
			},
			why: "the computer-use kill switch -- a user tripping it suspends no plans",
		},
		{
			name: "releaseWorkspaceOnPlanTerminal",
			call: "releaseWorkspaceOnPlanTerminal",
			args: map[string]any{
				"event": map[string]any{"node": map[string]any{"id": "v1:planner:plan:xyz"}},
			},
			why: "workbench workspaces are never released on plan terminal",
		},
		{
			name: "magicLinkExpirySweep_bareNow",
			call: "magicLinkExpirySweep",
			args: map[string]any{"event": map[string]any{}},
			why:  "the hourly magic-link expiry sweep never runs; proves the class is any expression, not coalesce",
		},
		{
			name: "nextDeploymentVersion_silentCorruption",
			call: "nextDeploymentVersion",
			// `current` is @required; supply it so the case exercises the
			// coalesce on `bump` rather than an unrelated missing-arg error.
			args: map[string]any{"current": "1.2.3", "bump": "minor"},
			why:  "a builtin receives the raw coalesce node as `bump` with NO error, even when the caller supplied a value",
		},
		{
			name:     "deployGateGreen_rootCoalesce",
			call:     "deployGateGreen",
			args:     map[string]any{"gate": map[string]any{"passed": true}},
			wantRoot: true,
			why:      "a root-position coalesce return fails `function \"coalesce\" not found`",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := functions.Get(tc.call)
			if err != nil || fn == nil {
				t.Skipf("construct %q is not in this tree (%v) -- nothing to assert", tc.call, err)
			}
			if fn.LogicSteps != nil {
				t.Fatalf("%s has LogicSteps set, so it runs the LogicRunner path and cannot "+
					"exercise this bug -- the test fixture is wrong, not the code", tc.call)
			}

			plan := &QueryPlan{Root: &FunctionCallExpression{Name: tc.call, Args: tc.args}}
			// Automations dispatch internally, which is the origin these
			// constructs actually run under.
			if err := resolvePlanFunctionsWithOrigin(plan, functions, specs, auth.OriginInternal); err != nil {
				t.Fatalf("resolve %s: %v\n\nIn production this means: %s", tc.call, err, tc.why)
			}

			// A converter-normalised positional builtin left at plan.Root is
			// evaluated in-memory by the engine's plan-root branch, so its
			// operands are SUPPOSED to still be nodes here. Evaluate it the way
			// the engine does and assert the value instead.
			if root, isCall := plan.Root.(*FunctionCallExpression); isCall && isPositionalBuiltin(root.Name) {
				val, evalErr := evalCollScalar(root, nil, nil)
				if evalErr != nil {
					t.Fatalf("%s: plan-root %s() failed to evaluate: %v\n\nIn production this means: %s",
						tc.call, root.Name, evalErr, tc.why)
				}
				if tc.wantRoot != nil && !reflect.DeepEqual(val, tc.wantRoot) {
					t.Fatalf("%s: plan-root evaluated to %#v, want %#v\n\nIn production this means: %s",
						tc.call, val, tc.wantRoot, tc.why)
				}
				return
			}

			// Resolving without error is necessary but not sufficient: the
			// nextDeploymentVersion case fails SILENTLY, so assert no raw
			// expression node survives anywhere in the resolved arguments.
			if raw := findUnevaluatedArg(plan); raw != "" {
				t.Fatalf("%s resolved but left an unevaluated argument: %s\n\nIn production this means: %s",
					tc.call, raw, tc.why)
			}
		})
	}
}

// isPositionalBuiltin reports whether name is a converter-normalised positional
// builtin, which the engine evaluates in-memory at plan.Root rather than
// expanding as a construct call.
func isPositionalBuiltin(name string) bool {
	return name == "coalesce" || name == "concat" || name == "cond" || IsDateBuiltin(name)
}

// TestNoLogicReachesValidationWithAnASTNode is the guard against a SIXTH
// instance shipping silently.
//
// The five known constructs are pinned by name above, but the defect is a
// property of the fn.Expr path, not of those five: any single-statement logic
// that passes an expression into a nested call has it. Three of the five were
// live automations that had never once run, and nothing noticed -- so the check
// that matters is the one that does not need to know the name in advance.
//
// It sweeps every logic on the fn.Expr path (LogicSteps == nil) and fails only
// on the AST-node signature. A logic that fails for an unrelated reason -- a
// required argument this test cannot synthesise, a missing executor -- is not
// this bug and is deliberately ignored.
func TestNoLogicReachesValidationWithAnASTNode(t *testing.T) {
	functions, specs := loadTreeForNestedArgTest(t)

	var offenders []string
	swept := 0
	for name, fn := range functions.Snapshot() {
		if fn == nil || !strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "logic") {
			continue
		}
		if fn.LogicSteps != nil {
			continue // LogicRunner path -- folds arguments already
		}
		swept++

		plan := &QueryPlan{Root: &FunctionCallExpression{
			Name: name,
			// Synthetic args: enough shape for the common event/id accessors.
			Args: map[string]any{
				"event": map[string]any{
					"node":    map[string]any{"id": "v1:identity:user:probe"},
					"payload": map[string]any{"id": "v1:identity:user:probe"},
				},
			},
		}}
		err := resolvePlanFunctionsWithOrigin(plan, functions, specs, auth.OriginInternal)
		if err != nil && isASTNodeLeak(err.Error()) {
			offenders = append(offenders, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if err == nil {
			if raw := findUnevaluatedArg(plan); raw != "" {
				if root, ok := plan.Root.(*FunctionCallExpression); ok && isPositionalBuiltin(root.Name) {
					continue // evaluated in-memory at plan.Root by design
				}
				offenders = append(offenders, fmt.Sprintf("%s: unevaluated argument %s", name, raw))
			}
		}
	}

	if swept == 0 {
		t.Fatal("swept zero single-statement logics, so this test asserts nothing -- the " +
			"registry walk or the LogicSteps filter has broken")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d logic construct(s) still reach argument validation with an unevaluated "+
			"expression (memql#2870):\n  %s\n\nAn expression in an argument must be folded to a "+
			"value before validation -- see foldArgExpression in function_validator.go.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	t.Logf("swept %d single-statement logic constructs, %d leaking an AST node", swept, len(offenders))
}

// isASTNodeLeak reports whether an error is the #2870 signature: an unevaluated
// expression reaching validation, or a positional builtin being looked up as a
// user function because it was never short-circuited.
//
// Deliberately signature-based rather than "any error": these constructs are
// swept with synthetic arguments, so unrelated failures (a required argument
// this test cannot invent, an unwired executor) are expected and are not this
// bug.
func isASTNodeLeak(msg string) bool {
	leaks := []string{
		"got *ast.",                         // a raw PARSER node
		"got *memql.FunctionCallExpression", // an engine node the converter made but nobody folded
		"got *memql.LiteralValueNode",       // bare `now`
		"got *memql.ArgRefExpression",
		`function "coalesce" not found`, // root-position positional builtin
		`function "concat" not found`,
	}
	for _, l := range leaks {
		if strings.Contains(msg, l) {
			return true
		}
	}
	return false
}

// TestDeployGateGreenFailsClosed pins the fail-closed contract deployGateGreen
// states in its own doc comment: "false when the result or its `passed` flag is
// absent".
//
// Making the root coalesce merely RESOLVE is not enough, and this is why. Bare
// `false` in value position converts to a SpecReferenceExpression, not a boolean
// literal, so the fallback evaluated through resolveLambdaPath and returned nil
// for an absent gate -- a deploy gate answering nil instead of false, on the
// branch that decides whether to auto-promote a release.
func TestDeployGateGreenFailsClosed(t *testing.T) {
	functions, specs := loadTreeForNestedArgTest(t)
	if fn, err := functions.Get("deployGateGreen"); err != nil || fn == nil {
		t.Skip("deployGateGreen is not in this tree")
	}

	cases := []struct {
		name string
		gate map[string]any
		want any
	}{
		{name: "gate passed", gate: map[string]any{"passed": true}, want: true},
		{name: "gate failed", gate: map[string]any{"passed": false}, want: false},
		{name: "passed flag absent -- must fail CLOSED", gate: map[string]any{}, want: false},
		{name: "gate result empty -- must fail CLOSED", gate: nil, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := &QueryPlan{Root: &FunctionCallExpression{
				Name: "deployGateGreen",
				Args: map[string]any{"gate": tc.gate},
			}}
			if err := resolvePlanFunctionsWithOrigin(plan, functions, specs, auth.OriginInternal); err != nil {
				t.Fatalf("resolve deployGateGreen: %v", err)
			}
			root, ok := plan.Root.(*FunctionCallExpression)
			if !ok {
				t.Fatalf("expected a coalesce call at plan.Root, got %T", plan.Root)
			}
			got, err := evalCollScalar(root, nil, nil)
			if err != nil {
				t.Fatalf("evaluate deployGateGreen: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("deployGateGreen = %#v, want %#v -- a deploy gate that does not "+
					"fail closed can auto-promote a release whose gate never reported", got, tc.want)
			}
		})
	}
}

// findUnevaluatedArg reports the first argument in the resolved plan that is
// not a concrete Go value, as "name=type".
//
// Concreteness -- not "is it an ExpressionNode" -- is the right assertion, and
// the nextDeploymentVersion case is why. Before the fix its `bump` argument is
// an *ast.CoalesceExpr: a raw PARSER node, which does not implement the engine's
// ExpressionNode interface at all. That is exactly how it reaches a Go executor
// with no error -- it is neither a value the executor can use nor a node the
// validator recognises. A detector keyed on ExpressionNode misses it, which the
// first version of this test did.
//
// A resolved call's arguments must be values by the time they reach a Go
// executor or SQL builder, so anything else is the #2870 defect whether or not
// validation happened to report it.
func findUnevaluatedArg(plan *QueryPlan) string {
	var walk func(any, string) string
	visits := 0
	walk = func(n any, path string) string {
		if visits++; visits > 10000 {
			return ""
		}
		// Both call flavours must be inspected. A builtin call resolves to a
		// *BuiltinFunctionExpression, NOT a *FunctionCallExpression -- which is
		// how nextDeploymentVersion's corrupted `bump` argument escaped the
		// first version of this walker.
		var callArgs map[string]any
		switch c := n.(type) {
		case *FunctionCallExpression:
			if c == nil {
				return ""
			}
			callArgs = c.Args
		case *BuiltinFunctionExpression:
			if c == nil {
				return ""
			}
			callArgs = c.Args
		default:
			return ""
		}
		names := make([]string, 0, len(callArgs))
		for k := range callArgs {
			names = append(names, k)
		}
		sort.Strings(names) // deterministic first-hit across runs
		for _, k := range names {
			if concreteArgValue(callArgs[k]) {
				continue
			}
			return strings.TrimPrefix(path+"."+k, ".") + "=" + fmt.Sprintf("%T", callArgs[k])
		}
		return ""
	}
	for _, root := range []any{plan.Root, plan.LogicCall, plan.MutationCall} {
		if hit := walk(root, ""); hit != "" {
			return hit
		}
	}
	return ""
}

// concreteArgValue reports whether v is a plain Go value an executor can consume
// directly, rather than an unevaluated expression of any flavour.
func concreteArgValue(v any) bool {
	switch t := v.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, time.Time:
		return true
	case []any:
		for _, e := range t {
			if !concreteArgValue(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range t {
			if !concreteArgValue(e) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
