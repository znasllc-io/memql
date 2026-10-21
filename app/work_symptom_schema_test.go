package app

import (
	"encoding/json"
	"testing"
)

func TestSymptomSchemaIsValidForStrictAppOutput(t *testing.T) {
	var schema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(classifySymptomSchema, &schema); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{}
	for _, key := range schema.Required {
		required[key] = true
	}
	for key := range schema.Properties {
		if !required[key] {
			t.Errorf("strict output must require property %s", key)
		}
	}
	if schema.AdditionalProperties {
		t.Fatal("strict output must close the schema")
	}
}
