package automations_test

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression for memql#580 (the ghost AI / "two assistants" bug).
//
// The autoJoinAI logic body derives the GA participant id with:
//
//	getGA := coalesce(getActiveGA, getFallbackGA)   // logic-runner path
//	... agentId: coalesce(getGA.first().id, "")     // arg-time path (memql#575)
//
// memql#575 taught only the ARG-TIME evaluator to resolve a bare step
// identifier to its result. The LOGIC-RUNNER path (which evaluates
// `coalesce(getActiveGA, getFallbackGA)`) bottoms out in
// Evaluator.EvaluateStepReference -> EvaluateValue, which still rendered a
// bare step name as its OWN LITERAL STRING. So `getGA` held the string
// "getActiveGA" instead of the query's *ExecuteResult, every downstream
// getGA.first().id collapsed to nil, and the unevaluated
// coalesce(getGA.first().id, "") literal flowed into the mutation as the
// agentId -> ghost AI.
//
// These tests pin the fix: a bare identifier that names a KNOWN step now
// resolves to that step's result in the logic-runner evaluator too; a bare
// identifier that is NOT a step still renders as a literal string.

func newAgentResult(id, name string) *memqlengine.ExecuteResult {
	payload, err := structpb.NewStruct(map[string]any{"name": name})
	if err != nil {
		panic(err)
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: payload}},
		},
	}
}

// Before the fix this returned the literal string "getActiveGA"; it must now
// return the step's ExecuteResult so coalesce(getActiveGA, getFallbackGA)
// selects a navigable bundle.
func TestBareStepResolvesToResult_LogicRunnerEvaluator(t *testing.T) {
	active := newAgentResult("v1:agents:agent:assistant-REAL", "Sofia")

	eval := automations.NewEvaluator()
	eval.SetStepResult("getActiveGA", &automations.StepResult{StepId: "getActiveGA", Status: "success", Result: active})

	// EvaluateValue is the primitive the logic-runner coalesce-arg path bottoms
	// out in (via EvaluateStepReference for a bare, dotless step name).
	gotVal, err := eval.EvaluateValue("getActiveGA")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if _, isStr := gotVal.(string); isStr {
		t.Fatalf("EvaluateValue(\"getActiveGA\") = %#v (a literal string) -- the memql#580 ghost-AI bug; want the step ExecuteResult", gotVal)
	}
	if gotVal != any(active) {
		t.Fatalf("EvaluateValue(\"getActiveGA\") = %#v, want the getActiveGA ExecuteResult", gotVal)
	}

	gotRef, err := eval.EvaluateStepReference("getActiveGA")
	if err != nil {
		t.Fatalf("EvaluateStepReference: %v", err)
	}
	if gotRef != any(active) {
		t.Fatalf("EvaluateStepReference(\"getActiveGA\") = %#v, want the getActiveGA ExecuteResult", gotRef)
	}
}

// A skipped step (nil Result) must resolve to nil so coalesce falls through to
// the next branch -- mirrors getFallbackGA being skipped when activeAssistantId
// is set.
func TestBareStepSkipped_ResolvesNil(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetStepResult("getFallbackGA", &automations.StepResult{StepId: "getFallbackGA", Status: "skipped"})

	got, err := eval.EvaluateValue("getFallbackGA")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if got != nil {
		t.Fatalf("EvaluateValue(skipped step) = %#v, want nil", got)
	}
}

// Regression guard: a bare identifier that is NOT a known step keeps its prior
// behaviour and renders as a literal string, so coalesce(field, "default") and
// similar non-step usages are unaffected.
func TestBareIdentifierNonStep_StaysLiteral(t *testing.T) {
	eval := automations.NewEvaluator()

	got, err := eval.EvaluateValue("writer")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if got != "writer" {
		t.Fatalf("EvaluateValue(\"writer\") = %#v, want literal %q", got, "writer")
	}
}
