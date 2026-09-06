package workjournal

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// internal_origin_precondition_test.go -- the assertion behind this
// package's entry on the ContextWithInternalOrigin allowlist
// (call_origin_conformance_test.go).
//
// The entry's claim is: "every call site is downstream of one gate -- Begin
// refuses a blank owner, so no row is ever written under an actor that could
// read nothing back". This file drives every method against an engine that
// refuses everything and counts what was attempted, which is the shape
// component/identity/adminops/gate_test.go set for the same purpose. The
// allowlist entry is not the safety property; this is.

type countingEngine struct {
	calls   []string
	origins []bool
	owners  []string
	fail    bool
}

func (e *countingEngine) Execute(ctx context.Context, query string) (any, error) {
	e.calls = append(e.calls, query)
	e.origins = append(e.origins, auth.OriginFromContext(ctx).IsInternal())
	access, _ := auth.AccessFromContext(ctx)
	owner := ""
	if access != nil {
		owner = access.UserId
	}
	e.owners = append(e.owners, owner)
	if e.fail {
		return nil, context.DeadlineExceeded
	}
	return nil, nil
}

func work() Work {
	return Work{
		OwnerUserID: "user-1",
		Template:    "libraryAnalyzeFile",
		Statement:   "Analyze notes.pdf",
		GoalKey:     "v1:library:file:abc",
		Input:       map[string]any{"fileId": "v1:library:file:abc"},
		Steps: []StepDecl{
			{Key: "extract", Kind: KindDeterministic},
			{Key: "summarize", Kind: KindReasoning},
		},
	}
}

// THE GATE. Nothing downstream can run, because there is no handle.
func TestBeginRefusesABlankOwnerAndWritesNothing(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")

	w := work()
	w.OwnerUserID = "   "
	run, err := j.Begin(context.Background(), w)
	if err == nil {
		t.Fatal("Begin accepted a blank owner")
	}
	if run != nil {
		t.Fatal("Begin returned a usable run for a blank owner")
	}
	if len(engine.calls) != 0 {
		t.Fatalf("calls = %v, want none", engine.calls)
	}
	if !strings.Contains(err.Error(), "readable by nobody") {
		t.Fatalf("error = %q, want it to say why", err)
	}
}

// Every write this package makes carries the stamp. Without it the function
// validator refuses each @serverOnly mutation with a WARN and nothing else,
// which is the silent-failure shape the allowlist entry describes.
func TestEveryWriteCarriesInternalOriginAndTheOwnersActor(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")

	run, err := j.Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	step := run.Step(context.Background(), "extract")
	step.Done(context.Background(), map[string]any{"characters": 10})
	second := run.Step(context.Background(), "summarize")
	second.Failed(context.Background(), "provider_down", "no summariser")
	run.Succeeded(context.Background(), map[string]any{"chunks": 3})

	if len(engine.calls) == 0 {
		t.Fatal("nothing was written")
	}
	for i, internal := range engine.origins {
		if !internal {
			t.Fatalf("call %d (%s) did not carry internal origin", i, firstWord(engine.calls[i]))
		}
		if engine.owners[i] != "user-1" {
			t.Fatalf("call %d (%s) ran as %q, want the owner", i, firstWord(engine.calls[i]), engine.owners[i])
		}
	}
}

// The journal is a RECORD of work, not the work. A pass that did its job must
// not be reported as failed because a step row did not land.
func TestAFailedJournalWriteIsLoggedRatherThanReturned(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")
	run, err := j.Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	engine.fail = true
	// None of these returns anything, which is the point: they cannot fail
	// the caller. The assertion is that they still ATTEMPTED the write.
	before := len(engine.calls)
	run.Step(context.Background(), "extract").Done(context.Background(), nil)
	run.Succeeded(context.Background(), nil)
	if len(engine.calls) <= before {
		t.Fatal("a failing engine silenced the journal instead of being logged")
	}
}

// A nil journal is the same shape as no journal: a caller wires one if it has
// one and calls it unconditionally either way.
func TestANilJournalIsSafeAllTheWayDown(t *testing.T) {
	var j *Journal
	run, err := j.Begin(context.Background(), work())
	if err != nil || run != nil {
		t.Fatalf("Begin on a nil journal = (%v, %v)", run, err)
	}
	if run.RunID() != "" || run.GoalID() != "" {
		t.Fatal("a nil run named ids")
	}
	step := run.Step(context.Background(), "extract")
	step.Done(context.Background(), nil)
	step.Failed(context.Background(), "x", "y")
	step.Skipped(context.Background(), "z")
	run.Succeeded(context.Background(), nil)
	run.Failed(context.Background(), "x", "y")
}

func TestNewWithNoEngineYieldsANilJournal(t *testing.T) {
	if New(nil, nil, "node-1") != nil {
		t.Fatal("New returned a journal with no engine to write through")
	}
}

// A goal is keyed to the SUBJECT and a run to the attempt, which is what
// makes a re-analysis a second run of one goal rather than a second goal.
func TestOneGoalPerSubjectAndOneRunPerAttempt(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")

	first, err := j.Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	w := work()
	w.RunKey = "second"
	second, err := j.Begin(context.Background(), w)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if first.GoalID() != second.GoalID() {
		t.Fatalf("goal moved between attempts: %q then %q", first.GoalID(), second.GoalID())
	}
	if first.RunID() == second.RunID() {
		t.Fatal("two attempts share a run id")
	}
}

// The derived-kind rule (spec section B) recorded honestly: the stage that
// reaches a prompt is `reasoning` and the rest are `deterministic`.
func TestAStepThatReachesAPromptIsRecordedAsReasoning(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")
	run, err := j.Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	run.Step(context.Background(), "extract")
	run.Step(context.Background(), "summarize")

	var extract, summarize string
	for _, q := range engine.calls {
		if !strings.HasPrefix(q, "mutation createWorkStep") {
			continue
		}
		if strings.Contains(q, `key: "extract"`) {
			extract = q
		}
		if strings.Contains(q, `key: "summarize"`) {
			summarize = q
		}
	}
	if !strings.Contains(extract, `kind: "deterministic"`) {
		t.Fatalf("extract step = %s", extract)
	}
	if !strings.Contains(summarize, `kind: "reasoning"`) {
		t.Fatalf("summarize step = %s -- a stage that reaches a prompt is reasoning", summarize)
	}
}

// An empty argument is DROPPED. Every mutation here is a read-merge update,
// so sending a blank would CLEAR a value a previous version legitimately
// holds -- an error message written over by a later success, say.
func TestABlankArgumentIsNotSent(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")
	run, _ := j.Begin(context.Background(), work())
	run.Step(context.Background(), "extract").Done(context.Background(), nil)

	for _, q := range engine.calls {
		if strings.Contains(q, `: ""`) {
			t.Fatalf("a blank argument was sent: %s", q)
		}
	}
}
