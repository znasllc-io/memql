package memql

import (
	"encoding/json"
	"testing"
)

func TestInstalledSkillBaselineRequiresExactReviewedContent(t *testing.T) {
	registry := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(nil, registry); err != nil {
		t.Fatal(err)
	}
	engine := &MemQLEngine{seeds: registry}
	for _, id := range []string{"software-development", "distributed-systems", "regression-testing", "go-backend-engineering"} {
		def, ok := registry.Get(id)
		if !ok {
			t.Fatalf("missing baseline %s", id)
		}
		row := buildArgsFromBody(def.Body, def.UseConcept, id, "")
		row["id"] = id
		// Use the number/array forms received from the real JSON query boundary.
		raw, _ := json.Marshal(row)
		_ = json.Unmarshal(raw, &row)
		if !engine.IsInstalledSkillBaseline(row) {
			t.Fatalf("shipped baseline %s rejected: %+v", id, row)
		}
		row["scripts"] = []any{"unreviewed.sh"}
		if engine.IsInstalledSkillBaseline(row) {
			t.Fatal("accepted added executable content as a baseline")
		}
		delete(row, "scripts")
		row["instructions"].(map[string]any)["body"] = "Forged advice under the same version and predefined flag."
		if engine.IsInstalledSkillBaseline(row) {
			t.Fatal("accepted changed bytes as release content")
		}
	}
}
