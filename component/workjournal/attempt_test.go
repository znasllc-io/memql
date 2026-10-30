package workjournal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// attempt_test.go -- how a pipeline step's ATTEMPTS are journaled (design record
// docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md,
// section 3). The pipelines driver retries a step whose failure another attempt
// can cure, and a replica that takes the run over must learn from the step's
// row alone which attempt was in flight and whether the step has ended. So every
// attempt is a version of the step's ONE row: the intent of attempt N, written
// before the attempt starts (Run.StepAttempt), and the outcome of an attempt
// that another follows, written without ending the step (Step.Retrying). Every
// call is handed to the real parser, as runner_writes_test.go does, because the
// string is what the engine receives.

// attemptWork is a pipeline's work as its driver opens it, with the step keys a
// compiled plan gives them (stage.name). The run key is fixed so the ids, and
// the calls rendered from them, are the same on every run of a test.
func attemptWork() Work {
	return Work{
		OwnerUserID:  "user-1",
		Template:     "pipeline",
		Statement:    "Run memql's checks on 1a2b3c4",
		GoalKey:      "v1:pipelines:run:pr-1",
		RunKey:       "1",
		RequestedVia: "pipeline",
		TriggeredBy:  "pipeline:affected",
		QueueSteps:   true,
		Steps: []StepDecl{
			{
				Key: "checks.build", Kind: KindDeterministic, StepType: "exec",
				Call: map[string]any{"construct": "pipeline", "name": "build", "stage": "checks"},
			},
			{
				Key: "tests.unit", Kind: KindDeterministic, StepType: "exec",
				DependsOn: []string{"checks.build"},
				Call:      map[string]any{"construct": "pipeline", "name": "unit", "stage": "tests"},
			},
		},
	}
}

// openAttemptRun opens attemptWork against a recording engine and a clock the
// test moves by hand.
func openAttemptRun(t *testing.T) (*countingEngine, *stepClock, *Run) {
	t.Helper()
	engine := &countingEngine{}
	j, clock := newClockedJournal(engine)
	run, err := j.Begin(context.Background(), attemptWork())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(run.stopHeartbeat)
	return engine, clock, run
}

// attemptBinding is a binding in the shape the driver gives it: a record for
// each attempt so far, and when the next one may start. The journal reads none of
// it, so the only claim a test can make is that it arrives as given -- the text
// a failed step printed included, which is what a record's message is.
func attemptBinding(attempts int) map[string]any {
	records := make([]any, 0, attempts)
	for n := 1; n <= attempts; n++ {
		records = append(records, map[string]any{
			"attempt":    n,
			"class":      "nothing_ran",
			"code":       "pipeline_clone_failed",
			"message":    "fatal: unable to access 'https://example.test/r.git/': \"Connection reset\"\n\t{retry} <soon> & \\ \u00e9",
			"surface":    "fleet",
			"workerId":   "v1:worker:registration:msi",
			"startedAt":  clockStart.Format(time.RFC3339),
			"finishedAt": clockStart.Add(9 * time.Second).Format(time.RFC3339),
		})
	}
	return map[string]any{
		"attempts":      records,
		"nextAttemptAt": clockStart.Add(39 * time.Second).Format(time.RFC3339),
	}
}

func mustAttempt(t *testing.T, run *Run, key string, attempt int, binding map[string]any) *Step {
	t.Helper()
	step, err := run.StepAttempt(context.Background(), key, attempt, binding)
	if err != nil {
		t.Fatalf("attempt %d of %s: %v", attempt, key, err)
	}
	return step
}

// sameJSON fails unless got and want are the same JSON value. The parser reads
// a number as int64 where a test builds an int; JSON is what both mean, and a
// comparison that depended on the Go type would be testing the parser.
func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, gerr := json.Marshal(got)
	w, werr := json.Marshal(want)
	if gerr != nil || werr != nil {
		t.Fatalf("%s: cannot compare (%v, %v)", what, gerr, werr)
	}
	if string(g) != string(w) {
		t.Errorf("%s = %s, want %s", what, g, w)
	}
}

// argNames is the sorted names of a call's arguments.
func argNames(args map[string]any) []string {
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestStepAttemptsAreVersionsOfOneRow. A retry is not a second step: the step
// concept documents it as a NEW VERSION of the same row with attempt
// incremented. So attempt 2 lands on the id the queued row and attempt 1 already
// wrote (a run with two rows under one key is one the driver's resume refuses),
// restates the declaration it was queued with, and carries the driver's history
// in the binding exactly as given. The outcome of an attempt that another follows
// is a version of that row too, and it does not end the step: it names the status
// `running` and the binding, and nothing a receipt would.
func TestStepAttemptsAreVersionsOfOneRow(t *testing.T) {
	ctx := context.Background()
	engine, clock, run := openAttemptRun(t)
	const key = "tests.unit"
	rowID := deriveID("step", run.RunID(), key)

	queued := parsedArgs(t, callsNamed(engine.calls, "createWorkStep")[1])
	if queued["stepId"] != rowID {
		t.Fatalf("the queued row is %v, want %s", queued["stepId"], rowID)
	}
	before := len(engine.calls)
	mustAttempt(t, run, key, 1, nil)
	first := onlyCallNamed(t, engine.calls[before:], "createWorkStep")

	// Attempt 2, after a wait, carrying the history of attempt 1.
	clock.advance(40 * time.Second)
	history := attemptBinding(1)
	before = len(engine.calls)
	second := mustAttempt(t, run, key, 2, history)
	intent := onlyCallNamed(t, engine.calls[before:], "createWorkStep")
	for field, want := range map[string]any{
		"stepId":         rowID,
		"runId":          run.RunID(),
		"key":            key,
		"seq":            int64(1),
		"stepType":       "exec",
		"kind":           KindDeterministic,
		"status":         "running",
		"attempt":        int64(2),
		"idempotencyKey": run.RunID() + ":" + key + ":2",
		"startedAt":      clock.at.Format(time.RFC3339),
	} {
		if intent[field] != want {
			t.Errorf("attempt 2: %s = %#v, want %#v", field, intent[field], want)
		}
	}
	decl := attemptWork().Steps[1]
	if !reflect.DeepEqual(intent["call"], decl.Call) {
		t.Errorf("attempt 2 did not restate the declared call: %#v, want %#v", intent["call"], decl.Call)
	}
	if got := stringsOf(intent["dependsOn"]); !reflect.DeepEqual(got, decl.DependsOn) {
		t.Errorf("attempt 2 did not restate the declared dependencies: %v, want %v", got, decl.DependsOn)
	}
	sameJSON(t, "attempt 2: binding", intent["binding"], history)
	for _, absent := range []string{"finishedAt", "durationMs", "errorCode", "errorMessage", "result"} {
		if _, present := intent[absent]; present {
			t.Errorf("an intent wrote %s, which only a receipt may: %v", absent, intent)
		}
	}
	for what, id := range map[string]any{"the queued row": queued["stepId"], "attempt 1": first["stepId"]} {
		if id != intent["stepId"] {
			t.Errorf("attempt 2 wrote step %v and %s is %v -- an attempt became a second row", intent["stepId"], what, id)
		}
	}

	// The outcome of attempt 2, with attempt 3 to come.
	clock.advance(12 * time.Second)
	history = attemptBinding(2)
	before = len(engine.calls)
	if err := second.Retrying(ctx, history); err != nil {
		t.Fatalf("Retrying: %v", err)
	}
	outcome := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")
	if _, present := outcome["finishedAt"]; present {
		t.Errorf("an attempt another follows wrote finishedAt, which ends a step: %v", outcome)
	}
	if got, want := argNames(outcome), []string{"binding", "status", "stepId"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an attempt another follows wrote %v, want only %v -- anything more is a receipt, and a step with attempts left has none", got, want)
	}
	if outcome["stepId"] != rowID || outcome["status"] != "running" {
		t.Errorf("outcome = %v, want step %s still running", outcome, rowID)
	}
	sameJSON(t, "outcome: binding", outcome["binding"], history)

	// The concept numbers attempts from 1: a 0 or a negative one is a counting
	// error, refused with nothing written.
	for _, bad := range []int{0, -1} {
		before = len(engine.calls)
		step, err := run.StepAttempt(ctx, key, bad, history)
		if err == nil || step != nil {
			t.Errorf("attempt %d was accepted: (%v, %v)", bad, step, err)
		}
		if written := engine.calls[before:]; len(written) != 0 {
			t.Errorf("attempt %d wrote %v", bad, written)
		}
	}
}

// TestStepAttemptsAttemptOneIsExactlyWhatStepAlwaysWrote. Run.Step is attempt 1
// with no binding, and the Library pass and every pipeline step that is never
// retried journal through it, so its rendering is pinned byte for byte. The
// strings below are what Step rendered BEFORE StepAttempt existed (captured from
// that code, not derived from this one): a delegation that reordered, added or
// dropped an argument would change what every one of those rows says.
func TestStepAttemptsAttemptOneIsExactlyWhatStepAlwaysWrote(t *testing.T) {
	ctx := context.Background()
	const (
		pipelineIntent = `mutation createWorkStep(stepId: "%s", runId: "%s", key: "tests.unit", seq: 1, stepType: "exec", kind: "deterministic", call: {"construct":"pipeline","name":"unit","stage":"tests"}, dependsOn: ["checks.build"], status: "running", attempt: 1, idempotencyKey: "%s:tests.unit:1", startedAt: "2026-10-03T12:00:00Z")`
		pipelineQueued = `mutation createWorkStep(stepId: "%s", runId: "%s", key: "tests.unit", seq: 1, stepType: "exec", kind: "deterministic", call: {"construct":"pipeline","name":"unit","stage":"tests"}, dependsOn: ["checks.build"], status: "pending", attempt: 1, idempotencyKey: "%s:tests.unit:1")`
		functionIntent = `mutation createWorkStep(stepId: "%s", runId: "%s", key: "extract", seq: 0, stepType: "function", kind: "deterministic", status: "running", attempt: 1, idempotencyKey: "%s:extract:1", startedAt: "2026-10-03T12:00:00Z")`
		undeclaredStep = `mutation createWorkStep(stepId: "%s", runId: "%s", key: "undeclared", seq: 2, stepType: "function", kind: "deterministic", status: "running", attempt: 1, idempotencyKey: "%s:undeclared:1", startedAt: "2026-10-03T12:00:00Z")`
	)
	for _, tc := range []struct {
		name, key, want string
		work            Work
	}{
		{"a declared pipeline step", "tests.unit", pipelineIntent, attemptWork()},
		{"a declared function step", "extract", functionIntent, work()},
		{"a step nobody declared", "undeclared", undeclaredStep, work()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &countingEngine{}
			j, _ := newClockedJournal(engine)
			run, err := j.Begin(ctx, tc.work)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			t.Cleanup(run.stopHeartbeat)
			want := fmt.Sprintf(tc.want, deriveID("step", run.RunID(), tc.key), run.RunID(), run.RunID())

			// A binding that is nil or empty writes no argument at all, so
			// both are the call Step has always written.
			for how, write := range map[string]func() error{
				"Step": func() error { _, err := run.Step(ctx, tc.key); return err },
				"StepAttempt(1, nil)": func() error {
					_, err := run.StepAttempt(ctx, tc.key, 1, nil)
					return err
				},
				"StepAttempt(1, empty)": func() error {
					_, err := run.StepAttempt(ctx, tc.key, 1, map[string]any{})
					return err
				},
			} {
				before := len(engine.calls)
				if err := write(); err != nil {
					t.Fatalf("%s: %v", how, err)
				}
				if written := engine.calls[before:]; len(written) != 1 || written[0] != want {
					t.Errorf("%s wrote\n%v\nwant\n%s", how, written, want)
				}
			}

			// THE CONTROL: a binding that is given is written, and is the only
			// thing that changes -- so the three equalities above come from the
			// binding being absent and not from it never being written.
			before := len(engine.calls)
			mustAttempt(t, run, tc.key, 1, attemptBinding(1))
			withBinding := parsedArgs(t, engine.calls[before])
			sameJSON(t, "binding", withBinding["binding"], attemptBinding(1))
			delete(withBinding, "binding")
			if !reflect.DeepEqual(withBinding, parsedArgs(t, want)) {
				t.Errorf("a binding changed more than itself:\n%v\nwant\n%v", withBinding, parsedArgs(t, want))
			}
		})
	}

	// The queued pending version and the first intent are versions of one row,
	// and they agree on the key the first attempt's side effects run under.
	engine := &countingEngine{}
	j, _ := newClockedJournal(engine)
	run, err := j.Begin(ctx, attemptWork())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(run.stopHeartbeat)
	queued := callsNamed(engine.calls, "createWorkStep")[1]
	if want := fmt.Sprintf(pipelineQueued, deriveID("step", run.RunID(), "tests.unit"), run.RunID(), run.RunID()); queued != want {
		t.Errorf("the queued row wrote\n%s\nwant\n%s", queued, want)
	}
	before := len(engine.calls)
	mustAttempt(t, run, "tests.unit", 1, nil)
	intent := onlyCallNamed(t, engine.calls[before:], "createWorkStep")
	pending := parsedArgs(t, queued)
	if intent["idempotencyKey"] != pending["idempotencyKey"] || intent["attempt"] != pending["attempt"] {
		t.Errorf("the first intent (%v, %v) disagrees with the queued row (%v, %v)",
			intent["attempt"], intent["idempotencyKey"], pending["attempt"], pending["idempotencyKey"])
	}
}

// TestStepAttemptsRetryingWritesNoReceiptEvenWithNoBinding. What an attempt that
// another follows may say is "still running, and here is the binding"; with
// nothing to bind it says only that it is still running, and never anything a
// receipt would.
func TestStepAttemptsRetryingWritesNoReceiptEvenWithNoBinding(t *testing.T) {
	for name, binding := range map[string]map[string]any{"a nil binding": nil, "an empty binding": {}} {
		t.Run(name, func(t *testing.T) {
			engine, _, run := openAttemptRun(t)
			step := mustAttempt(t, run, "tests.unit", 1, nil)
			before := len(engine.calls)
			if err := step.Retrying(context.Background(), binding); err != nil {
				t.Fatalf("Retrying: %v", err)
			}
			outcome := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")
			want := map[string]any{"stepId": deriveID("step", run.RunID(), "tests.unit"), "status": "running"}
			if !reflect.DeepEqual(outcome, want) {
				t.Fatalf("Retrying(%s) wrote %v, want exactly %v", name, outcome, want)
			}
		})
	}
}

// TestStepAttemptsEachHandleTimesItsOwnAttempt. A receipt's duration is the
// body's, measured from the handle's own intent. The handle StepAttempt returns
// for attempt 2 starts its clock at attempt 2, so the receipt that ends the step
// times the attempt that ended it and not the failed attempt before it or the
// wait between them.
func TestStepAttemptsEachHandleTimesItsOwnAttempt(t *testing.T) {
	ctx := context.Background()
	engine, clock, run := openAttemptRun(t)
	first := mustAttempt(t, run, "tests.unit", 1, nil)
	clock.advance(30 * time.Second)
	if err := first.Retrying(ctx, attemptBinding(1)); err != nil {
		t.Fatalf("Retrying: %v", err)
	}
	clock.advance(30 * time.Second) // the wait before attempt 2
	second := mustAttempt(t, run, "tests.unit", 2, attemptBinding(1))
	clock.advance(1500 * time.Millisecond)

	before := len(engine.calls)
	if err := second.Finish(ctx, Receipt{Status: "done"}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	receipt := onlyCallNamed(t, engine.calls[before:], "updateWorkStep")
	if receipt["status"] != "done" || receipt["durationMs"] != int64(1500) || receipt["finishedAt"] != clock.at.Format(time.RFC3339) {
		t.Fatalf("receipt = %v, want done after 1500 ms (attempt 2's own), finished now", receipt)
	}
	if receipt["stepId"] != deriveID("step", run.RunID(), "tests.unit") {
		t.Fatalf("the receipt closed step %v, not the one row every attempt wrote", receipt["stepId"])
	}
}

// TestStepAttemptsCarryInternalOriginAndTheOwnersActor. Both writes are
// @serverOnly, and this package's place on the internal-origin allowlist rests
// on every call site being downstream of the owner gate (see
// internal_origin_precondition_test.go). That includes a handle a replica gets
// by Reopen, which is the replica that continues a retry after a takeover.
func TestStepAttemptsCarryInternalOriginAndTheOwnersActor(t *testing.T) {
	ctx := context.Background()
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")
	w := attemptWork()
	run, err := j.Begin(ctx, w)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(run.stopHeartbeat)
	reopened := j.Reopen("user-1", run.GoalID(), run.RunID(), w.Steps, clockStart)

	before := len(engine.calls)
	for _, r := range []*Run{run, reopened} {
		first := mustAttempt(t, r, "checks.build", 1, nil)
		if err := first.Retrying(ctx, attemptBinding(1)); err != nil {
			t.Fatalf("Retrying: %v", err)
		}
		second := mustAttempt(t, r, "checks.build", 2, attemptBinding(1))
		if err := second.Retrying(ctx, attemptBinding(2)); err != nil {
			t.Fatalf("Retrying: %v", err)
		}
	}
	if written := len(engine.calls) - before; written != 8 {
		t.Fatalf("wrote %d calls, want 8 (two intents and two outcomes, on each handle): %v", written, engine.calls[before:])
	}
	for i := before; i < len(engine.calls); i++ {
		if !engine.origins[i] {
			t.Errorf("call %d (%s) did not carry internal origin", i, firstWord(engine.calls[i]))
		}
		if engine.owners[i] != "user-1" {
			t.Errorf("call %d (%s) ran as %q, want the owner", i, firstWord(engine.calls[i]), engine.owners[i])
		}
	}
}

// TestStepAttemptsAreFencedByTheWriteGuard. A driver that lost the run must not
// be able to start another attempt or move a step on: the guard is the
// ownership check, and both writes pass through it like every other.
func TestStepAttemptsAreFencedByTheWriteGuard(t *testing.T) {
	ctx := context.Background()
	engine := &countingEngine{}
	held := false
	refused := errors.New("another driver took the run over")
	j := New(engine, nil, "node-1").WithWriteGuard(func(ctx context.Context, write func(context.Context) error) error {
		if held {
			return refused
		}
		return write(ctx)
	})
	run, err := j.Begin(ctx, attemptWork())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(run.stopHeartbeat)
	step := mustAttempt(t, run, "tests.unit", 1, nil)

	held = true
	before := len(engine.calls)
	if next, err := run.StepAttempt(ctx, "tests.unit", 2, attemptBinding(1)); next != nil || !errors.Is(err, refused) {
		t.Errorf("a stale driver started attempt 2: (%v, %v)", next, err)
	}
	if err := step.Retrying(ctx, attemptBinding(1)); !errors.Is(err, refused) {
		t.Errorf("a stale driver recorded an outcome: %v", err)
	}
	if len(engine.calls) != before {
		t.Fatalf("a refused writer reached the engine: %v", engine.calls[before:])
	}
}

// TestStepAttemptsAFailedWriteIsReturnedToTheDriver. An intent that did not land
// cannot authorize the attempt it announces, and an outcome that did not land
// cannot authorize the next one; the driver can only refuse what it is told
// about.
func TestStepAttemptsAFailedWriteIsReturnedToTheDriver(t *testing.T) {
	ctx := context.Background()
	engine, _, run := openAttemptRun(t)
	step := mustAttempt(t, run, "tests.unit", 1, nil)

	engine.fail = true
	before := len(engine.calls)
	if next, err := run.StepAttempt(ctx, "tests.unit", 2, attemptBinding(1)); next != nil || err == nil {
		t.Error("an attempt whose intent did not land was reported as recorded")
	}
	if err := step.Retrying(ctx, attemptBinding(1)); err == nil {
		t.Error("an outcome that did not land was reported as recorded")
	}
	if got := len(engine.calls) - before; got != 2 {
		t.Errorf("a failing engine saw %d writes, want both attempted", got)
	}
}

// TestStepAttemptsNilHandlesAreSafe: a journal that is not there refuses an
// intent, so a driver cannot start an attempt it cannot record, and tolerates
// the outcome of a step it never recorded, like every other method on a nil
// handle.
func TestStepAttemptsNilHandlesAreSafe(t *testing.T) {
	ctx := context.Background()
	var run *Run
	if step, err := run.StepAttempt(ctx, "tests.unit", 1, nil); step != nil || err == nil {
		t.Errorf("a nil run recorded an intent: (%v, %v)", step, err)
	}
	if step, err := (&Run{}).StepAttempt(ctx, "tests.unit", 1, nil); step != nil || err == nil {
		t.Errorf("a run with no journal recorded an intent: (%v, %v)", step, err)
	}
	var step *Step
	if err := step.Retrying(ctx, attemptBinding(1)); err != nil {
		t.Errorf("a nil step refused an outcome: %v", err)
	}
	if err := (&Step{}).Retrying(ctx, attemptBinding(1)); err != nil {
		t.Errorf("a step with no run refused an outcome: %v", err)
	}
}
