package memql

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolInputSchemaFromArgs(t *testing.T) {
	schemaBytes, err := toolInputSchemaFromArgs(&ArgsSchemaConfig{
		Fields: []*FunctionArgsField{
			{Name: "userId", Type: "string", Optional: false},
			{Name: "tags", Type: "array", Optional: true},
			{
				Name:     "options",
				Type:     "object",
				Optional: true,
				Nested: []*FunctionArgsField{
					{Name: "limit", Type: "number", Optional: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("schema build failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(schemaBytes, &decoded); err != nil {
		t.Fatalf("schema json decode failed: %v", err)
	}

	if decoded["type"] != "object" {
		t.Fatalf("expected top-level object schema, got %v", decoded["type"])
	}
	props, _ := decoded["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("expected properties map")
	}
	if _, ok := props["userId"]; !ok {
		t.Fatalf("expected userId in properties")
	}
	userSchema, _ := props["userId"].(map[string]any)
	if userSchema == nil || userSchema["type"] != "string" {
		t.Fatalf("expected userId type string, got %v", userSchema)
	}

	tagsSchema, _ := props["tags"].(map[string]any)
	if tagsSchema == nil || tagsSchema["type"] != "array" {
		t.Fatalf("expected tags type array, got %v", tagsSchema)
	}
	if _, ok := tagsSchema["items"]; !ok {
		t.Fatalf("expected tags schema to include items (OpenAI tool schema requirement), got %v", tagsSchema)
	}

	optsSchema, _ := props["options"].(map[string]any)
	if optsSchema == nil || optsSchema["type"] != "object" {
		t.Fatalf("expected options type object, got %v", optsSchema)
	}
	optsProps, _ := optsSchema["properties"].(map[string]any)
	if optsProps == nil {
		t.Fatalf("expected options.properties")
	}
	limitSchema, _ := optsProps["limit"].(map[string]any)
	if limitSchema == nil || limitSchema["type"] != "number" {
		t.Fatalf("expected options.limit type number, got %v", limitSchema)
	}
}

func TestRegisterFunctionToolsDoesNotOverrideExistingTool(t *testing.T) {
	fnRegistry := newFunctionRegistry()
	if err := fnRegistry.add(&Function{
		Name:         "activeSpaces",
		Description:  "fn desc",
		Enabled:      true,
		FunctionKind: "query",
	}); err != nil {
		t.Fatalf("add function: %v", err)
	}

	toolRegistry := newToolRegistry()
	explicit := &Tool{
		Name:        "activeSpaces",
		Description: "explicit tool wins",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Handler:     &ToolHandler{Type: "query", Query: "concept==v1:examples:world"},
		Origin:      "explicit.json",
	}
	if err := toolRegistry.add(explicit); err != nil {
		t.Fatalf("add explicit tool: %v", err)
	}

	registerFunctionTools(nil, fnRegistry, toolRegistry)

	got, err := toolRegistry.Get("activeSpaces")
	if err != nil {
		t.Fatalf("get tool: %v", err)
	}
	if got.Origin != "explicit.json" {
		t.Fatalf("expected explicit tool to remain, origin=%q", got.Origin)
	}
	if got.Description != "explicit tool wins" {
		t.Fatalf("expected explicit tool description to remain, got %q", got.Description)
	}
}

func TestExtractLeadingCommentBlock(t *testing.T) {
	content := strings.Join([]string{
		"// title",
		"//",
		"// more detail",
		"",
		"@enabled",
		"func (Query) x() { concept==v1:test }",
	}, "\n")
	doc := extractLeadingCommentBlock(content)
	if !strings.Contains(doc, "title") || !strings.Contains(doc, "more detail") {
		t.Fatalf("unexpected doc: %q", doc)
	}
}

func TestLoadToolRegistryLoadsRootFiles(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	reg, err := loadToolRegistry(nil)
	if err != nil {
		t.Fatalf("loadToolRegistry: %v", err)
	}
	if reg == nil {
		t.Fatalf("expected registry")
	}
	if !reg.Has("searchUsers") {
		t.Fatalf("expected searchUsers tool to be loaded")
	}
	if !reg.Has("describeFunction") {
		t.Fatalf("expected describeFunction tool to be loaded")
	}
}

func TestFunctionSchemaReferenceExcerptNonEmpty(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	excerpt := functionSchemaReferenceExcerpt()
	if strings.TrimSpace(excerpt) == "" {
		t.Fatalf("expected non-empty excerpt")
	}
	if !strings.Contains(excerpt, "STRUCT FORM") {
		t.Fatalf("expected excerpt to include STRUCT FORM heading, got: %q", excerpt)
	}
}
