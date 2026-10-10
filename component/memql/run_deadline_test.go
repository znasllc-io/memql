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
	for _, prompt := range []string{"workAgentReply", "libraryRevisionResearch", "anotherAuthoredPrompt"} {
		for _, modality := range []airoute.Modality{airoute.ModalityTools, airoute.ModalityStreamingTools} {
			t.Run(prompt+"/"+string(modality), func(t *testing.T) {
				testAgentAttemptsConsumeBudget(t, prompt, modality)
			})
		}
	}
}

func testAgentAttemptsConsumeBudget(t *testing.T, prompt string, modality airoute.Modality) {
	t.Helper()
	guard := &fakeGuard{}
	journal := newCountingJournal()
	seam := &modelSeam{ceilings: guard, journal: journal}
	ctx, cancel := context.WithCancelCause(common.ContextWithRun(context.Background(), aRun()))
	defer cancel(nil)
	observe := seam.observeAgentSpend(ctx, cancel)
	for _, phase := range []string{"running", "completed"} {
		// The same DSL prompt can also be called through InvokeAI. Its
		// modality, not its name, identifies who owns the accounting.
		observe(airoute.CallObservation{ID: "prompt", Phase: phase, PromptName: prompt, Modality: string(airoute.ModalityChat)})
		observe(airoute.CallObservation{ID: "checkpoint", Phase: phase, PromptName: "workContextCheckpoint", Modality: string(airoute.ModalityStructured)})
	}
	if guard.admits != 0 || journal.records != 0 {
		t.Fatal("prompt calls were double charged")
	}
	observe(airoute.CallObservation{ID: "reply", Phase: "running", PromptName: prompt, Modality: string(modality)})
	finished := airoute.CallObservation{ID: "reply", Phase: "completed", PromptName: prompt, Modality: string(modality), Billing: "local", InputTokens: 10, OutputTokens: 2}
	observe(finished)
	observe(finished)
	if guard.admits != 1 || len(guard.charges) != 1 || journal.records != 1 {
		t.Fatalf("admits=%d charges=%d receipts=%d", guard.admits, len(guard.charges), journal.records)
	}
	if guard.charges[0].Served != ServedLocal || guard.charges[0].InputTokens != 10 {
		t.Fatalf("spend = %+v", guard.charges[0])
	}
	guard.breach = &RunCeilingBreach{Ceiling: "modelCalls", Limit: "1", Actual: "1"}
	observe(airoute.CallObservation{ID: "next", Phase: "running", PromptName: prompt, Modality: string(modality)})
	var breach *RunCeilingError
	if !errors.As(context.Cause(ctx), &breach) {
		t.Fatalf("next attempt was not stopped: %v", context.Cause(ctx))
	}
	observe(airoute.CallObservation{ID: "next", Phase: "failed", PromptName: prompt, Modality: string(modality)})
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

type adaptiveDeadlineGuard struct {
	deadlineGuard
	declared time.Time
}

func (g *adaptiveDeadlineGuard) DeclaredDeadline(context.Context, common.RunContext) (time.Time, error) {
	return g.declared, nil
}

func TestAgentModelDeadlineCanGrowWithinTheOriginalToolLoop(t *testing.T) {
	start := time.Now()
	guard := &adaptiveDeadlineGuard{deadlineGuard: deadlineGuard{at: start.Add(time.Minute)}, declared: start.Add(5 * time.Minute)}
	e := &MemQLEngine{modelSeam: &modelSeam{ceilings: guard}}
	ctx := common.ContextWithRun(context.Background(), aRun())
	loop, stop, err := e.ContextWithRunDeadline(ctx)
	defer stop()
	if err != nil {
		t.Fatal(err)
	}
	for _, duration := range []time.Duration{time.Minute, 5 * time.Minute} {
		guard.at = start.Add(duration)
		call, stopCall, err := e.ContextWithWorkCallDeadline(loop)
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := call.Deadline()
		stopCall()
		if !ok || !deadline.Equal(start.Add(duration)) {
			t.Fatalf("call inherited stale estimate: %v", deadline)
		}
		if loop.Err() != nil {
			t.Fatal("finishing a model call cancelled the run")
		}
	}
}

func TestExpiredWorkDeadlineReturnsTypedBudgetRefusalBeforeAnotherProviderAttempt(t *testing.T) {
	guard := &deadlineGuard{at: time.Now().Add(-time.Second), fakeGuard: fakeGuard{
		breach: &RunCeilingBreach{Ceiling: "wallClock", Limit: "2700000ms", Actual: "2700001ms"},
	}}
	e := &MemQLEngine{modelSeam: &modelSeam{ceilings: guard}}
	ctx := common.ContextWithRun(context.Background(), aRun())
	_, stop, err := e.ContextWithWorkCallDeadline(ctx)
	defer stop()
	var breach *RunCeilingError
	if !errors.As(err, &breach) || breach.Breach.Ceiling != "wallClock" {
		t.Fatalf("expired run became a provider timeout: %v", err)
	}
}

func TestInFlightRunDeadlineResolvesCurrentSpendAndKeepsProviderTimeoutsSeparate(t *testing.T) {
	guard := &deadlineGuard{at: time.Now().Add(20 * time.Millisecond)}
	e := &MemQLEngine{modelSeam: &modelSeam{ceilings: guard, journal: newCountingJournal()}}
	ctx := common.ContextWithRun(context.Background(), aRun())
	_, err := e.modelSeam.serve(ctx, common.ModelRequest{}, "deadline", func(call context.Context) (modelCallOutcome, error) {
		<-call.Done()
		guard.breach = &RunCeilingBreach{Ceiling: "wallClock", Limit: "2700000ms", Actual: "2700001ms"}
		return modelCallOutcome{}, call.Err()
	})
	var breach *RunCeilingError
	if !errors.As(err, &breach) || breach.RunId != aRun().RunId || len(guard.charges) != 1 {
		t.Fatalf("in-flight deadline did not park with its charged attempt: %v charges=%d", err, len(guard.charges))
	}
	provider, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	if err := e.WorkCallFailure(provider); err != nil {
		t.Fatalf("provider timeout was relabeled as the run's deadline: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.WorkCallFailure(cancelled); err != nil {
		t.Fatalf("user cancellation was relabeled: %v", err)
	}
}
