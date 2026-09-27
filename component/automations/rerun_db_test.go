package automations

// rerun_db_test.go -- a step of a finished goal run run again, against a real
// Postgres (epic memql#5414, task memql#5415).
//
// rerun_test.go asserts the CALLS the journal renders; this asserts what the
// engine does with them. The new arguments -- version, basis, override,
// authoredBy, the reset fields, the whole head, staleSteps, the cleared
// request -- are refused by a schema the recorder never sees, and a refused
// journal write is a Warn with the run carrying on: a re-run whose version 2
// intents were all refused would pass every DB-free test and leave the run
// showing version 1 forever.
//
// Postgres-gated: skips cleanly when no DB is reachable, FAILS under
// MEMQL_REQUIRE_DB=1.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestRerun_DB_AStepRunAgainLandsAsNewVersionsAndTheHeadRoundTrips(t *testing.T) {
	engine := openTestEngine(t)
	a := statementAutomation(t, rerunSource)
	owner, goalId, runId := id.NewShortId(), "v1:work:goal:"+id.NewShortId(), id.NewShortId()
	ownerCtx := auth.ContextWithUserActor(context.Background(), owner)
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := journalArgs(name, args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Execute(auth.ContextWithInternalOrigin(ownerCtx), call); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The goal and its run, as the planner opens them before an agent adopts
	// the run.
	write("createWorkGoal", map[string]any{"goalId": goalId, "statement": "Draft the weekly report", "origin": "user"})
	write("createWorkRun", map[string]any{
		"runId": runId, "goalId": goalId, "automationName": a.Name, "templateFingerprint": a.DefinitionFingerprint(fingerprintEngine),
		"status": "running", "mode": "live", "triggeredBy": "compiled", "startedAt": rfc3339(time.Now()),
	})

	probe := newStmtProbe()
	probe.answers["fetch"] = memql.NewResultWithOutput("A1")
	probe.answers["draft"] = memql.NewResultWithOutput("B1")
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: probe})
	defer e.Close()
	runCtx := common.ContextWithRun(ownerCtx, common.RunContext{RunId: runId, GoalId: goalId, OwnerUserId: owner, Mode: common.RunModeLive})
	if first, err := e.ExecuteAdopted(runCtx, a, RunAdoption{RunId: runId, TriggeredBy: "compiled"}); err != nil || first.Status != "completed" {
		t.Fatalf("first execution: %v %v", first, err)
	}

	before, err := LoadRunJournal(context.Background(), engine, runId)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	pristine := map[string]int{"a": 1, "b": 1, "publish": 1}
	for key, v := range pristine {
		if before.Head[key].Version != v || before.StepStates[key].Version != v {
			t.Fatalf("after the first execution %s: head %+v, row %+v -- want version %d on both", key, before.Head[key], before.StepStates[key], v)
		}
	}

	// The person's act, as integrations/work writes it on whichever node served
	// it: the request rides the run row, beside status running.
	requestId := "req-" + id.NewShortId()
	write("updateWorkRun", map[string]any{
		"runId": runId, "status": "running", "staleSteps": []string{"b", "publish"},
		"rerun": map[string]any{"requestId": requestId, "reason": RerunReasonRerun, "stepKey": "b",
			"override": map[string]any{"level": "reasoning", "prompt": "Shorter.", "requestedBy": owner}, "requestedBy": owner, "requestedAt": rfc3339(time.Now())},
	})

	journal, err := LoadRunJournal(context.Background(), engine, runId)
	if err != nil {
		t.Fatalf("LoadRunJournal with the request: %v", err)
	}
	if journal.Rerun == nil || journal.Rerun.RequestId != requestId || journal.Rerun.StepKey != "b" || journal.Rerun.Override == nil || journal.Rerun.Override.Level != "reasoning" {
		t.Fatalf("the request did not round-trip off the row: %+v", journal.Rerun)
	}
	resume, opts, err := PrepareRerun(journal, nil, a)
	if err != nil {
		t.Fatalf("PrepareRerun: %v", err)
	}
	probe.answers["draft"] = memql.NewResultWithOutput("B2")
	if again, err := e.ResumeFrom(runCtx, resume, a, opts); err != nil || again.Status != "completed" {
		t.Fatalf("re-run: %v %v", again, err)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"fetch", "draft", "publish", "draft", "publish"}) {
		t.Fatalf("calls %v, want the first execution and then draft and publish only", got)
	}

	after, err := LoadRunJournal(context.Background(), engine, runId)
	if err != nil {
		t.Fatalf("LoadRunJournal after the re-run: %v", err)
	}
	if after.Status != "succeeded" || after.Rerun != nil || len(after.StaleSteps) != 0 {
		t.Fatalf("the re-run did not close cleanly: status %s, request %+v, stale %v -- a closed run carries rerun {} and staleSteps []", after.Status, after.Rerun, after.StaleSteps)
	}
	for key, v := range map[string]int{"a": 1, "b": 2, "publish": 2} {
		if after.Head[key].Version != v || after.StepStates[key].Version != v || after.StepStates[key].Status != "done" {
			t.Errorf("%s after the re-run: head %+v, row %+v -- want version %d, done", key, after.Head[key], after.StepStates[key], v)
		}
	}

	// What the targeted version records, off its own row.
	call, err := journalArgs("workStepsForRun", map[string]any{"runId": runId})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(journalContext(context.Background()), "query "+call)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range memql.MaterializeRows(res) {
		switch row["key"] {
		case "b":
			if o, _ := row["override"].(map[string]any); o["prompt"] != "Shorter." || o["level"] != "reasoning" || row["authoredBy"] != owner {
				t.Errorf("version 2 of b records override %v and author %v, want the request's and the owner", row["override"], row["authoredBy"])
			}
		case "publish":
			if o, _ := row["override"].(map[string]any); len(o) != 0 || row["authoredBy"] == owner {
				t.Errorf("version 2 of publish records override %v and author %v; nobody overrode it", row["override"], row["authoredBy"])
			}
			if row["basis"] == nil {
				t.Error("version 2 of publish stored no basis; it ran against version 2 of b, which is not pristine")
			}
		}
	}
}
