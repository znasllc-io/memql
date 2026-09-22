package automations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// run_ceiling_park_test.go -- what a run does when it reaches its OWN ceiling
// (memql#5580).
//
// The sibling of journal_test.go's inference-park cases, and the differences
// between the two are the point: this one waits on a `budget` approval rather
// than an `inferenceUnavailable` one, it names WHICH ceiling, and it puts the
// run's spend on the row.

func aCeilingRefusal() *memql.RunCeilingError {
	return &memql.RunCeilingError{
		RunId:   "v1:work:run:r9",
		StepKey: "step-b",
		Breach: memql.RunCeilingBreach{
			Ceiling: work.CeilingModelCalls,
			Limit:   "3 calls",
			Actual:  "3 made",
			Reason:  "the run reached its model-call cap",
		},
		Spent: memql.RunSpend{Tokens: 1200, TokensLocal: 50, Cost: 0.34, ModelCalls: 3, WallClockMs: 4200},
	}
}

// A run refused by one of its own ceilings PARKS: an approval a person can
// decide, and a run they can resume. Failing it would throw away a compiled
// template and a journal because of a number somebody set.
func TestARunThatReachedItsCeilingParksOnABudgetApproval(t *testing.T) {
	exec := &recordingJournalExecutor{}
	j := newWorkJournal(exec, nil)
	run := &AutomationExecution{ID: "v1:work:run:r9", StepOrder: []string{"step-a", "step-b"}}
	run.Fail(aCeilingRefusal())
	j.closeRun(context.Background(), run, "head")

	if len(exec.calls) != 2 {
		t.Fatalf("expected the approval then the wait; got %d calls: %v", len(exec.calls), exec.calls)
	}

	name, approval := argsOf(t, exec.calls[0])
	if name != "createWorkApproval" {
		t.Fatalf("the approval must be written FIRST -- a run parked on an approval id that does not exist waits on nothing; got %s", name)
	}
	if approval["kind"] != work.ApprovalKindBudget {
		t.Errorf("kind = %v, want %q: an exhausted ceiling and a shut door are different questions", approval["kind"], work.ApprovalKindBudget)
	}
	subject, _ := approval["subject"].(map[string]any)
	if subject["ceiling"] != work.CeilingModelCalls {
		t.Errorf("the subject must name WHICH ceiling: %v", subject)
	}
	for _, key := range []string{"limit", "actual", "reason"} {
		if s, _ := subject[key].(string); strings.TrimSpace(s) == "" {
			t.Errorf("the subject is missing %q; \"over budget\" with no numbers is not a decision anyone can make: %v", key, subject)
		}
	}
	if s, _ := approval["artifactHash"].(string); s == "" {
		t.Error("an approval is a decision about one specific breach, and the hash is what stops it carrying to another")
	}
	if approval["stepKey"] != "step-b" {
		t.Errorf("stepKey = %v; the refusal names the step it stopped at", approval["stepKey"])
	}
}

// THE RUN ROW CARRIES THE BREACH AND THE SPEND. A reader scanning runs sees
// which limit stopped this one and what it had spent, without opening the
// approval -- and `spent` stops being a field only wallClockMs ever reached.
func TestTheParkedRunRecordsTheCeilingAndTheSpend(t *testing.T) {
	exec := &recordingJournalExecutor{}
	j := newWorkJournal(exec, nil)
	run := &AutomationExecution{ID: "v1:work:run:r9", StepOrder: []string{"step-b"}}
	run.Fail(aCeilingRefusal())
	j.closeRun(context.Background(), run, "head")

	name, args := argsOf(t, exec.calls[1])
	if name != "updateWorkRun" {
		t.Fatalf("expected the run update; got %s", name)
	}
	if args["status"] != "waiting" {
		t.Fatalf("status = %v, want waiting: a ceiling is not a failure of the work", args["status"])
	}
	if _, present := args["finishedAt"]; present {
		t.Error("a parked run has not finished; writing finishedAt makes every terminal-run reader treat it as done")
	}
	waiting, _ := args["waitingOn"].(map[string]any)
	if waiting["approvalKind"] != work.ApprovalKindBudget {
		t.Errorf("the wait must carry the kind: %v", waiting)
	}
	if waiting["ceiling"] != work.CeilingModelCalls {
		t.Errorf("the wait must name the ceiling so a run list is readable without opening the approval: %v", waiting)
	}
	spent, _ := args["spent"].(map[string]any)
	if spent == nil {
		t.Fatal("the run must record what it spent; a breach with no spend on the row is not legible afterwards")
	}
	if fmt.Sprint(spent["modelCalls"]) != "3" || fmt.Sprint(spent["tokens"]) != "1200" {
		t.Errorf("spent = %v; it must be the figures the refusal carried", spent)
	}
}

// A CEILING PARK IS NEVER RE-CHECKED ON A TIMER. Only a person changes a
// ceiling -- and unlike a shut door, no laptop opening will change this
// answer -- so a resumeAt here would burn a dispatch every five minutes to
// rediscover a number nobody touched.
func TestACeilingParkCarriesNoResumeAt(t *testing.T) {
	exec := &recordingJournalExecutor{}
	j := newWorkJournal(exec, nil)
	run := &AutomationExecution{ID: "v1:work:run:r9"}
	run.Fail(aCeilingRefusal())
	j.closeRun(context.Background(), run, "")

	_, args := argsOf(t, exec.calls[1])
	waiting, _ := args["waitingOn"].(map[string]any)
	if _, present := waiting["resumeAt"]; present {
		t.Errorf("a ceiling park must not carry resumeAt: %v", waiting)
	}
}

// THE RUN CEILING IS NOT THE PROCESS COST CEILING. The router refuses a
// federation hop with `ceiling_reached` and that parks on an
// inferenceUnavailable approval whose question is about a paid provider. A run
// ceiling that landed there would send a person to raise the wrong limit.
func TestARunCeilingDoesNotParkAsAShutDoor(t *testing.T) {
	exec := &recordingJournalExecutor{}
	j := newWorkJournal(exec, nil)
	run := &AutomationExecution{ID: "v1:work:run:r9"}
	run.Fail(aCeilingRefusal())
	j.closeRun(context.Background(), run, "")

	_, approval := argsOf(t, exec.calls[0])
	if approval["kind"] == work.ApprovalKindInferenceUnavailable {
		t.Fatal("a run ceiling parked as a shut inference door; the two ceilings must stay tellable apart")
	}
	// And the refusal's own words must not be matchable as one either --
	// work.DoorsFrom falls back to a substring match over the refusal codes.
	code, _, ok := work.DoorsFrom(aCeilingRefusal())
	if ok {
		t.Fatalf("DoorsFrom recognised a run-ceiling refusal as the door refusal %q", code)
	}
}

// A RUN THAT WAS NOWHERE NEAR A CEILING STILL FAILS. Without this the park
// could satisfy its own tests by parking everything, and a run that genuinely
// broke would wait forever on a budget nobody exceeded.
func TestAnOrdinaryFailureIsNotACeilingPark(t *testing.T) {
	exec := &recordingJournalExecutor{}
	j := newWorkJournal(exec, nil)
	run := &AutomationExecution{ID: "v1:work:run:r9"}
	run.Fail(fmt.Errorf("the step blew up"))
	j.closeRun(context.Background(), run, "")

	for _, call := range exec.calls {
		if strings.HasPrefix(call, "createWorkApproval") {
			_, approval := argsOf(t, call)
			if approval["kind"] == work.ApprovalKindBudget {
				t.Fatal("an ordinary failure parked on a budget approval")
			}
		}
	}
}
