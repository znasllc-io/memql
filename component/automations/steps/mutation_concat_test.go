package steps

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

func TestMutationExecutor_EvaluateConcat_StrictStepRef(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetStepResult("agent", &automations.StepResult{
		StepId:   "agent",
		Status:   "success",
		Result:   []any{map[string]any{"name": "TestBot"}},
		Metadata: map[string]any{"itemCount": 1},
	})

	exec := &MutationExecutor{}
	out, err := exec.evaluateConcat(eval, `concat("Hi ", agent.result.0.name, "!")`)
	if err != nil {
		t.Fatalf("evaluateConcat error: %v", err)
	}
	if out != "Hi TestBot!" {
		t.Fatalf("expected %q, got %q", "Hi TestBot!", out)
	}
}

func TestMutationExecutor_EvaluateConcat_StrictItemRef(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetItem(map[string]any{"payload": map[string]any{"name": "Jose"}}, "item")

	exec := &MutationExecutor{}
	out, err := exec.evaluateConcat(eval, `concat("Hello ", item.payload.name)`)
	if err != nil {
		t.Fatalf("evaluateConcat error: %v", err)
	}
	if out != "Hello Jose" {
		t.Fatalf("expected %q, got %q", "Hello Jose", out)
	}
}

func TestMutationExecutor_EvaluateConcat_UnknownPathIsLiteral(t *testing.T) {
	eval := automations.NewEvaluator()

	exec := &MutationExecutor{}
	out, err := exec.evaluateConcat(eval, `concat("X=", foo.bar)`)
	if err != nil {
		t.Fatalf("evaluateConcat error: %v", err)
	}
	if out != "X=foo.bar" {
		t.Fatalf("expected %q, got %q", "X=foo.bar", out)
	}
}
