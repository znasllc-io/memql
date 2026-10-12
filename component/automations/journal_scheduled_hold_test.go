package automations

// journal_scheduled_hold_test.go -- a scheduled firing's journal is held until
// the run writes (the journal.go header; storage growth stability release,
// 2026-10-11).
//
// An idle production instance held 226,713 work runs, 93% of them maintenance
// ticks that changed nothing, each a durable v1:work:run of three or more
// versions plus its v1:work:step rows. These tests drive the REAL executor
// through the recording fake the other journal tests use, so what they pin is
// the calls the journal renders and WHEN it renders them: a step "writes" by
// reporting to the context's write observer, which is the report the engine
// makes from its single write path (common.NotifyWrite). The engine-side half
// -- a real write reaching that observer, and the held rows being accepted and
// read back -- is journal_scheduled_hold_db_test.go.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

// scheduledProbe is the step registry of these tests. A step does what the test
// configured -- reports a graph write to the context's observer (writeAt), fails
// (failAt), or neither -- and the probe records what the journal had written at
// two moments: as the step began, and as its own write returned. The first is
// what tells a held journal from one that opened its run up front, because a
// run that is held has written nothing yet; the second is what pins the run
// opening AT the write, not at the end of the firing.
type scheduledProbe struct {
	rec     *recordingJournalExecutor
	writeAt string
	failAt  string

	atStart    map[string][]string
	afterWrite map[string][]string
}

func (p *scheduledProbe) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	if p.atStart == nil {
		p.atStart, p.afterWrite = map[string][]string{}, map[string][]string{}
	}
	p.atStart[step.ID] = journalWriteNames(p.rec)
	if step.ID == p.writeAt {
		common.NotifyWrite(ctx, "v1:probe:thing", "thing-1")
		p.afterWrite[step.ID] = journalWriteNames(p.rec)
	}
	now := time.Now()
	if step.ID == p.failAt {
		return &StepResult{StepId: step.ID, Status: "failed", Error: errBoom.Error(), StartedAt: now, CompletedAt: now}, errBoom
	}
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

// journalWriteNames lists the journal's writes in the order they reached the
// engine. Its reads are left out: the cancel read (`query workRunById`) is asked
// at the first step boundary whether or not the run's row exists yet.
func journalWriteNames(rec *recordingJournalExecutor) []string {
	var names []string
	for _, call := range rec.calls {
		if strings.HasPrefix(call, "query ") {
			continue
		}
		names = append(names, call[:strings.Index(call, "(")])
	}
	return names
}

// journalWrites is journalWriteNames with each write's arguments read back.
func journalWrites(t *testing.T, rec *recordingJournalExecutor) (names []string, args []map[string]any) {
	t.Helper()
	for _, call := range rec.calls {
		if strings.HasPrefix(call, "query ") {
			continue
		}
		name, a := argsOf(t, call)
		names, args = append(names, name), append(args, a)
	}
	return names, args
}

func scheduledSteps(ids ...string) []*Step {
	steps := make([]*Step, len(ids))
	for i, id := range ids {
		steps[i] = &Step{ID: id, Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}}
	}
	return steps
}

func scheduledHoldExecutor(rec *recordingJournalExecutor, probe *scheduledProbe) *Executor {
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	e.journal = newWorkJournal(rec, nil)
	return e
}

// A cron tick that changes nothing leaves nothing: no run row, no step row, no
// heartbeat, no close. This is the 93%.
func TestScheduledRunWithoutWritesLeavesNoJournal(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "idleTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a")}

	exec, err := e.Execute(context.Background(), auto, "schedule")
	if err != nil || exec.Status != "completed" {
		t.Fatalf("the firing itself must be unchanged: status %q, err %v", exec.Status, err)
	}
	if got := journalWriteNames(rec); len(got) != 0 {
		t.Fatalf("a scheduled firing that wrote nothing journaled %v; it must leave no run, step or heartbeat row", got)
	}
}

// The run opens at the step's first write -- the run row, then the step intent
// that was waiting behind it -- and everything after goes straight through. It
// is the order a run has always had, reached late.
func TestScheduledRunThatWritesIsJournaledInOrder(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec, writeAt: "a"}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "writingTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a")}

	exec, err := e.Execute(context.Background(), auto, "schedule")
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %q, err %v", exec.Status, err)
	}

	if got := probe.atStart["a"]; len(got) != 0 {
		t.Errorf("the journal had written %v before the step wrote anything; a held run writes nothing until it does", got)
	}
	if got, want := probe.afterWrite["a"], []string{"createWorkRun", "createWorkStep"}; !slices.Equal(got, want) {
		t.Errorf("as the step's write returned the journal had written %v, want %v: the run opens AT its first write, "+
			"with the intent that was waiting behind it", got, want)
	}

	names, args := journalWrites(t, rec)
	want := []string{"createWorkRun", "createWorkStep", "updateWorkStep", "updateWorkRun", "updateWorkRun"}
	if !slices.Equal(names, want) {
		t.Fatalf("journal writes = %v, want %v (run; intent; receipt; heartbeat; close)", names, want)
	}
	if args[0]["runId"] != exec.ID || args[0]["status"] != "running" || args[0]["triggeredBy"] != "schedule" || args[0]["automationName"] != "writingTick" {
		t.Errorf("run row: %v", args[0])
	}
	// The deferred open must not date the run from the write: it started when
	// the firing did, and the abandoned sweep and Nexus both read this field.
	if args[0]["startedAt"] != rfc3339(exec.StartedAt) {
		t.Errorf("run startedAt = %v, want the firing's own %s", args[0]["startedAt"], rfc3339(exec.StartedAt))
	}
	if args[1]["status"] != "running" || args[1]["key"] != "a" {
		t.Errorf("step intent: %v", args[1])
	}
	if args[2]["status"] != "done" {
		t.Errorf("step receipt: %v", args[2])
	}
	if args[4]["status"] != "succeeded" || args[4]["runId"] != exec.ID {
		t.Errorf("close: %v", args[4])
	}
}

// A step that ran before the first write is part of the run that write opens:
// its intent, receipt and heartbeat were held, not dropped, and they land
// ahead of anything the writing step records.
func TestScheduledRunKeepsTheStepsBeforeItsFirstWrite(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec, writeAt: "b"}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "readThenWrite", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a", "b")}

	exec, err := e.Execute(context.Background(), auto, "schedule")
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %q, err %v", exec.Status, err)
	}
	if got := probe.atStart["b"]; len(got) != 0 {
		t.Errorf("step a only read, yet the journal had written %v by the time b began", got)
	}
	names, args := journalWrites(t, rec)
	want := []string{
		"createWorkRun",
		"createWorkStep", "updateWorkStep", "updateWorkRun", // a, held then flushed
		"createWorkStep", "updateWorkStep", "updateWorkRun", // b
		"updateWorkRun", // close
	}
	if !slices.Equal(names, want) {
		t.Fatalf("journal writes = %v, want %v", names, want)
	}
	if args[1]["key"] != "a" || args[4]["key"] != "b" {
		t.Errorf("step intents out of order: %v then %v", args[1], args[4])
	}
}

// A failed firing is journaled whether or not it wrote. This is the case a
// hold that forgot to open on failure would lose, and the one the incident
// record needs: a retention sweep fails every night on a statement timeout, and
// its builtin writes through raw SQL, which reports nothing to the observer --
// so the failure is the only thing such a run can leave.
func TestScheduledRunFailureIsJournaled(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec, failAt: "a"}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "failingTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a")}

	exec, _ := e.Execute(context.Background(), auto, "schedule")
	if exec.Status != "failed" {
		t.Fatalf("status = %q, want failed", exec.Status)
	}
	// Without this the test cannot tell a held journal that opens on failure from
	// one that was never held.
	if got := probe.atStart["a"]; len(got) != 0 {
		t.Fatalf("the journal had written %v before the failing step ran; the run should still be held", got)
	}

	names, args := journalWrites(t, rec)
	want := []string{"createWorkRun", "createWorkStep", "updateWorkStep", "updateWorkRun", "updateWorkRun"}
	if !slices.Equal(names, want) {
		t.Fatalf("journal writes = %v, want %v (run; the failed step's intent and receipt; heartbeat; close)", names, want)
	}
	if args[0]["runId"] != exec.ID || args[0]["status"] != "running" {
		t.Errorf("run row: %v", args[0])
	}
	if args[2]["status"] != "failed" || args[2]["errorMessage"] != "boom" {
		t.Errorf("failed step receipt: %v", args[2])
	}
	last := args[len(args)-1]
	if last["status"] != "failed" || last["errorMessage"] != "boom" || last["runId"] != exec.ID || last["finishedAt"] == nil {
		t.Errorf("close: %v, want the run closed failed with its error", last)
	}
}

// @journalRequired promises confirmed boundaries, and a boundary that waits for
// the first write is not one: the run row is written at open, as before.
func TestScheduledJournalRequiredIsNotHeld(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "guardedTick", Schedule: "0 */10 * * * *", JournalRequired: true, Steps: scheduledSteps("a")}

	exec, err := e.Execute(context.Background(), auto, "schedule")
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %q, err %v", exec.Status, err)
	}
	if got := probe.atStart["a"]; len(got) < 2 || got[0] != "createWorkRun" || got[1] != "createWorkStep" {
		t.Errorf("by the time the step began the journal had written %v, want the run and the step intent already on file", got)
	}
	if names := journalWriteNames(rec); len(names) == 0 || names[0] != "createWorkRun" {
		t.Fatalf("journal writes = %v, want createWorkRun first", names)
	}
}

// Only a cron tick is held: an event-triggered firing opens its run before its
// first step, exactly as it always has.
func TestEventRunJournalUnchanged(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "onThing", Trigger: &TriggerConfig{Event: "graph.node.created.*.v1:probe:thing"}, Steps: scheduledSteps("a")}

	ev := events.NewEvent("graph.node.created.default.v1:probe:thing", events.KindMessage, map[string]any{"id": "thing-1"})
	exec, err := e.ExecuteWithEvent(context.Background(), auto, "event:"+ev.Topic, &ev)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %q, err %v", exec.Status, err)
	}
	if got := probe.atStart["a"]; len(got) < 2 || got[0] != "createWorkRun" || got[1] != "createWorkStep" {
		t.Errorf("by the time the step began the journal had written %v, want the run and the step intent already on file", got)
	}
	names, args := journalWrites(t, rec)
	if len(names) == 0 || names[0] != "createWorkRun" || args[0]["triggeredBy"] != "event:"+ev.Topic {
		t.Fatalf("journal writes = %v (first: %v), want the run opened first", names, args)
	}
}

// An adopted run's row already exists and the adoption advances it, so there is
// nothing to hold back even when the row says it was a scheduled run (a
// scheduler recovery re-adopts one): the adoption write goes out at open.
func TestScheduledRunOnAnAdoptedRowIsNotHeld(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec}
	e := scheduledHoldExecutor(rec, probe)
	auto := &Automation{Name: "recoveredTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a")}

	exec, err := e.ExecuteAdopted(context.Background(), auto, RunAdoption{RunId: "adopted-run-1", TriggeredBy: "schedule"})
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %q, err %v", exec.Status, err)
	}
	got := probe.atStart["a"]
	if len(got) == 0 || got[0] != "updateWorkRun" {
		t.Fatalf("by the time the step began the journal had written %v, want the adoption (updateWorkRun) first", got)
	}
	for _, name := range journalWriteNames(rec) {
		if name == "createWorkRun" {
			t.Fatalf("an adopted run must not insert a second row: %v", journalWriteNames(rec))
		}
	}
}

// The executor's journal is shared by every run it executes, so a held run must
// hold a COPY. Were the hold set on the shared journal, the first tick would
// leave every later run on the executor -- events and @journalRequired runs
// included -- waiting for a write that opens a run nobody is watching.
func TestScheduledRunDoesNotHoldTheExecutorsSharedJournal(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec}
	e := scheduledHoldExecutor(rec, probe)

	if _, err := e.Execute(context.Background(), &Automation{Name: "idleTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a")}, "schedule"); err != nil {
		t.Fatalf("scheduled run: %v", err)
	}
	if e.journal.hold != nil {
		t.Fatal("the scheduled run set its hold on the executor's shared journal")
	}
	if got := journalWriteNames(rec); len(got) != 0 {
		t.Fatalf("the idle tick wrote %v", got)
	}

	ev := events.NewEvent("graph.node.created.default.v1:probe:thing", events.KindMessage, nil)
	if _, err := e.ExecuteWithEvent(context.Background(), &Automation{Name: "onThing", Steps: scheduledSteps("b")}, "event:"+ev.Topic, &ev); err != nil {
		t.Fatalf("event run: %v", err)
	}
	if got := journalWriteNames(rec); len(got) == 0 || got[0] != "createWorkRun" {
		t.Fatalf("an event run after a scheduled one wrote %v, want its run opened at the start", got)
	}
}

// holdsScheduledJournal is the whole of who is held: a cron tick, not adopted,
// not @journalRequired. Every other way an automation starts is journaled at
// open as before.
func TestHoldsScheduledJournalDecision(t *testing.T) {
	plain := &Automation{Name: "tick"}
	required := &Automation{Name: "guardedTick", JournalRequired: true}
	adopted := &RunAdoption{RunId: "run-1"}
	for _, tc := range []struct {
		name        string
		automation  *Automation
		triggeredBy string
		adopt       *RunAdoption
		want        bool
	}{
		{"a cron tick", plain, "schedule", nil, true},
		{"a cron tick of a @journalRequired automation", required, "schedule", nil, false},
		{"an adopted run recorded as scheduled", plain, "schedule", adopted, false},
		{"an event firing", plain, "event:graph.node.created.default.v1:probe:thing", nil, false},
		{"a manual run", plain, "manual", nil, false},
		{"a sub-automation", plain, "sub-automation", nil, false},
		{"an http trigger", plain, "http-trigger", nil, false},
		{"a client's run", plain, "run:client", nil, false},
		{"an unnamed trigger", plain, "", nil, false},
		{"no automation", nil, "schedule", nil, false},
	} {
		if got := holdsScheduledJournal(tc.automation, tc.triggeredBy, tc.adopt); got != tc.want {
			t.Errorf("%s: holdsScheduledJournal = %v, want %v", tc.name, got, tc.want)
		}
	}
}
