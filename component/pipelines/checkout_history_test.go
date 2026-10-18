package pipelines

import (
	"encoding/json"
	"testing"
)

func TestCheckoutHistorySurvivesCompilationAndWire(t *testing.T) {
	spec := &Spec{Image: "toolchain", Platform: "linux/amd64", Stages: []StageSpec{{Name: "scan", Steps: []StepSpec{{Name: "history", Run: "scan", CheckoutHistory: "full"}}}}}
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet})
	if refusal != nil {
		t.Fatal(refusal)
	}
	raw, err := json.Marshal(plan.Steps()[0])
	if err != nil {
		t.Fatal(err)
	}
	var received Step
	if err = json.Unmarshal(raw, &received); err != nil || received.CheckoutHistory != "full" {
		t.Fatalf("history lost at wire: %s / %v", raw, err)
	}
	for _, change := range []func(*StepSpec){
		func(s *StepSpec) { s.CheckoutHistory = "unknown" },
		func(s *StepSpec) { s.Placement = PlacementFleet },
		func(s *StepSpec) { s.Execution = ExecutionNative; s.Platform = "darwin/arm64" },
		func(s *StepSpec) { s.Needs = map[string]bool{"docker": true} },
	} {
		step := spec.Stages[0].Steps[0]
		change(&step)
		if refusal := validateStepRuntime(spec, step, "scan/history"); refusal == nil {
			t.Fatalf("unsupported checkout admitted: %+v", step)
		}
	}
}
