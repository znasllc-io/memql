package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

func TestCompileSource_SimpleQuery(t *testing.T) {
	source := `
query user activeUsers {
  filter row => row.active == true
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	fn := result.Functions[0]
	if fn.Name != "activeUsers" {
		t.Errorf("Expected name 'activeUsers', got %q", fn.Name)
	}
	if fn.Type != "query" {
		t.Errorf("Expected type 'query', got %q", fn.Type)
	}
	// The internal form carries the filter lambda as its canonical source,
	// parenthesised as the rewriter joins it, and reads back.
	if want := `concept=="user";(row => row.active == true)`; fn.Query != want {
		t.Errorf("compiled query = %q, want %q", fn.Query, want)
	}
	if _, err := parser.ParseExpression(fn.Query); err != nil {
		t.Errorf("the compiled query does not read back: %v", err)
	}
}

// TestCompileSource_V1MutationValues: a mutation's values are edition-2026
// expressions, and the compiled insert(...) carries each as its canonical
// source -- `+` and `??` as written. It reads back where a mutation's values
// are read: in a mutation body, which parses them with the v1 grammar (the
// internal query form, ParseExpression, keeps the legacy grammar and has no
// `+` on strings).
func TestCompileSource_V1MutationValues(t *testing.T) {
	result, err := CompileSource(`
mutation thing createThing {
  args {
    id    string!
    name  string
  }
  insert {
    id: "thing-" + args.id
    name: args.name ?? "unnamed"
  }
}`)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}
	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}
	query := result.Functions[0].Query
	for _, want := range []string{`id="thing-" + args.id`, `name: args.name ?? "unnamed"`} {
		if !strings.Contains(query, want) {
			t.Errorf("compiled mutation %q does not carry %q", query, want)
		}
	}
	if strings.Contains(query, "<<unsupported") {
		t.Fatalf("compiled mutation carries an unsupported-expression placeholder: %q", query)
	}
	if _, err := parser.ParseFile("func (Mutation) createThing(args any) error {\n  return " + query + "\n}"); err != nil {
		t.Errorf("the compiled mutation does not read back as a mutation body: %v", err)
	}
}

func TestCompileSource_Automation(t *testing.T) {
	source := `
@enabled
@trigger(schedule="0 */30 * * * *")
automation leadProcessor {
  fetchLeads := query activeLeads(limit: 10)
  processLeads := logic processLeads(leads: fetchLeads)
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
@trigger(event="node.created", concept="v1:probe:thing")
automation testAuto {
  step1 := query listThings()
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
@trigger(event="node.created", concept="v1:probe:agent")
automation autoJoinAIExample {
  getAgents := query activeAgents()
  for agent in getAgents.nodes() if agent.status != "left" {
    mutation createParticipant(agentId: agent.id, status: "joined")
  }
}`

	jsonOutput, err := TranspileAutomation(source)
	if err != nil {
		t.Fatalf("TranspileAutomation error: %v", err)
	}

	// A reference to the loop variable is an expression the runtime
	// evaluates, never a string: it compiles to an {"$expr"} leaf, and a
	// literal beside it stays a literal. A statement loop keeps the name its
	// author gave the variable.
	if strings.Contains(jsonOutput, `"agentId": "agent.id"`) {
		t.Fatalf("the loop variable was written as a string, which the runtime reads as text: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, `"$expr": "agent.id"`) {
		t.Fatalf("expected the loop variable read as {\"$expr\": \"agent.id\"}, got: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, `"status": "joined"`) {
		t.Fatalf("expected the literal argument to stay a literal, got: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, `"filter": "agent.status != \"left\""`) {
		t.Fatalf("expected the loop's filter as canonical v1 source, got: %s", jsonOutput)
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
			source: `automation test {
  step1 := query readTest()
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

func TestCompileSource_FunctionCallStepInAutomation(t *testing.T) {
	source := `
@enabled
@trigger(event="node.created", concept="v1:probe:user")
automation testAuto {
  args {
    userId any
  }
  checkUser := query userById(userId: args.userId)
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
	if functionConfig["name"] != "userById" {
		t.Fatalf("expected function name userById, got %v", functionConfig["name"])
	}
	args, _ := functionConfig["args"].(map[string]any)
	if leaf, _ := args["userId"].(map[string]any); leaf["$expr"] != "args.userId" {
		t.Fatalf("expected the userId argument as an expression leaf, got %#v", args["userId"])
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
@trigger(event="node.created", concept="v1:probe:thing")
automation testAuto {
  step1 := query listThings()
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

// TestCompiler_ConditionalFilter: an optional argument's predicate is the
// edition-2026 guard `(args.x == nil || <predicate>)` -- the retired `?.`
// prefix it replaces is refused at parse -- and the compiled query carries
// it as written.
func TestCompiler_ConditionalFilter(t *testing.T) {
	source := `
query user activeUsers {
  args {
    role string
  }
  filter row => (args.role == nil || row.role == args.role)
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Functions) != 1 {
		t.Fatalf("Expected 1 function, got %d", len(result.Functions))
	}

	if want := "(args.role == nil || row.role == args.role)"; !strings.Contains(result.Functions[0].Query, want) {
		t.Errorf("Expected the query to carry the guard %q, got %q", want, result.Functions[0].Query)
	}
	if strings.Contains(result.Functions[0].Query, "?.") {
		t.Errorf("the retired ?. prefix reached the compiled query: %q", result.Functions[0].Query)
	}
}

func TestCompiler_AutomationWithCondition(t *testing.T) {
	source := `
@trigger(event="node.created", concept="v1:probe:thing")
automation conditional {
  checkExists := query thingById(id: "test-id")
  if checkExists.empty() {
    createIfMissing := mutation createThing(id: "test-id", created: true)
  }
}`

	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("CompileSource error: %v", err)
	}

	if len(result.Automations) != 1 {
		t.Fatalf("Expected 1 automation, got %d", len(result.Automations))
	}

	auto := result.Automations[0]
	if auto.Name != "conditional" {
		t.Errorf("Expected name 'conditional', got %q", auto.Name)
	}
	// The condition is canonical v1 source, and the gated call's literal
	// arguments stay literals.
	steps, _ := auto.JSON["steps"].([]map[string]any)
	var gated map[string]any
	for _, s := range steps {
		if s["id"] == "createIfMissing" {
			gated = s
		}
	}
	if gated == nil || gated["condition"] != "checkExists.empty()" {
		t.Fatalf("createIfMissing = %#v, want the condition checkExists.empty()", gated)
	}
	fn, _ := gated["function"].(map[string]any)
	args, _ := fn["args"].(map[string]any)
	if fn["name"] != "createThing" || args["id"] != "test-id" || args["created"] != true {
		t.Fatalf("the gated call = %#v", fn)
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
		{"now", `func (Query) ts() { now }`, "timestamp()"},
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

// TestCompiler_LogicCoalesceInFunctionStepResolvesArgRefs (memql#1065)
//
// A logic body that ends with a BARE `return <builtin>({field:
// coalesce(args.X, args.Y)})` compiled the whole call into the `_return`
// STRING, with the arg refs serialized as `arg("X")`. At runtime that string
// was handed to engine.Execute with no caller-args bound, so the refs
// resolved to empty -- the dailyspace builtin then errored "userId is
// required" on every login.
//
// The fallback is a statement's argument: an {"$expr"} leaf the runtime
// evaluates with EvalExpr, args in scope, before the builtin is called, and
// the return reads the statement's name -- never a re-parsed builtin call
// string.
func TestCompiler_LogicCoalesceInFunctionStepResolvesArgRefs(t *testing.T) {
	source := `
use common.builtins.{ ensureDailySpaceForUser }
@enabled
logic logicEnsureDailySpaceOnAuthSession {
  args {
    event object!
  }
  ensured := builtin ensureDailySpaceForUser(userId: args.event.payload.userId ?? args.event.payload.subject)
  return ensured
}`
	normalised, err := parser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := parser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	ast, err := parser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file := ast.(*parser.File)
	var body *parser.AutomationDef
	for _, def := range file.Definitions {
		if fd, ok := def.(*parser.FunctionDef); ok && fd.Type == parser.FunctionTypeLogic {
			body = fd.Body.(*parser.AutomationDef)
		}
	}
	if body == nil {
		t.Fatal("no logic body parsed")
	}
	fakeFunc := &parser.FunctionDef{Name: "logicEnsureDailySpaceOnAuthSession", Type: parser.FunctionTypeAutomation, Body: body}
	c := New(Config{})
	result, err := c.CompileFile(&parser.File{Definitions: []parser.Node{fakeFunc}})
	if err != nil {
		t.Fatalf("CompileFile: %v", err)
	}
	compiled := result.Automations[0].JSON

	// The return reads the statement's name, not a re-parsed builtin call.
	steps, _ := compiled["steps"].([]map[string]any)
	if len(steps) != 2 {
		t.Fatalf("expected the call and the return, got %d steps", len(steps))
	}
	if ret, _ := steps[1]["return"].(map[string]any); ret == nil || ret["value"] != "ensured" {
		t.Fatalf("step 1 = %#v, want the return of the statement's name", steps[1])
	}
	fn, _ := steps[0]["function"].(map[string]any)
	if fn == nil || fn["name"] != "ensureDailySpaceForUser" {
		t.Fatalf("step 0 is not the ensureDailySpaceForUser function step: %#v", steps[0])
	}
	argsMap, _ := fn["args"].(map[string]any)
	leaf, _ := argsMap["userId"].(map[string]any)
	if want := "args.event.payload.userId ?? args.event.payload.subject"; leaf["$expr"] != want {
		t.Fatalf("userId arg = %#v, want the expression leaf {\"$expr\": %q}", argsMap["userId"], want)
	}
}
