package automations_test

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression for memql#580 (the ghost AI / "two assistants" bug).
//
// The autoJoinAI logic body derived the GA participant id with:
//
//	getGA := getActiveGA ?? getFallbackGA
//	... agentId: getGA.first().id ?? ""
//
// The string evaluator rendered a bare step name as its OWN LITERAL STRING,
// so `getGA` held the string "getActiveGA" instead of the query's result,
// every downstream getGA.first().id collapsed to nil, and the unevaluated
// text flowed into the mutation as the agentId -> ghost AI.
//
// These tests pin the edition-2026 truth: a bare identifier that names a
// KNOWN step stands for that step's rows; a skipped step is unset, so `??`
// falls through; and a bare identifier that names nothing is refused as an
// unknown name -- never its own text.

// evalOver parses src and evaluates it over e.
func evalOver(t *testing.T, e *automations.Evaluator, src string) (any, error) {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	return e.EvalV1(context.Background(), n)
}

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

// The step's rows, never the literal string "getActiveGA".
func TestBareStepResolvesToResult_LogicRunnerEvaluator(t *testing.T) {
	active := newAgentResult("v1:agents:agent:assistant-REAL", "Sofia")

	eval := automations.NewEvaluator()
	eval.SetStepResult("getActiveGA", &automations.StepResult{StepId: "getActiveGA", Status: "success", Result: active})

	gotVal, err := evalOver(t, eval, "getActiveGA")
	if err != nil {
		t.Fatalf("getActiveGA: %v", err)
	}
	if _, isStr := gotVal.(string); isStr {
		t.Fatalf("getActiveGA = %#v (a literal string) -- the memql#580 ghost-AI bug; want the step's rows", gotVal)
	}
	if id, err := evalOver(t, eval, "getActiveGA.first().id"); err != nil || id != "v1:agents:agent:assistant-REAL" {
		t.Fatalf("getActiveGA.first().id = %#v (err %v), want the GA's id", id, err)
	}
	if id, err := evalOver(t, eval, `(getActiveGA ?? getActiveGA).first().id ?? ""`); err != nil || id != "v1:agents:agent:assistant-REAL" {
		t.Fatalf("the autoJoinAI shape = %#v (err %v), want the GA's id", id, err)
	}
}

// A skipped step (nil Result) is unset, so `??` falls through to the next
// branch -- mirrors getFallbackGA being skipped when activeAssistantId is
// set.
func TestBareStepSkipped_ResolvesNil(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetStepResult("getFallbackGA", &automations.StepResult{StepId: "getFallbackGA", Status: "skipped"})

	got, err := evalOver(t, eval, `getFallbackGA ?? "fallback"`)
	if err != nil {
		t.Fatalf("getFallbackGA ?? \"fallback\": %v", err)
	}
	if got != "fallback" {
		t.Fatalf("getFallbackGA ?? \"fallback\" = %#v, want the fallback (a skipped step is unset)", got)
	}
}

// A bare identifier that is NOT a known step is refused as an unknown name,
// never rendered as its own text.
func TestBareIdentifierNonStep_IsRefused(t *testing.T) {
	eval := automations.NewEvaluator()
	if got, err := evalOver(t, eval, "writer"); err == nil {
		t.Fatalf("writer = %#v, want an unknown-name refusal (never the literal)", got)
	}
}
