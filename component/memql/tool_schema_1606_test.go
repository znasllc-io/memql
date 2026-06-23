package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/znasllc-io/memql/component/language/ast"
)

// compileSchema mirrors the production validation path
// (executor_builtin.go): Draft 2019, inline resource.
func compileSchema(t *testing.T, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2019
	if err := c.AddResource("validate://tool/1606", strings.NewReader(string(raw))); err != nil {
		t.Fatalf("add resource: %v\nschema=%s", err, raw)
	}
	s, err := c.Compile("validate://tool/1606")
	if err != nil {
		t.Fatalf("compile: %v\nschema=%s", err, raw)
	}
	return s
}

// Test #1606 part 1: a DSL `tool` array field with no declared item type
// must accept scalar (string) items, not force objects. This is the
// notesCreate.tags failure: `items/type: expected object, but got
// string`.
func TestToolDeclToTool_ScalarArrayItemsAccepted(t *testing.T) {
	decl := &ast.ToolDecl{
		Name:        "notesCreate",
		Description: "create a note",
		Fields: []ast.ToolFieldDecl{
			{Name: "tags", Type: "array", Description: "Optional list of string tags"},
		},
	}
	tools, err := toolDeclToTool(decl, "test")
	if err != nil {
		t.Fatalf("toolDeclToTool: %v", err)
	}
	schema := compileSchema(t, tools[0].InputSchema)

	// A list of string tags must validate.
	if err := schema.Validate(map[string]any{"tags": []any{"a", "b"}}); err != nil {
		t.Fatalf("string-array args rejected by tool schema: %v\nschema=%s", err, tools[0].InputSchema)
	}
	// An array of objects must still validate (permissive items).
	if err := schema.Validate(map[string]any{"tags": []any{map[string]any{"k": "v"}}}); err != nil {
		t.Fatalf("object-array args rejected by tool schema: %v", err)
	}
}

// Test #1606 part 2: a free-form object args field (no declared
// sub-fields) must allow arbitrary keys. This is the
// updateTodo.payload failure: every payload key rejected by
// additionalProperties:false.
func TestJSONSchemaForArgsField_FreeFormObjectIsOpen(t *testing.T) {
	got := jsonSchemaForArgsField(&FunctionArgsField{Type: "object"})
	if ap, ok := got["additionalProperties"].(bool); !ok || !ap {
		t.Fatalf("free-form object additionalProperties = %v, want true; schema=%v", got["additionalProperties"], got)
	}

	// Compile a tool whose `payload` is a free-form object and confirm
	// arbitrary keys validate end-to-end.
	raw, err := toolInputSchemaFromArgs(&ArgsSchemaConfig{
		Fields: []*FunctionArgsField{
			{Name: "todoId", Type: "string"},
			{Name: "payload", Type: "object"}, // free-form partial-update payload
		},
	})
	if err != nil {
		t.Fatalf("toolInputSchemaFromArgs: %v", err)
	}
	schema := compileSchema(t, raw)
	args := map[string]any{
		"todoId":  "v1:planner:todo:abc",
		"payload": map[string]any{"done": true, "dueAt": "2026-01-01", "priority": "high", "title": "x"},
	}
	if err := schema.Validate(args); err != nil {
		t.Fatalf("partial-update payload rejected: %v\nschema=%s", err, raw)
	}
}

// Test #1606 guardrail: an object WITH declared sub-fields stays CLOSED,
// so the schema is still descriptive and rejects undeclared keys.
func TestJSONSchemaForArgsField_DeclaredObjectStaysClosed(t *testing.T) {
	got := jsonSchemaForArgsField(&FunctionArgsField{
		Type: "object",
		Nested: []*FunctionArgsField{
			{Name: "title", Type: "string"},
		},
	})
	if ap, ok := got["additionalProperties"].(bool); !ok || ap {
		t.Fatalf("declared object additionalProperties = %v, want false; schema=%v", got["additionalProperties"], got)
	}
}

// Test #1606 guardrail: an explicit @additionalProperties still wins
// over the free-form default.
func TestJSONSchemaForArgsField_ExplicitAdditionalPropertiesWins(t *testing.T) {
	no := false
	got := jsonSchemaForArgsField(&FunctionArgsField{Type: "object", AdditionalProperties: &no})
	if ap, ok := got["additionalProperties"].(bool); !ok || ap {
		t.Fatalf("explicit additionalProperties=false ignored: got %v", got["additionalProperties"])
	}
}

// Test #1606: the function-tools array path with no item type also
// accepts scalars (it already emits permissive items; lock it in).
func TestJSONSchemaForArgsField_BareArrayAcceptsScalars(t *testing.T) {
	raw, err := toolInputSchemaFromArgs(&ArgsSchemaConfig{
		Fields: []*FunctionArgsField{{Name: "tags", Type: "array"}},
	})
	if err != nil {
		t.Fatalf("toolInputSchemaFromArgs: %v", err)
	}
	schema := compileSchema(t, raw)
	if err := schema.Validate(map[string]any{"tags": []any{"a", "b"}}); err != nil {
		t.Fatalf("function-tool bare array rejected scalar items: %v\nschema=%s", err, raw)
	}
}
