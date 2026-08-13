package memql

// user_preferences_closed_3641_test.go pins @closed onto the block memql#3623
// named as the risk: v1:identity:user.preferences, which holds
// computerUseEnabled -- the per-user computer-use kill switch.
//
// The behaviour of @closed is covered by fixtures in
// component/database/memory-nodes/concept_closed_block_3641_test.go. What is
// checked HERE is the live concept: the annotation is on the real block, and it
// reaches the real emitted schema. A test that only exercised a fixture would
// have passed just as happily with the annotation missing from the tree, which
// is the failure mode this whole hardening pass keeps finding.

import (
	"encoding/json"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestUserPreferencesBlockIsClosed(t *testing.T) {
	concept.ReplaceAll(nil)
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	t.Cleanup(func() {
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(nil)
	})

	user, err := concept.DefaultRegistry().Get("v1:identity:user")
	if err != nil || user == nil {
		t.Fatalf("v1:identity:user must be registered: %v", err)
	}
	raw, err := user.DefinitionSchema()
	if err != nil {
		t.Fatalf("definition schema: %v", err)
	}

	var schema struct {
		Properties map[string]struct {
			AdditionalProperties *bool          `json:"additionalProperties"`
			Properties           map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	prefs, ok := schema.Properties["preferences"]
	if !ok {
		t.Fatal("v1:identity:user declares no `preferences` block; the concept shape changed and this " +
			"guard has silently stopped protecting anything")
	}
	if _, declared := prefs.Properties["computerUseEnabled"]; !declared {
		t.Error("preferences no longer declares computerUseEnabled -- the kill switch this closure " +
			"protects. Check the concept before assuming this test is stale.")
	}
	if prefs.AdditionalProperties == nil || *prefs.AdditionalProperties {
		t.Fatalf("v1:identity:user.preferences does not emit additionalProperties:false. An undeclared "+
			"key inside it is then ACCEPTED, so `computerUseEnbaled` stores beside the real field and "+
			"the computer-use kill switch keeps its old value with nothing reporting anything "+
			"(memql#3623 / memql#3641). additionalProperties = %v", prefs.AdditionalProperties)
	}
}
