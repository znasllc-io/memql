package pipelines

import (
	"encoding/json"
	"testing"
)

func poolSpec() *Spec {
	return &Spec{Image: "toolchain@sha256:test", Platform: "linux/amd64", Stages: []StageSpec{{Name: "test", Steps: []StepSpec{{Name: "unit", Run: "make test", Placement: PlacementPool, CPUMilli: 1500, MemoryMiB: 2048}}}}}
}

func TestPoolCompileRetainsRuntimeAndRequiresFleetConsent(t *testing.T) {
	spec := poolSpec()
	if r := Consent(spec, ComputeCluster, nil); r == nil || r.Code != CodeFleetNotConsented {
		t.Fatalf("pool bypassed Fleet consent: %v", r)
	}
	for _, placement := range []string{PlacementFleet, PlacementPool} {
		spec.Stages[0].Steps[0].Placement = placement
		plan, r := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet})
		if r != nil {
			t.Fatal(r)
		}
		b, err := json.Marshal(plan.Steps()[0])
		if err != nil {
			t.Fatal(err)
		}
		var step Step
		if err := json.Unmarshal(b, &step); err != nil {
			t.Fatal(err)
		}
		if step.Placement != placement || step.Execution != ExecutionContainer || step.Platform != "linux/amd64" || step.CPUMilli != 1500 || step.MemoryMiB != 2048 || step.Image != spec.Image {
			t.Fatalf("portable runtime/resource contract changed: %+v", step)
		}
	}
}

func TestPoolRefusesNonPortableEffects(t *testing.T) {
	for name, edit := range map[string]func(*Spec){
		"missing platform": func(s *Spec) { s.Platform = "" },
		"mac container":    func(s *Spec) { s.Platform = "darwin/arm64" },
		"native":           func(s *Spec) { s.Stages[0].Steps[0].Execution = ExecutionNative },
		"host need":        func(s *Spec) { s.Stages[0].Steps[0].Needs = map[string]bool{"docker": true} },
		"read credential":  func(s *Spec) { s.Stages[0].Steps[0].GitHubReadToken = true },
		"pull credential":  func(s *Spec) { s.Stages[0].Steps[0].ImagePullSecret = "registry" },
		"history":          func(s *Spec) { s.Stages[0].Steps[0].CheckoutHistory = "full" },
		"image build": func(s *Spec) {
			s.Stages[0].Steps[0].Run = ""
			s.Stages[0].Steps[0].ImageBuild = &ImageBuild{Context: ".", Dockerfile: "Dockerfile"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := poolSpec()
			edit(spec)
			if r := Validate(spec); r == nil {
				t.Fatal("non-portable job admitted to pool")
			}
		})
	}
}
