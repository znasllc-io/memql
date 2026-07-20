package memql

// Phase 4 (#1534) @mcp promotion tests: the annotation parses onto a Function,
// and the engine reflects promoted query/mutation constructs into MCP tool
// descriptors with a JSON-schema inputSchema. Pure -- no live DB.

import "testing"

// @mcp on a mutation flows through the compile path onto Function.MCPPromoted
// (the bundle reuses the Phase-3 session author path, which compiles the
// mutation to an executable *Function).
func TestAtMcpAnnotation_ParsedOntoFunction(t *testing.T) {
	const promotedMutation = `use mcpsess.concepts.{ mcpWidget }

@mcp
@description("Create a session widget (MCP-promoted)")
mutate mcpWidget mutationCreatePromotedWidget {
  args {
    widgetId  string  @required
  }
  insert {
    id:    canonicalId(args.widgetId, mcpWidget)
    label: "x"
  }
}`
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionConceptSrc+"\n\n"+promotedMutation); err != nil {
		t.Fatalf("author: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "mutation", "mutationCreatePromotedWidget")
	if !ok {
		t.Fatal("mutation not registered")
	}
	fn, isFn := c.Compiled.(*Function)
	if !isFn || fn == nil {
		t.Fatalf("Compiled = %T, want *Function", c.Compiled)
	}
	if !fn.MCPPromoted {
		t.Error("@mcp should set Function.MCPPromoted")
	}
}

// A non-@mcp construct is NOT promoted.
func TestAtMcpAnnotation_AbsentByDefault(t *testing.T) {
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionConceptSrc+"\n\n"+sessionMutationSrc); err != nil {
		t.Fatalf("author: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "mutation", "mutationCreateMcpWidget")
	if fn, ok := c.Compiled.(*Function); ok && fn.MCPPromoted {
		t.Error("a construct without @mcp must not be promoted")
	}
}

// MCPPromotedFunctionTools reflects only @mcp query/mutation, with a JSON-schema
// inputSchema; MCPPromotedFunctionKind routes a promoted name to its kind.
func TestMCPPromotedFunctionReflection(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	// A promoted query with an args schema.
	promoted := &Function{
		Name: "promotedQuery", FunctionKind: "query", Enabled: true, MCPPromoted: true,
		Description: "find things",
		ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{
			{Name: "limit", Type: "number", Optional: true},
			{Name: "kind", Type: "string", Optional: false, Enum: []any{"a", "b"}},
		}},
	}
	plain := &Function{Name: "plainQuery", FunctionKind: "query", Enabled: true} // not promoted
	logicPromoted := &Function{Name: "promotedLogic", FunctionKind: "logic", Enabled: true, MCPPromoted: true}
	for _, fn := range []*Function{promoted, plain, logicPromoted} {
		if err := e.functions.Upsert(fn); err != nil {
			t.Fatalf("seed %s: %v", fn.Name, err)
		}
	}

	tools := e.MCPPromotedFunctionTools()
	if len(tools) != 1 {
		t.Fatalf("expected exactly 1 promoted tool (query only, not logic/plain), got %d: %+v", len(tools), tools)
	}
	tool := tools[0]
	if tool["name"] != "promotedQuery" || tool["description"] != "find things" {
		t.Errorf("unexpected tool descriptor: %+v", tool)
	}
	schema, _ := tool["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["limit"]; !ok {
		t.Error("inputSchema missing 'limit' property")
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "kind" {
		t.Errorf("required = %v, want [kind]", required)
	}
	if kindProp, _ := props["kind"].(map[string]any); kindProp["type"] != "string" {
		t.Errorf("kind property type = %v, want string", kindProp["type"])
	}

	// Kind routing: promoted query yes, logic no, plain no, unknown no.
	if k, ok := e.MCPPromotedFunctionKind("promotedQuery"); !ok || k != "query" {
		t.Errorf("promotedQuery kind = %q,%v", k, ok)
	}
	if _, ok := e.MCPPromotedFunctionKind("promotedLogic"); ok {
		t.Error("logic should not be a promoted MCP function")
	}
	if _, ok := e.MCPPromotedFunctionKind("plainQuery"); ok {
		t.Error("non-@mcp function should not route")
	}
	if _, ok := e.MCPPromotedFunctionKind("nope"); ok {
		t.Error("unknown function should not route")
	}
}

func TestJSONSchemaType(t *testing.T) {
	cases := map[string]string{
		"string": "string", "number": "number", "int": "number", "integer": "number",
		"bool": "boolean", "boolean": "boolean", "object": "object", "array": "array",
		"any": "", "": "", "weird": "",
	}
	for in, want := range cases {
		if got := jsonSchemaType(in); got != want {
			t.Errorf("jsonSchemaType(%q) = %q, want %q", in, got, want)
		}
	}
}

// A @disabled @mcp function must drop out of the ADVERTISED surface entirely
// (memql#2647): the lifecycle annotation hides it from functions() and from
// function-backed tool registration (function_tools.go), and the promote path
// must be equally honest -- not advertise a tool the #2605 execution gate will
// refuse at call time. The enabled peer stays advertised.
func TestMCPPromotedFunctionTools_DisabledDropped(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	for _, fn := range []*Function{
		{Name: "disabledPromotedQuery", FunctionKind: "query", Enabled: false, MCPPromoted: true},
		{Name: "enabledPromotedQuery", FunctionKind: "query", Enabled: true, MCPPromoted: true},
		// Both routable kinds: the drop must not be query-scoped.
		{Name: "disabledPromotedMutation", FunctionKind: "mutation", Enabled: false, MCPPromoted: true},
		{Name: "enabledPromotedMutation", FunctionKind: "mutation", Enabled: true, MCPPromoted: true},
	} {
		if err := e.functions.Upsert(fn); err != nil {
			t.Fatalf("seed %s: %v", fn.Name, err)
		}
	}

	tools := e.MCPPromotedFunctionTools()
	if len(tools) != 2 {
		t.Fatalf("expected exactly the 2 enabled peers advertised, got %d: %+v", len(tools), tools)
	}
	if tools[0]["name"] != "enabledPromotedMutation" || tools[1]["name"] != "enabledPromotedQuery" {
		t.Errorf("advertised tools = %v, %v; want enabledPromotedMutation, enabledPromotedQuery (sorted)", tools[0]["name"], tools[1]["name"])
	}

	// The ROUTER deliberately keeps routing the @disabled name: a direct
	// call of the no-longer-advertised tool must reach the #2605 execution
	// gate's 'function is disabled' refusal, not an unknown-tool error.
	if kind, ok := e.MCPPromotedFunctionKind("disabledPromotedQuery"); !ok || kind != "query" {
		t.Errorf("MCPPromotedFunctionKind(disabled) = (%q, %v), want (query, true): the router must keep routing @disabled names", kind, ok)
	}
}
