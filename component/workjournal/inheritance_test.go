package workjournal

import (
	"context"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// inheritance_test.go -- what a CHILD run inherits at open (epic memql#5408,
// gap G2). Procedure learning mines the recordings of one goal, and a
// delegated app session is recorded into a child run: without its parent's
// goal signature the recording belongs to no corpus, and without the parent's
// variables no goal input can be mapped onto a parameter.

func runCallOf(t *testing.T, calls []string) string {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c, "mutation createWorkRun(") {
			return c
		}
	}
	t.Fatalf("no createWorkRun among %v", calls)
	return ""
}

// TestBeginWritesTheInheritedFieldsOnTheRunsFirstVersion. The control is the
// same Begin with the three left blank: none of them may appear at all, not
// even empty, because every argument here is a read-merge write and an empty
// one is a value.
func TestBeginWritesTheInheritedFieldsOnTheRunsFirstVersion(t *testing.T) {
	engine := &countingEngine{}
	w := work()
	w.GoalSignature = "sig-abc"
	w.ParentRunID = "v1:work:run:parent"
	w.Variables = map[string]any{"day": "2026-09-04", "note": `a "quoted" <tag>`}
	if _, err := New(engine, nil, "node-1").Begin(context.Background(), w); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	call := runCallOf(t, engine.calls)
	for _, want := range []string{`goalSignature: "sig-abc"`, `parentRunId: "v1:work:run:parent"`, `variables: {`} {
		if !strings.Contains(call, want) {
			t.Errorf("createWorkRun lacks %s:\n%s", want, call)
		}
	}
	// The call the engine receives is the one the parser reads, so it is
	// parsed here rather than trusted.
	parsed, err := langparser.ParseExpression(strings.TrimPrefix(call, "mutation "))
	if err != nil {
		t.Fatalf("the parser refused createWorkRun:\n%s\n%v", call, err)
	}
	fn, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("createWorkRun parsed as %T", parsed)
	}
	vars, ok := fn.Args["variables"].(map[string]any)
	if !ok || vars["note"] != `a "quoted" <tag>` {
		t.Fatalf("variables did not survive the round trip: %#v", fn.Args["variables"])
	}

	blank := &countingEngine{}
	if _, err := New(blank, nil, "node-1").Begin(context.Background(), work()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	control := runCallOf(t, blank.calls)
	for _, absent := range []string{"goalSignature", "parentRunId", "variables"} {
		if strings.Contains(control, absent) {
			t.Errorf("a run with no parent wrote %s anyway:\n%s", absent, control)
		}
	}
}
