package steps

// sandbox_logic_args_test.go -- white-box coverage for resolveLogicCallArgs, the
// dry-run sandbox's forwarded-logic-call arg resolver (memql#1727).
//
// The regression: an authored wrapper forwards the triggering event as the bare
// runtime-reference token `event` inside a SINGLE positional object arg
// ({"0": {event: "event"}}). The pre-#1727 sandbox resolved that through the
// mutation-style evaluateValue, which (a) treated bare `event` as a literal
// string and (b) never unwrapped the positional object -- so RunLogic received
// {"0": {event: "event"}} and args["event"] was nil. resolveLogicCallArgs must
// resolve the bare `event` to the seeded envelope AND flatten the positional
// object, exactly like the live FunctionExecutor.

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

func TestResolveLogicCallArgs_BindsBareEventAndUnwrapsPositional(t *testing.T) {
	envelope := map[string]any{
		"topic":   "node.created",
		"kind":    "NodeCreated",
		"payload": map[string]any{"id": "space-1", "ownerUserId": "user-1"},
	}
	evaluator := automations.NewEvaluator()
	evaluator.SetCustom("event", envelope)

	reg := newSandboxStepRegistry(NewRegistry(), nil, "sandbox:dryrun:test", "", "")
	stepCtx := &automations.StepContext{Evaluator: evaluator}

	// The compiled shape of `logic autoJoinAI { event: event }`: a single
	// positional object arg whose `event` value is the bare runtime-reference
	// token "event".
	fn := &automations.FunctionStepConfig{
		Name: "logicAutoJoinAI",
		Args: map[string]any{"0": map[string]any{"event": "event"}},
	}

	got := reg.resolveLogicCallArgs(fn, stepCtx)

	// Must be flattened to {event: <envelope>} -- NOT {"0": {...}} -- so the
	// LogicRunner's newEvaluatorForLogic reads args["event"].
	if _, wrapped := got["0"]; wrapped {
		t.Fatalf("positional object not unwrapped: %#v", got)
	}
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

// TestResolveLogicCallArgs_EmptyArgs returns an empty map (no panic) when the
// call carries no args -- the cron/no-arg logic shape.
func TestResolveLogicCallArgs_EmptyArgs(t *testing.T) {
	reg := newSandboxStepRegistry(NewRegistry(), nil, "sandbox:dryrun:test", "", "")
	stepCtx := &automations.StepContext{Evaluator: automations.NewEvaluator()}

	got := reg.resolveLogicCallArgs(&automations.FunctionStepConfig{Name: "serviceVersionProbe"}, stepCtx)
	if len(got) != 0 {
		t.Fatalf("expected empty args, got %#v", got)
	}
}
