package sihttp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildSpaceSuggestMessages_TitleOnly verifies the space suggest
// prompt is title-only under the one-assistant model (copresent #124):
// it takes just a description (no agents arg), embeds the description
// in the user turn, and instructs the model to return a single `title`
// field with no agentIds / architecture.
func TestBuildSpaceSuggestMessages_TitleOnly(t *testing.T) {
	msgs := BuildSpaceSuggestMessages("  analyze our Q1 sales pipeline  ")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system, user), got %d", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("expected system+user roles, got %q+%q", msgs[0].Role, msgs[1].Role)
	}

	// User turn carries the trimmed description.
	if !strings.Contains(msgs[1].Content, "analyze our Q1 sales pipeline") {
		t.Errorf("user prompt missing description: %q", msgs[1].Content)
	}

	// System turn must not reference the retired multi-agent model.
	for _, banned := range []string{"agentIds", "architecture", "polyphon", "standard"} {
		if strings.Contains(msgs[0].Content, banned) {
			t.Errorf("system prompt should not mention %q under the one-assistant model: %q", banned, msgs[0].Content)
		}
	}
}

// TestSpaceSuggestSchema_TitleOnly verifies the enforced output schema
// requires only `title` and carries no agentIds / architecture
// properties.
func TestSpaceSuggestSchema_TitleOnly(t *testing.T) {
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(SpaceSuggestSchemaJSON, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	if len(schema.Required) != 1 || schema.Required[0] != "title" {
		t.Errorf("required = %v, want [title]", schema.Required)
	}
	if _, ok := schema.Properties["title"]; !ok {
		t.Errorf("schema missing title property")
	}
	for _, gone := range []string{"agentIds", "architecture"} {
		if _, present := schema.Properties[gone]; present {
			t.Errorf("schema should not carry %q property anymore", gone)
		}
	}
}
