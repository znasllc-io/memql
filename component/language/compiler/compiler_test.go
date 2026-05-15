package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/visionarys-io/memql/component/language/parser"
)

func TestCompileSource_SimpleQuery(t *testing.T) {
	source := `
func (Query) queryActiveUsers(role any) {
	concept==v1:user;?.payload.role==args.role
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	fn := result.Functions[0]
	if fn.Name != "queryActiveUsers" {
		t.Errorf("Expected name 'queryActiveUsers', got %q", fn.Name)
	}
	if fn.Type != "query" {
		t.Errorf("Expected type 'query', got %q", fn.Type)
	}
}

func TestCompileSource_Automation(t *testing.T) {
	source := `
@enabled
@schedule("*/30 * * * *")
func (Automation) leadProcessor(_ any) {
	fetchLeads := query {
		concept==v1:lead;payload.active==true
	}

	processLeads := query {
		concept==v1:lead
	}

	return processLeads
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Automations) != 1 {
		t.Fatalf("Expected 1 automation, got %d", len(result.Automations))
	}

	auto := result.Automations[0]
	if auto.Name != "leadProcessor" {
		t.Errorf("Expected name 'leadProcessor', got %q", auto.Name)
	}
}

func TestTranspileAutomation(t *testing.T) {
	source := `
@enabled
func (Automation) testAuto(_ any) {
	step1 := query {
		concept==v1:test
	}
	return step1
}`

	jsonOutput, err := TranspileAutomation(source)
	if err != nil {
		t.Fatalf("TranspileAutomation error: %v", err)
	}

	// Verify it's valid JSON
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonOutput), &parsed); err != nil {
		t.Fatalf("Invalid JSON output: %v", err)
	}

	if parsed["name"] != "testAuto" {
		t.Errorf("Expected name 'testAuto', got %v", parsed["name"])
	}

	if parsed["enabled"] != true {
		t.Errorf("Expected enabled=true, got %v", parsed["enabled"])
	}
}

func TestTranspileAutomation_ForEachBareVarReferencesNotQuoted(t *testing.T) {
	source := `
@enabled
func (Automation) autoJoinAIExample(_ any) {
  getAgents := query {
    concept==v1:agents:agent;
    payload.active==true
  }

  for item := range getAgents.result.Bundle.nodes {
    checkExisting := query {
      concept==v1:cognition:participant;
      payload.agentId==item.id;
      payload.status!="left"
    }
  }

  return getAgents
}`

	jsonOutput, err := TranspileAutomation(source)
	if err != nil {
		t.Fatalf("TranspileAutomation error: %v", err)
	}

	// Compiler should not pre-quote bare forEach item references like item.id,
	// otherwise the runtime evaluator cannot resolve them.
	if strings.Contains(jsonOutput, `payload.agentId=="item.id"`) {
		t.Fatalf("expected item.id to remain unquoted in compiled query, got: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, `payload.agentId==item.id`) {
		t.Fatalf("expected compiled query to contain unquoted item.id, got: %s", jsonOutput)
	}
}

func TestDetectFileType(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected FileType
	}{
		{
			name:     "automation",
			source:   "func (Automation) test(_ any) { }",
			expected: FileTypeAutomation,
		},
		{
			name:     "query function",
			source:   "func (Query) test() { concept==v1:test }",
			expected: FileTypeQuery,
		},
		{
			name:     "mutation function",
			source:   "func (Mutation) test() { insert(\"v1:test\") }",
			expected: FileTypeMutation,
		},
		{
			name:     "plain query",
			source:   "concept==v1:test",
			expected: FileTypeQuery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fileType, err := DetectFileType(tt.source)
			if err != nil {
				t.Fatalf("DetectFileType error: %v", err)
			}
			if fileType != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, fileType)
			}
		})
	}
}

func TestValidateMemQL(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{
			name:    "valid query",
			source:  "concept==v1:test",
			wantErr: false,
		},
		{
			name: "valid automation",
			source: `func (Automation) test(_ any) {
				step1 := query { concept==v1:test }
				return step1
			}`,
			wantErr: false,
		},
		{
			name:    "invalid - unterminated string",
			source:  `concept=="unclosed`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMemQL(tt.source)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMemQL() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompileSource_EmitsNamingWarnings(t *testing.T) {
	// Unprefixed Query function must emit a naming-prefix warning.
	source := `
args { role string }
func (Query) activeUsers(args any) {
	concept==v1:user
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Warnings) == 0 {
		t.Fatalf("expected naming warnings, got none")
	}
	if result.Warnings[0].Rule != "naming.query-prefix" {
		t.Fatalf("unexpected rule: %s", result.Warnings[0].Rule)
	}
}

func TestCompileSource_EmitsInlineDeprecationWarnings(t *testing.T) {
	source := `
@enabled
func (Automation) testAuto(_ any) {
	step1 := query {
		concept==v1:test
	}
	return step1
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	found := false
	for _, warning := range result.Warnings {
		if warning.Rule == "deprecation.inline-block-step" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected inline deprecation warning")
	}
}

func TestCompileSource_FunctionCallStepInAutomation(t *testing.T) {
	source := `
@enabled
func (Automation) testAuto(_ any) {
	checkUser := queryUserById(userId=event.payload.userId)
	return checkUser
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}
	if len(result.Automations) != 1 {
		t.Fatalf("expected 1 automation, got %d", len(result.Automations))
	}

	stepsAny, ok := result.Automations[0].JSON["steps"]
	if !ok {
		t.Fatalf("compiled automation missing steps")
	}
	steps, ok := stepsAny.([]map[string]any)
	if !ok || len(steps) == 0 {
		t.Fatalf("compiled steps in unexpected format: %T", stepsAny)
	}
	if steps[0]["type"] != "function" {
		t.Fatalf("expected function step type, got %v", steps[0]["type"])
	}
	functionConfig, ok := steps[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected function config object, got %T", steps[0]["function"])
	}
	if functionConfig["name"] != "queryUserById" {
		t.Fatalf("expected function name queryUserById, got %v", functionConfig["name"])
	}
}

func TestCompileFile_StrictWarnings(t *testing.T) {
	// Unprefixed Query function triggers strict naming lint.
	source := `
args { role string }
func (Query) activeUsers(args any) {
	concept==v1:user
}`

	ast, err := ParseMemQL(source)
	if err != nil {
		t.Fatalf("ParseMemQL error: %v", err)
	}
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected *parser.File, got %T", ast)
	}

	compiler := New(Config{StrictWarnings: true})
	_, err = compiler.CompileFile(file)
	if err == nil {
		t.Fatalf("expected strict warning error")
	}
	if _, ok := err.(*LintError); !ok {
		t.Fatalf("expected LintError, got %T", err)
	}
}

func TestGetAutomationName(t *testing.T) {
	source := `
func (Automation) myAutomation(arg1 any, arg2 any) {
	step1 := query { concept==v1:test }
	return step1
}`

	name, err := GetAutomationName(source)
	if err != nil {
		t.Fatalf("GetAutomationName error: %v", err)
	}

	if name != "myAutomation" {
		t.Errorf("Expected 'myAutomation', got %q", name)
	}
}

func TestCompileResult_ToJSON(t *testing.T) {
	source := `
func (Automation) testAuto(_ any) {
	step1 := query { concept==v1:test }
	return step1
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	outputs, err := result.ToJSON(true)
	if err != nil {
		t.Fatalf("ToJSON error: %v", err)
	}

	// Should have one JSON file
	if len(outputs) != 1 {
		t.Errorf("Expected 1 output, got %d", len(outputs))
	}

	// Should be named testAuto.json
	jsonData, ok := outputs["testAuto.json"]
	if !ok {
		t.Error("Expected testAuto.json in outputs")
	}

	// Should be valid JSON
	var parsed map[string]any
	if err := json.Unmarshal(jsonData, &parsed); err != nil {
		t.Errorf("Invalid JSON: %v", err)
	}
}

func TestIsAutomationFile(t *testing.T) {
	tests := []struct {
		source   string
		expected bool
	}{
		{"func (Automation) test(_ any) {}", true},
		{"func (Query) test() { concept==v1:test }", false},
		{"concept==v1:test", false},
	}

	for _, tt := range tests {
		result := IsAutomationFile(tt.source)
		if result != tt.expected {
			t.Errorf("IsAutomationFile(%q) = %v, want %v", tt.source[:20], result, tt.expected)
		}
	}
}

func TestCompiler_ConditionalFilter(t *testing.T) {
	source := `
func (Query) queryActiveUsers(role any) {
	concept==v1:user;?.payload.role==args.role
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	// The query should contain the ?. syntax
	if !strings.Contains(result.Functions[0].Query, "?.") {
		t.Errorf("Expected query to contain '?.', got %q", result.Functions[0].Query)
	}
}

func TestCompiler_AutomationWithCondition(t *testing.T) {
	source := `
func (Automation) conditional(_ any) {
	checkExists := query {
		concept==v1:test;id=="test-id"
	}

	createIfMissing := mutation if checkExists.metadata.itemCount == 0 {
		insert("v1:test", id="test-id", payload={"created": true})
	}

	return createIfMissing
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Automations) != 1 {
		t.Fatalf("Expected 1 automation, got %d", len(result.Automations))
	}

	// The automation was successfully parsed - the body structure is implementation-specific
	auto := result.Automations[0]
	if auto.Name != "conditional" {
		t.Errorf("Expected name 'conditional', got %q", auto.Name)
	}
}

// ----------------------------------------------------------------------------
// Tests for New Accessor Translations
// ----------------------------------------------------------------------------

func TestCompiler_ExpressionToString_VarRef(t *testing.T) {
	source := `
func (Query) getDefault() {
	var("MEMQL_DEFAULT_USER_ROLE")
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	if !strings.Contains(result.Functions[0].Query, "var(") {
		t.Errorf("Expected query to contain 'var(', got %q", result.Functions[0].Query)
	}
}

func TestCompiler_ExpressionToString_StepRef(t *testing.T) {
	source := `
func (Query) checkResult() {
	step("checkUser")
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	if !strings.Contains(result.Functions[0].Query, "step(") {
		t.Errorf("Expected query to contain 'step(', got %q", result.Functions[0].Query)
	}
}

func TestCompiler_ExpressionToString_ConcatExpr(t *testing.T) {
	source := `
func (Query) makeId() {
	concat("user-", args.id)
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	query := result.Functions[0].Query
	if !strings.Contains(query, "concat(") {
		t.Errorf("Expected query to contain 'concat(', got %q", query)
	}
}

func TestCompiler_ExpressionToString_CoalesceExpr(t *testing.T) {
	source := `
func (Query) fallback() {
	coalesce(step("create"), step("existing"))
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	query := result.Functions[0].Query
	if !strings.Contains(query, "coalesce(") {
		t.Errorf("Expected query to contain 'coalesce(', got %q", query)
	}
}

func TestCompiler_ExpressionToString_CondExpr(t *testing.T) {
	source := `
func (Query) conditional() {
	cond(args.flag, "yes", "no")
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	query := result.Functions[0].Query
	if !strings.Contains(query, "cond(") {
		t.Errorf("Expected query to contain 'cond(', got %q", query)
	}
}

func TestCompiler_ExpressionToString_TernaryExpr(t *testing.T) {
	source := `
func (Query) conditional() {
	args.flag ? "yes" : "no"
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	query := result.Functions[0].Query
	if !strings.Contains(query, "?") || !strings.Contains(query, ":") {
		t.Errorf("Expected query to contain ternary '? :', got %q", query)
	}
}

func TestCompiler_ExpressionToString_FieldRef(t *testing.T) {
	source := `
func (Query) getField() {
	field(item(), "name")
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	query := result.Functions[0].Query
	if !strings.Contains(query, "field(") {
		t.Errorf("Expected query to contain 'field(', got %q", query)
	}
	if !strings.Contains(query, "item()") {
		t.Errorf("Expected query to contain 'item()', got %q", query)
	}
}

func TestCompiler_ExpressionToString_NoArgAccessors(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{"timestamp", `func (Query) ts() { timestamp() }`, "timestamp()"},
		{"now", `func (Query) ts() { now() }`, "timestamp()"},
		{"input", `func (Query) inp() { input() }`, "input()"},
		{"item", `func (Query) it() { item() }`, "item()"},
		{"index", `func (Query) idx() { index() }`, "index()"},
		{"event", `func (Query) ev() { event() }`, "event()"},
		{"error", `func (Query) err() { error() }`, "error()"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := CompileSource(tt.source)
			if err != nil {
				t.Fatalf("CompileSource error: %v", err)
			}

			if len(result.Functions) != 1 {
				t.Fatalf("Expected 1 function, got %d", len(result.Functions))
			}

			if !strings.Contains(result.Functions[0].Query, tt.expected) {
				t.Errorf("Expected query to contain %q, got %q", tt.expected, result.Functions[0].Query)
			}
		})
	}
}

func TestCompiler_ExpressionToString_ErrorWithMessage(t *testing.T) {
	source := `func (Query) throwError() { error("something went wrong") }`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	query := result.Functions[0].Query
	// Should contain error() with the message
	if !strings.Contains(query, `error("something went wrong")`) {
		t.Errorf("Expected query to contain 'error(\"something went wrong\")', got %q", query)
	}
}

func TestCompiler_ExpressionToString_StringFunctions(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{"lower", `func (Query) lw() { lower(args.x) }`, "lower("},
		{"upper", `func (Query) up() { upper(args.x) }`, "upper("},
		{"trim", `func (Query) tr() { trim(args.x) }`, "trim("},
		{"hash", `func (Query) hs() { hash(args.x) }`, "hash("},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := CompileSource(tt.source)
			if err != nil {
				t.Fatalf("CompileSource error: %v", err)
			}

			if len(result.Functions) != 1 {
				t.Fatalf("Expected 1 function, got %d", len(result.Functions))
			}

			if !strings.Contains(result.Functions[0].Query, tt.expected) {
				t.Errorf("Expected query to contain %q, got %q", tt.expected, result.Functions[0].Query)
			}
		})
	}
}

func TestCompiler_ExpressionToJSONExpr(t *testing.T) {
	// Test the JSON expression translation directly via the compiler
	c := NewDefault()

	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{
			name:   "var ref",
			source: `func (Query) t() { var("MY_VAR") }`,
			// We need to compile and check the JSON output contains $var.MY_VAR
			expected: "$var.MY_VAR",
		},
		{
			name:     "step ref",
			source:   `func (Query) t() { step("checkUser") }`,
			expected: "$steps.checkUser.result",
		},
		{
			name:     "input ref",
			source:   `func (Query) t() { input() }`,
			expected: "$input",
		},
		{
			name:     "item ref",
			source:   `func (Query) t() { item() }`,
			expected: "$item",
		},
		{
			name:     "timestamp ref",
			source:   `func (Query) t() { timestamp() }`,
			expected: "$timestamp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := ParseMemQL(tt.source)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			// Get the expression from the parsed file
			file := ast.(*parser.File)
			funcDef := file.Definitions[0].(*parser.FunctionDef)
			expr := funcDef.Body.(parser.ExpressionNode)

			// Test expressionToJSONExpr
			result := c.expressionToJSONExpr(expr)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestCompiler_ExpressionToJSONExpr_FieldAccess(t *testing.T) {
	// Test that field(item(), "name") becomes $item.name
	c := NewDefault()

	source := `func (Query) t() { field(item(), "name") }`
	ast, err := ParseMemQL(source)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	file := ast.(*parser.File)
	funcDef := file.Definitions[0].(*parser.FunctionDef)
	expr := funcDef.Body.(parser.ExpressionNode)

	result := c.expressionToJSONExpr(expr)
	expected := "$item.name"
	if result != expected {
		t.Errorf("Expected %q, got %q", expected, result)
	}
}

// TestParseObjectLiteral_BarePathShorthand covers the automation
// step-arg shorthand: a dotted path like `event.payload.spaceId` with no
// `key:` prefix infers the terminal segment as the key.
func TestParseObjectLiteral_BarePathShorthand(t *testing.T) {
	c := New(Config{})
	obj := c.parseObjectLiteral(`{
		event.payload.spaceId,
		event.payload.email,
		registerNode.result.node.id,
		name: "explicit",
		email: "override@example.com"
	}`)
	if obj == nil {
		t.Fatal("expected parsed object, got nil")
	}
	checks := map[string]string{
		"spaceId": "event.payload.spaceId",
		"id":      "registerNode.result.node.id",
		"name":    "explicit",
	}
	for key, want := range checks {
		got, _ := obj[key].(string)
		if got != want {
			t.Errorf("%s: expected %q, got %q", key, want, got)
		}
	}
	// Verbose `email:` entry must override the shorthand (map insertion order:
	// shorthand email first, then verbose email wins).
	if v, _ := obj["email"].(string); v != "override@example.com" {
		t.Errorf("email: expected verbose override, got %q", v)
	}
}

// Single-identifier bare values (no dots) must NOT be picked up as
// shorthand -- they'd collide with step-reference semantics like
// `allAgents` meaning "the step's result".
func TestParseObjectLiteral_BarePathRejectsSingleIdentifier(t *testing.T) {
	c := New(Config{})
	// `allAgents` with no key prefix should fall through to the
	// verbose parser, which will then fail to find a colon and
	// produce a malformed entry. We just verify shorthand didn't
	// silently claim it.
	obj := c.parseObjectLiteral(`{foo: bar, allAgents}`)
	if obj == nil {
		return // acceptable: parser rejected the malformed input
	}
	if _, ok := obj["allAgents"]; ok {
		t.Errorf("single identifier should not be shorthand, got %v", obj)
	}
}

// TestParseObjectLiteral_UnquotedKeys_Required verifies that object
// literal keys are expected in unquoted form. Quoted keys (`"id": ...`)
// still parse via the language parser's TokenString case, but our
// convention (see authoring rule #18) is to use unquoted identifiers.
// This test exists to lock in that the unquoted form is the canonical
// path that the AST and compiler produce.
func TestParseObjectLiteral_UnquotedKeys(t *testing.T) {
	c := New(Config{})
	obj := c.parseObjectLiteral(`{name: "Alice", age: 30, active: true}`)
	if obj == nil {
		t.Fatal("expected parsed object")
	}
	if obj["name"] != "Alice" || obj["age"] != int64(30) || obj["active"] != true {
		t.Errorf("expected unquoted keys to parse cleanly, got %+v", obj)
	}
}
