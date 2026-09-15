package automations

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression for memql#580 (the ghost AI / "two assistants" bug).
//
// The autoJoinAI logic derived the GA participant id with:
//
//	getGA := getActiveGA ?? getFallbackGA
//	... agentId: getGA.first().id ?? ""
//
// The string evaluator rendered a bare statement name as its OWN LITERAL
// STRING, so `getGA` held the string "getActiveGA" instead of the query's
// result, every downstream getGA.first().id collapsed to nil, and the
// unevaluated text flowed into the mutation as the agentId -> ghost AI.
//
// These tests pin the edition-2026 truth: a name a query statement bound
// stands for its rows; a statement that did not run left its name absent, so
// `??` falls through; and a bare identifier that names nothing is refused as
// an unknown name -- never its own text.

func newAgentResult(id, name string) *memqlengine.ExecuteResult {
	payload, err := structpb.NewStruct(map[string]any{"name": name})
	if err != nil {
		panic(err)
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: payload}},
		},
	}
}

// The statement's rows, never the literal string "getActiveGA".
func TestBareStatementNameIsItsRows(t *testing.T) {
	eval := NewEvaluator()
	eval.enterStatements()
	eval.Bind("getActiveGA", functionStatementValue("query", newAgentResult("v1:agents:agent:assistant-REAL", "Sofia")))

	gotVal, err := evalV1Src(t, eval, "getActiveGA")
	if err != nil {
		t.Fatalf("getActiveGA: %v", err)
	}
	if _, isStr := gotVal.(string); isStr {
		t.Fatalf("getActiveGA = %#v (a literal string) -- the memql#580 ghost-AI bug; want the statement's rows", gotVal)
	}
	if id, err := evalV1Src(t, eval, "getActiveGA.first().id"); err != nil || id != "v1:agents:agent:assistant-REAL" {
		t.Fatalf("getActiveGA.first().id = %#v (err %v), want the GA's id", id, err)
	}
	if id, err := evalV1Src(t, eval, `(getActiveGA ?? getActiveGA).first().id ?? ""`); err != nil || id != "v1:agents:agent:assistant-REAL" {
		t.Fatalf("the autoJoinAI shape = %#v (err %v), want the GA's id", id, err)
	}
}

// A statement that did not run left its name absent, so `??` falls through to
// the next branch -- mirrors getFallbackGA being skipped when
// activeAssistantId is set.
func TestSkippedStatementNameIsAbsent(t *testing.T) {
	eval := NewEvaluator()
	eval.enterStatements()
	eval.names.declare("getFallbackGA")

	got, err := evalV1Src(t, eval, `getFallbackGA ?? "fallback"`)
	if err != nil {
		t.Fatalf("getFallbackGA ?? \"fallback\": %v", err)
	}
	if got != "fallback" {
		t.Fatalf("getFallbackGA ?? \"fallback\" = %#v, want the fallback (a statement that did not run binds nothing)", got)
	}
}

// A bare identifier that names no statement is refused as an unknown name,
// never rendered as its own text.
func TestBareIdentifierNamingNothingIsRefused(t *testing.T) {
	eval := NewEvaluator()
	if got, err := evalV1(eval, "writer"); err == nil {
		t.Fatalf("writer = %#v, want an unknown-name refusal (never the literal)", got)
	}
}
