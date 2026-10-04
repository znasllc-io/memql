package work

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// failure_remedy_writes_test.go -- the writes a failure-path remedy makes back
// through this package (memql#5664): each re-reads the run and moves it only
// while it is still parked on the remedy's wait.

// INSTALLING A RE-PLAN IS COMPILE'S WRITE, onto a run still waiting on it:
// the template fields the executing node loads a run's template from, and the
// reopen a re-run writes, so the run's `running` event dispatches it.
func TestInstallReplanRecordsTheTemplateAndReopensTheRun(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", remedyWaitRow(waitKindReplan))

	err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, ReplanTemplate{
		AutomationName: "summariseReplanned", TemplateConstructId: "v1:authoring:construct:c2",
		TemplateFingerprint: "fp-2", TemplateVersion: "v-2", Outcome: map[string]any{"replannedFrom": "draft"},
	})
	if err != nil {
		t.Fatalf("InstallReplan: %v", err)
	}
	call := eng.callTo(t, "updateWorkRun")
	args := call.Args(t)
	for k, want := range map[string]any{
		"status": runStatusRunning, "automationName": "summariseReplanned", "templateConstructId": "v1:authoring:construct:c2",
		"templateFingerprint": "fp-2", "templateVersion": "v-2", "errorMessage": "",
	} {
		if args[k] != want {
			t.Errorf("%s = %v, want %v", k, args[k], want)
		}
	}
	if w, _ := args["waitingOn"].(map[string]any); w == nil || len(w) != 0 {
		t.Errorf("waitingOn = %v, want cleared: a running run that still names a wait is one every sweep keeps serving", args["waitingOn"])
	}
	if args["heartbeatAt"] == nil || args["heartbeatAt"] == "" {
		t.Error("the reopened run carries no fresh heartbeat; the abandoned sweep would call it lost before an agent took it")
	}
	if call.Actor != "u-alice" || !call.Origin.IsInternal() {
		t.Errorf("written as %q with origin %v, want the owner's borrowed authority under internal origin", call.Actor, call.Origin)
	}
	assertEveryCallParses(t, eng)
}

// A REPAIR IS A RE-RUN OF THE FAILED STEP WITH THE VIOLATION AS GUIDANCE: the
// request rerunStep writes, so the agent serves it on that path -- the step
// runs as its next version, the prefix is served, and the violation reaches
// the step's model calls as what was wrong with the previous version.
func TestRequestRepairWritesAGuidedRerunOfTheFailedStep(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "fetch", 0, 1, "done", nil, nil)
	addVersion(store, actRunId, "draft", 1, 1, "failed", nil, nil)
	row := actRunRow(runStatusWaiting, "fetch", "draft")
	row["waitingOn"] = map[string]any{"kind": waitKindRepair, "subject": "draft", "since": "2026-09-05T11:59:00Z"}
	eng.reply("workRunForOwner", row)

	if err := i.RequestRepair(context.Background(), actOwner, actRunId, "draft", "the summary must list every ticket"); err != nil {
		t.Fatalf("RequestRepair: %v", err)
	}
	args := argsOf(t, eng, "updateWorkRun")
	if args["status"] != runStatusRunning {
		t.Fatalf("status = %v, want running: the request is served off the run's running event", args["status"])
	}
	rerun, _ := args["rerun"].(map[string]any)
	if rerun["reason"] != rerunReasonRerun || rerun["stepKey"] != "draft" {
		t.Fatalf("rerun = %v, want a re-run of the failed step", rerun)
	}
	override, _ := rerun["override"].(map[string]any)
	guidance, _ := override["guidance"].(map[string]any)
	if guidance["reason"] != "the summary must list every ticket" {
		t.Fatalf("override = %v, want the violation as its guidance", override)
	}
	if versions, _ := rerun["versions"].(map[string]any); versions["draft"] != float64(2) {
		t.Errorf("versions = %v, want the failed step's next version", rerun["versions"])
	}
	if stale, _ := args["staleSteps"].([]any); len(stale) != 1 || stale[0] != "draft" {
		t.Errorf("staleSteps = %v, want only the failed step: the prefix is served, never run again", args["staleSteps"])
	}
	if w, _ := args["waitingOn"].(map[string]any); w == nil || len(w) != 0 {
		t.Errorf("waitingOn = %v, want cleared", args["waitingOn"])
	}
	assertEveryCallParses(t, eng)
}

// A REMEDY THAT COULD NOT BE CARRIED OUT ASKS A PERSON, on a question that can
// be decided -- the failure path's own shape, its hash over its subject -- and
// the approval exists before the run waits on it.
func TestAskAboutFailedRemedyParksOnADecidableQuestion(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", remedyWaitRow(waitKindReplan))

	if err := i.AskAboutFailedRemedy(context.Background(), "u-alice", remedyRunId, "draft", RemedyReplan, "The re-planned draft does not compile."); err != nil {
		t.Fatalf("AskAboutFailedRemedy: %v", err)
	}
	approval := eng.callTo(t, "createWorkApproval").Args(t)
	if approval["kind"] != work.ApprovalKindFeedback || approval["question"] != "The re-planned draft does not compile." {
		t.Fatalf("approval = %v", approval)
	}
	subject, _ := approval["subject"].(map[string]any)
	if subject["symptom"] != string(work.SymptomPlan) || subject["stepKey"] != "draft" {
		t.Fatalf("subject = %v", subject)
	}
	if approval["artifactHash"] != work.ArtifactHash(subject) {
		t.Fatal("the question's hash is not its subject's: approve and answer would be refused as changed")
	}
	if !work.AnswerAbandons(approval["options"], map[string]any{"value": work.FailureAnswerAbandon}) {
		t.Errorf("options = %v, want Retry and Abandon", approval["options"])
	}
	update := eng.callTo(t, "updateWorkRun").Args(t)
	waiting, _ := update["waitingOn"].(map[string]any)
	if waiting["kind"] != "approval" || waiting["approvalKind"] != work.ApprovalKindFeedback || waiting["subject"] != approval["approvalId"] {
		t.Fatalf("waitingOn = %v, want the run parked on the approval just raised (%v)", waiting, approval["approvalId"])
	}
	order := eng.summary()
	if strings.Index(order, "createWorkApproval") > strings.Index(order, "updateWorkRun") {
		t.Fatalf("the run waited before its approval existed: %s", order)
	}
}

// EVERY REMEDY WRITE RE-READS THE RUN, and refuses one that moved on while
// the remedy worked: a re-plan that took a minute must not overwrite a person's
// cancel, or a decision made meanwhile.
func TestARemedyWriteRefusesARunThatMovedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"running", func(r map[string]any) { r["status"] = runStatusRunning }},
		{"cancelled", func(r map[string]any) { r["cancelRequested"] = true }},
		{"waiting on something else", func(r map[string]any) { r["waitingOn"] = map[string]any{"kind": waitKindRetry} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			row := remedyWaitRow(waitKindReplan)
			tc.edit(row)
			eng.reply("workRunForOwner", row)
			err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, ReplanTemplate{AutomationName: "x", TemplateConstructId: "v1:authoring:construct:c"})
			if !errors.Is(err, ErrRemedyNotWaiting) {
				t.Fatalf("InstallReplan err = %v, want ErrRemedyNotWaiting", err)
			}
			if err := i.AskAboutFailedRemedy(context.Background(), "u-alice", remedyRunId, "draft", RemedyReplan, "q"); !errors.Is(err, ErrRemedyNotWaiting) {
				t.Fatalf("AskAboutFailedRemedy err = %v, want ErrRemedyNotWaiting", err)
			}
			if writes := mutationsIn(eng); len(writes) != 0 {
				t.Fatalf("a refused remedy wrote %s", eng.summary())
			}
		})
	}
}
