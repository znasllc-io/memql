package workjournal

import (
	"context"
	"strings"
	"testing"
	"time"

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

// THE SAME GATE ON THE SECOND WAY TO A HANDLE. Reopen hands a resumed driver a
// handle on a run another replica opened, so it is a way past Begin -- and the
// allowlist entry's claim is about every call site, not only Begin's. With no
// owner, no goal or no run to name there is no handle, and the nil one every
// method tolerates writes nothing all the way down.
func TestReopenRefusesWithoutItsOwnerAndWritesNothing(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")

	for name, ids := range map[string][3]string{
		"a blank owner": {"   ", "goal-1", "run-1"},
		"a blank goal":  {"user-1", "", "run-1"},
		"a blank run":   {"user-1", "goal-1", " "},
	} {
		t.Run(name, func(t *testing.T) {
			run := j.Reopen(ids[0], ids[1], ids[2], work().Steps, time.Now())
			if run != nil {
				t.Fatal("Reopen returned a usable run")
			}
			run.Heartbeat(context.Background())
			step, err := run.Step(context.Background(), "extract")
			if err == nil || step != nil {
				t.Fatal("missing journal must refuse an intent")
			}
			step.Finish(context.Background(), Receipt{Status: "done"})
			step.Cancelled(context.Background(), "why")
			run.Cancelled(context.Background(), "code", "message")
			run.Succeeded(context.Background(), nil)
		})
	}
	if len(engine.calls) != 0 {
		t.Fatalf("calls = %v, want none", engine.calls)
	}
}

// Every write a RUNNER-OWNED run adds (epic memql#5477) carries the stamp and
// the owner's actor too: the queued steps, the heartbeat, the full receipt,
// both cancels, and every write made through a reopened handle. Without the
// stamp each @serverOnly write is refused with a WARN and nothing else.
func TestEveryRunnerWriteCarriesInternalOriginAndTheOwnersActor(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")

	w := work()
	w.QueueSteps = true
	w.TriggeredBy = "pipeline:full"
	run, err := j.Begin(context.Background(), w)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	run.Heartbeat(context.Background())
	mustJournalStep(t, run, context.Background(), "extract").Finish(context.Background(), Receipt{
		Status: "done", DurationMs: 10, Binding: map[string]any{"surface": "cluster"},
		LogFileID: "v1:library:file:log", ArtifactFileIDs: []string{"v1:library:file:a"},
	})
	mustJournalStep(t, run, context.Background(), "summarize").Cancelled(context.Background(), "stopped")
	run.Cancelled(context.Background(), "pipeline_cancelled", "stopped")

	reopened := j.Reopen("user-1", run.GoalID(), run.RunID(), w.Steps, time.Now())
	reopened.Heartbeat(context.Background())
	mustJournalStep(t, reopened, context.Background(), "extract").Finish(context.Background(), Receipt{Status: "failed", Code: "x", Message: "y"})
	reopened.Failed(context.Background(), "x", "y")

	pending := 0
	for _, q := range engine.calls {
		if strings.HasPrefix(q, "mutation createWorkStep(") && strings.Contains(q, `status: "pending"`) {
			pending++
		}
	}
	if pending != len(w.Steps) {
		t.Fatalf("queued %d steps, want %d -- the queued writes are not covered", pending, len(w.Steps))
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
	step := mustJournalStep(t, run, context.Background(), "extract")
	step.Done(context.Background(), map[string]any{"characters": 10})
	second := mustJournalStep(t, run, context.Background(), "summarize")
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

// Execution drivers need a durable intent before work and a durable receipt
// before claiming success. Observability-only callers may still ignore errors.
func TestAFailedJournalWriteIsReturnedToTheDriver(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")
	run, err := j.Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	engine.fail = true
	// A caller must be able to stop before a side effect when its intent
	// did not land, and must never mistake a failed close for a receipt.
	before := len(engine.calls)
	if step, err := run.Step(context.Background(), "extract"); step != nil || err == nil {
		t.Fatal("failed intent was reported as recorded")
	}
	if err := run.Succeeded(context.Background(), nil); err == nil {
		t.Fatal("failed close was reported as recorded")
	}
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
	step, stepErr := run.Step(context.Background(), "extract")
	if stepErr == nil || step != nil {
		t.Fatal("missing journal must refuse an intent")
	}
	step.Done(context.Background(), nil)
	step.Failed(context.Background(), "x", "y")
	step.Skipped(context.Background(), "z")
	step.Finish(context.Background(), Receipt{Status: "done"})
	step.Cancelled(context.Background(), "z")
	run.Heartbeat(context.Background())
	run.Succeeded(context.Background(), nil)
	run.Failed(context.Background(), "x", "y")
	run.Cancelled(context.Background(), "x", "y")
	if reopened := j.Reopen("user-1", "goal-1", "run-1", nil, time.Now()); reopened != nil {
		t.Fatal("Reopen on a nil journal returned a usable run")
	}
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
	mustJournalStep(t, run, context.Background(), "extract")
	mustJournalStep(t, run, context.Background(), "summarize")

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
	mustJournalStep(t, run, context.Background(), "extract").Done(context.Background(), nil)

	for _, q := range engine.calls {
		if strings.Contains(q, `: ""`) {
			t.Fatalf("a blank argument was sent: %s", q)
		}
	}
}
