package memql

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

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
// -- failed at its decide step and NEVER suspended a plan. The path it lived
// on -- a single-statement logic run through fn.Expr and the function
// validator, beside the multi-statement one the LogicRunner ran -- went with
// the statement bodies (epic memql#5370): every logic's body runs on the
// LogicRunner, which evaluates each argument before the call. What stays pins
// two contracts the bug surfaced: deployGateGreen fails closed, and `??`
// evaluates no operand it does not need.

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

// TestDeployGateGreenFailsClosed pins the fail-closed contract deployGateGreen
// states in its own doc comment: "false when the result or its `passed` flag is
// absent" -- a deploy gate answering nil instead of false is on the branch
// that decides whether to auto-promote a release.
//
// The body is `return args.gate.passed ?? false`. A logic's body is a
// statement body (epic memql#5370), compiled at load into fn.LogicBody; the
// LogicRunner parses its return statement's value and evaluates it with
// EvalExpr over the call's arguments. So the contract is asserted on exactly
// that: the shipped construct's returned expression, as its compiled body
// carries it, evaluated by EvalExpr. (Before the flips the body was a
// `coalesce(...)` left at plan.Root for the engine's in-memory branch, and
// this test drove that branch.)
func TestDeployGateGreenFailsClosed(t *testing.T) {
	functions, _ := loadTreeForNestedArgTest(t)
	fn, err := functions.Get("deployGateGreen")
	if err != nil || fn == nil {
		t.Fatalf("deployGateGreen is not in this tree: %v", err)
	}
	ret := statementReturnExpr(t, fn)

	cases := []struct {
		name string
		gate any
		want any
	}{
		{name: "gate passed", gate: map[string]any{"passed": true}, want: true},
		{name: "gate failed", gate: map[string]any{"passed": false}, want: false},
		{name: "passed flag absent -- must fail CLOSED", gate: map[string]any{}, want: false},
		{name: "gate result empty -- must fail CLOSED", gate: nil, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalExpr(context.Background(), ret, MapScope{"args": map[string]any{"gate": tc.gate}}, EvalOptions{})
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

// TestCoalesceShortCircuits pins that ?? does not evaluate operands it does not
// need. An eager fold turns `a ?? (b / 0)` into a hard error where the correct
// answer is `a` -- which is how the first attempt at the #2870 fix broke cond().
func TestCoalesceShortCircuits(t *testing.T) {
	node := &FunctionCallExpression{Name: "coalesce", Args: map[string]any{
		"0": &LiteralValueNode{Value: "present"},
		"1": &ArithmeticExpression{
			Left:  &LiteralValueNode{Value: 1},
			Op:    "/",
			Right: &LiteralValueNode{Value: 0},
		},
	}}
	got, err := evalCollCoalesce(node, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("coalesce short-circuit: the first operand is present, so the "+
			"division-by-zero fallback must never be evaluated; got %v", err)
	}
	if got != "present" {
		t.Fatalf("coalesce = %#v, want %q", got, "present")
	}
}
