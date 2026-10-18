package pipelines

import (
	"encoding/json"
	"testing"
)

func TestRepositoryReadGrantSurvivesCompilationAndWire(t *testing.T) {
	spec := &Spec{Image: "toolchain", Platform: "linux/amd64", Stages: []StageSpec{{Name: "audit", Steps: []StepSpec{{Name: "scan", Run: "scan", GitHubReadToken: true}}}}}
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventSchedule, Compute: ComputeClusterAndFleet})
	if refusal != nil {
		t.Fatal(refusal)
	}
	raw, err := json.Marshal(plan.Steps()[0])
	if err != nil {
		t.Fatal(err)
	}
	var received Step
	if err := json.Unmarshal(raw, &received); err != nil || !received.GitHubReadToken {
		t.Fatalf("grant lost at hop: %s / %v", raw, err)
	}
	for _, change := range []func(*StepSpec){
		func(s *StepSpec) { s.Placement = PlacementFleet },
		func(s *StepSpec) { s.Execution = ExecutionNative; s.Platform = "darwin/arm64" },
		func(s *StepSpec) { s.Needs = map[string]bool{"docker": true} },
		func(s *StepSpec) { s.ImageBuild = &ImageBuild{} },
		func(s *StepSpec) { s.Run = ""; s.SARIF = &SARIFReport{Step: "before.scan", Path: "scan.sarif"} },
	} {
		step := spec.Stages[0].Steps[0]
		change(&step)
		if refusal := validateStep(spec, "audit", 0, step, map[string]bool{}); refusal == nil {
			t.Fatalf("unsupported read grant admitted: %+v", step)
		}
	}
}
