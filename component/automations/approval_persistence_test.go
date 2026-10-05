package automations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type rejectingApprovalJournal struct{ recordingJournalExecutor }

func (r *rejectingApprovalJournal) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	r.calls = append(r.calls, query)
	if strings.HasPrefix(query, "createWorkApproval(") {
		return nil, errors.New("approval storage unavailable")
	}
	return &memql.ExecuteResult{}, nil
}

func TestFailedApprovalWriteNeverParksRun(t *testing.T) {
	for _, path := range []string{"feedback", "inference", "budget"} {
		t.Run(path, func(t *testing.T) {
			store := &rejectingApprovalJournal{}
			j := newWorkJournal(store, nil)
			run := failedRun("v1:work:run:approval-write-failure", "connection refused", 4, 3)
			switch path {
			case "feedback":
				j.closeRun(context.Background(), run, "")
			case "inference":
				j.parkOnInference(context.Background(), run, "", work.RefusalNoLocalModel, nil)
			case "budget":
				j.parkOnRunCeiling(context.Background(), run, "", &memql.RunCeilingError{})
			}
			if !anyCallNamed(store.calls, "createWorkApproval") {
				t.Fatal("test did not attempt to persist an approval")
			}
			terminal := false
			for _, query := range store.calls {
				name, args := argsOf(t, query)
				if name != "updateWorkRun" {
					continue
				}
				if args["status"] == "waiting" {
					t.Fatal("failed approval write left a phantom wait")
				}
				terminal = terminal || args["status"] == "failed"
			}
			if !terminal {
				t.Fatal("failed attempt did not record its terminal state")
			}
		})
	}
}

func TestScheduledMaintenanceFailureDoesNotCreateHumanWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		trigger string
		want    string
	}{
		{"maintenance tick", auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), "schedule", "failed"},
		{"manual caller", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "user-a", Role: auth.RoleOwner}), "schedule", "waiting"},
		{"adopted user goal", common.ContextWithRun(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), common.RunContext{RunId: "user-run", OwnerUserId: "user-a", GoalId: "goal-a"}), "schedule", "waiting"},
		{"nonperiodic maintenance", auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), "manual", "waiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingJournalExecutor{}
			j := newWorkJournal(store, nil)
			run := failedRun("v1:work:run:maintenance-failure", "connection refused", 4, 3)
			run.AutomationName, run.TriggeredBy = "sweepWaitingWorkRuns", tc.trigger
			j.closeRun(tc.ctx, run, "")
			_, args := argsOf(t, lastCallNamed(t, store.calls, "updateWorkRun"))
			if args["status"] != tc.want {
				t.Fatalf("status=%v, want %s", args["status"], tc.want)
			}
			if tc.want == "failed" && anyCallNamed(store.calls, "createWorkApproval") {
				t.Fatal("periodic maintenance failure asked a nonexistent human owner")
			}
		})
	}
}

func TestJournalDB_FeedbackWaitHasPersistedApproval(t *testing.T) {
	engine := sharedJournalEngine(t)
	j := newWorkJournal(engine, nil)
	auto := &Automation{Name: "approvalPersistenceProbe"}
	run := NewExecution(auto.Name, "manual")
	run.ID = fmt.Sprintf("approvalpersistence%d", time.Now().UnixNano())
	j.openRun(context.Background(), auto, run, nil, events.Cause{})
	run.RecordFailedStep(&Step{ID: "run", Type: StepTypeFunction, RetryCount: 3}, 4)
	run.Fail(errors.New("connection refused"))
	j.closeRun(context.Background(), run, "")
	journal, err := LoadRunJournal(context.Background(), engine, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Status != "waiting" || journal.WaitingOn["kind"] != WaitKindApproval {
		t.Fatalf("run did not park on its saved approval: %+v", journal)
	}
	query, err := journalArgs("workApprovalById", map[string]any{"approvalId": journal.WaitingOn["subject"]})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Execute(journalContext(context.Background()), "query "+query)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(memql.MaterializeRows(result)) != 1 {
		t.Fatalf("waiting run names no persisted approval: %+v", result)
	}
}

func TestHumanQuestionIsNotClassifiedRepairedOrClosedAgain(t *testing.T) {
	store := &recordingJournalExecutor{}
	j := newWorkJournal(store, nil)
	run := failedRun("human-wait", "waiting for person", 4, 3)
	run.ErrorValue = fmt.Errorf("tool requestUserFeedback: %w", &work.HumanWait{ApprovalID: "q1"})
	j.closeRun(context.Background(), run, "")
	if len(store.calls) != 0 {
		t.Fatalf("late close overwrote a persisted or already answered question: %v", store.calls)
	}
}

func TestHumanQuestionSuspendsStepWithoutRetryContinueOrFailure(t *testing.T) {
	for _, missingResult := range []bool{false, true} {
		t.Run(fmt.Sprint(missingResult), func(t *testing.T) {
			store := &recordingJournalExecutor{}
			bus := events.NewBus()
			lifecycle := captureLifecycle(t, bus)
			calls := 0
			parkedAt := 0
			e := NewExecutor(ExecutorOptions{EventBus: bus, ChainTrackingEnabled: true, StepRegistry: modeRegistryFunc(func(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
				calls++
				parkedAt = len(store.calls)
				if calls != 1 || step.ID != "a" {
					t.Fatal("suspended work retried or executed a later effect")
				}
				var result *StepResult
				if !missingResult {
					result = &StepResult{StepId: step.ID, Status: "failed", Error: "waiting for answer", CompletedAt: time.Now()}
				}
				return result, fmt.Errorf("wrapped function: %w", &work.HumanWait{ApprovalID: "q1"})
			})})
			defer e.Close()
			e.journal = newWorkJournal(store, nil)
			auto := adoptProbeAutomation()
			auto.Steps[0].RetryCount, auto.Steps[0].OnError = 3, ErrorStrategyContinue
			auto.Steps = append(auto.Steps, &Step{ID: "effect", Type: StepTypeFunction})
			run, err := e.ExecuteAdopted(context.Background(), auto, RunAdoption{RunId: "human-wait"})
			if !isHumanWait(err) || calls != 1 || run.Status != "waiting" || run.FailedStepId != "" || !run.CompletedAt.IsZero() {
				t.Fatalf("wait became terminal: %+v %v, calls=%d", run, err, calls)
			}
			step := run.Steps["a"]
			if step.Status != "waiting" || step.Error != "" || !step.CompletedAt.IsZero() || step.ContentId != "" {
				t.Fatalf("wait produced a completed/failed receipt: %+v", step)
			}
			_, receipt := argsOf(t, lastCallNamed(t, store.calls, "updateWorkStep"))
			if receipt["status"] != "waiting" || receipt["finishedAt"] != nil || receipt["errorMessage"] != nil || receipt["result"] != nil {
				t.Fatalf("incorrect persisted step: %v", receipt)
			}
			for _, call := range store.calls[parkedAt:] {
				name, args := argsOf(t, call)
				if name == "updateWorkRun" && (args["status"] != nil || args["chainHead"] != nil) {
					t.Fatalf("late wait overwrote shared run: %s", call)
				}
			}
			// Event handlers run asynchronously. Let both the legitimate start
			// and any erroneous terminal publications reach the capture.
			time.Sleep(20 * time.Millisecond)
			for _, topic := range []string{events.TopicAutomationStepFailed, events.TopicAutomationStepCompleted, events.TopicAutomationFailed, events.TopicAutomationCompleted} {
				if got := lifecycle.topic(topic); len(got) != 0 {
					t.Fatalf("suspension published %s: %v", topic, got)
				}
			}
			if len(lifecycle.topic(events.TopicAutomationStepStarted)) != 1 {
				t.Fatal("step did not enter the event path")
			}
		})
	}
}

func TestJournalDB_HumanPauseResumesOnAnotherExecutorWithoutRepeatingEffects(t *testing.T) {
	engine := sharedJournalEngine(t)
	auto := &Automation{Name: fmt.Sprintf("humanPause%d", time.Now().UnixNano()), Steps: []*Step{
		{ID: "effect", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "function"}},
		{ID: "question", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "function"}, RetryCount: 3, OnError: ErrorStrategyContinue},
	}}
	first := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: modeRegistryFunc(func(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
		if step.ID == "question" {
			// The feedback capability commits this before returning HumanWait.
			newWorkJournal(engine, nil).call(ctx, "updateWorkRun", map[string]any{"runId": sc.Execution.ID, "status": "waiting"})
			return nil, &work.HumanWait{ApprovalID: "q"}
		}
		return &StepResult{StepId: step.ID, Status: "success", Result: "effect receipt", CompletedAt: time.Now()}, nil
	})})
	defer first.Close()
	run, err := first.Execute(context.Background(), auto, "test")
	if !isHumanWait(err) {
		t.Fatalf("expected pause: %v", err)
	}
	journal, err := LoadRunJournal(context.Background(), engine, run.ID)
	if err != nil || journal.Status != "waiting" || journal.StepStates["question"].Status != "waiting" || journal.StepStates["question"].Attempt != 1 {
		t.Fatalf("pause did not survive persistence: %+v %v", journal, err)
	}
	secondCalls := 0
	second := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: modeRegistryFunc(func(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
		secondCalls++
		if step.ID != "question" {
			t.Fatal("another executor repeated a completed effect")
		}
		return &StepResult{StepId: step.ID, Status: "success", Result: "recorded answer", CompletedAt: time.Now()}, nil
	})})
	defer second.Close()
	resumed, err := second.ResumeFrom(context.Background(), journal, auto, &ResumeOptions{})
	if err != nil || resumed.ID != run.ID || resumed.Status != "completed" || secondCalls != 1 {
		t.Fatalf("resume skipped the waiting step or made a new run: %+v %v", resumed, err)
	}
}
