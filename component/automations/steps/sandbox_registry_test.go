package steps

// sandbox_registry_test.go -- white-box unit coverage for the Gate-2 dry-run
// metering helpers that the engine-backed integration tests can't reach without
// a live provider: si() AI-call metering + the cost estimate. si() is an
// in-logic projection expression (not a registry function), so it surfaces as a
// function step only inside a logic body; this test drives meterRead on a
// synthetic si step directly.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// evaluatorWithArgs builds an automations.Evaluator seeded so the metering
// arg-resolver can resolve the function step's literal args.
func newMeterRegistry() *sandboxStepRegistry {
	return newSandboxStepRegistry(NewRegistry(), nil, "sandbox:dryrun:test", memql.DryRunModeIsolated, "")
}

// TestMeterRead_SiCallRecordsAiCallAndCost: a function step named "si" records
// an aiCalls entry with a positive token + cost estimate, and the aggregate
// costEstimate reflects it.
func TestMeterRead_SiCallRecordsAiCallAndCost(t *testing.T) {
	reg := newMeterRegistry()
	step := &automations.Step{
		ID:   "askSi",
		Type: automations.StepTypeFunction,
		Function: &automations.FunctionStepConfig{
			Name: "si",
			Args: map[string]any{
				"0": "someTemplate",
				"1": map[string]any{"question": "what is the capital of france"},
			},
		},
	}
	stepCtx := &automations.StepContext{Evaluator: automations.NewEvaluator()}

	reg.meterRead(step, stepCtx)

	mani := reg.manifest()
	if len(mani.AiCalls) != 1 {
		t.Fatalf("expected 1 metered AI call, got %d: %+v", len(mani.AiCalls), mani.AiCalls)
	}
	ai := mani.AiCalls[0]
	if ai.Function != "si" {
		t.Errorf("expected function si, got %q", ai.Function)
	}
	if ai.PromptTokens <= 0 {
		t.Errorf("expected a positive prompt-token estimate, got %d", ai.PromptTokens)
	}
	if ai.OutputTokens != defaultSiOutputTokens {
		t.Errorf("expected output tokens %d, got %d", defaultSiOutputTokens, ai.OutputTokens)
	}
	if ai.EstimatedCost <= 0 {
		t.Errorf("expected a positive cost estimate, got %v", ai.EstimatedCost)
	}

	est := reg.costEstimate()
	if est.Tokens != ai.PromptTokens+ai.OutputTokens {
		t.Errorf("aggregate tokens %d != call tokens %d", est.Tokens, ai.PromptTokens+ai.OutputTokens)
	}
	if est.Usd != ai.EstimatedCost {
		t.Errorf("aggregate usd %v != call cost %v", est.Usd, ai.EstimatedCost)
	}
}

// TestEstimateTokens_FloorAndScaling: the token estimate floors at 1 for empty
// args and scales with the serialized arg size.
func TestEstimateTokens_FloorAndScaling(t *testing.T) {
	if got := estimateTokens(nil); got != 1 {
		t.Errorf("empty args should floor to 1 token, got %d", got)
	}
	small := estimateTokens(map[string]any{"q": "hi"})
	big := estimateTokens(map[string]any{"q": "a much much longer question that should clearly produce more estimated tokens than the short one above"})
	if big <= small {
		t.Errorf("expected larger args to estimate more tokens (small=%d big=%d)", small, big)
	}
}

// TestEstimateSiCostUsd_AppliesRates verifies the cost math applies the default
// per-million input/output rates.
func TestEstimateSiCostUsd_AppliesRates(t *testing.T) {
	got := estimateSiCostUsd(1_000_000, 1_000_000)
	want := siInputUsdPerMillion + siOutputUsdPerMillion
	if got != want {
		t.Errorf("estimateSiCostUsd(1M,1M) = %v, want %v", got, want)
	}
}
