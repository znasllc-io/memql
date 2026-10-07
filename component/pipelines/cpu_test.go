package pipelines

import (
	"encoding/json"
	"testing"
)

func TestCommandCPUIsCompiledAndBoundToTheWireContract(t *testing.T) {
	for _, cpu := range []int{0, 1, 250, 4000, 256000} {
		spec := &Spec{Image: "toolchain@sha256:test", Stages: []StageSpec{{Name: "checks", Steps: []StepSpec{{Name: "scan", Run: "scan", CPUMilli: cpu}}}}}
		plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster})
		if refusal != nil {
			t.Fatal(refusal)
		}
		encoded, err := json.Marshal(plan.Steps()[0])
		if err != nil {
			t.Fatal(err)
		}
		var decoded Step
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.CPUMilli != cpu {
			t.Fatalf("CPU reservation lost in compilation or serialization: %+v, %v", decoded, err)
		}
	}
	for _, step := range []StepSpec{
		{Name: "scan", Run: "scan", CPUMilli: -1},
		{Name: "scan", Run: "scan", CPUMilli: 256001},
		{Name: "scan", Run: "scan", CPUMilli: 4000, Placement: PlacementFleet, Platform: "linux/arm64"},
		{Name: "scan", Run: "scan", CPUMilli: 4000, Execution: ExecutionNative, Platform: "darwin/arm64"},
	} {
		spec := &Spec{Image: "toolchain@sha256:test", Stages: []StageSpec{{Name: "checks", Steps: []StepSpec{step}}}}
		if refusal := Validate(spec); refusal == nil {
			t.Fatalf("unsupported CPU contract accepted: %+v", step)
		}
	}
}
