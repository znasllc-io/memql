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
	engine := openTestEngine(t)
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
