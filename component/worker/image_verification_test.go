package worker

import (
	"encoding/json"
	"testing"
)

func TestImageVerificationDescriptorSurvivesPersistenceAndRefusesInventedSupport(t *testing.T) {
	for name, mutate := range map[string]func(*CapabilityDescriptor){
		"valid":           func(*CapabilityDescriptor) {},
		"Mac":             func(d *CapabilityDescriptor) { d.Platform = "darwin" },
		"ARM":             func(d *CapabilityDescriptor) { d.Architecture = "arm64" },
		"old worker":      func(d *CapabilityDescriptor) { d.ActionContracts["workerHost.pipeline_step"] = 4 },
		"unknown feature": func(d *CapabilityDescriptor) { d.PipelineImageVerification = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			d := CapabilityDescriptor{Platform: "linux", Architecture: "amd64", DisplayServer: "none", SchemaVersion: 1, PipelineImageVerification: 1, ActionContracts: ActionContracts{"workerHost.pipeline_step": 5}}
			mutate(&d)
			raw, _ := json.Marshal(d)
			parsed, err := ParseCapabilityDescriptor(string(raw))
			if name != "valid" {
				if err == nil {
					t.Fatal("unsupported capability accepted")
				}
				return
			}
			if err != nil || PipelineImageVerificationFromMap(parsed.AsMap()) != 1 {
				t.Fatalf("lost capability: %+v %v", parsed, err)
			}
		})
	}
	if PipelineImageVerificationFromMap(map[string]any{"pipelineImageVerification": 1}) != 0 {
		t.Fatal("unvalidated partial descriptor admitted")
	}
}
