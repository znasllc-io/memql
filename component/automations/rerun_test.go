package automations

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// rerun_test.go -- a step of a finished run run again, and a branch serving
// its prefix by reference (epic memql#5414, task memql#5415), DB-free: the
// journal is built from rows exactly as LoadRunJournal builds it, the step
// registry is sequence_test.go's probe (plus the run context each call reached
// it with, which is what the model seam reads), and the journal writes into a
// recorder. rerun_db_test.go is the round trip through Postgres.

// rerunSource is the template every test here runs: fetch, draft from what was
// fetched, publish the draft.
const rerunSource = `@trigger(event="probe.fired")
automation drafts {
  a := builtin fetch()
  b := builtin draft(from: a)
  builtin publish(text: b)
}`

// runContextProbe records, per callee, the run context each call reached the
// registry with.
type runContextProbe struct {
	*stmtProbe
	mu   sync.Mutex
	runs map[string][]common.RunContext
}

func newRunContextProbe() *runContextProbe {
	return &runContextProbe{stmtProbe: newStmtProbe(), runs: map[string][]common.RunContext{}}
}

func (p *runContextProbe) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	rc, _ := common.RunFromContext(ctx)
	if step.Function != nil {
		p.mu.Lock()
		p.runs[step.Function.Name] = append(p.runs[step.Function.Name], rc)
		p.mu.Unlock()
	}
	return p.stmtProbe.Execute(ctx, step, stepCtx)
}

func (p *runContextProbe) runOf(t *testing.T, callee string) common.RunContext {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.runs[callee]) == 0 {
		t.Fatalf("%s was never called", callee)
	}
	return p.runs[callee][len(p.runs[callee])-1]
}

// finishedRun is the run row and step rows of one finished execution of the
// template, version 1 of every step, as the journal wrote them.
func finishedRun(runId string) (map[string]any, []map[string]any) {
	run := map[string]any{
		"id": "v1:work:run:" + runId, "automationName": "drafts", "status": "succeeded",
		"goalId": "v1:work:goal:g1", "ownerUserId": "u1", "mode": "live",
		"callerSuppliedPayload": true, "stepOrder": []any{"a", "b", "publish"},
		"head": map[string]any{"a": map[string]any{"version": float64(1)}, "b": map[string]any{"version": float64(1)}, "publish": map[string]any{"version": float64(1)}},
	}
	steps := []map[string]any{
		{"key": "a", "status": "done", "attempt": float64(1), "version": float64(1), "result": map[string]any{"stepId": "a", "status": "success", "value": "A1"}},
		{"key": "b", "status": "done", "attempt": float64(1), "version": float64(1), "result": map[string]any{"stepId": "b", "status": "success", "value": "B1"}},
		{"key": "publish", "status": "done", "attempt": float64(1), "version": float64(1), "result": map[string]any{"stepId": "publish", "status": "success"}},
	}
	return run, steps
}

// rerunRequest is the run.rerun the person's act writes.
func rerunRequest(reason, stepKey string, override map[string]any) map[string]any {
	req := map[string]any{"requestId": "req-" + stepKey, "reason": reason, "stepKey": stepKey, "requestedBy": "u1", "requestedAt": "2026-09-26T10:00:00Z"}
	if override != nil {
		req["override"] = override
	}
	return req
}

// runAgain resumes a run carrying a re-run request the way the dispatcher
// does: PrepareRerun over the loaded journals, then ResumeFrom, on a context
// that names the run as workExecutionContext does. It answers the probe, the
// journal's writes and the execution.
func runAgain(t *testing.T, run map[string]any, steps []map[string]any, sources ...*RunJournal) (*runContextProbe, *journalRecorder, *AutomationExecution) {
	t.Helper()
	probe := newRunContextProbe()
	probe.answers["fetch"] = memql.NewResultWithOutput("A-live")
	probe.answers["draft"] = memql.NewResultWithOutput("B-live")
	rec, exec, err := runAgainWith(t, probe, run, steps, sources...)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	return probe, rec, exec
}

func runAgainWith(t *testing.T, probe StepExecutorRegistry, run map[string]any, steps []map[string]any, sources ...*RunJournal) (*journalRecorder, *AutomationExecution, error) {
	t.Helper()
	a := statementAutomation(t, rerunSource)
	j, err := runJournalFromRows(run, steps)
	if err != nil {
		t.Fatal(err)
	}
	resume, opts, err := PrepareRerun(j, sources, a)
	if err != nil {
		return nil, nil, err
	}
	rec := &journalRecorder{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	e.journal = newWorkJournal(rec, nil)
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: j.RunId, GoalId: j.GoalId, OwnerUserId: j.OwnerUserId, Mode: common.RunModeLive})
	exec, err := e.ResumeFrom(ctx, resume, a, opts)
	if err == nil && exec.Status != "completed" {
		err = errors.New(exec.Error)
	}
	return rec, exec, err
}

// intentOf is the createWorkStep a key's version opened, parsed.
func intentOf(t *testing.T, rec *journalRecorder, key string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, c := range rec.all() {
		name, args := argsOf(t, c)
		if name == "createWorkStep" && args["key"] == key {
			found = args
		}
	}
	if found == nil {
		t.Fatalf("no intent for %s in:\n%s", key, strings.Join(rec.all(), "\n"))
	}
	return found
}

// runWrites are the updateWorkRun calls, parsed, in the order written.
func runWrites(t *testing.T, rec *journalRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range rec.all() {
		if name, args := argsOf(t, c); name == "updateWorkRun" {
			out = append(out, args)
		}
	}
	return out
}

func stepVersion(args map[string]any) int {
	v, _ := args["version"].(float64)
	return int(v)
}

func TestARerunExecutesFromTheStepAsNewVersions(t *testing.T) {
	run, steps := finishedRun("r1")
	run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", map[string]any{"level": "reasoning"}), []any{"b", "publish"}
	probe, rec, exec := runAgain(t, run, steps)

	if got := probe.callees(); !reflect.DeepEqual(got, []string{"draft", "publish"}) {
		t.Fatalf("calls %v, want draft then publish: the re-run executes from the step it targets", got)
	}
	for _, key := range []string{"b", "publish"} {
		intent := intentOf(t, rec, key)
		if stepVersion(intent) != 2 || intent["attempt"] != float64(2) {
			t.Errorf("%s ran as version %v (attempt %v), want 2: one past the highest it recorded", key, intent["version"], intent["attempt"])
		}
		if want := "r1:" + key + ":2"; intent["idempotencyKey"] != want {
			t.Errorf("%s idempotency key %v, want %s -- a re-run must never reuse the key a side effect already ran under", key, intent["idempotencyKey"], want)
		}
	}
	if exec.ID != "r1" {
		t.Fatalf("the re-run ran as %s, want the run's own id r1", exec.ID)
	}
}

func TestARerunNeverReExecutesThePrefix(t *testing.T) {
	run, steps := finishedRun("r1")
	run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", nil), []any{"b", "publish"}
	probe, rec, _ := runAgain(t, run, steps)

	for _, callee := range probe.callees() {
		if callee == "fetch" {
			t.Fatal("fetch ran again: the step before the re-run's target is served from what it recorded")
		}
	}
	if from := probe.argsOf(t, "draft", 0)["from"]; from != "A1" {
		t.Fatalf("draft(from: %v), want A1 -- the value version 1 of a recorded", from)
	}
	for _, c := range rec.all() {
		if name, args := argsOf(t, c); name == "createWorkStep" && args["key"] == "a" {
			t.Fatalf("the served step wrote a row: %s", c)
		}
	}
}

func TestTheOverrideReachesOnlyTheTargetedStep(t *testing.T) {
	run, steps := finishedRun("r1")
	override := map[string]any{"level": "reasoning", "model": "fleet:qwen3", "effort": "high", "prompt": "Name the regions.",
		"guidance": map[string]any{"axes": map[string]any{"product": true, "process": false, "performance": true}, "reason": "The totals are missing.", "feedbackId": "v1:work:observation:f1"}}
	req := rerunRequest(RerunReasonRerun, "b", override)
	req["snapshot"] = map[string]any{"files": []any{map[string]any{"path": "report.md", "fileId": "v1:library:file:f9"}}, "unrecordedCommands": float64(1)}
	run["status"], run["rerun"], run["staleSteps"] = "running", req, []any{"b", "publish"}
	probe, _, _ := runAgain(t, run, steps)

	draft := probe.runOf(t, "draft")
	want := &common.StepOverride{Level: "reasoning", Model: "fleet:qwen3", Effort: "high", Prompt: "Name the regions.",
		GuidanceAxes: []string{"product", "performance"}, GuidanceReason: "The totals are missing.", FeedbackId: "v1:work:observation:f1"}
	if !reflect.DeepEqual(draft.Override, want) {
		t.Fatalf("the targeted step's override = %+v, want %+v", draft.Override, want)
	}
	if draft.Snapshot == nil || len(draft.Snapshot.Files) != 1 || draft.Snapshot.Files[0] != (common.SnapshotFile{Path: "report.md", FileId: "v1:library:file:f9"}) {
		t.Fatalf("the targeted step's snapshot = %+v", draft.Snapshot)
	}
	if draft.StepKey != "b" || draft.RunId != "r1" || draft.GoalId != "v1:work:goal:g1" {
		t.Fatalf("the targeted step lost its run: %+v", draft)
	}
	publish := probe.runOf(t, "publish")
	if publish.Override != nil || publish.Snapshot != nil {
		t.Fatalf("the override leaked into the next step: %+v", publish)
	}
}

// A context an earlier frame left an override on cannot carry it into a step
// the request does not target, and a step INSIDE the targeted one -- a loop
// body, a branch -- is part of its version and does carry it.
func TestAnOverrideIsClearedOffEveryStepTheRequestDoesNotTarget(t *testing.T) {
	spec := &RerunSpec{Reason: RerunReasonRerun, StepKey: "draft", Override: &common.StepOverride{Level: "strong"}, Snapshot: &common.WorkspaceSnapshot{}, Workspace: "ws-1"}
	exec := &AutomationExecution{ID: "r1", rerun: spec}
	e := NewExecutor(ExecutorOptions{})
	leaked := common.ContextWithRun(context.Background(), common.RunContext{RunId: "r1", Override: &common.StepOverride{Level: "reasoning"}, Snapshot: &common.WorkspaceSnapshot{}})

	next, _ := common.RunFromContext(e.withRunContext(leaked, &StepContext{Execution: exec}, &Step{ID: "publish"}))
	if next.Override != nil || next.Snapshot != nil || next.Workspace != "ws-1" {
		t.Fatalf("a step the request does not target: %+v", next)
	}
	nested, _ := common.RunFromContext(e.withRunContext(withListKey(leaked, "draft/0"), &StepContext{Execution: exec}, &Step{ID: "touch"}))
	if nested.StepKey != "draft/0/touch" || nested.Override != spec.Override {
		t.Fatalf("a step inside the targeted one: %+v", nested)
	}
	// An execution serving no re-run changes none of the three.
	plain, _ := common.RunFromContext(e.withRunContext(leaked, &StepContext{Execution: &AutomationExecution{ID: "r1"}}, &Step{ID: "publish"}))
	if plain.Override == nil || plain.Workspace != "" {
		t.Fatalf("an execution serving no re-run touched the override: %+v", plain)
	}
}

func TestTheWorkspaceReachesEveryStepOfTheRerun(t *testing.T) {
	run, steps := finishedRun("r1")
	req := rerunRequest(RerunReasonRerun, "b", map[string]any{"prompt": "Again."})
	req["workspace"] = "r1-v2"
	run["status"], run["rerun"], run["staleSteps"] = "running", req, []any{"b", "publish"}
	probe, _, _ := runAgain(t, run, steps)

	for _, callee := range []string{"draft", "publish"} {
		if ws := probe.runOf(t, callee).Workspace; ws != "r1-v2" {
			t.Errorf("%s ran in workspace %q, want the request's r1-v2", callee, ws)
		}
	}
}

func TestANewVersionResetsTheOldResult(t *testing.T) {
	run, steps := finishedRun("r1")
	run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", map[string]any{"prompt": "Shorter.", "requestedBy": "u1"}), []any{"b", "publish"}
	_, rec, _ := runAgain(t, run, steps)

	resets := map[string]any{"result": map[string]any{}, "resultFingerprint": "", "binding": map[string]any{}, "errorCode": "", "errorMessage": "", "childRunId": ""}
	for _, key := range []string{"b", "publish"} {
		intent := intentOf(t, rec, key)
		for field, empty := range resets {
			if got, ok := intent[field]; !ok || !reflect.DeepEqual(got, empty) {
				t.Errorf("%s version 2's intent names %s = %v (present %v), want %v -- an unnamed field keeps version 1's value", key, field, got, ok, empty)
			}
		}
	}
	b := intentOf(t, rec, "b")
	if !reflect.DeepEqual(b["override"], map[string]any{"prompt": "Shorter.", "requestedBy": "u1"}) || b["authoredBy"] != "u1" {
		t.Errorf("the targeted version records override %v and author %v, want the request's and u1", b["override"], b["authoredBy"])
	}
	publish := intentOf(t, rec, "publish")
	if !reflect.DeepEqual(publish["override"], map[string]any{}) || publish["authoredBy"] != "" {
		t.Errorf("a version nobody overrode records override %v and author %v, want {} and empty", publish["override"], publish["authoredBy"])
	}

	// A first version names none of them: an ordinary run writes nothing more
	// than it did.
	fresh := &journalRecorder{}
	e := NewExecutor(ExecutorOptions{StepRegistry: newStmtProbe()})
	e.journal = newWorkJournal(fresh, nil)
	if exec, err := e.ExecuteWithEvent(context.Background(), statementAutomation(t, rerunSource), "test", nil); err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	for _, key := range []string{"a", "b", "publish"} {
		intent := intentOf(t, fresh, key)
		for field := range resets {
			if _, named := intent[field]; named {
				t.Errorf("version 1 of %s names the reset field %s", key, field)
			}
		}
		if stepVersion(intent) != 1 {
			t.Errorf("version 1 of %s wrote version %v", key, intent["version"])
		}
	}
}

func TestAPristineBasisIsOmittedAndAReRunBasisIsWritten(t *testing.T) {
	fresh := &journalRecorder{}
	e := NewExecutor(ExecutorOptions{StepRegistry: newStmtProbe()})
	e.journal = newWorkJournal(fresh, nil)
	if exec, err := e.ExecuteWithEvent(context.Background(), statementAutomation(t, rerunSource), "test", nil); err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	for _, key := range []string{"a", "b", "publish"} {
		if basis, written := intentOf(t, fresh, key)["basis"]; written {
			t.Errorf("an ordinary run wrote a basis for %s (%v); every earlier step is version 1, so it is omitted", key, basis)
		}
	}

	run, steps := finishedRun("r1")
	run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", nil), []any{"b", "publish"}
	_, rec, _ := runAgain(t, run, steps)
	if basis, written := intentOf(t, rec, "b")["basis"]; written {
		t.Errorf("version 2 of b wrote basis %v; its upstream is still version 1 of a, which is pristine", basis)
	}
	want := map[string]any{"a": map[string]any{"version": float64(1)}, "b": map[string]any{"version": float64(2)}}
	if got := intentOf(t, rec, "publish")["basis"]; !reflect.DeepEqual(got, want) {
		t.Errorf("version 2 of publish wrote basis %v, want %v -- it ran against version 2 of b", got, want)
	}

	run, steps = finishedRun("r1")
	run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "a", nil), []any{"a", "b", "publish"}
	_, rec, _ = runAgain(t, run, steps)
	if got := intentOf(t, rec, "b")["basis"]; !reflect.DeepEqual(got, map[string]any{"a": map[string]any{"version": float64(2)}}) {
		t.Errorf("re-running a: b's basis = %v, want {a: version 2}", got)
	}
}

func TestEveryReceiptWritesTheWholeHead(t *testing.T) {
	run, steps := finishedRun("r1")
	run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", nil), []any{"b", "publish"}
	_, rec, _ := runAgain(t, run, steps)

	head := func(pairs ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(pairs); i += 2 {
			m[pairs[i].(string)] = map[string]any{"version": float64(pairs[i+1].(int))}
		}
		return m
	}
	var heads []any
	for _, w := range runWrites(t, rec) {
		if h, ok := w["head"]; ok {
			heads = append(heads, h)
		}
	}
	want := []any{
		head("a", 1, "b", 2, "publish", 1), // b's receipt: a is served, and still in the head
		head("a", 1, "b", 2, "publish", 2), // publish's receipt
		head("a", 1, "b", 2, "publish", 2), // the close
	}
	if !reflect.DeepEqual(heads, want) {
		t.Fatalf("heads written %v, want %v -- the row's merge is shallow, so each write carries every entry", heads, want)
	}
}

func TestTheRerunRequestIsClearedWhenTheRunCloses(t *testing.T) {
	check := func(t *testing.T, rec *journalRecorder, wantStatus string) {
		t.Helper()
		writes := runWrites(t, rec)
		last := writes[len(writes)-1]
		if last["status"] != wantStatus {
			t.Fatalf("the last run write is %v, want the close at %s", last, wantStatus)
		}
		if !reflect.DeepEqual(last["rerun"], map[string]any{}) || !reflect.DeepEqual(last["staleSteps"], []any{}) {
			t.Fatalf("the close wrote rerun %v and staleSteps %v, want {} and []", last["rerun"], last["staleSteps"])
		}
		for _, w := range writes[:len(writes)-1] {
			if _, cleared := w["rerun"]; cleared {
				t.Fatalf("a write before the close cleared the request: %v", w)
			}
		}
	}

	t.Run("succeeded", func(t *testing.T) {
		run, steps := finishedRun("r1")
		run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", nil), []any{"b", "publish"}
		_, rec, _ := runAgain(t, run, steps)
		check(t, rec, "succeeded")
		// Before the close, each receipt takes its step off the stale list.
		writes := runWrites(t, rec)
		if got := writes[1]["staleSteps"]; !reflect.DeepEqual(got, []any{"publish"}) {
			t.Errorf("b's receipt left staleSteps %v, want [publish]", got)
		}
	})
	t.Run("failed", func(t *testing.T) {
		run, steps := finishedRun("r1")
		run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", nil), []any{"b", "publish"}
		probe := newRunContextProbe()
		probe.fails["publish"] = -1
		rec, _, err := runAgainWith(t, probe, run, steps)
		if err == nil {
			t.Fatal("the re-run succeeded; publish was told to fail")
		}
		check(t, rec, "failed")
	})
}

func TestAnInterruptedRerunResumesWithItsOverride(t *testing.T) {
	t.Run("the targeted step had not finished", func(t *testing.T) {
		// Version 2 of b was written at running and its node died.
		run, steps := finishedRun("r1")
		run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", map[string]any{"level": "reasoning"}), []any{"b", "publish"}
		steps[1] = map[string]any{"key": "b", "status": "running", "attempt": float64(2), "version": float64(2)}
		probe, rec, _ := runAgain(t, run, steps)

		if got := probe.callees(); !reflect.DeepEqual(got, []string{"draft", "publish"}) {
			t.Fatalf("calls %v, want the interrupted step and what follows it", got)
		}
		if o := probe.runOf(t, "draft").Override; o == nil || o.Level != "reasoning" {
			t.Fatalf("the interrupted targeted step resumed without its override: %+v", o)
		}
		if probe.runOf(t, "publish").Override != nil {
			t.Fatal("the override reached the step after the targeted one")
		}
		if v := stepVersion(intentOf(t, rec, "b")); v != 3 {
			t.Errorf("b resumed as version %d, want 3: version 2 never finished and keeps its number", v)
		}
	})
	t.Run("the targeted step had finished", func(t *testing.T) {
		// b's version 2 finished and left the stale list; publish's version 2
		// was running when the node died.
		run, steps := finishedRun("r1")
		run["status"], run["rerun"], run["staleSteps"] = "running", rerunRequest(RerunReasonRerun, "b", map[string]any{"level": "reasoning"}), []any{"publish"}
		steps[1] = map[string]any{"key": "b", "status": "done", "attempt": float64(2), "version": float64(2), "result": map[string]any{"stepId": "b", "status": "success", "value": "B2"}}
		steps[2] = map[string]any{"key": "publish", "status": "running", "attempt": float64(2), "version": float64(2)}
		probe, rec, _ := runAgain(t, run, steps)

		if got := probe.callees(); !reflect.DeepEqual(got, []string{"publish"}) {
			t.Fatalf("calls %v, want only the interrupted step", got)
		}
		if text := probe.argsOf(t, "publish", 0)["text"]; text != "B2" {
			t.Fatalf("publish(text: %v), want B2 -- the version the request already produced", text)
		}
		if probe.runOf(t, "publish").Override != nil {
			t.Fatal("the override reached a step it does not target")
		}
		if v := stepVersion(intentOf(t, rec, "publish")); v != 3 {
			t.Errorf("publish resumed as version %d, want 3", v)
		}
	})
}

// After a head move re-asserted an earlier version, the newest row no longer
// carries the highest version number; the plan's numbers keep a re-run from
// reusing one.
func TestARerunAfterAHeadMoveTakesThePlansVersions(t *testing.T) {
	run, steps := finishedRun("r1")
	req := rerunRequest(RerunReasonRerun, "b", nil)
	req["versions"] = map[string]any{"b": float64(3), "publish": float64(3)}
	run["status"], run["rerun"], run["staleSteps"] = "running", req, []any{"b", "publish"}
	_, rec, _ := runAgain(t, run, steps)
	for _, key := range []string{"b", "publish"} {
		if v := stepVersion(intentOf(t, rec, key)); v != 3 {
			t.Errorf("%s ran as version %d, want the plan's 3", key, v)
		}
	}
}

func TestAForkRehydratesThePrefixFromTheSourceAndRunsTheForkStepLive(t *testing.T) {
	srcRun, srcSteps := finishedRun("src1")
	source, err := runJournalFromRows(srcRun, srcSteps)
	if err != nil {
		t.Fatal(err)
	}
	fork := map[string]any{
		"id": "v1:work:run:fork1", "automationName": "drafts", "status": "running",
		"goalId": "v1:work:goal:g1", "ownerUserId": "u1", "mode": "fork",
		"forkedFromRunId": "src1", "forkAtStepKey": "b", "triggeredBy": "branch:src1",
		"head":  map[string]any{"a": map[string]any{"version": float64(1), "runId": "src1"}},
		"rerun": rerunRequest(RerunReasonBranch, "b", map[string]any{"level": "strong", "prompt": "Use last quarter."}),
	}
	probe, rec, exec := runAgain(t, fork, nil, source)

	if got := probe.callees(); !reflect.DeepEqual(got, []string{"draft", "publish"}) {
		t.Fatalf("calls %v, want the fork step and what follows: the prefix never executes in a branch", got)
	}
	if from := probe.argsOf(t, "draft", 0)["from"]; from != "A1" {
		t.Fatalf("draft(from: %v), want A1 -- the source's recorded value, served by reference", from)
	}
	if o := probe.runOf(t, "draft").Override; o == nil || o.Prompt != "Use last quarter." {
		t.Fatalf("the fork step ran without the override: %+v", o)
	}
	if probe.runOf(t, "publish").Override != nil {
		t.Fatal("the override reached the step after the fork step")
	}
	if exec.ID != "fork1" {
		t.Fatalf("the branch ran as %s, want its own run fork1", exec.ID)
	}

	if got := rec.keys(); !reflect.DeepEqual(got, []string{"b", "publish"}) {
		t.Fatalf("step rows %v, want only the fork step and after", got)
	}
	b := intentOf(t, rec, "b")
	if b["runId"] != "fork1" || stepVersion(b) != 1 {
		t.Fatalf("the fork step's intent: %v", b)
	}
	if want := map[string]any{"a": map[string]any{"version": float64(1), "runId": "src1"}}; !reflect.DeepEqual(b["basis"], want) {
		t.Errorf("the fork step's basis = %v, want %v -- it ran against the source's version of a", b["basis"], want)
	}
	if !reflect.DeepEqual(b["override"], map[string]any{"level": "strong", "prompt": "Use last quarter."}) || b["authoredBy"] != "u1" {
		t.Errorf("the fork step records override %v and author %v", b["override"], b["authoredBy"])
	}
	if _, named := intentOf(t, rec, "publish")["override"]; named {
		t.Error("version 1 of the step after the fork step records an override; the override is the fork step's alone")
	}
	writes := runWrites(t, rec)
	wantHead := map[string]any{"a": map[string]any{"version": float64(1), "runId": "src1"}, "b": map[string]any{"version": float64(1)}, "publish": map[string]any{"version": float64(1)}}
	if got := writes[len(writes)-1]["head"]; !reflect.DeepEqual(got, wantHead) {
		t.Errorf("the branch closed with head %v, want %v", got, wantHead)
	}
}

// A branch serves a version by reference only while the source still holds
// it; serving what the source holds now would put under the branch an answer
// its upstream never computed.
func TestAForkRefusesAPrefixWhoseSourceMovedOn(t *testing.T) {
	srcRun, srcSteps := finishedRun("src1")
	srcSteps[0] = map[string]any{"key": "a", "status": "done", "attempt": float64(2), "version": float64(2), "result": map[string]any{"stepId": "a", "status": "success", "value": "A2"}}
	source, _ := runJournalFromRows(srcRun, srcSteps)
	fork := map[string]any{
		"id": "v1:work:run:fork1", "automationName": "drafts", "status": "running", "goalId": "v1:work:goal:g1", "ownerUserId": "u1",
		"mode": "fork", "forkedFromRunId": "src1", "forkAtStepKey": "b",
		"head":  map[string]any{"a": map[string]any{"version": float64(1), "runId": "src1"}},
		"rerun": rerunRequest(RerunReasonBranch, "b", nil),
	}
	probe := newRunContextProbe()
	if _, _, err := runAgainWith(t, probe, fork, nil, source); !errors.Is(err, ErrForkPrefixUnavailable) {
		t.Fatalf("err = %v, want ErrForkPrefixUnavailable", err)
	}
	if n := len(probe.callees()); n != 0 {
		t.Fatalf("%d step(s) ran for a refused branch", n)
	}
	// And the run holding the prefix must be loaded at all.
	if _, _, err := runAgainWith(t, newRunContextProbe(), fork, nil); !errors.Is(err, ErrForkPrefixUnavailable) {
		t.Fatalf("with no source loaded: err = %v, want ErrForkPrefixUnavailable", err)
	}
}

// Re-running a later step OF A BRANCH still serves the branch's shared prefix
// from the source: the branch holds no rows for it.
func TestARerunInsideABranchStillServesItsPrefixByReference(t *testing.T) {
	srcRun, srcSteps := finishedRun("src1")
	source, _ := runJournalFromRows(srcRun, srcSteps)
	fork := map[string]any{
		"id": "v1:work:run:fork1", "automationName": "drafts", "status": "running", "goalId": "v1:work:goal:g1", "ownerUserId": "u1",
		"mode": "fork", "forkedFromRunId": "src1", "forkAtStepKey": "b",
		"head":       map[string]any{"a": map[string]any{"version": float64(1), "runId": "src1"}, "b": map[string]any{"version": float64(1)}, "publish": map[string]any{"version": float64(1)}},
		"rerun":      rerunRequest(RerunReasonRerun, "publish", nil),
		"staleSteps": []any{"publish"},
	}
	forkSteps := []map[string]any{
		{"key": "b", "status": "done", "attempt": float64(1), "version": float64(1), "result": map[string]any{"stepId": "b", "status": "success", "value": "B-fork"}},
		{"key": "publish", "status": "done", "attempt": float64(1), "version": float64(1), "result": map[string]any{"stepId": "publish", "status": "success"}},
	}
	a := statementAutomation(t, rerunSource)
	j, _ := runJournalFromRows(fork, forkSteps)
	if got := RerunSources(j, a); !reflect.DeepEqual(got, []string{"src1"}) {
		t.Fatalf("RerunSources = %v, want [src1]", got)
	}
	probe, rec, _ := runAgain(t, fork, forkSteps, source)
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"publish"}) {
		t.Fatalf("calls %v, want publish alone", got)
	}
	if text := probe.argsOf(t, "publish", 0)["text"]; text != "B-fork" {
		t.Fatalf("publish(text: %v), want the branch's own b", text)
	}
	if v := stepVersion(intentOf(t, rec, "publish")); v != 2 {
		t.Errorf("publish ran as version %d, want 2", v)
	}
}

func TestARerunDecodesTheRequestOffTheRunRow(t *testing.T) {
	run, steps := finishedRun("r1")
	run["staleSteps"] = []any{"b", "publish"}
	run["rerun"] = map[string]any{
		"requestId": "req-9", "reason": "rerun", "stepKey": "b", "workspace": "r1-v2", "requestedBy": "u1",
		"override": map[string]any{"level": "fast", "inputs": map[string]any{"region": "EMEA"},
			"guidance": map[string]any{"axes": map[string]any{"performance": true, "product": true}, "reason": "Too slow."}},
		"snapshot": map[string]any{"files": []any{}, "unrecordedCommands": float64(0)},
		"versions": map[string]any{"b": float64(4), "publish": float64(2)},
	}
	steps[1]["attempt"], steps[1]["version"] = float64(1), float64(3)
	j, err := runJournalFromRows(run, steps)
	if err != nil {
		t.Fatal(err)
	}
	s := j.Rerun
	if s == nil || s.RequestId != "req-9" || s.Reason != RerunReasonRerun || s.StepKey != "b" || s.Workspace != "r1-v2" || s.RequestedBy != "u1" {
		t.Fatalf("request = %+v", s)
	}
	if s.Override == nil || s.Override.Level != "fast" || s.Override.Inputs["region"] != "EMEA" ||
		!reflect.DeepEqual(s.Override.GuidanceAxes, []string{"product", "performance"}) || s.Override.GuidanceReason != "Too slow." {
		t.Fatalf("override = %+v -- the axes are named in the framework's order", s.Override)
	}
	if s.Snapshot == nil || len(s.Snapshot.Files) != 0 {
		t.Fatalf("a snapshot naming no files is still a snapshot: %+v", s.Snapshot)
	}
	if !reflect.DeepEqual(s.Versions, map[string]int{"b": 4, "publish": 2}) {
		t.Fatalf("versions = %v", s.Versions)
	}
	if !reflect.DeepEqual(j.StaleSteps, []string{"b", "publish"}) || j.Head["b"].Version != 1 {
		t.Fatalf("stale %v, head %v", j.StaleSteps, j.Head)
	}
	if j.MaxAttempt["b"] != 3 || j.MaxAttempt["a"] != 1 {
		t.Fatalf("MaxAttempt = %v, want the higher of attempt and version", j.MaxAttempt)
	}

	run["rerun"] = map[string]any{}
	if j, _ := runJournalFromRows(run, steps); j.Rerun != nil {
		t.Fatalf("a cleared request decoded as %+v", j.Rerun)
	}
	delete(run, "rerun")
	if j, _ := runJournalFromRows(run, steps); j.Rerun != nil {
		t.Fatal("an absent request decoded as one")
	}
}

func TestARerunTargetMustBeOneOfTheRunsOwnSteps(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want error
	}{
		{"for_x/0/touch", work.ErrNestedStep},
		{"b/a", work.ErrNestedStep},
		{"nowhere", work.ErrStepNotInRun},
	} {
		run, steps := finishedRun("r1")
		run["status"], run["rerun"] = "running", rerunRequest(RerunReasonRerun, tc.key, nil)
		probe := newRunContextProbe()
		_, _, err := runAgainWith(t, probe, run, steps)
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrRerunStepInvalid) || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("target %q: err = %v, want %v naming the step", tc.key, err, tc.want)
		}
		if n := len(probe.callees()); n != 0 {
			t.Errorf("target %q: %d step(s) ran", tc.key, n)
		}
	}
	// ResumeFrom refuses it on its own, for a caller that skips PrepareRerun.
	a := statementAutomation(t, rerunSource)
	run, steps := finishedRun("r1")
	j, _ := runJournalFromRows(run, steps)
	_, err := NewExecutor(ExecutorOptions{StepRegistry: newStmtProbe()}).ResumeFrom(context.Background(), j, a, &ResumeOptions{Rerun: &RerunSpec{Reason: RerunReasonRerun, StepKey: "b/x"}})
	if !errors.Is(err, work.ErrNestedStep) {
		t.Fatalf("ResumeFrom with a nested target: %v", err)
	}
}

// A head move's re-run (D18) runs the first stale step and everything after
// it with nothing overridden: the versions the move made current are served.
func TestAHeadMoveRerunRunsTheStaleStepsWithNoOverride(t *testing.T) {
	run, steps := finishedRun("r1")
	run["status"], run["staleSteps"] = "running", []any{"publish"}
	run["rerun"] = map[string]any{"requestId": "req-hm", "reason": RerunReasonHeadMove, "stepKey": "publish", "override": map[string]any{}}
	probe, rec, _ := runAgain(t, run, steps)

	if got := probe.callees(); !reflect.DeepEqual(got, []string{"publish"}) {
		t.Fatalf("calls %v, want the stale step alone", got)
	}
	if o := probe.runOf(t, "publish").Override; o != nil {
		t.Fatalf("a head move's re-run carried an override: %+v", o)
	}
	publish := intentOf(t, rec, "publish")
	if stepVersion(publish) != 2 || !reflect.DeepEqual(publish["override"], map[string]any{}) || publish["authoredBy"] != "" {
		t.Fatalf("the stale step's new version: %v", publish)
	}
}
