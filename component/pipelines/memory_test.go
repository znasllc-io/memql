package pipelines

import "testing"

func TestCommandMemorySurvivesCompilationAndRejectsUnsupportedExecution(t *testing.T) {
	spec := &Spec{Image: "toolchain@sha256:test", Stages: []StageSpec{{Name: "checks", Steps: []StepSpec{{Name: "scan", Run: "scan", MemoryMiB: 6144}}}}}
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster})
	if refusal != nil {
		t.Fatal(refusal)
	}
	if plan.Steps()[0].MemoryMiB != 6144 {
		t.Fatal("memory reservation lost")
	}
	for _, mib := range []int{-1, 1, 127, 1048577} {
		spec.Stages[0].Steps[0].MemoryMiB = mib
		if refusal := Validate(spec); refusal == nil {
			t.Fatalf("invalid memory %d accepted", mib)
		}
	}
	spec.Stages[0].Steps[0].MemoryMiB = 6144
	spec.Stages[0].Steps[0].Placement = PlacementFleet
	if refusal := Validate(spec); refusal == nil {
		t.Fatal("fleet memory silently ignored")
	}
}
