package memql

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// tool_handler_builtin_args_test.go -- a tool whose builtin handler requires
// an argument the tool never declares refuses at load (memql#5436,
// tool_handler_arg_undeclared).
//
// requestUserFeedback is the case that found it: its tool moved to `runId`
// with the work spine while its builtin still required `planId`, so every
// call was refused by validateBuiltinCallArgs before it reached the
// integration -- and the tool loaded, registered and was advertised to the
// model as if it worked.

func builtinArgsFixture(t *testing.T) *FunctionRegistry {
	t.Helper()
	functions := newFunctionRegistry()
	if err := functions.Upsert(&Function{
		Name:     "parkRun",
		Type:     FunctionTypeBuiltin,
		Enabled:  true,
		Executor: "integration.test.parkRun",
		BuiltinArgs: &BuiltinArgContract{
			Profile:  BuiltinArgProfileObject,
			Required: []string{"question", "runId"},
		},
	}); err != nil {
		t.Fatalf("seed builtin: %v", err)
	}
	if err := functions.Upsert(&Function{Name: "createTodo", Enabled: true}); err != nil {
		t.Fatalf("seed function: %v", err)
	}
	return functions
}

func toolWithFields(name, handler string, fields ...string) *Tool {
	props := map[string]any{}
	for _, f := range fields {
		props[f] = map[string]any{"type": "string"}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": props})
	return &Tool{
		Name: name, Description: "d", Origin: "test.memql",
		InputSchema: schema,
		Handler:     &ToolHandler{Type: "function", FunctionName: handler},
	}
}

func TestToolWhoseBuiltinRequiresAnUndeclaredArgIsRefused(t *testing.T) {
	functions := builtinArgsFixture(t)
	tools := newToolRegistry()
	// The requestUserFeedback shape: the tool passes runId under another name.
	mustUpsertTool(t, tools, toolWithFields("zzAskStale", "parkRun", "question", "planId"))

	errs := validateToolHandlerTargets(tools, functions)
	if len(errs) != 1 {
		t.Fatalf("want exactly one refusal (runId), got %d: %v", len(errs), errs)
	}
	var argErr *toolHandlerArgError
	if !errors.As(errs[0], &argErr) {
		t.Fatalf("want a *toolHandlerArgError, got %T: %v", errs[0], errs[0])
	}
	if got := baseloader.RuleCode(errs[0]); got != ruleToolHandlerArgUndeclared {
		t.Errorf("rule code = %q, want %q", got, ruleToolHandlerArgUndeclared)
	}
	msg := errs[0].Error()
	for _, want := range []string{`tool "zzAskStale"`, `builtin "parkRun"`, `requires "runId"`, "parkRun() requires 'runId' field in argument", "@autoInjected"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %q; got:\n%s", want, msg)
		}
	}
}

func TestToolDeclaringEveryRequiredBuiltinArgLoads(t *testing.T) {
	functions := builtinArgsFixture(t)
	tools := newToolRegistry()
	mustUpsertTool(t, tools, toolWithFields("zzAskLive", "parkRun", "question", "runId", "kind"))
	// A handler that is not a builtin takes its arguments through its own
	// args block, so the builtin rule has nothing to say about it.
	mustUpsertTool(t, tools, toolWithFields("zzTodo", "createTodo"))

	if errs := validateToolHandlerTargets(tools, functions); len(errs) != 0 {
		t.Fatalf("a tool that declares every field its builtin requires must load: %v", errs)
	}
}

// The index holds a tool under its qualified name and its bare alias; one
// fault must be reported once, not once per name.
func TestToolHandlerProblemsAreReportedOncePerTool(t *testing.T) {
	functions := builtinArgsFixture(t)
	tools := newToolRegistry()
	mustUpsertTool(t, tools, toolWithFields("agents.zzAskStaleQualified", "parkRun", "question"))

	errs := validateToolHandlerTargets(tools, functions)
	if len(errs) != 1 {
		t.Fatalf("want one refusal for the one missing field, got %d: %v", len(errs), errs)
	}
}

func TestToolHandlerArgRefusalReachesTheLoadReportWithItsCode(t *testing.T) {
	functions := builtinArgsFixture(t)
	tools := newToolRegistry()
	mustUpsertTool(t, tools, toolWithFields("zzAskStale", "parkRun", "question", "planId"))

	report := newLoadReport()
	recordToolHandlerTargetProblems(report, tools, functions)
	skips := report.Skipped
	if len(skips) != 1 {
		t.Fatalf("want one skip, got %d: %+v", len(skips), skips)
	}
	if skips[0].Code != ruleToolHandlerArgUndeclared {
		t.Errorf("skip code = %q, want %q", skips[0].Code, ruleToolHandlerArgUndeclared)
	}
}
