package workjournal

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// runner_writes_test.go -- the writes a RUNNER-OWNED run makes (epic
// memql#5477, design record D7 and D11): a pipeline's work run, opened by the
// pipelines driver on an agent node with every step queued, closed one
// receipt at a time, and resumed by another replica's driver when the first
// goes silent. Every call is handed to the real parser rather than matched as
// text, because the string is what the engine receives.

// stepClock is a clock a test moves by hand, so a measured duration and a
// written timestamp are assertable values.
type stepClock struct{ at time.Time }

func (c *stepClock) now() time.Time          { return c.at }
func (c *stepClock) advance(d time.Duration) { c.at = c.at.Add(d) }

var clockStart = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func newClockedJournal(engine Executor) (*Journal, *stepClock) {
	clock := &stepClock{at: clockStart}
	j := New(engine, nil, "agent-1")
	j.now = clock.now
	return j, clock
}

// parsedArgs is a rendered call's arguments as the parser reads them.
func parsedArgs(t *testing.T, call string) map[string]any {
	t.Helper()
	parsed, err := langparser.ParseExpression(strings.TrimPrefix(call, "mutation "))
	if err != nil {
		t.Fatalf("the parser refused:\n%s\n%v", call, err)
	}
	fn, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("%s parsed as %T", call, parsed)
	}
	return fn.Args
}

// callsNamed is every recorded call to one mutation, in order.
func callsNamed(calls []string, name string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "mutation "+name+"(") {
			out = append(out, c)
		}
	}
	return out
}

// onlyCallNamed is the single call to name among calls, failing otherwise: a
// write made twice is as wrong as one never made.
func onlyCallNamed(t *testing.T, calls []string, name string) map[string]any {
	t.Helper()
	found := callsNamed(calls, name)
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 %s, got %d among %v", name, len(found), calls)
	}
	return parsedArgs(t, found[0])
}

// stringsOf reads a parsed list argument as strings.
func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// pipelineWork is a pipeline's work as its driver opens it: two stages, the
// second sharded, each shard waiting on the first stage's step.
func pipelineWork() Work {
	return Work{
		OwnerUserID:  "user-1",
		Template:     "pipeline",
		Statement:    "Run memql's checks on 1a2b3c4",
		GoalKey:      "v1:pipelines:run:pr-1",
		RequestedVia: "pipeline",
		TriggeredBy:  "pipeline:affected",
		QueueSteps:   true,
		Steps: []StepDecl{
			{
				Key: "checks/build-vet", Kind: KindDeterministic, StepType: "exec",
				Call: map[string]any{"construct": "pipeline", "name": "build-vet", "stage": "checks"},
			},
			{
				Key: "tests/go-tests#1", Kind: KindDeterministic, StepType: "exec",
				DependsOn: []string{"checks/build-vet"},
				Call:      map[string]any{"construct": "pipeline", "name": "go-tests", "stage": "tests"},
			},
			{
				Key: "tests/go-tests#2", Kind: KindDeterministic, StepType: "exec",
				DependsOn: []string{"checks/build-vet"},
				Call:      map[string]any{"construct": "pipeline", "name": "go-tests", "stage": "tests"},
			},
		},
	}
}

// TestBeginRecordsWhoTriggeredTheRun. The integrations/work dispatcher, its
// sweep and the Nexus acts all decide on triggeredBy whether a run is theirs,
// so a pipeline's run must carry pipeline:<mode> from its FIRST version -- a
// later write could not, since updateWorkRun does not accept the field. A pass
// that names nothing carries the journal's own marker, journal:<template>, so
// the dispatcher never claims a run its driver is still recording.
func TestBeginRecordsWhoTriggeredTheRun(t *testing.T) {
	for _, tc := range []struct{ name, triggeredBy, want string }{
		{"a pass that names no trigger is the journal's own", "", TriggeredBy("libraryAnalyzeFile")},
		{"a pipeline's run carries its mode", "pipeline:affected", "pipeline:affected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &countingEngine{}
			w := work()
			w.TriggeredBy = tc.triggeredBy
			if _, err := New(engine, nil, "node-1").Begin(context.Background(), w); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if got := parsedArgs(t, runCallOf(t, engine.calls))["triggeredBy"]; got != tc.want {
				t.Fatalf("triggeredBy = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestIDsAreTheIdsBeginOpens. A runner that names its work before opening it
// (the pipelines driver records the ids on its own row first) must name
// exactly the rows Begin then writes -- the goal, the run, and through the run
// every step -- and nothing is written by asking.
func TestIDsAreTheIdsBeginOpens(t *testing.T) {
	w := pipelineWork()
	w.RunKey = "2"
	engine := &countingEngine{}
	goalID, runID, err := IDs(w)
	if err != nil || goalID == "" || runID == "" {
		t.Fatalf("IDs: %q %q %v", goalID, runID, err)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("asking writes nothing")
	}
	run, err := New(engine, nil, "node-1").Begin(context.Background(), w)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if run.GoalID() != goalID || run.RunID() != runID {
		t.Errorf("Begin opened goal %q run %q; IDs named %q %q", run.GoalID(), run.RunID(), goalID, runID)
	}
	if got := onlyCallNamed(t, engine.calls, "createWorkGoal")["goalId"]; got != goalID {
		t.Errorf("the goal written is %v", got)
	}
	if got := onlyCallNamed(t, engine.calls, "createWorkRun")["runId"]; got != runID {
		t.Errorf("the run written is %v", got)
	}

	// Another attempt of the same goal is another run of the same goal.
	w.RunKey = "3"
	goal3, run3, _ := IDs(w)
	if goal3 != goalID || run3 == runID {
		t.Errorf("attempt 3: goal %q (want %q), run %q (want a new one)", goal3, goalID, run3)
	}
	// A run key Begin would take from the clock cannot be named in advance.
	w.RunKey = "  "
	if _, _, err := IDs(w); err == nil {
		t.Errorf("an unpredictable run key is refused")
	}
}

// TestQueueStepsWritesEveryDeclaredStepPendingAtOpen. A pipeline's later
// stages must exist as rows before they run -- the run page draws them as the
// stops ahead (D13) -- so every declared step is written at `pending` when the
// run opens, carrying what it is and what it waits for. The running intent the
// driver writes later is a VERSION of that same row, never a second row.
func TestQueueStepsWritesEveryDeclaredStepPendingAtOpen(t *testing.T) {
	engine := &countingEngine{}
	j, _ := newClockedJournal(engine)
	w := pipelineWork()
	run, err := j.Begin(context.Background(), w)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	queued := callsNamed(engine.calls, "createWorkStep")
	if len(queued) != len(w.Steps) {
		t.Fatalf("queued %d steps, want every declared step (%d): %v", len(queued), len(w.Steps), engine.calls)
	}
	openedAt := -1
	for n, c := range engine.calls {
		if strings.HasPrefix(c, "mutation createWorkRun(") {
			openedAt = n
		}
		if strings.HasPrefix(c, "mutation createWorkStep(") && openedAt < 0 {
			t.Fatalf("a step was queued before its run was opened: %v", engine.calls)
		}
	}
	for n, c := range queued {
		args := parsedArgs(t, c)
		decl := w.Steps[n]
		for field, want := range map[string]any{
			"runId": run.RunID(), "key": decl.Key, "seq": int64(n), "status": "pending",
			"stepType": "exec", "kind": KindDeterministic, "attempt": int64(1),
		} {
			if args[field] != want {
				t.Errorf("%s: %s = %#v, want %#v", decl.Key, field, args[field], want)
			}
		}
		if !reflect.DeepEqual(args["call"], decl.Call) {
			t.Errorf("%s: call = %#v, want %#v", decl.Key, args["call"], decl.Call)
		}
		if got := stringsOf(args["dependsOn"]); !reflect.DeepEqual(got, append([]string{}, decl.DependsOn...)) {
			t.Errorf("%s: dependsOn = %v, want %v", decl.Key, got, decl.DependsOn)
		}
		if _, present := args["dependsOn"]; present && len(decl.DependsOn) == 0 {
			t.Errorf("%s: a step that waits on nothing wrote dependsOn anyway: %s", decl.Key, c)
		}
		// A queued step has not started, and a start time would say it had.
		if _, present := args["startedAt"]; present {
			t.Errorf("%s: a pending step wrote startedAt: %s", decl.Key, c)
		}
	}

	before := len(engine.calls)
	mustJournalStep(t, run, context.Background(), "tests/go-tests#1")
	intent := onlyCallNamed(t, engine.calls[before:], "createWorkStep")
	if intent["stepId"] != parsedArgs(t, queued[1])["stepId"] {
		t.Fatalf("the running intent wrote step %v, the queued row is %v -- one step became two rows",
			intent["stepId"], parsedArgs(t, queued[1])["stepId"])
	}
	if intent["status"] != "running" || intent["stepType"] != "exec" || intent["seq"] != int64(1) {
		t.Fatalf("the running intent = %v, want status running, the declared stepType and the queued seq", intent)
	}

	// THE CONTROL: the same work without QueueSteps queues nothing, so the
	// rows above come from the flag and not from Begin itself.
	plain := &countingEngine{}
	w.QueueSteps = false
	if _, err := New(plain, nil, "agent-1").Begin(context.Background(), w); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if n := len(callsNamed(plain.calls, "createWorkStep")); n != 0 {
		t.Fatalf("a run that asked for no queue wrote %d step rows at open", n)
	}
}

// TestAStepWithNoDeclaredTypeIsStillAFunction pins the journal's existing
// writers: a declaration that names no step type, call or dependency writes
// the running intent exactly as before -- `function`, with neither field.
func TestAStepWithNoDeclaredTypeIsStillAFunction(t *testing.T) {
	engine := &countingEngine{}
	run, err := New(engine, nil, "node-1").Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mustJournalStep(t, run, context.Background(), "extract")
	args := onlyCallNamed(t, engine.calls, "createWorkStep")
	if args["stepType"] != "function" {
		t.Fatalf("stepType = %v, want function", args["stepType"])
	}
	for _, absent := range []string{"call", "dependsOn"} {
		if _, present := args[absent]; present {
			t.Errorf("a step declaring no %s wrote one: %v", absent, args)
		}
	}
}

// TestFinishWritesTheWholeReceipt. A pipeline step's close carries what its
// runner reported: where it ran, its log and artifacts as Library files, and
// its own measured duration -- which is the one to record, since the runner
// saw the command run and the journal saw only a round trip.
func TestFinishWritesTheWholeReceipt(t *testing.T) {
	engine := &countingEngine{}
	j, clock := newClockedJournal(engine)
	run, err := j.Begin(context.Background(), pipelineWork())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	before := len(engine.calls)
	step := mustJournalStep(t, run, context.Background(), "tests/go-tests#1")
	intent := onlyCallNamed(t, engine.calls[before:], "createWorkStep")
	clock.advance(90 * time.Second)

	before = len(engine.calls)
	binding := map[string]any{"surface": "cluster", "nodeId": "workbench-0", "jobName": "pr-1-go-tests-1"}
	result := map[string]any{"status": "failed", "error": "go test exited 1"}
	step.Finish(context.Background(), Receipt{
		Status:          "failed",
		Result:          result,
		Code:            "pipeline_step_failed",
		Message:         "go test exited 1",
		DurationMs:      84250,
		Binding:         binding,
		LogFileID:       "v1:library:file:log-1",
		ArtifactFileIDs: []string{"v1:library:file:cover-1", "v1:library:file:junit-1"},
	})
	args := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")
	for field, want := range map[string]any{
		"stepId":       intent["stepId"],
		"status":       "failed",
		"errorCode":    "pipeline_step_failed",
		"errorMessage": "go test exited 1",
		"durationMs":   int64(84250),
		"logFileId":    "v1:library:file:log-1",
		"finishedAt":   clock.at.Format(time.RFC3339),
	} {
		if args[field] != want {
			t.Errorf("%s = %#v, want %#v", field, args[field], want)
		}
	}
	if !reflect.DeepEqual(args["binding"], binding) {
		t.Errorf("binding = %#v, want %#v", args["binding"], binding)
	}
	if !reflect.DeepEqual(args["result"], result) {
		t.Errorf("result = %#v, want %#v", args["result"], result)
	}
	if got := stringsOf(args["artifactFileIds"]); !reflect.DeepEqual(got, []string{"v1:library:file:cover-1", "v1:library:file:junit-1"}) {
		t.Errorf("artifactFileIds = %v", got)
	}

	// THE CONTROL: a receipt reporting nothing more than its status writes
	// none of the optional fields -- every argument here is a read-merge
	// write, and an empty one is a value -- and a duration the runner did not
	// report is measured from the running write.
	other := mustJournalStep(t, run, context.Background(), "tests/go-tests#2")
	clock.advance(1500 * time.Millisecond)
	before = len(engine.calls)
	other.Finish(context.Background(), Receipt{Status: "done"})
	bare := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")
	for _, absent := range []string{"result", "errorCode", "errorMessage", "binding", "logFileId", "artifactFileIds"} {
		if _, present := bare[absent]; present {
			t.Errorf("a receipt with no %s wrote one: %v", absent, bare)
		}
	}
	if bare["durationMs"] != int64(1500) {
		t.Errorf("durationMs = %#v, want 1500 measured from the running write", bare["durationMs"])
	}
}

// TestFinishWritesOnlyTheStepConceptsTerminalStatuses. A receipt names how a
// step ENDED, so only the step concept's four terminal values are written. An
// unknown one is refused here, loudly in the log, rather than sent for the
// engine to refuse -- or, worse, sent as `running` and left open.
func TestFinishWritesOnlyTheStepConceptsTerminalStatuses(t *testing.T) {
	for _, status := range []string{"done", "failed", "skipped", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			engine := &countingEngine{}
			run, _ := New(engine, nil, "agent-1").Begin(context.Background(), pipelineWork())
			step := mustJournalStep(t, run, context.Background(), "checks/build-vet")
			before := len(engine.calls)
			step.Finish(context.Background(), Receipt{Status: status})
			if got := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")["status"]; got != status {
				t.Fatalf("status = %v, want %s", got, status)
			}
		})
	}
	for _, status := range []string{"", "succeeded", "running", "pending", "DONE"} {
		t.Run("refused "+status, func(t *testing.T) {
			engine := &countingEngine{}
			run, _ := New(engine, nil, "agent-1").Begin(context.Background(), pipelineWork())
			step := mustJournalStep(t, run, context.Background(), "checks/build-vet")
			before := len(engine.calls)
			step.Finish(context.Background(), Receipt{Status: status, Code: "x", Message: "y"})
			if written := engine.calls[before:]; len(written) != 0 {
				t.Fatalf("a receipt with status %q was written: %v", status, written)
			}
		})
	}
}

// TestCancelledClosesTheStepAndTheRun. A pipeline run a person cancels, or a
// newer push supersedes (D11), ends `cancelled` -- the step concept and the
// run concept both declare the value -- and its goal closes with it, because a
// goal whose only run has stopped is not still being worked.
func TestCancelledClosesTheStepAndTheRun(t *testing.T) {
	engine := &countingEngine{}
	j, clock := newClockedJournal(engine)
	run, err := j.Begin(context.Background(), pipelineWork())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	step := mustJournalStep(t, run, context.Background(), "checks/build-vet")
	clock.advance(time.Minute)

	before := len(engine.calls)
	step.Cancelled(context.Background(), "a newer push superseded this run")
	receipt := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")
	if receipt["status"] != "cancelled" {
		t.Fatalf("step status = %v, want cancelled", receipt["status"])
	}
	if reason, _ := receipt["result"].(map[string]any); reason["reason"] != "a newer push superseded this run" {
		t.Errorf("the step's result does not say why: %v", receipt["result"])
	}

	before = len(engine.calls)
	run.Cancelled(context.Background(), "pipeline_cancelled", "a newer push superseded this run")
	closed := onlyCallNamed(t, engine.calls[before:], "updateWorkRun")
	for field, want := range map[string]any{
		"runId":        run.RunID(),
		"status":       "cancelled",
		"errorCode":    "pipeline_cancelled",
		"errorMessage": "a newer push superseded this run",
		"finishedAt":   clock.at.Format(time.RFC3339),
	} {
		if closed[field] != want {
			t.Errorf("run %s = %#v, want %#v", field, closed[field], want)
		}
	}
	goal := onlyCallNamed(t, engine.calls[before:], "updateWorkGoal")
	if goal["goalId"] != run.GoalID() || goal["status"] != "closed" || goal["closeReason"] != "a newer push superseded this run" {
		t.Fatalf("the goal was not closed with its run: %v", goal)
	}

	// A cancel nobody explained still closes the goal, saying what happened
	// rather than that the run finished.
	quiet := &countingEngine{}
	other, _ := New(quiet, nil, "agent-1").Begin(context.Background(), pipelineWork())
	before = len(quiet.calls)
	other.Cancelled(context.Background(), "", "")
	if reason := onlyCallNamed(t, quiet.calls[before:], "updateWorkGoal")["closeReason"]; reason != "the run was cancelled" {
		t.Fatalf("closeReason = %v, want the run was cancelled", reason)
	}
}

// TestHeartbeatWritesTheBeatAndNothingElse. The driver beats every 30 s from
// a goroutine that can lose a race with the run's close; a beat that also
// wrote a status would reopen a closed run, so it writes the time and the node
// beating -- the replica a resumed driver runs on, which is the run's node now
// -- and nothing else.
func TestHeartbeatWritesTheBeatAndNothingElse(t *testing.T) {
	engine := &countingEngine{}
	j, clock := newClockedJournal(engine)
	run, err := j.Begin(context.Background(), pipelineWork())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	clock.advance(30 * time.Second)
	before := len(engine.calls)
	run.Heartbeat(context.Background())
	beat := onlyCallNamed(t, engine.calls[before:], "updateWorkRun")
	want := map[string]any{"runId": run.RunID(), "heartbeatAt": clock.at.Format(time.RFC3339), "nodeId": "agent-1"}
	if !reflect.DeepEqual(beat, want) {
		t.Fatalf("heartbeat = %v, want exactly %v", beat, want)
	}
}

// TestReopenWritesNothingAndAddressesTheRowsBeginWrote. A driver that takes a
// silent run over resumes it on ANOTHER replica, from ids it read back off the
// pipelines run row -- where a reference is stored canonicalized. Reopen must
// write nothing (the run, its goal and its steps exist; writing at reopen
// would be a second opening of one run), and the handle it returns must
// address the SAME rows Begin wrote, from either form of the ids: a step id
// derived from the canonical form would be a different row, and the resumed
// step would appear beside its own pending row instead of advancing it.
func TestReopenWritesNothingAndAddressesTheRowsBeginWrote(t *testing.T) {
	opener := &countingEngine{}
	first, clock := newClockedJournal(opener)
	w := pipelineWork()
	opened, err := first.Begin(context.Background(), w)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	queued := parsedArgs(t, callsNamed(opener.calls, "createWorkStep")[2])

	forms := map[string][2]string{
		"bare ids":      {opened.GoalID(), opened.RunID()},
		"canonical ids": {"v1:work:goal:" + opened.GoalID(), "v1:work:run:" + opened.RunID()},
	}
	for name, ids := range forms {
		t.Run(name, func(t *testing.T) {
			resumer := &countingEngine{}
			second, later := newClockedJournal(resumer)
			later.at = clock.at.Add(5 * time.Minute)

			reopened := second.Reopen("user-1", ids[0], ids[1], w.Steps, clockStart)
			if len(resumer.calls) != 0 {
				t.Fatalf("Reopen wrote %v; it must write nothing", resumer.calls)
			}
			if reopened.RunID() != opened.RunID() || reopened.GoalID() != opened.GoalID() {
				t.Fatalf("reopened (%s, %s), opened (%s, %s)", reopened.GoalID(), reopened.RunID(), opened.GoalID(), opened.RunID())
			}

			mustJournalStep(t, reopened, context.Background(), "tests/go-tests#2")
			intent := onlyCallNamed(t, resumer.calls, "createWorkStep")
			if intent["stepId"] != queued["stepId"] {
				t.Fatalf("the resumed step wrote %v, the queued row is %v", intent["stepId"], queued["stepId"])
			}
			if intent["runId"] != opened.RunID() || intent["seq"] != int64(2) || intent["stepType"] != "exec" {
				t.Fatalf("the resumed intent = %v, want the opened run's id, the declared seq and type", intent)
			}

			reopened.Succeeded(context.Background(), nil)
			closed := onlyCallNamed(t, resumer.calls, "updateWorkRun")
			if closed["runId"] != opened.RunID() || closed["status"] != "succeeded" {
				t.Fatalf("the close = %v, want the opened run succeeded", closed)
			}
			// The wall clock is the RUN's, from when it started, not from when
			// this replica picked it up.
			spent, _ := closed["spent"].(map[string]any)
			if got := fmt.Sprint(spent["wallClockMs"]); got != fmt.Sprint((5 * time.Minute).Milliseconds()) {
				t.Errorf("wallClockMs = %v, want five minutes from the run's start", got)
			}
			if goal := onlyCallNamed(t, resumer.calls, "updateWorkGoal"); goal["goalId"] != opened.GoalID() {
				t.Fatalf("the goal close named %v, want %s", goal["goalId"], opened.GoalID())
			}
		})
	}
}

// A run reopened with no start time cannot say how long it ran, so its close
// says nothing about it rather than a wall clock measured from year one.
func TestAReopenedRunWithNoStartWritesNoWallClock(t *testing.T) {
	engine := &countingEngine{}
	run := New(engine, nil, "agent-1").Reopen("user-1", "goal-1", "run-1", nil, time.Time{})
	run.Failed(context.Background(), "pipeline_driver_lost", "the run could not be resumed")
	if _, present := onlyCallNamed(t, engine.calls, "updateWorkRun")["spent"]; present {
		t.Fatalf("a run with no start wrote a wall clock: %v", engine.calls)
	}
}

func mustJournalStep(t *testing.T, run *Run, ctx context.Context, key string) *Step {
	t.Helper()
	step, err := run.Step(ctx, key)
	if err != nil {
		t.Fatalf("step intent: %v", err)
	}
	return step
}
