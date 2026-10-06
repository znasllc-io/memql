package pipelines

import (
	"slices"
	"testing"
)

func TestStepCacheOverridesPermitMixedRuntimes(t *testing.T) {
	empty, npm := []string{}, []string{"npm"}
	spec := &Spec{Image: "toolchain@sha256:test", Platform: "linux/arm64", Caches: []string{"go", "npm"}, Stages: []StageSpec{{Name: "build", Steps: []StepSpec{
		{Name: "inherited", Run: "make test"},
		{Name: "selected", Run: "npm test", Caches: &npm},
		{Name: "uncached", Run: "make verify", Caches: &empty},
		{Name: "native", Execution: ExecutionNative, Platform: "darwin/arm64", Run: "make images", Needs: map[string]bool{"docker": true}, Caches: &empty},
	}}}}
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet})
	if refusal != nil {
		t.Fatal(refusal)
	}
	steps := plan.Steps()
	for n, want := range [][]string{{"go", "npm"}, {"npm"}, {}, {}} {
		if !slices.Equal(steps[n].Caches, want) {
			t.Errorf("step %s caches %v, want %v", steps[n].Name, steps[n].Caches, want)
		}
	}
	if steps[3].Execution != ExecutionNative || steps[3].Image != "" || !steps[3].RequiresFleet() {
		t.Fatalf("native build lost its runtime contract: %+v", steps[3])
	}
	// Later edits to the manifest cannot change the compiled request another
	// replica may execute.
	npm[0] = "changed"
	spec.Caches[0] = "changed"
	fresh := plan.Steps()
	if !slices.Equal(fresh[0].Caches, []string{"go", "npm"}) || !slices.Equal(fresh[1].Caches, []string{"npm"}) {
		t.Fatalf("cache lists alias mutable input: %+v", fresh)
	}

	// An explicit override does not make native caches or services supported.
	spec.Stages[0].Steps[3].Caches = &npm
	if refusal := Validate(spec); refusal == nil || refusal.Code != CodeStepInvalid {
		t.Fatalf("native cache override was admitted: %v", refusal)
	}
	spec.Stages[0].Steps[3].Caches = &empty
	spec.Services = map[string]Service{"db": {Image: "postgres"}}
	spec.Stages[0].Steps[3].Services = []string{"db"}
	if refusal := Validate(spec); refusal == nil || refusal.Code != CodeStepInvalid {
		t.Fatalf("empty caches admitted native services: %v", refusal)
	}
}

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
