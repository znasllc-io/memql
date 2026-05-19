package memql

import (
	"strings"
	"testing"
)

// TestParseToolMemQL_GoldenPath locks the canonical struct-form
// tool syntax with a function-shaped handler and typed fields.
func TestParseToolMemQL_GoldenPath(t *testing.T) {
	src := []byte(`@enabled
@description("Search for users across the organization.")
@handler(type="function", name="searchUsers")
tool searchUsers {
  query   string  @required @description("Search query for user lookup.")
  limit   int     @default("10") @description("Max results to return.")
}`)

	tools, err := parseToolMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0]
	if tool.Name != "searchUsers" {
		t.Errorf("Name = %q, want searchUsers", tool.Name)
	}
	if tool.Handler == nil {
		t.Fatal("Handler is nil, want non-nil")
	}
	if tool.Handler.Type != "function" {
		t.Errorf("Handler.Type = %q, want function", tool.Handler.Type)
	}
	if tool.Handler.FunctionName != "searchUsers" {
		t.Errorf("Handler.FunctionName = %q, want searchUsers", tool.Handler.FunctionName)
	}
}

// TestParseToolMemQL_RejectsLegacyFuncForm locks the deletion of
// the `func (Tool) name { ... }` form. Migration hint must mention
// the canonical `tool name { ... }` shape.
func TestParseToolMemQL_RejectsLegacyFuncForm(t *testing.T) {
	src := []byte(`@description("legacy form")
func (Tool) toolFoo() {
  query string @required
}`)

	_, err := parseToolMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "func (Tool)") {
		t.Errorf("error should mention `func (Tool)` retirement, got %v", err)
	}
	if !strings.Contains(err.Error(), "tool name") {
		t.Errorf("error should hint at the canonical `tool name { ... }` form, got %v", err)
	}
}

// TestParseToolMemQL_RejectsMissingTool locks the rule: a file with
// no `tool` declaration is an error.
func TestParseToolMemQL_RejectsMissingTool(t *testing.T) {
	src := []byte(`@description("nothing here")`)

	_, err := parseToolMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no tool definition") {
		t.Errorf("error should describe missing tool definition, got %v", err)
	}
}

// TestParseToolMemQL_QueryHandler exercises the query-handler form.
func TestParseToolMemQL_QueryHandler(t *testing.T) {
	src := []byte(`@enabled
@description("List active spaces.")
@handler(type="query", query="queryActiveSpaces")
tool listSpaces {
  filter string @default("all")
}`)

	tools, err := parseToolMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Handler.Type != "query" {
		t.Errorf("Handler.Type = %q, want query", tools[0].Handler.Type)
	}
	if tools[0].Handler.Query != "queryActiveSpaces" {
		t.Errorf("Handler.Query = %q, want queryActiveSpaces", tools[0].Handler.Query)
	}
}
