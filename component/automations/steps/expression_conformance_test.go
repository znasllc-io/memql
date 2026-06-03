package steps

// Expression-evaluator conformance matrix (memql#593).
//
// memQL evaluates DSL value-expressions with TWO engines depending on where
// the expression appears: ARG-TIME (mutation/function step args ->
// MutationExecutor.evaluateValue) and LOGIC-TIME (logic-body RHS / return ->
// Evaluator.EvaluateValue). Same syntax, two implementations -- so a fix to
// one silently leaves the other broken (exactly how the #575/#580 ghost SI
// survived).
//
// This matrix runs ONE table of (expression, seeded steps) -> expected value
// against BOTH entry points, using REAL *memql.ExecuteResult step results (the
// #575 lesson: fabricated map[string]any{"Bundle":...} shapes hid the bug). A
// case that diverges between the two evaluators fails here, so they can never
// drift apart undetected again.
//
// STAGE 1 (this file): the SHARED LEAF-RESOLUTION contract -- the
// bare-step / skipped-step / non-step-literal behaviours both `evaluateValue`
// (arg-time) and `Evaluator.EvaluateValue` (logic-time leaf) already produce
// identically, pinned on the REAL result shape. This is the regression net the
// #580 ghost-SI fix lacked.
//
// The remaining divergences are NOT leaf-level: the arg-time `evaluateValue` is
// ONE cohesive resolver (literals, coalesce, path nav, .First()/.Len()/...),
// whereas the logic-time path fragments that same work across the LogicRunner
// helpers (tryEvaluateBuiltinLocally / tryEvaluateReturnLocally) sitting ON TOP
// of this leaf. Converging them means relocating the cohesive resolver into the
// `automations` package so both call paths share it; this table then extends to
// the builtin/path cases run through the FULL logic-time path.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// nodeResult builds a real *memql.ExecuteResult carrying one node (id + a
// flat payload), the shape a real query step yields.
func nodeResult(id string, payload map[string]any) *memqlengine.ExecuteResult {
	pb, err := structpb.NewStruct(payload)
	if err != nil {
		panic(err)
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: pb}},
		},
	}
}

// argTime / logicTime are the two entry points under test, sharing one seeded
// *automations.Evaluator.
func argTime(eval *automations.Evaluator, expr string) (any, error) {
	return (&MutationExecutor{}).evaluateValue(eval, expr)
}

func logicTime(eval *automations.Evaluator, expr string) (any, error) {
	return eval.EvaluateValue(expr)
}

type conformanceCase struct {
	name  string
	setup func(*automations.Evaluator) // seed steps
	expr  string
	want  any
}

// newSeededEvaluator returns a fresh evaluator with the case's steps applied,
// so the two entry points never share mutable state.
func (c conformanceCase) newSeededEvaluator() *automations.Evaluator {
	eval := automations.NewEvaluator()
	if c.setup != nil {
		c.setup(eval)
	}
	return eval
}

func seedStep(id string, result any, status string) func(*automations.Evaluator) {
	return func(eval *automations.Evaluator) {
		eval.SetStepResult(id, &automations.StepResult{StepId: id, Status: status, Result: result})
	}
}

// conformanceCases are behaviours BOTH evaluators must produce identically.
// Keep every entry here green; a regression on either side is a bug.
var conformanceCases = []conformanceCase{
	{
		name: "bare_step_resolves_to_result",
		setup: seedStep("getActiveGA",
			nodeResult("v1:agents:agent:real", map[string]any{"name": "Sofia"}), "success"),
		expr: "getActiveGA",
		want: nodeResult("v1:agents:agent:real", map[string]any{"name": "Sofia"}),
	},
	{
		name:  "skipped_step_resolves_nil",
		setup: seedStep("getFallbackGA", nil, "skipped"),
		expr:  "getFallbackGA",
		want:  nil,
	},
	{
		name: "bare_non_step_stays_literal",
		expr: "writer",
		want: "writer",
	},
}

func TestExpressionEvaluators_Conformance(t *testing.T) {
	engines := []struct {
		name string
		fn   func(*automations.Evaluator, string) (any, error)
	}{
		{"argTime", argTime},
		{"logicTime", logicTime},
	}

	for _, c := range conformanceCases {
		for _, eng := range engines {
			t.Run(c.name+"/"+eng.name, func(t *testing.T) {
				got, err := eng.fn(c.newSeededEvaluator(), c.expr)
				if err != nil {
					t.Fatalf("%s(%q): unexpected error: %v", eng.name, c.expr, err)
				}
				if !conformanceEqual(got, c.want) {
					t.Fatalf("%s(%q) = %#v, want %#v -- the two evaluators must agree (memql#593)", eng.name, c.expr, got, c.want)
				}
			})
		}
	}
}

// conformanceEqual compares results structurally enough for the matrix: two
// *ExecuteResult are "equal" when their first node ids match (the navigable
// identity the downstream payload/.First().id reads care about); everything
// else falls back to ==.
func conformanceEqual(got, want any) bool {
	gr, gok := got.(*memqlengine.ExecuteResult)
	wr, wok := want.(*memqlengine.ExecuteResult)
	if gok && wok {
		gid := firstNodeID(gr)
		wid := firstNodeID(wr)
		return gid == wid
	}
	if gok != wok {
		return false
	}
	return got == want
}

func firstNodeID(r *memqlengine.ExecuteResult) string {
	if r == nil || r.Bundle == nil || len(r.Bundle.Nodes) == 0 {
		return ""
	}
	return r.Bundle.Nodes[0].Id
}
