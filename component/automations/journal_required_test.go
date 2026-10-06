package automations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type failingRequiredJournal struct {
	mu    sync.Mutex
	t     *testing.T
	fail  func(string, map[string]any) bool
	calls []string
}

func (j *failingRequiredJournal) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	name, args := argsOf(j.t, query)
	j.calls = append(j.calls, query)
	if j.fail != nil && j.fail(name, args) {
		return nil, errBoom
	}
	return &memql.ExecuteResult{}, nil
}

func TestRequiredJournalStopsAtEveryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, statement, wantCalls string
		fail                       func(string, map[string]any) bool
	}{
		{"run-intent", "builtin prepare()", "", func(n string, _ map[string]any) bool { return n == "createWorkRun" }},
		{"step-intent", "builtin prepare() retry(2) on error continue", "", func(n string, _ map[string]any) bool { return n == "createWorkStep" }},
		{"step-receipt", "builtin prepare() retry(2) on error continue", "prepare", func(n string, _ map[string]any) bool { return n == "updateWorkStep" }},
		{"run-progress", "builtin prepare()", "prepare", func(n string, a map[string]any) bool { return n == "updateWorkRun" && a["status"] == nil }},
		{"terminal", "builtin prepare()", "prepare,publish", func(n string, a map[string]any) bool { return n == "updateWorkRun" && a["status"] == "succeeded" }},
		{"expression-intent", "value := 1", "", func(n string, _ map[string]any) bool { return n == "createWorkStep" }},
		{"expression-receipt", "value := 1", "", func(n string, _ map[string]any) bool { return n == "updateWorkStep" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := statementAutomation(t, fmt.Sprintf("@template\n@journalRequired\nautomation guarded {\n%s\nbuiltin publish()\n}", tc.statement))
			if !a.JournalRequired {
				t.Fatal("compiler discarded the required-journal declaration")
			}
			probe := newStmtProbe()
			e := NewExecutor(ExecutorOptions{StepRegistry: probe})
			j := &failingRequiredJournal{t: t, fail: tc.fail}
			e.journal = newWorkJournal(j, nil)
			exec, err := e.Execute(context.Background(), a, "test")
			if !errors.Is(err, ErrJournalRequired) || exec.Status != "failed" {
				t.Fatalf("unconfirmed journal was accepted: status=%s err=%v", exec.Status, err)
			}
			if got := strings.Join(probe.callees(), ","); got != tc.wantCalls {
				t.Fatalf("effects=%s, want %s", got, tc.wantCalls)
			}
			// An outage is per run, not a poisoned shared executor. A later
			// independent request can run when the journal is available again.
			j.fail = nil
			exec, err = e.Execute(context.Background(), a, "test")
			if err != nil || exec.Status != "completed" {
				t.Fatalf("recovered journal: %s %v", exec.Status, err)
			}
		})
	}
}

func TestRequiredJournalMissingAndPreview(t *testing.T) {
	a := statementAutomation(t, "@template\n@journalRequired\nautomation guarded { builtin prepare() }")
	probe := newStmtProbe()
	_, err := NewExecutor(ExecutorOptions{StepRegistry: probe}).Execute(context.Background(), a, "test")
	if !errors.Is(err, ErrJournalRequired) || len(probe.callees()) != 0 {
		t.Fatalf("missing journal ran work: %v %v", err, probe.callees())
	}
	// The sandbox registry remains responsible for intercepting effects.
	e := NewExecutor(ExecutorOptions{StepRegistry: probe, SandboxRun: true})
	exec, err := e.Execute(context.Background(), a, "test")
	if err != nil || exec.Status != "completed" || e.journal != nil {
		t.Fatalf("preview must remain unjournaled: %s %v", exec.Status, err)
	}
}

func TestRequiredJournalLeavesOrdinaryAutomationBestEffort(t *testing.T) {
	a := statementAutomation(t, "@template\nautomation unguarded { builtin prepare() }")
	probe := newStmtProbe()
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	e.journal = newWorkJournal(&failingRequiredJournal{t: t, fail: func(string, map[string]any) bool { return true }}, nil)
	exec, err := e.Execute(context.Background(), a, "test")
	if err != nil || exec.Status != "completed" || len(probe.callees()) != 1 {
		t.Fatalf("ordinary automation changed behavior: %s %v", exec.Status, err)
	}
}

type awaitJournalCancellation struct{}

func (awaitJournalCancellation) Execute(ctx context.Context, _ *Step, _ *StepContext) (*StepResult, error) {
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-time.After(2 * time.Second):
		return nil, errors.New("journal failure did not cancel the running call")
	}
}

func TestRequiredJournalHeartbeatCancelsInflightWork(t *testing.T) {
	a := statementAutomation(t, "@template\n@journalRequired\nautomation guarded { builtin wait() }")
	e := NewExecutor(ExecutorOptions{StepRegistry: awaitJournalCancellation{}})
	e.journal = newWorkJournal(&failingRequiredJournal{t: t, fail: func(n string, _ map[string]any) bool { return n == "updateWorkRun" }}, nil)
	e.journal.heartbeatEvery = time.Millisecond
	exec, err := e.Execute(context.Background(), a, "test")
	if !errors.Is(err, ErrJournalRequired) || exec.Status != "failed" {
		t.Fatalf("heartbeat failure: %s %v", exec.Status, err)
	}
}

func TestRequiredJournalPropagatesThroughChildExecutions(t *testing.T) {
	parent := statementAutomation(t, "@template\n@journalRequired\nautomation parent { builtin child() retry(2) on error continue\nbuiltin publish() }")
	child := statementAutomation(t, "@template\nautomation child { builtin prepare() }")
	probe := newStmtProbe()
	childExecutor := NewExecutor(ExecutorOptions{StepRegistry: probe})
	childExecutor.journal = newWorkJournal(&failingRequiredJournal{t: t, fail: func(n string, _ map[string]any) bool { return n == "updateWorkStep" }}, nil)
	parentCalls := 0
	e := NewExecutor(ExecutorOptions{StepRegistry: modeRegistryFunc(func(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
		parentCalls++
		_, err := childExecutor.Execute(ctx, child, "parent")
		return nil, err
	})})
	e.journal = newWorkJournal(&recordingJournalExecutor{}, nil)
	exec, err := e.Execute(context.Background(), parent, "test")
	if !errors.Is(err, ErrJournalRequired) || exec.Status != "failed" || parentCalls != 1 || len(probe.callees()) != 1 {
		t.Fatalf("child's unconfirmed effect advanced its parent: %s %v calls=%d child=%v", exec.Status, err, parentCalls, probe.callees())
	}
}

type failRequiredReceipt struct {
	engine *memql.MemQLEngine
	stepID string
}

func (j failRequiredReceipt) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(query, "updateWorkStep(") && strings.Contains(query, j.stepID) {
		return nil, errBoom
	}
	return j.engine.Execute(ctx, query)
}

func TestRequiredJournalDBResumesOnAnotherExecutor(t *testing.T) {
	engine := sharedJournalEngine(t)
	a := statementAutomation(t, fmt.Sprintf("@template\n@journalRequired\nautomation durableRead%d { query first()\nbuiltin serviceVersion()\nquery third() }", time.Now().UnixNano()))
	firstProbe := newStmtProbe()
	firstProbe.answers["first"] = memql.NewResultWithOutput(map[string]any{"value": "kept"})
	first := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: firstProbe})
	defer first.Close()
	first.journal = newWorkJournal(failRequiredReceipt{engine: engine, stepID: "-" + a.Steps[1].ID + `"`}, nil)
	exec, err := first.Execute(context.Background(), a, "test")
	if !errors.Is(err, ErrJournalRequired) {
		t.Fatalf("receipt failure was hidden: %v", err)
	}
	journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Status != "running" || journal.StepStates[a.Steps[1].ID].Status != "running" || journal.Steps[a.Steps[0].ID] == nil {
		t.Fatalf("last confirmed journal state lost: %+v", journal)
	}
	// A new executor has no memory of the interrupted run. The recorded
	// first result is kept; the uncertain read is safe to repeat. External
	// effects require their own reconciliation contract before this point.
	secondProbe := newStmtProbe()
	second := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: secondProbe})
	defer second.Close()
	resumed, err := second.ResumeFrom(context.Background(), journal, a, nil)
	if err != nil || resumed.ID != exec.ID || resumed.Status != "completed" {
		t.Fatalf("resume on another executor: %+v %v", resumed, err)
	}
	if got := strings.Join(secondProbe.callees(), ","); got != "serviceVersion,third" {
		t.Fatalf("resumed calls=%s, want serviceVersion,third", got)
	}
}
