package worker

import (
	"encoding/json"
	"testing"
)

func TestActionContractsValidateAndSurvivePersistence(t *testing.T) {
	const action = "workerHost.pipeline_step"
	d := CapabilityDescriptor{Platform: "linux", Architecture: "arm64", DisplayServer: "none", SchemaVersion: 1, ActionContracts: ActionContracts{action: 2}}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCapabilityDescriptor(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	stored := ActionContractsFromMap(parsed.AsMap())
	if NativePlatformFromMap(parsed.AsMap()) != "linux/arm64" || NativePlatformFromMap(nil) != "" {
		t.Fatal("native platform was lost or invented")
	}
	if !stored.Supports(action, 2) || stored.Supports(action, 1) || stored.Supports(action, 0) || stored.Supports("workerHost.exec", 2) {
		t.Fatalf("contracts: %v", stored)
	}
	for _, version := range []int{0, -1, 65536} {
		d.ActionContracts[action] = version
		raw, _ = json.Marshal(d)
		if _, err = ParseCapabilityDescriptor(string(raw)); err == nil {
			t.Errorf("accepted version %d", version)
		}
	}
	if ActionContractsFromMap(map[string]any{"actionContracts": map[string]any{action: 2}}).Supports(action, 2) {
		t.Fatal("unvalidated stored descriptor admitted")
	}
	if (ActionContracts(nil)).Supports(action, 2) {
		t.Fatal("old worker admitted")
	}
}
