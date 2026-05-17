package steps

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

func TestEmitConceptCardExecutor_RequiresConfig(t *testing.T) {
	exec := &EmitConceptCardExecutor{}

	step := &automations.Step{
		ID:              "emitCard",
		Type:            automations.StepTypeEmitConceptCard,
		EmitConceptCard: nil, // missing config
	}

	eval := automations.NewEvaluator()
	result, err := exec.Execute(context.Background(), step, &Context{
		Evaluator: eval,
	})

	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if result.Status != "failed" {
		t.Fatalf("expected status 'failed', got %q", result.Status)
	}
}

func TestEmitConceptCardExecutor_RequiresEngine(t *testing.T) {
	exec := &EmitConceptCardExecutor{}

	step := &automations.Step{
		ID:   "emitCard",
		Type: automations.StepTypeEmitConceptCard,
		EmitConceptCard: &automations.EmitConceptCardStepConfig{
			CardType:   "lead_captured",
			SpaceId:    "test-space",
			ConceptRef: "v1:crm:lead:test-lead",
			Data: map[string]any{
				"name":  "Test Lead",
				"email": "test@example.com",
			},
		},
	}

	eval := automations.NewEvaluator()
	result, err := exec.Execute(context.Background(), step, &Context{
		Evaluator: eval,
		Engine:    nil, // no engine
	})

	if err == nil {
		t.Fatal("expected error for missing engine")
	}
	if result.Status != "failed" {
		t.Fatalf("expected status 'failed', got %q", result.Status)
	}
}

func TestEmitConceptCardExecutor_EvaluatesExpressions(t *testing.T) {
	exec := &EmitConceptCardExecutor{}

	step := &automations.Step{
		ID:   "emitCard",
		Type: automations.StepTypeEmitConceptCard,
		EmitConceptCard: &automations.EmitConceptCardStepConfig{
			CardType:   "lead_captured",
			SpaceId:    "$event.payload.spaceId",
			ConceptRef: "$steps.upsertLead.result.Bundle.nodes.0.id",
			Data: map[string]any{
				"name":  "$steps.extractLead.result[0].extraction.name",
				"email": "literal@example.com",
			},
		},
	}

	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{
		"payload": map[string]any{
			"spaceId": "space-123",
		},
	})
	eval.SetStepResult("upsertLead", &automations.StepResult{
		StepId: "upsertLead",
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "v1:crm:lead:lead-test@example.com",
					},
				},
			},
		},
	})
	eval.SetStepResult("extractLead", &automations.StepResult{
		StepId: "extractLead",
		Status: "success",
		Result: []any{
			map[string]any{
				"extraction": map[string]any{
					"name":  "John Doe",
					"email": "test@example.com",
				},
			},
		},
	})

	// Still fails because no engine, but we can verify the error is about engine not expressions
	result, err := exec.Execute(context.Background(), step, &Context{
		Evaluator: eval,
		Engine:    nil,
	})

	if err == nil {
		t.Fatal("expected error")
	}
	if result.Error != "MemQL engine not configured" {
		t.Fatalf("expected error about engine, got: %s", result.Error)
	}
}
