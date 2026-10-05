package memql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type deadlineGuard struct {
	fakeGuard
	at time.Time
}

func TestAgentToolLoopInheritsThePersistedDeadline(t *testing.T) {
	deadline := time.Now().Add(30 * time.Millisecond)
	e := &MemQLEngine{modelSeam: &modelSeam{ceilings: &deadlineGuard{at: deadline}}}
	ctx := common.ContextWithRun(context.Background(), aRun())
	bounded, stop, err := e.ContextWithRunDeadline(ctx)
	defer stop()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := bounded.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("deadline = %v, %v", got, ok)
	}
	<-bounded.Done()
	if !errors.Is(bounded.Err(), context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("agent loop did not inherit an isolated deadline")
	}
}

func TestAgentAttemptsConsumeTheRunBudgetWithoutDoubleChargingPromptCalls(t *testing.T) {
	guard := &fakeGuard{}
	journal := newCountingJournal()
	seam := &modelSeam{ceilings: guard, journal: journal}
	ctx, cancel := context.WithCancelCause(common.ContextWithRun(context.Background(), aRun()))
	defer cancel(nil)
	observe := seam.observeAgentSpend(ctx, cancel)
	for _, phase := range []string{"running", "completed"} {
		observe(airoute.CallObservation{ID: "prompt", Phase: phase, PromptName: "goalComplexityTriage"})
	}
	if guard.admits != 0 || journal.records != 0 {
		t.Fatal("prompt calls were double charged")
	}
	observe(airoute.CallObservation{ID: "reply", Phase: "running", PromptName: "workAgentReply"})
	finished := airoute.CallObservation{ID: "reply", Phase: "completed", PromptName: "workAgentReply", Billing: "local", InputTokens: 10, OutputTokens: 2}
	observe(finished)
	observe(finished)
	if guard.admits != 1 || len(guard.charges) != 1 || journal.records != 1 {
		t.Fatalf("admits=%d charges=%d receipts=%d", guard.admits, len(guard.charges), journal.records)
	}
	if guard.charges[0].Served != ServedLocal || guard.charges[0].InputTokens != 10 {
		t.Fatalf("spend = %+v", guard.charges[0])
	}
	guard.breach = &RunCeilingBreach{Ceiling: "modelCalls", Limit: "1", Actual: "1"}
	observe(airoute.CallObservation{ID: "next", Phase: "running", PromptName: "workAgentReply"})
	var breach *RunCeilingError
	if !errors.As(context.Cause(ctx), &breach) {
		t.Fatalf("next attempt was not stopped: %v", context.Cause(ctx))
	}
	observe(airoute.CallObservation{ID: "next", Phase: "failed", PromptName: "workAgentReply"})
	if journal.records != 1 {
		t.Fatal("refused attempt was charged")
	}
}

func (g *deadlineGuard) Deadline(context.Context, common.RunContext) (time.Time, error) {
	return g.at, nil
}

func TestRunDeadlineInterruptsAnInFlightModelAndRecordsFailure(t *testing.T) {
	journal := newCountingJournal()
	guard := &deadlineGuard{at: time.Now().Add(40 * time.Millisecond)}
	seam := &modelSeam{ceilings: guard, journal: journal}
	ctx := common.ContextWithRun(context.Background(), aRun())
	entered := false
	_, err := seam.serve(ctx, common.ModelRequest{}, "deadline-test", func(callCtx context.Context) (modelCallOutcome, error) {
		entered = true
		<-callCtx.Done()
		return modelCallOutcome{}, callCtx.Err()
	})
	if !entered || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call was not interrupted: entered=%v err=%v", entered, err)
	}
	if ctx.Err() != nil {
		t.Fatal("model deadline cancelled the caller's receipt context")
	}
	if journal.records != 1 {
		t.Fatalf("timed-out call was not journaled: %d", journal.records)
	}
}
