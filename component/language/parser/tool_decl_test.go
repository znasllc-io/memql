package parser

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseToolDecl_GoldenPath_QueryHandler locks the most common
// authoring shape: a tool with @description + @handler(type="query",
// query="...") + @executionTime + a small typed-field body.
// Mirrors dsl/memql/tools.memql::searchUsers.
func TestParseToolDecl_GoldenPath_QueryHandler(t *testing.T) {
	source := `@enabled
@handler(type="query", query="concept==v1:memql:backend:user")
@executionTime("fast")
@description("Search for users")
tool searchUsers {
  active  boolean  @description("Filter by active status")
  limit   integer  @default("10") @description("Max results to return")
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if got.Name != "searchUsers" {
		t.Errorf("Name = %q, want searchUsers", got.Name)
	}
	if got.Description != "Search for users" {
		t.Errorf("Description = %q, want \"Search for users\"", got.Description)
	}
	if got.HandlerType != "query" {
		t.Errorf("HandlerType = %q, want query", got.HandlerType)
	}
	if got.HandlerName != "concept==v1:memql:backend:user" {
		t.Errorf("HandlerName = %q, want concept==v1:memql:backend:user", got.HandlerName)
	}
	if got.ExecutionTime != "fast" {
		t.Errorf("ExecutionTime = %q, want fast", got.ExecutionTime)
	}
	if len(got.Fields) != 2 {
		t.Fatalf("Fields = %d, want 2", len(got.Fields))
	}
	if got.Fields[0].Name != "active" || got.Fields[0].Type != "boolean" {
		t.Errorf("Fields[0] = {%q, %q}, want {active, boolean}", got.Fields[0].Name, got.Fields[0].Type)
	}
	if got.Fields[0].Description != "Filter by active status" {
		t.Errorf("Fields[0].Description = %q", got.Fields[0].Description)
	}
	if got.Fields[1].Name != "limit" || got.Fields[1].Type != "integer" {
		t.Errorf("Fields[1] = {%q, %q}, want {limit, integer}", got.Fields[1].Name, got.Fields[1].Type)
	}
	if got.Fields[1].Default != "10" {
		t.Errorf("Fields[1].Default = %q, want 10", got.Fields[1].Default)
	}
}

// TestParseToolDecl_FunctionHandler locks @handler(type="function",
// name="...") parsing.
func TestParseToolDecl_FunctionHandler(t *testing.T) {
	source := `@handler(type="function", name="invokeAgent")
@description("Invoke an agent")
tool invokeAgent {
  agentId  string  @required @description("Target agent id")
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if got.HandlerType != "function" {
		t.Errorf("HandlerType = %q, want function", got.HandlerType)
	}
	if got.HandlerName != "invokeAgent" {
		t.Errorf("HandlerName = %q, want invokeAgent", got.HandlerName)
	}
	if !got.Fields[0].Required {
		t.Errorf("Fields[0].Required = false, want true (@required)")
	}
}

// TestParseToolDecl_WebhookHandler locks @handler(type="webhook",
// url=..., method=...) parsing -- including the method
// upper-casing.
func TestParseToolDecl_WebhookHandler(t *testing.T) {
	source := `@handler(type="webhook", url="https://example.com/hook", method="post")
@description("Webhook tool")
tool webhookTool {
  payload  object  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if got.HandlerType != "webhook" {
		t.Errorf("HandlerType = %q, want webhook", got.HandlerType)
	}
	if got.HandlerURL != "https://example.com/hook" {
		t.Errorf("HandlerURL = %q", got.HandlerURL)
	}
	if got.HandlerMethod != "POST" {
		t.Errorf("HandlerMethod = %q, want POST (upper-cased)", got.HandlerMethod)
	}
}

// TestParseToolDecl_AutoInjectedField locks the @autoInjected
// field-level marker -- the security-relevant flag that drops
// LLM-supplied values for server-stamped fields (memql#107).
func TestParseToolDecl_AutoInjectedField(t *testing.T) {
	source := `@handler(type="function", name="createSomething")
@description("Create something")
tool createSomething {
  spaceId  string  @required @autoInjected @description("Server-stamped")
  name     string  @required @description("User-supplied")
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if !got.Fields[0].AutoInjected {
		t.Error("Fields[0].AutoInjected = false, want true")
	}
	if got.Fields[1].AutoInjected {
		t.Error("Fields[1].AutoInjected = true, want false")
	}
}

// TestParseToolDecl_RateLimit locks
// @rateLimit(maxCalls=N, periodSeconds=M) parsing.
func TestParseToolDecl_RateLimit(t *testing.T) {
	source := `@handler(type="function", name="rateLimitedTool")
@rateLimit(maxCalls=10, periodSeconds=60)
@description("Rate-limited tool")
tool rateLimitedTool {
  q  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if got.RateLimitMaxCalls != 10 {
		t.Errorf("RateLimitMaxCalls = %d, want 10", got.RateLimitMaxCalls)
	}
	if got.RateLimitPeriod != 60 {
		t.Errorf("RateLimitPeriod = %d, want 60", got.RateLimitPeriod)
	}
}

// TestParseToolDecl_DestructiveAndConfirmation locks the boolean
// annotation pair (@destructive + @requiresConfirmation).
func TestParseToolDecl_DestructiveAndConfirmation(t *testing.T) {
	source := `@destructive
@requiresConfirmation
@handler(type="function", name="deleteSpace")
@description("Delete a space")
tool deleteSpace {
  spaceId  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if !got.Destructive {
		t.Error("Destructive = false, want true")
	}
	if !got.RequiresConfirmation {
		t.Error("RequiresConfirmation = false, want true")
	}
}

// TestParseToolDecl_RejectsEmptyBody locks: a tool with no fields
// still parses (some tools take no input), but a malformed header
// errors.
func TestParseToolDecl_EmptyBodyOK(t *testing.T) {
	source := `@handler(type="function", name="noop")
@description("Takes no input")
tool noop {
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if len(got.Fields) != 0 {
		t.Errorf("Fields = %d, want 0", len(got.Fields))
	}
}

// TestParseToolDecl_RejectsMissingName errors when the tool keyword
// isn't followed by an identifier.
func TestParseToolDecl_RejectsMissingName(t *testing.T) {
	source := `tool {
  x  string
}`

	_, err := ParseToolDecl(source)
	if err == nil {
		t.Fatal("expected error for missing tool name, got nil")
	}
	if !strings.Contains(err.Error(), "tool") {
		t.Errorf("error should mention tool, got %v", err)
	}
}

// TestParseToolDecl_ClientExecution locks @clientExecution as a flag
// annotation that flips ToolDecl.ClientExecution. The operator UI
// primitives (uiClick / uiNavigate / etc.) carry this so the agent's
// tool loop dispatches to the browser via ClientToolCall instead of
// trying to execute server-side.
func TestParseToolDecl_ClientExecution(t *testing.T) {
	source := `@clientExecution
@description("Click a UI element in the browser")
tool uiClick {
  selector  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if !got.ClientExecution {
		t.Error("ClientExecution = false, want true")
	}
}

// TestParseToolDecl_MCPExposed locks @mcp as a flag annotation on a tool
// that flips ToolDecl.MCPExposed -- the opt-in for the curated MCP
// connector surface (memql#1596).
func TestParseToolDecl_MCPExposed(t *testing.T) {
	source := `@mcp
@description("Create a note")
tool notesCreate {
  body  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if !got.MCPExposed {
		t.Error("MCPExposed = false, want true")
	}

	// Absent @mcp -> MCPExposed stays false (untagged tools are excluded
	// once any tool is curated).
	plain, err := ParseToolDecl(`@description("x")
tool plainTool {
  arg  string  @required
}`)
	if err != nil {
		t.Fatalf("ParseToolDecl(plain): %v", err)
	}
	if plain.MCPExposed {
		t.Error("MCPExposed = true for untagged tool, want false")
	}
}

// TestParseToolDecl_AllowedRoles locks @allowedRoles("a", "b") as a
// positional string list landing on ToolDecl.AllowedRoles in order.
func TestParseToolDecl_AllowedRoles(t *testing.T) {
	source := `@allowedRoles("assistant", "specialist")
@description("Restricted tool")
tool restrictedTool {
  arg  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	want := []string{"assistant", "specialist"}
	if !reflect.DeepEqual(got.AllowedRoles, want) {
		t.Errorf("AllowedRoles = %v, want %v", got.AllowedRoles, want)
	}
}

// TestParseToolDecl_AllowedRolesSingleValue locks the single-string
// form @allowedRoles("assistant") -- still lands as a 1-element list.
func TestParseToolDecl_AllowedRolesSingleValue(t *testing.T) {
	source := `@allowedRoles("assistant")
@description("Restricted tool")
tool restrictedTool {
  arg  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	want := []string{"assistant"}
	if !reflect.DeepEqual(got.AllowedRoles, want) {
		t.Errorf("AllowedRoles = %v, want %v", got.AllowedRoles, want)
	}
}

// TestParseToolDecl_Scopes locks @scopes("a", "b") as a positional
// string list landing on ToolDecl.Scopes in order. Mirrors the
// dispatcher's superset check (caller scopes must contain all tool
// scopes).
func TestParseToolDecl_Scopes(t *testing.T) {
	source := `@scopes("operator", "navigate")
@description("Operator tool")
tool operatorTool {
  arg  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	want := []string{"operator", "navigate"}
	if !reflect.DeepEqual(got.Scopes, want) {
		t.Errorf("Scopes = %v, want %v", got.Scopes, want)
	}
}

// TestParseToolDecl_AllThreeAnnotations locks the realistic operator-UI
// authoring shape: every operator primitive will carry all three.
func TestParseToolDecl_AllThreeAnnotations(t *testing.T) {
	source := `@clientExecution
@allowedRoles("assistant", "specialist")
@scopes("operator")
@description("Operator UI: click a target element")
tool uiClick {
  selector  string  @required
}`

	got, err := ParseToolDecl(source)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	if !got.ClientExecution {
		t.Error("ClientExecution = false, want true")
	}
	if !reflect.DeepEqual(got.AllowedRoles, []string{"assistant", "specialist"}) {
		t.Errorf("AllowedRoles = %v, want [assistant specialist]", got.AllowedRoles)
	}
	if !reflect.DeepEqual(got.Scopes, []string{"operator"}) {
		t.Errorf("Scopes = %v, want [operator]", got.Scopes)
	}
}
