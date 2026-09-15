package automations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// cancel_test.go -- memql#5066.
//
// `v1:work:run.cancelRequested` was written by the cancel builtin
// (integrations/work/goal.go) and READ BY NOTHING. A person asking a run to
// stop got a flag set on a row and a run that continued to completion, with no
// error anywhere -- the same documented-and-inert shape as the kill-switch
// automation this issue is about. These tests are what make the field mean
// something, and every one of them fails against an executor that does not
// read it.

// cancelAnsweringExecutor is the journal seam, answering `workRunById` from a
// scripted sequence so a run can be un-cancelled at one boundary and cancelled
// at the next.
type cancelAnsweringExecutor struct {
	calls []string
	// flags is consumed one entry per workRunById; the last entry repeats.
	flags []bool
	// failReads makes every workRunById fail, which is the fail-open case.
	failReads bool
	reads     int
}

func (c *cancelAnsweringExecutor) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	c.calls = append(c.calls, query)
	if !strings.HasPrefix(strings.TrimSpace(query), "query workRunById") {
		return &memql.ExecuteResult{}, nil
	}
	c.reads++
	if c.failReads {
		return nil, errors.New("read timeout")
	}
	flag := false
	if len(c.flags) > 0 {
		i := c.reads - 1
		if i >= len(c.flags) {
			i = len(c.flags) - 1
		}
		flag = c.flags[i]
	}
	return memql.NewResultWithOutput([]any{map[string]any{
		"id":              "run-1",
		"cancelRequested": flag,
		"cancelledBy":     "u-alice",
	}}), nil
}

func (c *cancelAnsweringExecutor) journalNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, call := range c.calls {
		if strings.HasPrefix(strings.TrimSpace(call), "query workRunById") {
			out = append(out, "workRunById")
			continue
		}
		n, _ := argsOf(t, call)
		out = append(out, n)
	}
	return out
}

// countingRegistry records which steps actually ran, which is the only thing
// that distinguishes "stopped" from "reported stopped".
type countingRegistry struct{ ran []string }

func (r *countingRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	r.ran = append(r.ran, step.ID)
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

func threeStepAutomation() *Automation {
	return &Automation{Name: "demo", Steps: []*Step{
		{ID: "a", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}},
		{ID: "b", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}},
		{ID: "c", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}},
	}}
}

// THE PROPERTY. A cancel that arrives mid-run stops the run at the NEXT step
// boundary -- the step already in flight finishes and is journaled, and the
// ones after it never start.
func TestExecutor_StopsAtTheNextStepBoundaryWhenTheRunIsCancelled(t *testing.T) {
	rec := &cancelAnsweringExecutor{flags: []bool{false, true}}
	steps := &countingRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: steps, CancelPollInterval: time.Nanosecond})
	e.journal = newWorkJournal(rec, nil)

	exec, err := e.Execute(context.Background(), threeStepAutomation(), "test")
	if err != nil {
		t.Fatalf("a cancelled run is not an error: %v", err)
	}
	if strings.Join(steps.ran, ",") != "a" {
		t.Fatalf("steps run = %v, want just [a]: the cancel arrived at b's boundary, so b and c must never start", steps.ran)
	}
	if exec.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", exec.Status)
	}
	last := rec.calls[len(rec.calls)-1]
	name, args := argsOf(t, last)
	if name != "updateWorkRun" || args["status"] != "cancelled" {
		t.Fatalf("the run did not close cancelled: %s %v", name, args)
	}
	if args["cancelledBy"] != "u-alice" {
		t.Errorf("cancelledBy = %v, want the person who asked -- 'cancelled' with no author reads as a fault", args["cancelledBy"])
	}
	if msg, _ := args["errorMessage"].(string); msg == "" {
		t.Error("the run closed with no reason; the run rail is where a person finds out why their work stopped")
	}
}

// A cancel that landed while the run was QUEUED must cost zero steps. The
// poller's zero value asks at the first boundary for exactly this.
func TestExecutor_RunsNoStepAtAllWhenTheCancelArrivedFirst(t *testing.T) {
	rec := &cancelAnsweringExecutor{flags: []bool{true}}
	steps := &countingRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: steps})
	e.journal = newWorkJournal(rec, nil)

	exec, err := e.Execute(context.Background(), threeStepAutomation(), "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(steps.ran) != 0 {
		t.Fatalf("ran %v; a run cancelled before it started must execute nothing", steps.ran)
	}
	if exec.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", exec.Status)
	}
}

// THE NEGATIVE CONTROL. Without it every assertion above is satisfied by an
// executor that stops on everything.
func TestExecutor_RunsToCompletionWhenNothingCancelledIt(t *testing.T) {
	rec := &cancelAnsweringExecutor{flags: []bool{false}}
	steps := &countingRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: steps, CancelPollInterval: time.Nanosecond})
	e.journal = newWorkJournal(rec, nil)

	exec, err := e.Execute(context.Background(), threeStepAutomation(), "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Join(steps.ran, ",") != "a,b,c" {
		t.Fatalf("steps run = %v, want all three", steps.ran)
	}
	if exec.Status != "completed" {
		t.Fatalf("status = %q, want completed", exec.Status)
	}
}

// IT FAILS OPEN. A read error means the executor could not reach the journal,
// which is a database problem; reading it as "cancelled" would stop every
// running automation in the cluster the moment a query hiccupped.
func TestExecutor_AnUnreadableCancelFlagDoesNotStopTheRun(t *testing.T) {
	rec := &cancelAnsweringExecutor{failReads: true}
	steps := &countingRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: steps, CancelPollInterval: time.Nanosecond})
	e.journal = newWorkJournal(rec, nil)

	exec, err := e.Execute(context.Background(), threeStepAutomation(), "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(steps.ran) != 3 {
		t.Fatalf("ran %v; a failed READ of the flag must not be read as a cancel", steps.ran)
	}
	if exec.Status != "completed" {
		t.Fatalf("status = %q", exec.Status)
	}
}

// THE COST, asserted rather than asserted-about. At the shipped interval a
// short run asks ONCE, not once per step -- automation steps are frequently
// sub-millisecond and a read per step would often cost more than the step.
func TestExecutor_AsksForTheCancelFlagOnceOnAShortRun(t *testing.T) {
	rec := &cancelAnsweringExecutor{flags: []bool{false}}
	e := NewExecutor(ExecutorOptions{StepRegistry: &countingRegistry{}})
	e.journal = newWorkJournal(rec, nil)

	if _, err := e.Execute(context.Background(), threeStepAutomation(), "test"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.reads != 1 {
		t.Fatalf("the run read its cancel flag %d times across 3 steps; at the shipped %s interval a run this short must ask once",
			rec.reads, CancelPollInterval)
	}
	names := rec.journalNames(t)
	if names[0] != "createWorkRun" || names[1] != "workRunById" {
		t.Errorf("journal order = %v; the run row must be OPEN before the first cancel read, or the read asks about a row that does not exist yet", names)
	}
}

// A run with no journal -- a sandboxed dry-run, or an automation that reacts to
// work rows -- has no row to read and no cancel to honour. It must not query
// for one.
func TestExecutor_AnUnjournaledRunAsksNothingAboutCancellation(t *testing.T) {
	steps := &countingRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: steps, SandboxRun: true, CancelPollInterval: time.Nanosecond})
	if e.journal != nil {
		t.Fatal("a sandboxed run must hold no journal")
	}
	exec, err := e.Execute(context.Background(), threeStepAutomation(), "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(steps.ran) != 3 || exec.Status != "completed" {
		t.Fatalf("a preview must run normally: ran %v, status %q", steps.ran, exec.Status)
	}
}
