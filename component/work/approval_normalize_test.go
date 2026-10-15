package work

import (
	"encoding/json"
	"testing"
)

func TestApprovalSubjectStorageRoundTrip(t *testing.T) {
	subject := map[string]any{"requireLabels": map[string]string{"machineId": "machine"}, "nested": map[string]any{"items": []any{map[string]string{"x": "y"}}}}
	normalized, err := NormalizeApprovalSubject(subject)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(normalized)
	var stored map[string]any
	_ = json.Unmarshal(raw, &stored)
	if ArtifactHash(normalized) != ArtifactHash(stored) {
		t.Fatal("storage changed the approval")
	}
	subject["requireLabels"].(map[string]string)["machineId"] = "other"
	if normalized["requireLabels"].(map[string]any)["machineId"] != "machine" {
		t.Fatal("caller mutated the saved approval")
	}
}
