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
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
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
// record needs: the logs sweep failed on a statement timeout on 2026-10-10 and
// again on 2026-10-11, and its builtin archives and deletes through raw SQL,
// which reports nothing to the observer -- so the failure is the only thing
// such a run can leave.
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

// waitingProbe parks the run on a human wait at waitAt, and reports no write to
// the observer: the question was persisted where the hold does not hear it (on
// another node, or through a write masked the way the journal's own are).
type waitingProbe struct{ waitAt string }

func (p *waitingProbe) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	if step.ID == p.waitAt {
		return &StepResult{StepId: step.ID, StartedAt: now}, &work.HumanWait{ApprovalID: "approval-1"}
	}
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

// A run parked on a human wait is not finished, and only the answer carries it
// on: resume reads its run row and its waiting step back. A tick whose question
// reached the observer as no write is still held when it parks, so parking must
// open it -- the run row, the steps held behind it, the waiting receipt -- and
// leave it open: no close, for the reason closeRun writes none on a human wait
// (the answer may already have landed on another replica). Held and returned
// as it was, the run would be waiting on an approval with no run on file.
func TestScheduledRunParkedOnAHumanWaitLeavesAnOpenRun(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: &waitingProbe{waitAt: "b"}})
	e.journal = newWorkJournal(rec, nil)
	auto := &Automation{Name: "askingTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a", "b")}

	exec, err := e.Execute(context.Background(), auto, "schedule")
	if !isHumanWait(err) || exec.Status != "waiting" {
		t.Fatalf("status %q, err %v; want the run parked on its human wait", exec.Status, err)
	}

	names, args := journalWrites(t, rec)
	want := []string{
		"createWorkRun",
		"createWorkStep", "updateWorkStep", "updateWorkRun", // a, held then flushed
		"createWorkStep", "updateWorkStep", // b: the intent and the waiting receipt
	}
	if !slices.Equal(names, want) {
		t.Fatalf("journal writes = %v, want %v: a held run that parks must open, and stay open", names, want)
	}
	if args[0]["runId"] != exec.ID || args[0]["status"] != "running" || args[0]["triggeredBy"] != "schedule" {
		t.Errorf("run row: %v, want this run opened running by schedule", args[0])
	}
	if receipt := args[len(args)-1]; receipt["status"] != "waiting" {
		t.Errorf("the parked step's receipt: %v, want status waiting", receipt)
	}
	for i, name := range names {
		if name == "updateWorkRun" && args[i]["status"] != nil {
			t.Errorf("the parked run was closed %v: nothing closes a waiting run but its answer", args[i]["status"])
		}
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

// lockedRecorder is recordingJournalExecutor for a journal that beats from a
// second goroutine (duringStep's heartbeat), which that recorder is not safe
// against.
type lockedRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *lockedRecorder) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, query)
	return &memql.ExecuteResult{}, nil
}

func (r *lockedRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// slowWriter is a registry whose steps take their time -- the slow read of a
// maintenance sweep -- and one of which then writes. releasedAt is taken the
// instant before that write is reported, which is the instant the run opens.
type slowWriter struct {
	sleeps     map[string]time.Duration
	writeAt    string
	releasedAt time.Time
	// backlog names what the run's hold was keeping when the write was reported.
	backlog []string
}

func (p *slowWriter) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	time.Sleep(p.sleeps[step.ID])
	if step.ID == p.writeAt {
		p.backlog = heldBacklog(ctx)
		p.releasedAt = time.Now()
		common.NotifyWrite(ctx, "v1:probe:thing", "thing-1")
	}
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

// heldBacklog names, in order, the writes the hold of ctx's run is keeping. It
// reads the hold directly because what it is after -- that a pulse nobody could
// observe is not kept at all -- leaves no trace in what is eventually written:
// kept and restamped, a pulse is indistinguishable there from a live one.
func heldBacklog(ctx context.Context) []string {
	j, ok := runJournalFrom(ctx)
	if !ok || j == nil || j.hold == nil {
		return nil
	}
	j.hold.mu.Lock()
	defer j.hold.mu.Unlock()
	names := make([]string, len(j.hold.writes))
	for i, w := range j.hold.writes {
		names[i] = w.name
	}
	return names
}

// A run that opens late must open LIVE. The work sweep judges a running run by
// its newest heartbeat -- heartbeatAt, else startedAt (integrations/work
// sweep.go lastHeartbeat) -- and hands a run it finds silent for a minute to
// another replica, which resumes it from the step in flight while this one is
// still executing it. A row born with the firing's start time and then fed the
// heartbeats it held back, each stamped when it was MADE, would read as
// minutes silent until the last of them landed.
//
// So the row is created carrying a heartbeat of the moment it opens; the beats
// a step's duringStep kept while the run was held are dropped (there was no row
// to keep alive, and nobody could judge it); and the receipt-time heartbeats
// that ride the held step updates are restamped to the moment they are written.
// The steps below are slower than the heartbeat interval on purpose: held, the
// old behavior replayed about twenty stale beats ahead of these.
func TestScheduledJournalOpensLiveAndNeverReplaysStaleHeartbeats(t *testing.T) {
	rec := &lockedRecorder{}
	probe := &slowWriter{
		sleeps:  map[string]time.Duration{"a": 60 * time.Millisecond, "b": 30 * time.Millisecond},
		writeAt: "b",
	}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	e.journal = newWorkJournal(rec, nil)
	e.journal.heartbeatEvery = 5 * time.Millisecond
	auto := &Automation{Name: "slowTick", Schedule: "0 */10 * * * *", Steps: scheduledSteps("a", "b")}

	exec, err := e.Execute(context.Background(), auto, "schedule")
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %q, err %v", exec.Status, err)
	}
	opened := probe.releasedAt
	if opened.IsZero() {
		t.Fatal("the probe never reported its write")
	}

	var names []string
	var runWrites []map[string]any
	for _, call := range rec.snapshot() {
		if strings.HasPrefix(call, "query ") {
			continue
		}
		name, args := argsOf(t, call)
		// A heartbeat on a run version, whatever write carries it, may not predate
		// the moment the run opened: that is liveness moving backwards.
		if raw, beats := args["heartbeatAt"]; beats && (name == "createWorkRun" || name == "updateWorkRun") {
			at, perr := time.Parse(time.RFC3339Nano, raw.(string))
			if perr != nil {
				t.Fatalf("%s heartbeatAt %v does not parse: %v", name, raw, perr)
			}
			if at.Before(opened) {
				t.Errorf("%s carries heartbeatAt %s, %s before the run opened: a late-opened run must never replay a stale beat",
					name, at.Format(time.RFC3339Nano), opened.Sub(at))
			}
		}
		if name == "createWorkRun" || name == "updateWorkRun" {
			runWrites = append(runWrites, args)
		}
		// A beat on its own (runId and heartbeatAt, nothing else) is the heartbeat
		// goroutine's; one may tick live after the run is open, so those are checked
		// above for time and left out of the sequence below.
		if len(args) == 2 && args["runId"] != nil && args["heartbeatAt"] != nil {
			continue
		}
		names = append(names, name)
	}

	want := []string{
		"createWorkRun",
		"createWorkStep", "updateWorkStep", "updateWorkRun", // a: held, then flushed
		"createWorkStep", "updateWorkStep", "updateWorkRun", // b
		"updateWorkRun", // close
	}
	if !slices.Equal(names, want) {
		t.Fatalf("journal writes (heartbeat-only beats left out) = %v, want %v", names, want)
	}
	// The hold kept a's intent, receipt and receipt heartbeat and b's intent, and
	// nothing else: about twenty pulses ticked while a and b slept, and none of
	// them was kept.
	if wantBacklog := []string{"createWorkStep", "updateWorkStep", "updateWorkRun", "createWorkStep"}; !slices.Equal(probe.backlog, wantBacklog) {
		t.Errorf("the hold was keeping %v when the run opened, want %v: a pulse made while the run is held has no row to renew and is dropped, not kept to be replayed",
			probe.backlog, wantBacklog)
	}
	// The FIRST version of the run is live: the sweep may read the row the moment
	// it exists, before any held write has replayed.
	if len(runWrites) == 0 || runWrites[0]["heartbeatAt"] == nil {
		t.Fatalf("the run was created without a heartbeat (%v); until a later write lands it would be judged by its start time alone", runWrites)
	}
}

// A synchronous child belongs to the held parent's journal lifetime. The
// child may finish before a later parent step writes; it must then be replayed
// after its parent, or leave nothing when the whole tick remains idle.
func TestScheduledJournalHoldsSynchronousChildren(t *testing.T) {
	for _, mode := range []string{"idle", "child-writes", "parent-writes-later", "child-fails", "required-child"} {
		t.Run(mode, func(t *testing.T) {
			e, rec, reg, parent, child := subAutomationHarness(t)
			parent.Trigger = nil
			parent.Schedule = "0 */10 * * * *"
			parent.Steps = append(parent.Steps, scheduledSteps("after")...)
			child.JournalRequired = mode == "required-child"
			probe := &heldChildProbe{base: reg, rec: rec, mode: mode, child: child.Name}
			e.stepRegistry = probe
			exec, err := e.Execute(context.Background(), parent, "schedule")
			if mode != "child-fails" && err != nil {
				t.Fatal(err)
			}
			if mode == "idle" {
				if writes := journalWriteNames(rec); len(writes) != 0 {
					t.Fatalf("idle parent/child wrote %v", writes)
				}
				return
			}
			if mode != "required-child" && len(probe.beforeChild) != 0 {
				t.Fatalf("child opened before an effect: %v", probe.beforeChild)
			}
			var opens []map[string]any
			_, args := journalWrites(t, rec)
			names, _ := journalWrites(t, rec)
			for n, name := range names {
				if name == "createWorkRun" {
					opens = append(opens, args[n])
				}
			}
			if len(opens) != 2 || opens[0]["runId"] != exec.ID || opens[1]["parentRunId"] != exec.ID {
				t.Fatalf("parent must precede child and exist exactly once: %v", opens)
			}
		})
	}
}

type heldChildProbe struct {
	base        *subAutomationRegistry
	rec         *recordingJournalExecutor
	mode, child string
	beforeChild []string
}

func (p *heldChildProbe) Execute(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
	if sc.Execution.AutomationName == p.child {
		p.beforeChild = journalWriteNames(p.rec)
		if p.mode == "child-writes" {
			common.NotifyWrite(ctx, "v1:probe:thing", "child")
		}
		if p.mode == "child-fails" {
			return nil, errBoom
		}
	}
	if step.ID == "after" && p.mode == "parent-writes-later" {
		common.NotifyWrite(ctx, "v1:probe:thing", "parent")
	}
	return p.base.Execute(ctx, step, sc)
}
