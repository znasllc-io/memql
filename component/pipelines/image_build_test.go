package pipelines

import (
	"reflect"
	"testing"
)

func imageBuildSpec() *Spec {
	return &Spec{Image: "unused-default", Platform: "linux/arm64", Stages: []StageSpec{{Name: "build", Steps: []StepSpec{{
		Name: "engine", MemoryMiB: 2048, ImageBuild: &ImageBuild{Context: ".", Dockerfile: "docker/Enginefile", Args: map[string]string{"BUILD_TAGS": "edge"}},
	}}}}}
}

func TestImageBuildCompilesWithoutSourceControlledRuntime(t *testing.T) {
	spec := imageBuildSpec()
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet})
	if refusal != nil {
		t.Fatal(refusal)
	}
	step := plan.Steps()[0]
	if step.Run != "" || step.Image != "" || step.RequiresFleet() || !reflect.DeepEqual(step.Artifacts, ImageBuildArtifacts()) {
		t.Fatalf("wrong compiled image build: %+v", step)
	}
	spec.Stages[0].Steps[0].ImageBuild.Args["BUILD_TAGS"] = "different"
	if step.ImageBuild.Args["BUILD_TAGS"] != "edge" {
		t.Fatal("compiled build arguments alias the editable manifest")
	}
}

func TestImageBuildRefusesCredentialAndExecutionOverrides(t *testing.T) {
	for name, edit := range map[string]func(*StepSpec){
		"shell":                func(s *StepSpec) { s.Run = "push" },
		"image":                func(s *StepSpec) { s.Image = "attacker:latest" },
		"fleet":                func(s *StepSpec) { s.Placement = PlacementFleet },
		"native":               func(s *StepSpec) { s.Execution = ExecutionNative },
		"secrets":              func(s *StepSpec) { s.Secrets = []string{"PUBLISH_TOKEN"} },
		"pull credential":      func(s *StepSpec) { s.ImagePullSecret = "REGISTRY_AUTH" },
		"cache":                func(s *StepSpec) { cache := []string{"go"}; s.Caches = &cache },
		"artifact override":    func(s *StepSpec) { s.Artifacts = []string{"anything"} },
		"escape":               func(s *StepSpec) { s.ImageBuild.Context = "../other" },
		"absolute dockerfile":  func(s *StepSpec) { s.ImageBuild.Dockerfile = "/etc/passwd" },
		"empty platform":       func(s *StepSpec) { s.Platform = "darwin/arm64" },
		"argument injection":   func(s *StepSpec) { s.ImageBuild.Args["KEY --secret"] = "value" },
		"forged source commit": func(s *StepSpec) { s.ImageBuild.Args["MEMQL_COMMIT"] = "deadbeef" },
		"forged release":       func(s *StepSpec) { s.ImageBuild.Args["MEMQL_RELEASE"] = "9.9.9" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := imageBuildSpec()
			edit(&spec.Stages[0].Steps[0])
			if refusal := Validate(spec); refusal == nil {
				t.Fatal("unsafe image build was admitted")
			}
		})
	}
}
