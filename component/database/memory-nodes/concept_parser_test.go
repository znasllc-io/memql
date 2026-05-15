package memoryNodes

import (
	"encoding/json"
	"log/slog"
	"testing"
)

func TestParseConceptMemQL_Agent(t *testing.T) {
	content := []byte(`
@description("AI agent templates.")
concept Agent {
  name         string  @required @description("Display name.")
  active       bool    @default("true")
  status       enum("active", "archived")    @required

  capabilities {
    avatar       bool  @default("false")
    domains      array(string)
  }

  providerConfig {
    llm {
      model        string
      temperature  float
      maxTokens    int
    }
  }
}
`)

	concept, err := ParseConceptMemQL(content, "v1/copresent/agent")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	if concept.Name != "v1:agents:agent" {
		t.Errorf("Name = %q, want %q", concept.Name, "v1:agents:agent")
	}
	if concept.Description != "AI agent templates." {
		t.Errorf("Description = %q, want %q", concept.Description, "AI agent templates.")
	}
	if concept.SchemaId != "v1:agents:agent" {
		t.Errorf("SchemaId = %q, want %q", concept.SchemaId, "v1:agents:agent")
	}
	if concept.NodeType != NodeTypeObject {
		t.Errorf("NodeType = %q, want %q", concept.NodeType, NodeTypeObject)
	}

	// Verify schema was generated
	raw, ok := concept.Schemas["definition"]
	if !ok {
		t.Fatal("missing definition schema")
	}

	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	if schema["$id"] != "v1:agents:agent" {
		t.Errorf("schema.$id = %v, want %q", schema["$id"], "v1:agents:agent")
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties in schema")
	}

	// Check string property
	nameProp, ok := props["name"].(map[string]any)
	if !ok {
		t.Fatal("missing 'name' property")
	}
	if nameProp["type"] != "string" {
		t.Errorf("name.type = %v, want string", nameProp["type"])
	}

	// Check bool property with default
	activeProp, ok := props["active"].(map[string]any)
	if !ok {
		t.Fatal("missing 'active' property")
	}
	if activeProp["type"] != "boolean" {
		t.Errorf("active.type = %v, want boolean", activeProp["type"])
	}
	if activeProp["default"] != true {
		t.Errorf("active.default = %v, want true", activeProp["default"])
	}

	// Check enum property
	statusProp, ok := props["status"].(map[string]any)
	if !ok {
		t.Fatal("missing 'status' property")
	}
	if statusProp["type"] != "string" {
		t.Errorf("status.type = %v, want string", statusProp["type"])
	}
	enumVals, ok := statusProp["enum"].([]any)
	if !ok || len(enumVals) != 2 {
		t.Errorf("status.enum = %v, want [active, archived]", statusProp["enum"])
	}

	// Check nested object
	capsProp, ok := props["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("missing 'capabilities' property")
	}
	if capsProp["type"] != "object" {
		t.Errorf("capabilities.type = %v, want object", capsProp["type"])
	}
	capsNested, ok := capsProp["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing capabilities.properties")
	}
	domainsProp, ok := capsNested["domains"].(map[string]any)
	if !ok {
		t.Fatal("missing capabilities.domains")
	}
	if domainsProp["type"] != "array" {
		t.Errorf("domains.type = %v, want array", domainsProp["type"])
	}

	// Check deeply nested object
	pcProp, ok := props["providerConfig"].(map[string]any)
	if !ok {
		t.Fatal("missing 'providerConfig' property")
	}
	pcNested, ok := pcProp["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing providerConfig.properties")
	}
	llmProp, ok := pcNested["llm"].(map[string]any)
	if !ok {
		t.Fatal("missing providerConfig.llm")
	}
	llmNested, ok := llmProp["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing providerConfig.llm.properties")
	}
	tempProp, ok := llmNested["temperature"].(map[string]any)
	if !ok {
		t.Fatal("missing temperature property")
	}
	if tempProp["type"] != "number" {
		t.Errorf("temperature.type = %v, want number", tempProp["type"])
	}
	maxProp, ok := llmNested["maxTokens"].(map[string]any)
	if !ok {
		t.Fatal("missing maxTokens property")
	}
	if maxProp["type"] != "integer" {
		t.Errorf("maxTokens.type = %v, want integer", maxProp["type"])
	}

	// Check required fields
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("missing required array")
	}
	requiredNames := make(map[string]bool)
	for _, r := range required {
		requiredNames[r.(string)] = true
	}
	if !requiredNames["name"] || !requiredNames["status"] {
		t.Errorf("required = %v, want to include 'name' and 'status'", required)
	}
}

func TestParseConceptMemQL_WithRelationships(t *testing.T) {
	content := []byte(`
@description("A participant in a space.")
concept Participant {
  spaceId          string  @required
  participantType  enum("human", "si")  @required
  displayName      string  @required

  @relationship(type="parent", field="spaceId", target="v1:cognition:space", direction="outgoing")
}
`)

	concept, err := ParseConceptMemQL(content, "v1/cognition/participant")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	if len(concept.Relationships) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(concept.Relationships))
	}

	rel := concept.Relationships[0]
	if rel.Type != "parent" {
		t.Errorf("relationship.Type = %q, want %q", rel.Type, "parent")
	}
	if rel.Field != "spaceId" {
		t.Errorf("relationship.Field = %q, want %q", rel.Field, "spaceId")
	}
	if rel.TargetConcept != "v1:cognition:space" {
		t.Errorf("relationship.TargetConcept = %q, want %q", rel.TargetConcept, "v1:cognition:space")
	}
}

func TestParseConceptMemQL_Comments(t *testing.T) {
	content := []byte(`
// This is a comment
@description("Test concept.")
concept Test {
  // A property comment
  name  string  @required
  /* block comment */
  age   int
}
`)

	concept, err := ParseConceptMemQL(content, "v1/test/example")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	if concept.Name != "v1:test:example" {
		t.Errorf("Name = %q, want %q", concept.Name, "v1:test:example")
	}

	raw := concept.Schemas["definition"]
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Error("missing 'name' property")
	}
	if _, ok := props["age"]; !ok {
		t.Error("missing 'age' property")
	}
}

// TestLoadAllConcepts verifies that all concept definitions in the embedded
// concepts/ directory parse and validate without errors (e.g. no reserved field names).
func TestLoadAllConcepts(t *testing.T) {
	logger := slog.Default()
	if _, err := loadAllConcepts(logger); err != nil {
		t.Fatalf("loadAllConcepts failed: %v", err)
	}
}
