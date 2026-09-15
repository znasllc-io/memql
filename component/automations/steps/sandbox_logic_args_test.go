package steps

// sandbox_logic_args_test.go -- white-box coverage for stepCallArgs, the
// dry-run sandbox's forwarded-logic-call arg resolver (memql#1727).
//
// The regression: an authored wrapper forwards the triggering event as
// `logic autoJoinAI(event: event)`. The pre-#1727 sandbox resolved that
// through the mutation-style evaluator, which treated bare `event` as a
// literal string -- so the logic received args["event"] = "event" and every
// nested `args.event.payload.X` navigated into a string. stepCallArgs must
// resolve the argument to the seeded envelope, exactly like the live
// FunctionExecutor.

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestStepCallArgs_BindsTheEventEnvelope(t *testing.T) {
	envelope := map[string]any{
		"topic":   "node.created",
		"kind":    "NodeCreated",
		"payload": map[string]any{"id": "space-1", "ownerUserId": "user-1"},
	}
	evaluator := automations.NewEvaluator()
	evaluator.SetCustom("event", envelope)

	reg := newSandboxStepRegistry(NewRegistry(), nil, "sandbox:dryrun:test", "", "")
	stepCtx := &automations.StepContext{Evaluator: evaluator}

	// The compiled shape of `logic autoJoinAI ( event: event )`: a named
	// argument whose value is the expression leaf `event`.
	node, err := langparser.ParseV1Expression("event")
	if err != nil {
		t.Fatal(err)
	}
	step := &automations.Step{ID: "join", Type: automations.StepTypeFunction, Function: &automations.FunctionStepConfig{
		Name: "logicAutoJoinAI",
		Args: map[string]any{"event": &automations.ExprLeaf{Src: "event", Node: node}},
	}}

	got := reg.stepCallArgs(step, stepCtx)

	bound, ok := got["event"].(map[string]any)
	if !ok {
		t.Fatalf("event did not resolve to the envelope object; got %T: %#v", got["event"], got["event"])
	}
	if !reflect.DeepEqual(bound, envelope) {
		t.Fatalf("event bound to the wrong object.\n got: %#v\nwant: %#v", bound, envelope)
	}
	// Spot-check the nested payload threads through (this is what nested
	// args.event.payload.<field> reads navigate).
	payload, _ := bound["payload"].(map[string]any)
	if payload["ownerUserId"] != "user-1" {
		t.Fatalf("event.payload.ownerUserId did not thread through: %#v", payload)
	}
}

// TestStepCallArgs_EmptyArgs returns an empty map (no panic) when the call
// carries no args -- the cron/no-arg logic shape.
func TestStepCallArgs_EmptyArgs(t *testing.T) {
	reg := newSandboxStepRegistry(NewRegistry(), nil, "sandbox:dryrun:test", "", "")
	stepCtx := &automations.StepContext{Evaluator: automations.NewEvaluator()}

	step := &automations.Step{ID: "probe", Type: automations.StepTypeFunction, Function: &automations.FunctionStepConfig{Name: "serviceVersionProbe"}}
	if got := reg.stepCallArgs(step, stepCtx); len(got) != 0 {
		t.Fatalf("expected empty args, got %#v", got)
	}
}
