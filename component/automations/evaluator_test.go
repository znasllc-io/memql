package automations

import "testing"

func TestEvaluator_EvaluateValue_Coalesce(t *testing.T) {
	e := NewEvaluator()

	e.SetStepResult("a", &StepResult{StepId: "a", Status: "success", Result: map[string]any{"value": ""}})
	e.SetStepResult("b", &StepResult{StepId: "b", Status: "success", Result: map[string]any{"value": "hello"}})

	val, err := e.EvaluateValue(`$coalesce($steps.a.result.value, $steps.b.result.value, "fallback")`)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != "hello" {
		t.Fatalf("expected %q, got %#v", "hello", val)
	}
}

func TestEvaluator_EvaluateValue_Coalesce_SkipsResolutionErrors(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("empty", &StepResult{StepId: "empty", Status: "success", Result: []any{}})

	val, err := e.EvaluateValue(`$coalesce($steps.empty.result[0].payload.name, "fallback")`)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != "fallback" {
		t.Fatalf("expected %q, got %#v", "fallback", val)
	}
}

func TestEvaluator_EvaluateValue_Pretty(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("x", &StepResult{StepId: "x", Status: "success", Result: map[string]any{"k": "v"}})

	val, err := e.EvaluateValue(`$pretty($steps.x.result)`)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	s, ok := val.(string)
	if !ok {
		t.Fatalf("expected string, got %T", val)
	}
	if s == "" {
		t.Fatalf("expected non-empty pretty string")
	}
}

