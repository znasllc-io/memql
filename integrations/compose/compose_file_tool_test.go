package compose

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// THE composeFile TOOL, RUN THE WAY AN AGENT'S TOOL LOOP RUNS IT.
//
// composeFile is how an agent asks for a file, and an agent that sent no
// statement did not ask for the empty document the Materializer files for a
// request with nothing to compose from (a person's client asks for that on
// purpose, by leaving the statement out). So the tool must refuse a statement
// that says nothing, and it must do so BEFORE the Materializer runs: after, a
// blank file already exists and the agent is told it wrote something.
//
// A tool field cannot say "not empty" (the annotation set has only `required`),
// so the refusal is not in the tool. It is in the engine's call contract: a
// builtin refuses an empty or whitespace-only value for a field typed `string`
// when the call is parsed (validateBuiltinCallArgs, component/memql/parser.go),
// and a tool whose handler is `type="function"` reaches its builtin by rendering
// that call and executing it (ExecuteTool -> buildFunctionCallQuery -> Execute).
// This test is what holds the claim, with the pieces that matter real:
//
//   - the shipped tool and the shipped builtin, loaded from the embedded DSL tree;
//   - the engine's own ExecuteTool, with an agent's role on the context, which is
//     what the tool loop calls (schema validation, the handler, the parse of the
//     rendered call, the builtin's contract);
//   - the Materializer's capability, replaced by a recorder, so that "refused
//     before the Materializer runs" is something the test can see.
//
// It needs a database because Execute does (an engine without one refuses every
// call before it parses it), so it is one of this package's DB tests and runs
// with the rest of them.
//
// If a tool field ever grows a way to say "not empty", or the engine stops
// policing a builtin's string fields, this fails before an agent is handed a
// blank file it believes it wrote.

// recordingMaterializer is the compose integration with the Materializer's own
// capability swapped for a recorder. Every other capability is the real one, so
// the DSL's executors still resolve exactly as in the shipped tree.
type recordingMaterializer struct {
	mu      sync.Mutex
	reached []map[string]any
}

func (r *recordingMaterializer) IntegrationName() string { return integrationName }

func (r *recordingMaterializer) Capabilities() []memql.IntegrationCapability {
	caps := (&Integration{}).Capabilities()
	for n := range caps {
		if caps[n].Name != "materialize" {
			continue
		}
		caps[n].Handler = func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.reached = append(r.reached, args)
			return nil, nil
		}
	}
	return caps
}

func (r *recordingMaterializer) calls() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.reached...)
}

// composeFileEngine boots the shipped tree on the test database, with the
// Materializer's capability replaced by a recorder.
func composeFileEngine(t *testing.T) (*memql.MemQLEngine, *recordingMaterializer) {
	t.Helper()
	e := materializeDBEngine(t)
	recorder := &recordingMaterializer{}
	if err := e.RegisterIntegration(recorder); err != nil {
		t.Fatal(err)
	}
	return e, recorder
}

// anAgentsToolCall is the context the agent tool loop calls ExecuteTool under:
// the acting agent's role, for a person.
func anAgentsToolCall() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-alice", Role: auth.RoleWriter})
	return memql.WithActingAgentRole(ctx, "specialist")
}

// refusalOf is what the tool loop hands the model for a call: the error, or the
// text of a result flagged as one. "" when the call was accepted.
func refusalOf(result *memql.ToolCallResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if result != nil && result.IsError {
		var text []string
		for _, content := range result.Content {
			text = append(text, content.Text)
		}
		return strings.Join(text, " ")
	}
	return ""
}

func TestComposeFileToolDB_RefusesAStatementThatSaysNothingBeforeTheMaterializerRuns(t *testing.T) {
	e, materializer := composeFileEngine(t)
	tool, err := e.Tools().Get("composeFile")
	if err != nil {
		t.Fatalf("the shipped tree has no composeFile tool: %v", err)
	}

	// The tool reaches the Materializer's own builtin, not a copy of it with
	// another set of rules: that is what this test is about.
	if kind, callee, ok := e.ToolCallee("composeFile"); !ok || kind != "builtin" || callee != "composeMaterialize" {
		t.Fatalf("composeFile calls %s %q (found = %v), want the builtin composeMaterialize", kind, callee, ok)
	}

	// A control first, so that "the Materializer was not reached" below means
	// the call was refused and not that this harness never reaches it.
	args := map[string]any{"name": "Heroes", "format": "markdown", "statement": "Rank ten heroes"}
	result, err := e.ExecuteTool(anAgentsToolCall(), tool, args)
	if refusal := refusalOf(result, err); refusal != "" {
		t.Fatalf("a call with a statement was refused: %s", refusal)
	}
	got := materializer.calls()
	if len(got) != 1 || got[0]["statement"] != "Rank ten heroes" || got[0]["name"] != "Heroes" || got[0]["format"] != "markdown" {
		t.Fatalf("the Materializer did not receive the agent's request as sent: %v", got)
	}

	for name, statement := range map[string]string{
		"empty":                    "",
		"spaces":                   "   ",
		"line breaks and tabs":     "\n\t \n",
		"a no-break space and tab": " \t",
	} {
		before := len(materializer.calls())
		result, err := e.ExecuteTool(anAgentsToolCall(), tool, map[string]any{"name": "Heroes", "format": "markdown", "statement": statement})
		refusal := refusalOf(result, err)
		if refusal == "" {
			t.Errorf("%s: the tool accepted a statement that says nothing", name)
			continue
		}
		if !strings.Contains(refusal, "statement cannot be empty") {
			t.Errorf("%s: refused, but not by the engine's sentence about the statement: %q", name, refusal)
		}
		if after := len(materializer.calls()); after != before {
			t.Errorf("%s: refused, but the Materializer had already run (%d calls, was %d): a blank file would exist", name, after, before)
		}
	}

	// And a call with no statement at all never gets that far: the tool's own
	// schema requires one.
	before := len(materializer.calls())
	result, err = e.ExecuteTool(anAgentsToolCall(), tool, map[string]any{"name": "Heroes", "format": "markdown"})
	if refusal := refusalOf(result, err); !strings.Contains(refusal, "statement") {
		t.Errorf("a call with no statement was not refused for it: %q", refusal)
	}
	if after := len(materializer.calls()); after != before {
		t.Errorf("a call with no statement reached the Materializer")
	}
}
