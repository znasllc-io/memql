package memql

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/language/ast"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// tool_declaration_validation_3625_test.go -- memql#3625, the load-time half.
//
// ValidateTool existed, was correct, and was never called on an authored tool:
// its only caller was registerFunctionTools, which validates the tools the
// engine GENERATES from functions. A `.memql` tool went
// LoadUnifiedTools -> toolDeclToTool -> registry.Upsert untouched.
//
// And even a called ValidateTool cannot answer the question that actually
// broke things -- "does the thing this handler NAMES exist" -- because a
// declaration does not carry the registry. That is the resolution pass.

// ---------------------------------------------------------------------------
// The handler shapes ValidateTool can judge on its own
// ---------------------------------------------------------------------------

func TestValidateToolRefusesUnknownHandlerType(t *testing.T) {
	tool := &Tool{
		Name:        "zzBadType",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "notAHandlerType"},
	}
	err := ValidateTool(tool)
	if err == nil {
		t.Fatal("an unknown handler type was accepted")
	}
	if !strings.Contains(err.Error(), "unknown handler type") {
		t.Errorf("got: %v", err)
	}
}

func TestValidateToolRefusesEmptyFunctionName(t *testing.T) {
	tool := &Tool{
		Name:        "zzEmptyFn",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "function"},
	}
	if err := ValidateTool(tool); err == nil {
		t.Fatal("a function handler with no function name was accepted -- this is what a " +
			"typo'd @handler kwarg leaves behind")
	}
}

// A tool with NO handler registered fine and was advertised to the model,
// which then received "Tool %q has no handler defined" as a tool RESULT.
func TestValidateToolRefusesMissingHandler(t *testing.T) {
	tool := &Tool{
		Name:        "zzNoHandler",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	err := ValidateTool(tool)
	if err == nil {
		t.Fatal("a tool with no handler was accepted. It has no way to execute, and was still " +
			"advertised to the model (memql#3625).")
	}
	if !strings.Contains(err.Error(), "clientExecution") {
		t.Errorf("the error must name the one legitimate exception, or an author of a "+
			"browser-executed tool reads it as a wall; got: %v", err)
	}
}

// The exception, kept honest: a client-executed tool's body lives in the
// browser and ExecuteTool routes to the ClientToolInvoker before it ever reads
// tool.Handler.
func TestValidateToolAllowsClientExecutedToolWithoutHandler(t *testing.T) {
	tool := &Tool{
		Name:            "zzClientTool",
		Description:     "d",
		InputSchema:     json.RawMessage(`{"type":"object"}`),
		ClientExecution: true,
	}
	if err := ValidateTool(tool); err != nil {
		t.Fatalf("a @clientExecution tool legitimately carries no handler: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The authored-tool load path now runs that validation
// ---------------------------------------------------------------------------

// This drives the REAL loader -- a mounted tree walked by LoadUnifiedTools,
// exactly as the agent node walks it at boot -- rather than calling
// ValidateTool directly. That distinction is the whole issue: ValidateTool was
// already correct, and the authored path simply never called it. A test that
// calls it itself would have passed before the fix and proved nothing.
//
// The loader logs + records a Skip rather than returning an error (a bad slice
// must not abort the walk), and the skip is what the strict-boot gate refuses
// on, so the skip is what this asserts.
func TestAuthoredToolWithDeadHandlerShapeIsRefusedAtLoad(t *testing.T) {
	for _, tc := range []struct{ name, tool, src, want string }{
		{
			name: "unknown handler type",
			tool: "zzBadType",
			src: `@description("d")
@handler(type="notAHandlerType")
tool zzBadType {
  a string
}
`,
			want: "unknown handler type",
		},
		{
			name: "function handler with no name",
			tool: "zzEmptyFn",
			src: `@description("d")
@handler(type="function")
tool zzEmptyFn {
  a string
}
`,
			want: "function name",
		},
		{
			name: "no handler at all",
			tool: "zzNoHandler",
			src: `@description("d")
tool zzNoHandler {
  a string
}
`,
			want: "handler is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := "toolvalidation3625" + strings.ToLower(tc.tool)
			memqldsl.RegisterTree(domain, fstest.MapFS{"tools.memql": {Data: []byte(tc.src)}})
			t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

			registry := newToolRegistry()
			report := newLoadReport()
			if _, err := LoadUnifiedTools(discardLogger(), registry, report); err != nil {
				t.Fatalf("LoadUnifiedTools: %v", err)
			}

			if registry.Has(tc.tool) {
				t.Fatalf("%q registered and is advertised to the model. Authored tools were "+
					"never validated at all -- ValidateTool's only caller was "+
					"registerFunctionTools, which validates GENERATED function-tools "+
					"(memql#3625).\n  source:\n%s", tc.tool, tc.src)
			}
			if !report.HasProblems() {
				t.Fatalf("%q was dropped but recorded no load problem, so strict boot would "+
					"come up green on a half-loaded tool surface", tc.tool)
			}
			if detail := report.Detail(); !strings.Contains(detail, tc.want) {
				t.Errorf("the recorded problem must explain what is wrong (%q); got:\n%s", tc.want, detail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A mistyped field type is no longer a silent "string"
// ---------------------------------------------------------------------------

func TestToolFieldWithUnknownTypeIsRefused(t *testing.T) {
	decl := &ast.ToolDecl{
		Name:        "zzTypes",
		Description: "d",
		Fields: []ast.ToolFieldDecl{
			{Name: "typoInt", Type: "interger", Default: "10"},
		},
		HandlerType: "function",
		HandlerName: "someFn",
	}
	_, err := toolDeclToTool(decl, "test")
	if err == nil {
		t.Fatal("`interger` was accepted and emitted as \"string\". That is not a permissive " +
			"degrade -- the model is told the field is a string, coerceSchemaDefault coerces " +
			"the @default to a string, and the value reaches a @required integer handler arg " +
			"as \"10\" (memql#3625).")
	}
	if !strings.Contains(err.Error(), "interger") {
		t.Errorf("the error must name the offending type; got: %v", err)
	}
	if !strings.Contains(err.Error(), "integer") {
		t.Errorf("the error must list the supported spellings so a transposition is obvious; got: %v", err)
	}
}

// The direction that keeps the change honest: every type the corpus uses still
// converts, and still converts to what it says.
func TestKnownToolFieldTypesStillConvert(t *testing.T) {
	want := map[string]string{
		"string": "string", "number": "number", "float": "number",
		"integer": "integer", "int": "integer",
		"bool": "boolean", "boolean": "boolean",
		"object": "object", "array": "array",
	}
	for declared, emitted := range want {
		decl := &ast.ToolDecl{
			Name:        "zzTypes",
			Description: "d",
			Fields:      []ast.ToolFieldDecl{{Name: "f", Type: declared}},
			HandlerType: "function",
			HandlerName: "someFn",
		}
		tools, err := toolDeclToTool(decl, "test")
		if err != nil {
			t.Fatalf("type %q must still convert: %v", declared, err)
		}
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if uerr := json.Unmarshal(tools[0].InputSchema, &schema); uerr != nil {
			t.Fatalf("unmarshal: %v", uerr)
		}
		if got := schema.Properties["f"].Type; got != emitted {
			t.Errorf("type %q emitted %q, want %q", declared, got, emitted)
		}
	}
}

// ---------------------------------------------------------------------------
// Handler TARGET resolution -- the half no declaration can answer alone
// ---------------------------------------------------------------------------

func TestDeadHandlerTargetIsDetected(t *testing.T) {
	functions := newFunctionRegistry()
	if err := functions.Upsert(&Function{Name: "createTodo", Enabled: true}); err != nil {
		t.Fatalf("seed function: %v", err)
	}

	tools := newToolRegistry()
	mustUpsertTool(t, tools, &Tool{
		Name: "zzLive", Description: "d", Origin: "test.memql",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "function", FunctionName: "createTodo"},
	})
	mustUpsertTool(t, tools, &Tool{
		Name: "zzDeadFn", Description: "d", Origin: "test.memql",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "function", FunctionName: "zzNoSuchFunctionAnywhere"},
	})
	mustUpsertTool(t, tools, &Tool{
		Name: "zzDeadQuery", Description: "d", Origin: "test.memql",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "query", Query: `query zzNoSuchQuery(a: "b")`},
	})

	errs := validateToolHandlerTargets(tools, functions)
	if len(errs) != 2 {
		t.Fatalf("want exactly the two dead targets, got %d: %v", len(errs), errs)
	}
	joined := errs[0].Error() + "\n" + errs[1].Error()
	for _, want := range []string{"zzNoSuchFunctionAnywhere", "zzNoSuchQuery"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the unresolved target %q must be named; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "zzLive") {
		t.Errorf("a resolvable handler must not be reported; got:\n%s", joined)
	}
}

// The query-handler forms the corpus actually uses, so the extractor is pinned
// against the shapes it has to read rather than only the simple one.
func TestQueryHandlerTargetsAreExtractedFromEveryCorpusShape(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`query todos(done: $args.done)`, []string{"todos"}},
		{`mutation updateNote(noteId: "$args.noteId")`, []string{"updateNote"}},
		{`builtin help(name: "$args.name")`, []string{"help"}},
		{`paginate(query searchUsers(active: $args.active), $args.limit)`, []string{"searchUsers"}},
		{`query activeProjects()`, []string{"activeProjects"}},
		// A raw filter expression names no construct; there is nothing to
		// resolve and nothing is invented.
		{`concept==v1:memql:backend:user`, nil},
	} {
		got := toolHandlerTargets(&Tool{
			Name:    "zzProbe",
			Handler: &ToolHandler{Type: "query", Query: tc.query},
		})
		if len(got) != len(tc.want) {
			t.Fatalf("query %q -> %v, want %v", tc.query, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("query %q -> %v, want %v", tc.query, got, tc.want)
			}
		}
	}
}

// A webhook / delegate handler resolves against no registry, so it must not be
// reported as unresolved.
func TestNonRegistryHandlersResolveTrivially(t *testing.T) {
	functions := newFunctionRegistry()
	tools := newToolRegistry()
	mustUpsertTool(t, tools, &Tool{
		Name: "zzHook", Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "webhook", URL: "https://example.test/hook", Method: "POST"},
	})
	mustUpsertTool(t, tools, &Tool{
		Name: "zzDelegate", Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "delegate"},
	})
	if errs := validateToolHandlerTargets(tools, functions); len(errs) != 0 {
		t.Errorf("webhook / delegate handlers name nothing in the registry: %v", errs)
	}
}

// ---------------------------------------------------------------------------
// The corpus sweep -- the evidence that this stays clean
// ---------------------------------------------------------------------------

// TestCorpusToolsLoadAndEveryHandlerResolves is the standing guard: every tool
// in the tree passes the new gates AND every handler target resolves against
// the loaded functions + builtins. It is the assertion the issue's "corpus is
// clean today" claim becomes, so a dead handler cannot be added quietly.
func TestCorpusToolsLoadAndEveryHandlerResolves(t *testing.T) {
	logger := discardLogger()

	tools := newToolRegistry()
	n, err := LoadUnifiedTools(logger, tools)
	if err != nil {
		t.Fatalf("the tool tree must load clean under the memql#3625 gates: %v", err)
	}
	if n == 0 {
		t.Fatal("no tools loaded -- the sweep would assert nothing")
	}

	functions := newFunctionRegistry()
	if _, _, ferr := LoadUnifiedFunctions(logger, functions, nil); ferr != nil {
		t.Fatalf("load functions: %v", ferr)
	}
	if _, berr := LoadUnifiedBuiltins(logger, functions); berr != nil {
		t.Fatalf("load builtins: %v", berr)
	}

	if errs := validateToolHandlerTargets(tools, functions); len(errs) > 0 {
		var b strings.Builder
		for _, e := range errs {
			b.WriteString("\n  " + e.Error())
		}
		t.Fatalf("%d tool handler target(s) do not resolve:%s", len(errs), b.String())
	}
}

func mustUpsertTool(t *testing.T, registry *ToolRegistry, tool *Tool) {
	t.Helper()
	if err := registry.Upsert(tool); err != nil {
		t.Fatalf("upsert %s: %v", tool.Name, err)
	}
}
