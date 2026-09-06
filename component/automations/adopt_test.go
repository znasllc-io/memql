package automations

import (
	"context"
	"strings"
	"testing"
)

// adoptProbeAutomation is the smallest runnable template: one step that
// succeeds. The tests here are about WHICH RUN ROW the execution lands on,
// not about what a step does.
func adoptProbeAutomation() *Automation {
	return &Automation{Name: "demo", Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}},
	}}
}

// TestExecuteAdoptedWritesNoSecondRunRow is the regression test for the whole
// of memql#5054's mechanism.
//
// The bug it pins is not "the dispatcher was missing" -- that is the issue's
// finding -- but the trap waiting for whoever fixed it: reaching for
// Execute() to run a compiled run mints a SECOND run row, because
// NewExecution generates a fresh id and journal.openRun INSERTS under it. The
// goal's own run would then sit at `running` with no heartbeat until the
// abandoned sweep closed it, while a second row nobody is watching carried
// the work.
//
// So this asserts two things a passing dispatch must have: exactly one
// createWorkRun anywhere (and it is NOT ours), and every write naming the run
// id we were told to adopt.
func TestExecuteAdoptedWritesNoSecondRunRow(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(rec, nil)

	const runId = "existing-run-1"
	exec, err := e.ExecuteAdopted(context.Background(), adoptProbeAutomation(), RunAdoption{
		RunId:       runId,
		TriggeredBy: "compiled",
	})
	if err != nil {
		t.Fatalf("ExecuteAdopted: %v", err)
	}
	if exec.Status != "completed" {
		t.Fatalf("status = %q, want completed", exec.Status)
	}

	// The execution IS the run.
	if exec.ID != runId {
		t.Errorf("execution id = %q, want the adopted run id %q -- the journal keys every write off exec.ID, so a fresh id here is the second-row bug", exec.ID, runId)
	}

	var names []string
	for _, c := range rec.calls {
		n, args := argsOf(t, c)
		names = append(names, n)
		if n == "createWorkRun" {
			t.Errorf("createWorkRun was called for an adopted run; that INSERTS a second run row and leaves the goal's own run to be closed as abandoned. Call: %s", c)
		}
		// Every write must name the run we adopted.
		if got, ok := args["runId"].(string); ok && got != runId {
			t.Errorf("%s wrote runId %q, want %q", n, got, runId)
		}
	}
	if len(names) == 0 {
		t.Fatal("the journal recorded nothing")
	}
	// The row is ADVANCED, not inserted.
	if !containsName(names, "updateWorkRun") {
		t.Errorf("no updateWorkRun in %v -- an adopted run has to advance the existing row", names)
	}
}

// TestExecuteAdoptedRefusesAnEmptyRunId pins the refusal rather than a
// default. Defaulting to a fresh id would silently produce exactly the
// second-row bug above, on the one call path that got the argument wrong.
func TestExecuteAdoptedRefusesAnEmptyRunId(t *testing.T) {
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(&recordingJournalExecutor{}, nil)

	for _, id := range []string{"", "   "} {
		_, err := e.ExecuteAdopted(context.Background(), adoptProbeAutomation(), RunAdoption{RunId: id})
		if err == nil {
			t.Fatalf("ExecuteAdopted with run id %q returned no error; it must refuse rather than mint one", id)
		}
		if !strings.Contains(err.Error(), "run id") {
			t.Errorf("error %q does not name the missing run id", err)
		}
	}
}

// TestAdoptedRunsAreNotDeduplicatedAgainstEachOther is the property that made
// the dedup exemption necessary, stated as a test.
//
// Both dedup gates key on the INITIAL CHAIN HEAD, a hash of (automation,
// triggeredBy, event, input). Two different goals that compile to the same
// template with the same input therefore produce the SAME key -- so with the
// executor's dedup applied, the second goal's run is skipped as a duplicate
// and never executes, while its row stays at `running` until the abandoned
// sweep closes it. Two people asking for the same thing is not a double-fire.
func TestAdoptedRunsAreNotDeduplicatedAgainstEachOther(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{
		StepRegistry:         journalProbeRegistry{},
		ChainTrackingEnabled: true,
		DedupEnabled:         true,
	})
	e.journal = newWorkJournal(rec, nil)

	// Identical automation, identical (absent) variables: same chain head.
	first, err := e.ExecuteAdopted(context.Background(), adoptProbeAutomation(), RunAdoption{RunId: "run-A", TriggeredBy: "compiled"})
	if err != nil {
		t.Fatalf("first ExecuteAdopted: %v", err)
	}
	second, err := e.ExecuteAdopted(context.Background(), adoptProbeAutomation(), RunAdoption{RunId: "run-B", TriggeredBy: "compiled"})
	if err != nil {
		t.Fatalf("second ExecuteAdopted: %v", err)
	}

	if first.Status == "skipped" {
		t.Fatalf("the FIRST adopted run was skipped (%q) -- the test cannot say anything about the second", first.Error)
	}
	if second.Status == "skipped" {
		t.Fatalf("the second adopted run was skipped as a duplicate (%q).\n\n"+
			"Two goals compiling to one template with one input share an initial chain head, so "+
			"applying the executor's dedup to adopted runs skips the second goal's run entirely and "+
			"leaves its row at `running` for the abandoned sweep. An adopted run's identity is its "+
			"RUN ID, and its claim is held by the caller.", second.Error)
	}
	if first.ID == second.ID {
		t.Errorf("both executions took id %q; each must be its own run", first.ID)
	}
}

// TestNonAdoptedRunsStillOpenTheirOwnRow is the negative control for the
// first test: it proves createWorkRun is reachable at all on this fake, so
// "no createWorkRun" above is a real observation rather than a journal that
// was never wired.
func TestNonAdoptedRunsStillOpenTheirOwnRow(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(rec, nil)

	if _, err := e.Execute(context.Background(), adoptProbeAutomation(), "test"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var names []string
	for _, c := range rec.calls {
		n, _ := argsOf(t, c)
		names = append(names, n)
	}
	if !containsName(names, "createWorkRun") {
		t.Fatalf("a NON-adopted run recorded %v with no createWorkRun; the control is broken, so the adopted-run assertion proves nothing", names)
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
