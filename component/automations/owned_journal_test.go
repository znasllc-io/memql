package automations

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

func TestAdoptedRunJournalKeepsTheRequestersOwnership(t *testing.T) {
	ctx := auth.ContextWithUserActor(context.Background(), "user-materializer")
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "owned-run", GoalId: "owned-goal", OwnerUserId: "user-materializer", Mode: common.RunModeLive})
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(rec, nil)
	result, err := e.ExecuteAdopted(ctx, adoptProbeAutomation(), RunAdoption{RunId: "owned-run"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("execute: result=%v err=%v", result, err)
	}
	writes := 0
	for n, q := range rec.calls {
		name, _ := argsOf(t, q)
		if name != "createWorkStep" && name != "updateWorkStep" && name != "updateWorkRun" {
			continue
		}
		writes++
		ac, _ := auth.AccessFromContext(rec.ctxs[n])
		if ac == nil || ac.UserId != "user-materializer" || ac.Synthetic {
			t.Errorf("%s wrote as %+v; Nexus cannot read a synthetic step belonging to someone else", name, ac)
		}
	}
	if writes == 0 {
		t.Fatal("no work writes were exercised")
	}
}

func TestStepContextAddsItsKeyWithoutLosingRunLineage(t *testing.T) {
	want := common.RunContext{RunId: "r", GoalId: "g", OwnerUserId: "u", Mode: common.RunModeLive}
	ctx := common.ContextWithRun(context.Background(), want)
	e := NewExecutor(ExecutorOptions{})
	ctx = e.withRunContext(ctx, &StepContext{Execution: &AutomationExecution{ID: "r"}}, &Step{ID: "compose"})
	got, _ := common.RunFromContext(ctx)
	if got.RunId != "r" || got.GoalId != "g" || got.OwnerUserId != "u" || got.StepKey != "compose" {
		t.Fatalf("step lost its journal association: %+v", got)
	}
}

func TestLoadedRunCarriesExecutionOwnershipAndReplayLineage(t *testing.T) {
	j, err := runJournalFromRows(map[string]any{
		"id": "v1:work:run:r", "status": "running", "goalId": "v1:work:goal:g", "ownerUserId": "u",
		"mode": "fork", "forkedFromRunId": "source", "forkAtStepKey": "draft", "replayPolicy": "strict",
		"variables": map[string]any{"name": "Marvel heroes"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "running" || j.GoalId != "v1:work:goal:g" || j.OwnerUserId != "u" || j.Variables["name"] != "Marvel heroes" || j.Mode != "fork" || j.ForkedFromRunId != "source" || j.ForkAtStepKey != "draft" || j.ReplayPolicy != "strict" {
		t.Fatalf("the execution hop discarded persisted metadata: %+v", j)
	}
}

func TestAdoptionPersistsCallerTrustForResume(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(rec, nil)
	a := adoptProbeAutomation()
	a.Trusted = true
	_, err := e.ExecuteAdopted(context.Background(), a, RunAdoption{RunId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	_, args := argsOf(t, rec.calls[0])
	if args["callerSuppliedPayload"] != true {
		t.Fatal("adoption did not persist client-origin boundary; resume would promote caller arguments")
	}
}

type resumedArgumentProbe struct{ got any }

func (p *resumedArgumentProbe) Execute(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
	p.got, _ = evalV1(sc.Evaluator, "args.compositionId")
	return &StepResult{StepId: step.ID, Status: "completed"}, nil
}

func TestResumeBindsPersistedMaterializerArguments(t *testing.T) {
	p := &resumedArgumentProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: p})
	a := adoptProbeAutomation()
	a.Args = &ArgsSchema{Fields: []*ArgsField{{Name: "compositionId", Type: "string"}}}
	j := &RunJournal{RunId: "r", GoalId: "g", AutomationName: "demo", FailedStep: "a", CallerSuppliedPayload: true, Variables: map[string]any{"compositionId": "composition"}}
	result, err := e.ResumeFrom(context.Background(), j, a, nil)
	if err != nil || result.Status != "completed" {
		t.Fatalf("resume: %v %v", result, err)
	}
	if p.got != "composition" {
		t.Fatalf("resumed argument=%v; lost persisted template input", p.got)
	}
}

func TestJournalRecordsForkOrderWithoutChainTracking(t *testing.T) {
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	a := adoptProbeAutomation()
	a.Steps = append(a.Steps, &Step{ID: "b", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}})
	first, err := e.ExecuteAdopted(context.Background(), a, RunAdoption{RunId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.StepOrder, []string{"a", "b"}) {
		t.Fatalf("fork lost executed prefix order: %v", first.StepOrder)
	}
	j := &RunJournal{RunId: "r", AutomationName: "demo", FailedStep: "b", StepOrder: first.StepOrder, Steps: map[string]*MinimalStepResult{"a": {StepId: "a", Status: "completed"}}}
	resumed, err := e.ResumeFrom(context.Background(), j, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.StepOrder, []string{"a", "b"}) {
		t.Fatalf("resume lost prefix order: %v", resumed.StepOrder)
	}
}

func TestLoadedJournalFindsEveryRunningIntent(t *testing.T) {
	now := time.Now().UTC()
	j, err := runJournalFromRows(map[string]any{"id": "r", "automationName": "a", "status": "running", "heartbeatAt": now.Format(time.RFC3339Nano)}, []map[string]any{{"key": "oldFailure", "status": "failed"}, {"key": "current", "status": "running"}})
	if err != nil {
		t.Fatal(err)
	}
	if !j.HasRunningStep || !j.HeartbeatAt.Equal(now) {
		t.Fatalf("lost live execution evidence: %+v", j)
	}
}
