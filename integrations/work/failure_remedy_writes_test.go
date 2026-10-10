package work

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// failure_remedy_writes_test.go -- the writes a failure-path remedy makes back
// through this package (memql#5664): each re-reads the run and moves it only
// while it is still parked on the remedy's wait.

// replannedTemplate is a re-plan that kept `gather`, dropped the failed
// `draft`, and starts the run again at `write`.
func replannedTemplate() ReplanTemplate {
	return ReplanTemplate{
		AutomationName: "summariseReplanned", TemplateConstructId: "v1:authoring:construct:c2",
		TemplateFingerprint: "fp-2", TemplateVersion: "v-2",
		StepKeys: []string{"gather", "write", "publish"}, ResumeAt: "write",
		Outcome: map[string]any{"replannedFrom": "draft"},
	}
}

// replanStepRows are the run's step rows as the install reads them: the
// completed `gather`, and the failed `draft` the new template replaced.
func replanStepRows() []map[string]any {
	return []map[string]any{
		{"id": "v1:work:step:rm1-gather", "runId": remedyRunId, "key": "gather", "status": "done", "attempt": 1, "version": 1},
		{"id": "v1:work:step:rm1-draft", "runId": remedyRunId, "key": "draft", "status": "failed", "attempt": 1, "version": 1, "errorMessage": "the summary format does not exist"},
	}
}

// INSTALLING A RE-PLAN IS COMPILE'S WRITE PLUS A REQUEST OF ITS OWN
// (memql#5664): the template fields the executing node loads a run's template
// from, and a `replan` request naming the first step the run never reached --
// so the agents claim the run under that request rather than under the bare
// run id, which the execution that failed still holds for its lease.
func TestInstallReplanRecordsTheTemplateAndARequestOfItsOwn(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", remedyWaitRow(waitKindReplan))
	eng.reply("workStepsForOwnerRun", replanStepRows()...)

	if err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, replannedTemplate()); err != nil {
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
	rerun, _ := args["rerun"].(map[string]any)
	if rerun["reason"] != rerunReasonReplan || rerun["stepKey"] != "write" || rerun["requestId"] == nil || rerun["requestId"] == "" {
		t.Fatalf("rerun = %v, want a replan request naming the first new step", rerun)
	}
	if stale, _ := args["staleSteps"].([]any); len(stale) != 2 || stale[0] != "write" || stale[1] != "publish" {
		t.Errorf("staleSteps = %v, want the new steps from the first one on: an interrupted execution resumes where the request stands", args["staleSteps"])
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

// THE FAILED STEP A RE-PLAN DROPPED IS SUPERSEDED, BEFORE THE RUN MOVES. A
// failed row for a step the template no longer has is a resume point no
// template can serve; marked skipped -- and written before the run's own
// write, which is what starts the run -- the journal says plainly the step is
// behind the run.
func TestInstallReplanSupersedesTheFailedStepItDropped(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", remedyWaitRow(waitKindReplan))
	eng.reply("workStepsForOwnerRun", replanStepRows()...)

	if err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, replannedTemplate()); err != nil {
		t.Fatalf("InstallReplan: %v", err)
	}
	step := eng.callTo(t, "updateWorkStep").Args(t)
	if step["stepId"] != "v1:work:step:rm1-draft" || step["status"] != "skipped" {
		t.Fatalf("updateWorkStep = %v, want the dropped `draft` superseded as skipped", step)
	}
	order := eng.summary()
	if strings.Index(order, "updateWorkStep") > strings.Index(order, "updateWorkRun") {
		t.Fatalf("the run moved before its dropped step was superseded: %s", order)
	}
}

// AN INSTALLED RE-PLAN IS DISPATCHED THOUGH THE FAILING EXECUTION STILL HOLDS
// THE RUN'S CLAIM (memql#5664). Installed with no request of its own, the
// run's `running` event was claimed under the bare run id, which the execution
// that failed held for four minutes from its own dispatch: every agent lost
// that claim, no second event came, and the sweep closed the run as abandoned.
func TestAnInstalledReplanIsDispatchedDespiteTheFailingExecutionsClaim(t *testing.T) {
	i, eng := newTestIntegration(t)
	row := remedyWaitRow(waitKindReplan)
	eng.reply("workRunForOwner", row)
	eng.reply("workStepsForOwnerRun", replanStepRows()...)
	if err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, replannedTemplate()); err != nil {
		t.Fatalf("InstallReplan: %v", err)
	}
	installed := map[string]any{}
	for k, v := range row {
		installed[k] = v
	}
	for k, v := range eng.callTo(t, "updateWorkRun").Args(t) {
		if k != "runId" {
			installed[k] = v
		}
	}

	claims := &pkClaims{}
	if !claims.ClaimWithTTL(context.Background(), runClaimName, remedyRunId, runClaimTTL) {
		t.Fatal("could not stand in for the failing execution's claim")
	}
	agent, _ := newTestIntegration(t)
	d := &signallingDispatcher{}
	agent.SetDispatcher(d)
	agent.SetRunClaimer(claims)
	agent.HandleRunEvent(remedyEvent(installed))

	got := d.settled(t, 1)
	if len(got) != 1 {
		t.Fatalf("the re-planned run was dispatched %d times, want once: claimed under the bare run id, it loses to the execution that failed", len(got))
	}
	if got[0].RerunRequestId == "" {
		t.Fatalf("dispatched %+v without its request", got[0])
	}
}

// A REMEDY NEVER CLEARS A CANCEL IT DID NOT OBSERVE. Each remedy write re-reads
// the run and refuses one already asked to stop; a cancel that lands between
// that read and the write must survive the write, so neither writes the flag.
func TestARemedyWriteNeverClearsACancel(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", remedyWaitRow(waitKindReplan))
	eng.reply("workStepsForOwnerRun", replanStepRows()...)
	if err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, replannedTemplate()); err != nil {
		t.Fatalf("InstallReplan: %v", err)
	}
	args := eng.callTo(t, "updateWorkRun").Args(t)
	for _, k := range []string{"cancelRequested", "cancelledBy"} {
		if _, written := args[k]; written {
			t.Errorf("the install wrote %s = %v: a cancel landing after its read would be erased", k, args[k])
		}
	}

	r, reng, store := newActsIntegration(t)
	addVersion(store, actRunId, "fetch", 0, 1, "done", nil, nil)
	addVersion(store, actRunId, "draft", 1, 1, "failed", nil, nil)
	waiting := actRunRow(runStatusWaiting, "fetch", "draft")
	waiting["waitingOn"] = map[string]any{"kind": waitKindRepair, "subject": "draft", "since": "2026-09-05T11:59:00Z"}
	reng.reply("workRunForOwner", waiting)
	if err := r.RequestRepair(context.Background(), actOwner, actRunId, "draft", "v"); err != nil {
		t.Fatalf("RequestRepair: %v", err)
	}
	repair := argsOf(t, reng, "updateWorkRun")
	for _, k := range []string{"cancelRequested", "cancelledBy"} {
		if _, written := repair[k]; written {
			t.Errorf("the repair wrote %s = %v", k, repair[k])
		}
	}
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
			err := i.InstallReplan(context.Background(), "u-alice", remedyRunId, replannedTemplate())
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

// A re-plan's context carries what the run's resume will bind into the new
// template's args (memql#5664): the run's stored variables, or, for a run with
// none, its triggering event's payload -- ResumeFrom's rule, so the draft is
// held to exactly what its resume will hold it to.
func TestLoadReplanContextCarriesWhatResumeBinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"the run's variables", func(r map[string]any) {}, "region=emea,week=2026-39"},
		{"the trigger's payload, for a run with no variables", func(r map[string]any) {
			delete(r, "variables")
			r["triggerEvent"] = map[string]any{"topic": "report.requested", "payload": map[string]any{"week": "2026-40"}}
		}, "week=2026-40"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			run := installedReplanRun(t)
			run["variables"] = map[string]any{"week": "2026-39", "region": "emea"}
			tc.edit(run)
			eng.reply("workRunForOwner", run)
			rc, err := i.LoadReplanContext(context.Background(), "u-alice", remedyRunId, "draft")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for k, v := range rc.Variables {
				got = append(got, k+"="+v.(string))
			}
			sort.Strings(got)
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("variables = %v, want %s", rc.Variables, tc.want)
			}
		})
	}
}
