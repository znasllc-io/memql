package pipelines

import "testing"

func TestCompilePreservesExecutionAndPlatform(t *testing.T) {
	spec := &Spec{Image: "toolchain@sha256:test", Platform: "linux/arm64", Stages: []StageSpec{{Name: "build", Steps: []StepSpec{
		{Name: "container", Run: "make test"},
		{Name: "native", Execution: ExecutionNative, Platform: "darwin/arm64", Run: "make app"},
	}}}}
	input := CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet}
	plan, refusal := Compile(spec, input)
	if refusal != nil {
		t.Fatal(refusal)
	}
	steps := plan.Steps()
	if len(steps) != 2 || steps[0].Execution != ExecutionContainer || steps[0].Platform != "linux/arm64" || steps[0].Image != spec.Image || steps[0].RequiresFleet() {
		t.Fatalf("container contract changed: %+v", steps)
	}
	if steps[1].Execution != ExecutionNative || steps[1].Platform != "darwin/arm64" || steps[1].Image != "" || !steps[1].RequiresFleet() {
		t.Fatalf("native work was reinterpreted: %+v", steps[1])
	}
	spec.Stages[0].Steps = spec.Stages[0].Steps[:1]
	spec.Stages[0].Steps[0].Placement = PlacementFleet
	plan, refusal = Compile(spec, input)
	if refusal != nil || !plan.Steps()[0].RequiresFleet() || plan.Steps()[0].Execution != ExecutionContainer {
		t.Fatalf("fleet placement changed execution: %+v %v", plan, refusal)
	}
	input.Compute = ComputeCluster
	if _, refusal := Compile(spec, input); refusal == nil || refusal.Code != CodeFleetNotConsented {
		t.Fatalf("native step without fleet consent: %v", refusal)
	}
}

func TestRuntimeContractRefusesAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		execution, platform string
		fleet               bool
	}{
		{"", "linux/arm64", false}, {"auto", "linux/arm64", true},
		{ExecutionNative, "", true}, {ExecutionContainer, "", true},
		{ExecutionContainer, "darwin/arm64", false}, {ExecutionNative, "linux/x86_64", true},
	} {
		if err := CheckExecution(tc.execution, tc.platform, tc.fleet); err == nil {
			t.Errorf("admitted %+v", tc)
		}
	}
	for _, change := range []func(*Spec){
		func(s *Spec) { s.Caches = []string{"go"} },
		func(s *Spec) {
			s.Services = map[string]Service{"db": {Image: "postgres"}}
			s.Stages[0].Steps[0].Services = []string{"db"}
		},
	} {
		spec := &Spec{Stages: []StageSpec{{Name: "build", Steps: []StepSpec{{Name: "native", Execution: ExecutionNative, Platform: "linux/arm64", Run: "make"}}}}}
		change(spec)
		if refusal := Validate(spec); refusal == nil {
			t.Fatal("native step accepted unimplemented container state")
		}
	}
}
