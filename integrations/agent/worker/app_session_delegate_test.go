//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/planner"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/common"
)

type recordingExecutor struct {
	got  planner.ExecutorRequest
	runs int
	out  planner.ExecutorResult
	err  error
}

// admitSession admits: these tests are about what the delegate hands over, and
// the gate's own tests (app_gate_test.go) drive the real one.
func (r *recordingExecutor) admitSession(context.Context, string, memqlengine.AppDoorPin, string) *appGateRefusal {
	return nil
}

func (r *recordingExecutor) runAdmitted(_ context.Context, req planner.ExecutorRequest, _ planner.ProgressCallback) (planner.ExecutorResult, error) {
	r.runs++
	r.got = req
	return r.out, r.err
}

// recordingStamper captures the childRunId write without a database.
type recordingStamper struct {
	queries []string
	err     error
}

func (s *recordingStamper) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	s.queries = append(s.queries, q)
	return nil, s.err
}

// The handover becomes an ExecutorRequest carrying the STEP'S OWN identity,
// because an app session opened outside a step is a session nobody can trace
// back to the work it was doing.
func TestDelegateCarriesTheStepAndTheCallsOwnKnobs(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{
		Output: map[string]any{
			"sessionId":  "v1:worker:appSession:s1",
			"workerId":   "reg-laptop",
			"answer":     "worked",
			"transcript": "worked",
			"model":      "claude-sonnet-4-6",
			"effort":     "high",
			"app":        "claude-code",
		},
		Billing:     planner.BillingSubscription,
		TokensSpent: 1200,
		ArtifactIds: []string{"v1:library:artifact:a1"},
	}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)

	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AgentId: "ag1", AppId: "claude-code", Model: "claude-sonnet-4-6",
		Level: "reasoning", RunId: "v1:work:run:r1", StepId: "v1:work:step:s1",
		Prompt: "ship it",
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if ex.got.OwnerUserId != "u1" || ex.got.RunId != "v1:work:run:r1" || ex.got.StepId != "v1:work:step:s1" {
		t.Fatalf("the executor request must carry the step: %+v", ex.got)
	}
	if got := ex.got.Input["prompt"]; got != "ship it" {
		t.Fatalf("Input[prompt] = %v, want the handover's prompt", got)
	}
	if got := ex.got.Input["executorBackend"]; got != "cockpit-app:claude-code" {
		t.Fatalf("Input[executorBackend] = %v, want the app the door resolved", got)
	}
	if got := ex.got.Input["level"]; got != "reasoning" {
		t.Fatalf("Input[level] = %v, want the call's level", got)
	}
	if got := ex.got.Input["model"]; got != "claude-sonnet-4-6" {
		t.Fatalf("Input[model] = %v, want the policy's pin", got)
	}
	if out.Model != "claude-sonnet-4-6" || out.Effort != "high" {
		t.Fatalf("the app's reported model and effort must come back: %+v", out)
	}
	if out.Content != "worked" || len(out.ArtifactIds) != 1 {
		t.Fatalf("the step's result is the session's answer plus its artifacts: %+v", out)
	}
	if out.Billing != planner.BillingSubscription {
		t.Fatalf("Billing = %q", out.Billing)
	}
	if out.SessionId != "v1:worker:appSession:s1" {
		t.Fatalf("SessionId = %q", out.SessionId)
	}
	// The SURFACE distinguishes a whole step handed over from one turn inside
	// MemQL's own reasoning. One string for both would make the ledger unable
	// to tell them apart.
	if out.ExecutionSurface != "cockpit-app:claude-code@reg-laptop" {
		t.Fatalf("ExecutionSurface = %q", out.ExecutionSurface)
	}
}

// A structured answer comes back as RAW JSON beside the text answer, not
// instead of it: a harness can answer the schema and still exit non-zero, and
// a session that answered no schema still said something. The text answer is
// the app's own output, never the transcript, whose stderr carries the
// cockpit's diagnostics.
func TestDelegateKeepsBothTheAnswerAndTheTranscript(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{
		"sessionId":  "s",
		"answer":     "chatter",
		"transcript": "[memql] level strong runs claude-code with --model sonnet --effort high (the cockpit's built-in table)\nchatter",
		"result":     map[string]any{"ok": true},
	}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)

	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if len(out.Result) == 0 {
		t.Fatalf("the structured answer must come back as raw JSON")
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Result, &decoded); err != nil {
		t.Fatalf("Result is not JSON: %v (%q)", err, out.Result)
	}
	if decoded["ok"] != true {
		t.Fatalf("Result = %q", out.Result)
	}
	if out.Content != "chatter" {
		t.Fatalf("the text answer is the app's own output, got %q", out.Content)
	}
}

// A session with a structured answer and NO transcript still answers: the
// structured answer is what the caller gets rather than an empty string, which
// would read as a session that said nothing.
func TestDelegateFallsBackToTheStructuredAnswerForText(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{
		"sessionId": "s", "result": map[string]any{"answer": "42"},
	}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if !strings.Contains(out.Content, "42") {
		t.Fatalf("Content = %q, want the structured answer when there is no transcript", out.Content)
	}
}

// The response schema rides as JSON, and an ABSENT one is the empty string --
// which is what "no structured answer was asked for" means on AppSessionStart.
// A session that asked and got nothing is a different state, and only one of
// them can be disappointed.
func TestDelegateSendsTheSchemaOnlyWhenOneWasAskedFor(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)

	if _, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s",
	}); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if got := ex.got.Input["responseSchema"]; got != "" {
		t.Fatalf("responseSchema = %v, want empty when none was asked for", got)
	}

	if _, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s",
		ResponseSchema: &common.StructuredSchema{Name: "x", Schema: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if got := ex.got.Input["responseSchema"]; got != `{"type":"object"}` {
		t.Fatalf("responseSchema = %v, want the schema document", got)
	}
}

// AN EMPTY OWNER IS A REFUSAL, and it is made BEFORE the child run is opened:
// a run row written under a blank actor is readable by nobody, including the
// operator asking what ran on their machine.
func TestDelegateRefusesAnUnattributedStep(t *testing.T) {
	ex := &recordingExecutor{}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	_, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		AppId: "claude-code", RunId: "r", StepId: "s", Prompt: "go",
	})
	if err == nil || !strings.Contains(err.Error(), "no owner") {
		t.Fatalf("RunStep with no owner = %v, want a refusal naming the missing owner", err)
	}
	if ex.runs != 0 {
		t.Fatalf("nothing may run unattributed, got %d runs", ex.runs)
	}
}

// A node with no executor refuses as a WIRING fault. "This node cannot open
// sessions" and "no machine can run this app" need different fixes.
func TestDelegateRefusesWithNoExecutor(t *testing.T) {
	d := newAppSessionDelegateFor(nil, nil, nil, nil)
	_, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s",
	})
	if err == nil || !strings.Contains(err.Error(), "no cockpit-app executor wired") {
		t.Fatalf("RunStep with no executor = %v, want the wiring refusal", err)
	}
}

// A FAILED session still reports what it knows. The run happened on somebody's
// machine and spent their subscription; dropping the session id and the
// artifacts because the exit code was non-zero would lose the only evidence of
// what it did.
func TestDelegateReportsAFailedSession(t *testing.T) {
	ex := &recordingExecutor{
		out: planner.ExecutorResult{Output: map[string]any{
			"sessionId": "v1:worker:appSession:s9", "answer": "got halfway", "transcript": "got halfway",
		}, ArtifactIds: []string{"v1:library:artifact:a9"}},
		err: errors.New("cockpit-app: claude-code run failed: exit 1"),
	}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s", Prompt: "go",
	})
	if err == nil {
		t.Fatalf("a failed session must surface its error")
	}
	if out.SessionId != "v1:worker:appSession:s9" || len(out.ArtifactIds) != 1 {
		t.Fatalf("a failed session still reports what it produced: %+v", out)
	}
}

// With NO journal there is no child run, so the parent step is not stamped
// with an empty one. An empty childRunId written over an absent one would read
// as "a subrun that has no id", which is not a state.
func TestDelegateStampsNothingWithoutAChildRun(t *testing.T) {
	stamper := &recordingStamper{}
	d := newAppSessionDelegateFor(&recordingExecutor{out: planner.ExecutorResult{
		Output: map[string]any{"sessionId": "s"},
	}}, nil, stamper, nil)
	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "r", StepId: "s", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if out.ChildRunId != "" {
		t.Fatalf("ChildRunId = %q with no journal", out.ChildRunId)
	}
	if len(stamper.queries) != 0 {
		t.Fatalf("nothing may be stamped when there is no child run: %v", stamper.queries)
	}
}

// TestTheRecordingRunIsTheSubrunTheStepPointsAt (epic memql#5396).
//
// The delegate opens a subrun and stamps its id on the delegating step's
// childRunId. The session's ACTIONS must be recorded into that same run: a
// recording in a second run would leave the pointer a reader follows aimed at
// a run holding one step and no actions, which is the "points at nothing"
// failure this file's own header warns about.
func TestTheRecordingRunIsTheSubrunTheStepPointsAt(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{
		Output: map[string]any{"sessionId": "v1:worker:appSession:s1"},
	}}
	stamper := &recordingStamper{}
	var goalWritten bool
	journal := workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		if strings.HasPrefix(q, "mutation createWorkGoal(") {
			expr, err := langparser.ParseExpression(strings.TrimPrefix(q, "mutation "))
			if err != nil {
				t.Fatal(err)
			}
			if via := expr.(*langparser.FunctionCallExpr).Args["requestedVia"]; via != "agent" {
				t.Fatalf("delegated work must use the schema's agent origin, got %v", via)
			}
			goalWritten = true
		}
		return nil, nil
	}), nil, "node-a")
	d := newAppSessionDelegateFor(ex, journal, stamper, nil)

	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code",
		RunId: "v1:work:run:r1", StepId: "v1:work:step:s1", Prompt: "ship it",
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if out.ChildRunId == "" {
		t.Fatal("the delegate opened no child run, so this test measures nothing")
	}
	if !goalWritten {
		t.Fatal("the child run needs a durable goal")
	}
	if got := ex.got.Input["recordingRunId"]; got != out.ChildRunId {
		t.Fatalf("Input[recordingRunId] = %v, want the child run %q the step now points at",
			got, out.ChildRunId)
	}
	// And the step really does point at it, or the two facts would agree with
	// each other while both being wrong.
	var stamped bool
	for _, q := range stamper.queries {
		if strings.Contains(q, "childRunId") && strings.Contains(q, out.ChildRunId) {
			stamped = true
		}
	}
	if !stamped {
		t.Fatalf("childRunId was never stamped on the delegating step: %v", stamper.queries)
	}
}

// TestNoJournalLeavesTheRecordingRunToTheRecorder. A node whose journal is
// unwired still runs the session; the recorder then opens its own run rather
// than the actions going unrecorded.
func TestNoJournalLeavesTheRecordingRunToTheRecorder(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)

	if _, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", StepId: "v1:work:step:s1",
	}); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if got := ex.got.Input["recordingRunId"]; got != "" {
		t.Fatalf("Input[recordingRunId] = %v, want empty so the recorder opens one", got)
	}
}

// parentRunEngine answers the delegating run's read and records the actor it
// was asked under, so "the parent was read as its owner" is an observation.
type parentRunEngine struct {
	parent  map[string]any
	queries []string
	actors  []string
}

func (e *parentRunEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	e.actors = append(e.actors, actor)
	if strings.HasPrefix(q, "query workRunForOwner(") && e.parent != nil {
		return memqlengine.NewResultWithOutput([]any{e.parent}), nil
	}
	return nil, nil
}

// TestADelegatedSessionRunCarriesItsParentsGoalSignature (epic memql#5408, gap
// G2). The session is RECORDED into the child run the delegate opens, and
// procedure learning mines the recordings of one goal signature -- so the child
// must be born carrying its parent's signature, the parent's id and the
// parent's variables (the goal's input, which the lift maps parameters onto).
// A child without them belongs to no corpus, and the app's work on this goal
// could never become a procedure.
//
// The parent is read under its OWNER's actor, because the composite tier
// answers anybody else zero rows and no error. The control is a handover that
// names no parent run: nothing is read and none of the three is written.
func TestADelegatedSessionRunCarriesItsParentsGoalSignature(t *testing.T) {
	var opened []string
	journal := workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		if strings.HasPrefix(q, "mutation createWorkRun(") {
			opened = append(opened, q)
		}
		return nil, nil
	}), nil, "node-a")
	eng := &parentRunEngine{parent: map[string]any{
		"id":            "v1:work:run:r1",
		"goalSignature": "sig-of-the-goal",
		"variables":     map[string]any{"day": "2026-09-04"},
	}}
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	d := newAppSessionDelegateFor(ex, journal, eng, nil)

	if _, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code",
		RunId: "v1:work:run:r1", StepId: "v1:work:step:s1", Prompt: "ship it",
	}); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if len(opened) != 1 {
		t.Fatalf("expected one child run opened, got %d", len(opened))
	}
	for _, want := range []string{
		`goalSignature: "sig-of-the-goal"`,
		`parentRunId: "v1:work:run:r1"`,
		`variables: {"day":"2026-09-04"}`,
	} {
		if !strings.Contains(opened[0], want) {
			t.Errorf("the child run lacks %s:\n%s", want, opened[0])
		}
	}
	var readAsOwner bool
	for i, q := range eng.queries {
		if strings.HasPrefix(q, "query workRunForOwner(") && eng.actors[i] == "u1" {
			readAsOwner = true
		}
	}
	if !readAsOwner {
		t.Fatalf("the parent run was never read under its owner's actor: %v / %v", eng.queries, eng.actors)
	}

	// The control: no parent run named, nothing inherited.
	opened = nil
	control := &parentRunEngine{parent: eng.parent}
	d = newAppSessionDelegateFor(ex, journal, control, nil)
	if _, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", StepId: "v1:work:step:s2", Prompt: "go",
	}); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	for _, q := range control.queries {
		if strings.HasPrefix(q, "query workRunForOwner(") {
			t.Fatalf("a handover with no parent run read one anyway: %s", q)
		}
	}
	if len(opened) != 1 {
		t.Fatalf("expected one child run opened, got %d", len(opened))
	}
	for _, absent := range []string{"goalSignature", "parentRunId", "variables"} {
		if strings.Contains(opened[0], absent+":") {
			t.Errorf("a child with no parent wrote %s anyway:\n%s", absent, opened[0])
		}
	}
}

// discardingJournal is a journal whose writes go nowhere, for a test that
// needs a child run opened and does not read what the journal wrote.
func discardingJournal() *workjournal.Journal {
	return workjournal.New(workjournal.ExecutorFunc(func(context.Context, string) (any, error) {
		return nil, nil
	}), nil, "node-a")
}

// A handover names the run and the step KEY, and the step ROW is the
// journal's composition of the two. The subrun must be stamped on that row:
// updateWorkStep on the key names no row, which is how every delegated step
// used to keep no childRunId (epic memql#5414).
func TestTheDelegateStampsTheRealStepRow(t *testing.T) {
	stamper := &recordingStamper{}
	d := newAppSessionDelegateFor(&recordingExecutor{out: planner.ExecutorResult{
		Output: map[string]any{"sessionId": "s"},
	}}, discardingJournal(), stamper, nil)

	out, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code",
		RunId: "v1:work:run:run-1", StepId: "layer0.sales", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if out.ChildRunId == "" {
		t.Fatal("no child run was opened, so there was nothing to stamp and this test measures nothing")
	}
	row := automations.WorkStepId("v1:work:run:run-1", "layer0.sales")
	if row != "run-1-layer0-sales" {
		t.Fatalf("the journal's step id is %q; the fixture assumes run-1-layer0-sales", row)
	}
	want := `mutation updateWorkStep(stepId: "run-1-layer0-sales", childRunId: "` + out.ChildRunId + `")`
	var stamped bool
	for _, q := range stamper.queries {
		if q == want {
			stamped = true
		}
		if strings.Contains(q, `stepId: "layer0.sales"`) {
			t.Fatalf("the stamp targeted the step KEY, which names no row: %s", q)
		}
	}
	if !stamped {
		t.Fatalf("the step row was never stamped; want %s among %v", want, stamper.queries)
	}
}

// createdGoals records the goal id of every createWorkGoal a journal is asked
// to write.
func createdGoals(goals *[]string) *workjournal.Journal {
	return workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		if strings.HasPrefix(q, "mutation createWorkGoal(") {
			_, rest, _ := strings.Cut(q, `goalId: "`)
			id, _, _ := strings.Cut(rest, `"`)
			*goals = append(*goals, id)
		}
		return nil, nil
	}), nil, "node-a")
}

// The child goal is keyed on the step ROW. Keyed on the key alone, two runs
// that each have a step called `draft` put their sessions into one goal; a
// retried step of ONE run is still one goal with two runs.
func TestTwoRunsWithTheSameStepKeyGetDifferentChildGoals(t *testing.T) {
	var goals []string
	d := newAppSessionDelegateFor(&recordingExecutor{out: planner.ExecutorResult{
		Output: map[string]any{"sessionId": "s"},
	}}, createdGoals(&goals), nil, nil)

	for _, runId := range []string{"v1:work:run:r1", "v1:work:run:r2", "v1:work:run:r1"} {
		if _, err := d.RunStep(context.Background(), memqlengine.AppSessionHandover{
			ActingUserId: "u1", AppId: "claude-code", RunId: runId, StepId: "draft", Prompt: "go",
		}); err != nil {
			t.Fatalf("RunStep(%s): %v", runId, err)
		}
	}
	if len(goals) != 3 || goals[0] == "" {
		t.Fatalf("goals = %v, want one goal written per handover", goals)
	}
	if goals[0] == goals[1] {
		t.Fatalf("two runs' `draft` steps share child goal %s", goals[0])
	}
	if goals[0] != goals[2] {
		t.Fatalf("the same step of the same run opened a second goal (%s, then %s); a retried step is one goal", goals[0], goals[2])
	}
}

// overriddenSessionStep is the run context the executor gives the ONE step a
// re-run targets.
func overriddenSessionStep(ov *common.StepOverride, snapshot *common.WorkspaceSnapshot) context.Context {
	return common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "revise", OwnerUserId: "u1",
		Mode: common.RunModeLive, Override: ov, Snapshot: snapshot, Workspace: "r1-v2",
	})
}

// A session step's prompt is the text recorded on v1:worker:appSession.prompt,
// which is what the person saw and edited, so their prompt REPLACES the whole
// session prompt -- the flattened conversation the door handed over goes -- and
// the guidance follows it once. The level, model and effort reach the spec the
// cockpit is sent, the model as the part after `app:<id>:`.
func TestASessionOverrideReplacesThePromptAndSetsTheKnobs(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	handover := memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "v1:work:run:r1", StepId: "revise", Level: "strong",
		Prompt: "[system]\nYou are the reviser.\n\nRevise the report.",
	}
	ov := &common.StepOverride{
		Prompt: "Revise report.md and put the totals in bold.", WholePrompt: true,
		Level: "reasoning", Model: "app:claude-code:opus", Effort: "xhigh",
		GuidanceAxes: []string{"product"}, GuidanceReason: "the totals are missing",
	}
	if _, err := d.RunStep(overriddenSessionStep(ov, nil), handover); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	wantPrompt := "Revise report.md and put the totals in bold.\n\n" +
		"What was wrong with the previous version (product): the totals are missing"
	for key, want := range map[string]string{
		"prompt": wantPrompt, "level": "reasoning", "model": "opus", "effort": "xhigh", "freshWorkspace": "r1-v2",
	} {
		if got := ex.got.Input[key]; got != want {
			t.Errorf("Input[%s] = %v, want %q", key, got, want)
		}
	}
	spec := sessionRunSpec(ex.got, "claude-code", "u1", sessionWorkspace(ex.got, "/home/u1/work/"), 0)
	if spec.Prompt != wantPrompt || spec.Level != "reasoning" || spec.Model != "opus" || spec.Effort != "xhigh" {
		t.Fatalf("the cockpit would be sent prompt=%q level=%q model=%q effort=%q", spec.Prompt, spec.Level, spec.Model, spec.Effort)
	}
	if spec.Workspace != "/home/u1/work/r1-v2" {
		t.Fatalf("workspace = %q, want the re-run's fresh directory under the owner's root", spec.Workspace)
	}

	// Guidance with no new prompt is appended to the door's prompt -- once:
	// when the conversation the door flattened already carries it, it is not
	// said a second time.
	guidanceOnly := &common.StepOverride{GuidanceAxes: []string{"process"}, GuidanceReason: "it guessed the week"}
	line := "What was wrong with the previous version (process): it guessed the week"
	for _, prompt := range []string{"Revise the report.", "Revise the report.\n\n" + line} {
		handover.Prompt = prompt
		if _, err := d.RunStep(overriddenSessionStep(guidanceOnly, nil), handover); err != nil {
			t.Fatalf("RunStep: %v", err)
		}
		if got := ex.got.Input["prompt"]; got != "Revise the report.\n\n"+line {
			t.Fatalf("from %q the session got %q, want the guidance exactly once", prompt, got)
		}
	}

	// A level no step is re-run at refuses before anything runs on a machine.
	runs := ex.runs
	if _, err := d.RunStep(overriddenSessionStep(&common.StepOverride{Level: "embeddings"}, nil), handover); err == nil {
		t.Fatal("an embeddings override started a session")
	}
	if ex.runs != runs {
		t.Fatal("a refused override still ran the session")
	}

	// The control: a step nobody re-ran is started with the door's own values.
	handover.Prompt = "Revise the report."
	if _, err := d.RunStep(context.Background(), handover); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if ex.got.Input["prompt"] != "Revise the report." || ex.got.Input["level"] != "strong" ||
		ex.got.Input["effort"] != "" || ex.got.Input["freshWorkspace"] != "" {
		t.Fatalf("a step nobody re-ran was changed: %+v", ex.got.Input)
	}
}

func TestSessionWholePromptOverridePreservesApprovedNativeContinuation(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "resumed"}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	ov := &common.StepOverride{Prompt: "Revise the saved museum inventory and verify its totals.", WholePrompt: true, Model: "app:codex:test-model", Effort: "xhigh"}
	ctx := overriddenSessionStep(ov, nil)
	run, _ := common.RunFromContext(ctx)
	key, err := common.StepOverrideFingerprint(ov)
	if err != nil {
		t.Fatal(err)
	}
	run.Continuation = &common.StepContinuation{StepKey: "revise", ObservationID: "saved-checkpoint", ApprovalID: "approved-command", OverrideKey: key}
	ctx = common.ContextWithRun(ctx, run)
	// The engine has already restored the immutable checkpoint and current
	// recorded decision. WholePrompt belongs to the original rerun; applying
	// it again here used to erase this evidence at the last worker boundary.
	prompt := ov.Prompt + "\n\n[tool readInventory completed-read]\n287 specimens; recorded content hash unchanged.\n\n[assistant]\nPrepared inspect-inventory command, not executed.\n\n[Recorded answer for this suspended step]\nApproved exact inspect-inventory command once; permission is rechecked at dispatch.\n\n[MemQL runtime]\nEnd the session when awaiting_user."
	scope := &common.DelegatedTools{Run: run}
	h := memqlengine.AppSessionHandover{ActingUserId: "u1", AppId: "codex", RunId: run.RunId, StepId: run.StepKey, Prompt: prompt, ToolScope: scope}
	if _, err := d.RunStep(ctx, h); err != nil {
		t.Fatal(err)
	}
	spec := sessionRunSpec(ex.got, "codex", "u1", sessionWorkspace(ex.got, "/home/u1/work/"), 0)
	if spec.Prompt != prompt {
		t.Fatal("whole-prompt rerun override erased completed results, prepared operation or current approval before AppSessionStart")
	}
	if spec.Model != "test-model" || spec.Effort != "xhigh" || ex.got.Input["toolScope"] != scope {
		t.Fatal("continuation changed the selected model, effort or delegated authority")
	}
	for _, mode := range []string{"override", "step", "run", "owner", "checkpoint", "decision"} {
		bad := run
		proof := *run.Continuation
		bad.Continuation = &proof
		switch mode {
		case "override":
			proof.OverrideKey = "different"
		case "step":
			proof.StepKey = "another-step"
		case "run":
			bad.RunId = "another-run"
		case "owner":
			bad.OwnerUserId = "another-owner"
		case "checkpoint":
			proof.ObservationID = ""
		case "decision":
			proof.ApprovalID = ""
		}
		before := ex.runs
		if _, err := d.RunStep(common.ContextWithRun(ctx, bad), h); err == nil {
			t.Fatalf("%s mismatch launched a continuation", mode)
		}
		if ex.runs != before {
			t.Fatalf("%s mismatch reached the executor", mode)
		}
	}
	// A later explicit rerun without a native continuation still replaces the
	// whole prompt, as the person requested. Suspension is not a fresh rerun.
	run.Continuation = nil
	if _, err := d.RunStep(common.ContextWithRun(ctx, run), h); err != nil {
		t.Fatal(err)
	}
	if got := ex.got.Input["prompt"]; got != ov.Prompt {
		t.Fatal("a fresh whole-prompt rerun stopped honoring the explicit replacement")
	}
}

// INSTRUCTIONS ARE NOT A PROMPT. When the version being replaced was not a
// session -- a model answered it, or a learned procedure did -- the person
// wrote instructions to add to the step's own prompt, and a session handed
// only those would lose its goal. The handed-over prompt is kept, and the
// instructions are added exactly once: a tool loop has already put them in the
// conversation it flattened, a procedure's hand-back has not.
func TestInstructionsAreAddedToTheSessionPromptNeverSwappedForIt(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	ov := &common.StepOverride{Prompt: "Put the totals in bold."}
	instructions := memqlengine.StepOverrideInstructions(ov)
	for _, prompt := range []string{"Revise the report.", "Revise the report.\n\n" + instructions} {
		handover := memqlengine.AppSessionHandover{
			ActingUserId: "u1", AppId: "claude-code", RunId: "v1:work:run:r1", StepId: "revise", Prompt: prompt,
		}
		if _, err := d.RunStep(overriddenSessionStep(ov, nil), handover); err != nil {
			t.Fatalf("RunStep: %v", err)
		}
		if got := ex.got.Input["prompt"]; got != "Revise the report.\n\n"+instructions {
			t.Fatalf("from %q the session got %q, want the goal's prompt with the instructions once", prompt, got)
		}
	}
}

func TestSessionKeepsRecoveredOverrideBeforeNewerHumanGuidance(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	d := newAppSessionDelegateFor(ex, nil, nil, nil)
	ov := &common.StepOverride{Prompt: "Publish the revised document.", GuidanceReason: "The draft was unfinished."}
	instructions, priorFeedback := memqlengine.StepOverrideInstructions(ov), memqlengine.StepOverrideGuidance(ov)
	correction := "Set the revision aside and restore the original. Do not publish."
	// The native guidance reader restores the exact override at its recorded
	// position, even when its first occurrence was archived during compaction.
	prompt := strings.Join([]string{"Guidance from the person for this existing work:", instructions, priorFeedback, correction}, "\n\n")
	h := memqlengine.AppSessionHandover{ActingUserId: "u1", AppId: "claude-code", RunId: "v1:work:run:r1", StepId: "revise", Prompt: prompt}
	if _, err := d.RunStep(overriddenSessionStep(ov, nil), h); err != nil {
		t.Fatal(err)
	}
	got, _ := ex.got.Input["prompt"].(string)
	if got != prompt || strings.Count(got, instructions) != 1 || !strings.HasSuffix(got, correction) {
		t.Fatal("handoff renewed the older override after the human correction")
	}
}

// libraryEngine answers the owner-scoped Library reads a snapshot needs and
// records every query with the actor it ran under.
type libraryEngine struct {
	files     map[string]string // bare file id -> the file row's name
	artifacts map[string]string // bare file id -> its artifact's bare id
	queries   []string
	actors    []string
}

func (e *libraryEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	e.actors = append(e.actors, actor)
	for id, name := range e.files {
		if q == `query libraryFileById(fileId: "`+id+`")` {
			return memqlengine.NewResultWithOutput([]any{map[string]any{"id": "v1:library:file:" + id, "name": name}}), nil
		}
	}
	for id, artifact := range e.artifacts {
		if q == `query libraryArtifactBySourceConceptRef(sourceConceptRef: "v1:library:file:`+id+`")` {
			return memqlengine.NewResultWithOutput([]any{map[string]any{"id": "v1:library:artifact:" + artifact}}), nil
		}
	}
	return nil, nil
}

// A snapshot's files become the session's INPUTS as Library artifacts, the
// cockpit lands each flat under its file row's name, and the prompt opens by
// telling the app where each one belongs -- in a fresh workspace, so the new
// session does not run inside the tree later steps changed (design D19).
func TestASnapshotLandsAsInputsWithARestorePreamble(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	eng := &libraryEngine{
		files: map[string]string{
			"f1": "report.md.5e551011aaaa.ac7101111111",
			"f2": "sales.csv.5e551011aaaa.ac7102222222",
		},
		artifacts: map[string]string{"f1": "art-report", "f2": "art-sales"},
	}
	d := newAppSessionDelegateFor(ex, nil, eng, nil)
	snapshot := &common.WorkspaceSnapshot{Files: []common.SnapshotFile{
		{Path: "data/sales.csv", FileId: "v1:library:file:f2"},
		{Path: "report.md", FileId: "f1"},
	}}
	if _, err := d.RunStep(overriddenSessionStep(nil, snapshot), memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "v1:work:run:r1", StepId: "revise",
		Prompt: "Revise the report.", Inputs: []string{"art-brief"},
	}); err != nil {
		t.Fatalf("RunStep: %v", err)
	}

	wantInputs := []string{"art-brief", "art-sales", "art-report"}
	if strings.Join(ex.got.Inputs, ",") != strings.Join(wantInputs, ",") {
		t.Fatalf("inputs = %v, want the handover's own then the snapshot's artifacts %v", ex.got.Inputs, wantInputs)
	}
	wantPrompt := "Before you start, restore the workspace: each file below was delivered into the working directory " +
		"under the name on the left; move it to the path on the right (create directories as needed).\n" +
		"sales.csv.5e551011aaaa.ac7102222222 -> data/sales.csv\n" +
		"report.md.5e551011aaaa.ac7101111111 -> report.md\n\n" +
		"Revise the report."
	if got := ex.got.Input["prompt"]; got != wantPrompt {
		t.Fatalf("prompt = %q\nwant %q", got, wantPrompt)
	}
	spec := sessionRunSpec(ex.got, "claude-code", "u1", sessionWorkspace(ex.got, "/home/u1/work"), 0)
	if spec.Workspace != "/home/u1/work/r1-v2" || strings.Join(spec.Inputs, ",") != strings.Join(wantInputs, ",") || spec.Prompt != wantPrompt {
		t.Fatalf("the cockpit would be sent workspace=%q inputs=%v prompt=%q", spec.Workspace, spec.Inputs, spec.Prompt)
	}
	// Every Library read is the owner's: the Library is owner-tiered, and a
	// read as anybody else would answer zero rows.
	for i, q := range eng.queries {
		if strings.Contains(q, "library") && eng.actors[i] != "u1" {
			t.Fatalf("%s ran as %q, want the owner", q, eng.actors[i])
		}
	}
}

// A snapshot file with no artifact to hand the session REFUSES the step, naming
// the file -- a session restored from a partial snapshot diverges silently --
// and it refuses before anything is opened: no child run, no session.
func TestASnapshotFileWithNoArtifactRefusesTheStepNamingIt(t *testing.T) {
	ex := &recordingExecutor{}
	var goals []string
	eng := &libraryEngine{files: map[string]string{"f1": "report.md.5e551011aaaa.ac7101111111"}}
	d := newAppSessionDelegateFor(ex, createdGoals(&goals), eng, nil)
	d.snapshotWait, d.snapshotPoll = 30*time.Millisecond, 5*time.Millisecond
	snapshot := &common.WorkspaceSnapshot{Files: []common.SnapshotFile{{Path: "report.md", FileId: "f1"}}}
	handover := memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "v1:work:run:r1", StepId: "revise", Prompt: "Revise the report.",
	}

	_, err := d.RunStep(overriddenSessionStep(nil, snapshot), handover)
	if err == nil || !strings.Contains(err.Error(), "report.md") || !strings.Contains(err.Error(), "f1") {
		t.Fatalf("err = %v, want a refusal naming the file and its id", err)
	}
	if ex.runs != 0 || len(goals) != 0 {
		t.Fatalf("a refused snapshot still opened %d goal(s) and ran %d session(s)", len(goals), ex.runs)
	}

	// A snapshot with no fresh workspace to land in is refused as well.
	noWorkspace := common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "revise", OwnerUserId: "u1",
		Mode: common.RunModeLive, Snapshot: snapshot,
	})
	if _, err := d.RunStep(noWorkspace, handover); err == nil || !strings.Contains(err.Error(), "fresh workspace") {
		t.Fatalf("err = %v, want a refusal for a snapshot with no fresh workspace", err)
	}
	if ex.runs != 0 {
		t.Fatal("a snapshot with no fresh workspace still ran a session")
	}
}

// TestARepairRecordingDoesNotInheritTheReplaysProcedure (epic memql#5408). When
// a learned procedure serves a goal, compile lays procedureConstructId over the
// goal's input; when the replay diverges, the app takes the goal back in a
// session delegated from that same run. The recording inherits the goal's
// INPUT -- the lift maps parameters onto it and lists its keys as the goal's
// inputs -- so it must not inherit the replay's own variable, or the next lift
// would learn an input nobody gave. The control: a parent whose only variable
// is the replay's writes no variables at all.
func TestARepairRecordingDoesNotInheritTheReplaysProcedure(t *testing.T) {
	var opened []string
	journal := workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		if strings.HasPrefix(q, "mutation createWorkRun(") {
			opened = append(opened, q)
		}
		return nil, nil
	}), nil, "node-a")
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s"}}}
	handover := memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code",
		RunId: "v1:work:run:r1", StepId: "v1:work:step:s1", Prompt: "finish it",
	}

	eng := &parentRunEngine{parent: map[string]any{
		"id":            "v1:work:run:r1",
		"goalSignature": "sig-of-the-goal",
		"variables":     map[string]any{"day": "2026-09-04", "procedureConstructId": "v1:authoring:construct:c1"},
	}}
	if _, err := newAppSessionDelegateFor(ex, journal, eng, nil).RunStep(context.Background(), handover); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if len(opened) != 1 {
		t.Fatalf("expected one child run opened, got %d", len(opened))
	}
	if !strings.Contains(opened[0], `variables: {"day":"2026-09-04"}`) {
		t.Errorf("the recording lost the goal's input:\n%s", opened[0])
	}
	if strings.Contains(opened[0], "procedureConstructId") {
		t.Errorf("the recording inherited the replay's procedure:\n%s", opened[0])
	}

	opened = nil
	only := &parentRunEngine{parent: map[string]any{
		"id":            "v1:work:run:r1",
		"goalSignature": "sig-of-the-goal",
		"variables":     map[string]any{"procedureConstructId": "v1:authoring:construct:c1"},
	}}
	if _, err := newAppSessionDelegateFor(ex, journal, only, nil).RunStep(context.Background(), handover); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if len(opened) != 1 {
		t.Fatalf("expected one child run opened, got %d", len(opened))
	}
	if strings.Contains(opened[0], "variables:") {
		t.Errorf("a parent whose only variable is the replay's wrote variables anyway:\n%s", opened[0])
	}
}

// TestAppSessionStopsWhenItsRunIsCancelled. Cancelling a goal flags its runs,
// and a run notices at its next step boundary -- but a step handed to an app is
// ONE step, and a session can run for an hour, so its boundary is the end of
// the session. While the session runs, its run's flag is read every five
// seconds with the read a boundary makes, and a set flag stops the session: the
// context it was opened under is cancelled, which is how the runner asks the
// machine to stop.
func TestAppSessionStopsWhenItsRunIsCancelled(t *testing.T) {
	handover := memqlengine.AppSessionHandover{
		ActingUserId: "u1", AppId: "claude-code", RunId: "v1:work:run:r1", StepId: "composeEmail", Prompt: "write the email",
	}

	t.Run("a cancelled run stops its session within six seconds", func(t *testing.T) {
		x := startCancelWatch(t, handover)
		if every := x.clock.waitTicker(t); every != 5*time.Second {
			t.Fatalf("the watcher reads its run every %v, want every 5s", every)
		}
		x.app.waitStarted(t)

		x.run.cancelled.Store(true)
		for second := 1; second <= 6; second++ {
			for taken := x.clock.advance(t, time.Second); taken > 0; taken-- {
				x.run.read(t)
			}
		}
		if at := x.app.waitStopped(t); at > 6*time.Second {
			t.Fatalf("the app saw its context cancelled %v after its run was, want within 6s", at)
		}

		end := x.ended(t)
		var stopped *memqlengine.WorkCancelledError
		if !errors.As(end.err, &stopped) || stopped.By != "u1" {
			t.Fatalf("RunStep = %v, want the person's Stop naming who asked", end.err)
		}
		if !errors.Is(end.err, context.Canceled) {
			t.Fatalf("RunStep = %v, want it still to read as a cancellation", end.err)
		}
		if end.out.SessionId != "v1:worker:appSession:s1" {
			t.Fatalf("a stopped session still reports what it was: %+v", end.out)
		}
		// THE SESSION'S RECORD SAYS WHY. Its child run and step close cancelled
		// -- failed would read as a fault for what was somebody's decision --
		// and the log names the run and who cancelled it.
		if run, step := x.writes.closed(); run != "cancelled" || step != "cancelled" {
			t.Fatalf("the session's child run closed %q and its step %q, want both cancelled", run, step)
		}
		if !x.log.has(slog.LevelInfo, "its run was cancelled", "run_id", "v1:work:run:r1", "cancelled_by", "u1") {
			t.Fatalf("nothing logged that the session stopped because its run was cancelled: %v", x.log.messages())
		}
		x.clock.requireWatcherGone(t)
	})

	t.Run("a run nobody cancels lets its session finish, and the watcher goes with it", func(t *testing.T) {
		x := startCancelWatch(t, handover)
		x.clock.waitTicker(t)
		x.app.waitStarted(t)

		reads := 0
		for second := 1; second <= 20; second++ {
			for taken := x.clock.advance(t, time.Second); taken > 0; taken-- {
				if x.run.read(t) {
					t.Fatal("the fixture's run answered cancelled")
				}
				reads++
			}
		}
		if reads != 4 {
			t.Fatalf("%d reads in 20 seconds, want one every 5 seconds", reads)
		}
		// The fourth tick was taken, so the third read was acted on: a session
		// nobody asked to stop is still running.
		if err := x.app.ctx.Err(); err != nil {
			t.Fatalf("the session's context was cancelled with nobody asking: %v", err)
		}
		close(x.app.finish)
		if end := x.ended(t); end.err != nil {
			t.Fatalf("RunStep: %v", end.err)
		}
		if run, step := x.writes.closed(); run != "succeeded" || step != "done" {
			t.Fatalf("the session's child run closed %q and its step %q, want succeeded and done", run, step)
		}
		x.clock.requireWatcherGone(t)
	})

	t.Run("a read that fails stops nothing and is asked again at the next tick", func(t *testing.T) {
		x := startCancelWatch(t, handover)
		x.clock.waitTicker(t)
		x.app.waitStarted(t)

		x.run.failing.Store(true)
		for round := 1; round <= 2; round++ {
			if taken := x.clock.advance(t, 5*time.Second); taken != 1 {
				t.Fatalf("round %d: the watcher took %d ticks, want 1 -- a failed read must not end the watch", round, taken)
			}
			x.run.read(t)
		}
		// The second tick was taken, so the first failed read was acted on.
		if err := x.app.ctx.Err(); err != nil {
			t.Fatalf("a failed read cancelled the session: %v", err)
		}
		if !x.log.has(slog.LevelDebug, "could not read whether its run was cancelled", "run_id", "v1:work:run:r1") {
			t.Fatalf("a failed read was not logged at debug: %v", x.log.messages())
		}

		x.run.failing.Store(false)
		x.run.cancelled.Store(true)
		if taken := x.clock.advance(t, 5*time.Second); taken != 1 {
			t.Fatalf("the watcher took %d ticks after two failed reads, want 1", taken)
		}
		if !x.run.read(t) {
			t.Fatal("the third read did not see the run cancelled")
		}
		x.app.waitStopped(t)
		var stopped *memqlengine.WorkCancelledError
		if end := x.ended(t); !errors.As(end.err, &stopped) {
			t.Fatalf("RunStep = %v, want the person's Stop", end.err)
		}
		x.clock.requireWatcherGone(t)
	})

	// A stop the app reports while its run is being stopped -- out of usage
	// partway, say -- would move the step to another app (app_takeover.go).
	// Once the run is stopped there is no step left to move.
	t.Run("a session its run stopped hands nothing on to another app", func(t *testing.T) {
		x := startCancelWatch(t, handover, func(x *cancelWatch) {
			x.app.stopErr = &memqlengine.AppStoppedMidWork{AppId: "claude-code", Kind: "out_of_usage", SessionId: "v1:worker:appSession:s1"}
		})
		x.clock.waitTicker(t)
		x.app.waitStarted(t)

		x.run.cancelled.Store(true)
		if taken := x.clock.advance(t, 5*time.Second); taken != 1 {
			t.Fatalf("the watcher took %d ticks, want 1", taken)
		}
		x.run.read(t)
		x.app.waitStopped(t)
		end := x.ended(t)
		var stopped *memqlengine.WorkCancelledError
		var moved *memqlengine.AppStoppedMidWork
		if !errors.As(end.err, &stopped) || !errors.Is(end.err, context.Canceled) || errors.As(end.err, &moved) {
			t.Fatalf("RunStep = %v, want only the person's Stop, read as a cancellation", end.err)
		}
	})

	t.Run("a session that belongs to no run starts no watcher", func(t *testing.T) {
		noRun := handover
		noRun.RunId = ""
		x := startCancelWatch(t, noRun)
		x.app.waitStarted(t)
		close(x.app.finish)
		if end := x.ended(t); end.err != nil {
			t.Fatalf("RunStep: %v", end.err)
		}
		if len(x.clock.made) != 0 || len(x.run.reads) != 0 {
			t.Fatalf("a session with no run started a watcher (%d tickers, %d reads)", len(x.clock.made), len(x.run.reads))
		}
	})
}

// cancelWatch is one RunStep against an app that runs until it is stopped or
// told to finish, a run whose cancel flag the test sets, and a clock the test
// moves.
type cancelWatch struct {
	clock  *cancelWatchClock
	run    *watchedRun
	app    *appUntilStopped
	log    *sessionLog
	writes *journalWrites
	done   chan sessionEnd
}

type sessionEnd struct {
	out memqlengine.AppSessionOutcome
	err error
}

// startCancelWatch starts the RunStep, after configure has set up anything the
// session must start with.
func startCancelWatch(t *testing.T, h memqlengine.AppSessionHandover, configure ...func(*cancelWatch)) *cancelWatch {
	t.Helper()
	clock := newCancelWatchClock()
	x := &cancelWatch{
		clock:  clock,
		run:    &watchedRun{runId: h.RunId, reads: make(chan bool, 64)},
		app:    &appUntilStopped{clock: clock, started: make(chan struct{}), finish: make(chan struct{}), stopped: make(chan time.Duration, 1)},
		log:    &sessionLog{},
		writes: &journalWrites{},
		done:   make(chan sessionEnd, 1),
	}
	for _, c := range configure {
		c(x)
	}
	d := newAppSessionDelegateFor(x.app, x.writes.journal(), x.run, slog.New(x.log))
	d.cancelTicker = clock.ticker
	go func() {
		out, err := d.RunStep(context.Background(), h)
		x.done <- sessionEnd{out, err}
	}()
	return x
}

func (x *cancelWatch) ended(t *testing.T) sessionEnd {
	t.Helper()
	select {
	case end := <-x.done:
		return end
	case <-time.After(5 * time.Second):
		t.Fatal("RunStep did not return once its session ended")
		return sessionEnd{}
	}
}

// cancelWatchClock is the watcher's clock in these tests: the tickers it makes
// fire only when the test moves it, so five seconds pass without anybody
// sleeping them.
type cancelWatchClock struct {
	mu      sync.Mutex
	now     time.Duration
	tickers []*cancelWatchTicker
	made    chan time.Duration
}

type cancelWatchTicker struct {
	every   time.Duration
	next    time.Duration
	c       chan time.Time
	stopped chan struct{}
}

func newCancelWatchClock() *cancelWatchClock {
	return &cancelWatchClock{made: make(chan time.Duration, 4)}
}

// ticker is the delegate's cancelTicker.
func (c *cancelWatchClock) ticker(every time.Duration) (<-chan time.Time, func()) {
	tk := &cancelWatchTicker{every: every, c: make(chan time.Time), stopped: make(chan struct{})}
	c.mu.Lock()
	tk.next = c.now + every
	c.tickers = append(c.tickers, tk)
	c.mu.Unlock()
	c.made <- every
	var once sync.Once
	return tk.c, func() { once.Do(func() { close(tk.stopped) }) }
}

func (c *cancelWatchClock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock on by d and hands every tick that falls due to the
// watcher, answering how many it took. A tick taken means the watcher has
// finished with the one before it; a stopped ticker takes none.
func (c *cancelWatchClock) advance(t *testing.T, d time.Duration) int {
	t.Helper()
	type due struct {
		tk *cancelWatchTicker
		at time.Duration
	}
	var ticks []due
	c.mu.Lock()
	c.now += d
	for _, tk := range c.tickers {
		for tk.next <= c.now {
			ticks = append(ticks, due{tk, tk.next})
			tk.next += tk.every
		}
	}
	c.mu.Unlock()
	taken := 0
	for _, tick := range ticks {
		select {
		case tick.tk.c <- time.Unix(0, 0).Add(tick.at):
			taken++
		case <-tick.tk.stopped:
		case <-time.After(5 * time.Second):
			t.Fatalf("the watcher did not take its tick at %v", tick.at)
		}
	}
	return taken
}

func (c *cancelWatchClock) waitTicker(t *testing.T) time.Duration {
	t.Helper()
	select {
	case every := <-c.made:
		return every
	case <-time.After(5 * time.Second):
		t.Fatal("the session started no cancel watcher")
		return 0
	}
}

// requireWatcherGone fails unless every ticker the clock made is stopped
// already: the watcher stops its ticker as it leaves, and RunStep must not
// return while it is still there.
func (c *cancelWatchClock) requireWatcherGone(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	tickers := append([]*cancelWatchTicker(nil), c.tickers...)
	c.mu.Unlock()
	if len(tickers) == 0 {
		t.Fatal("no watcher was started")
	}
	for _, tk := range tickers {
		select {
		case <-tk.stopped:
		default:
			t.Fatal("RunStep returned while its session's cancel watcher was still running")
		}
	}
}

// watchedRun is the session's run as its cancel flag is read: the journal's
// row for runId, which the test can flip to cancel-requested or make
// unreadable. It answers only the read a step boundary makes
// (automations' cancelRequested); every other query reads nothing.
type watchedRun struct {
	runId     string
	cancelled atomic.Bool
	failing   atomic.Bool
	// reads carries what each read of the flag answered, in order.
	reads chan bool
}

func (r *watchedRun) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if q != `query workRunById(runId: "`+r.runId+`")` {
		return nil, nil
	}
	if r.failing.Load() {
		r.reads <- false
		return nil, errors.New("the database is unavailable")
	}
	row := map[string]any{"id": r.runId, "status": "running", "cancelRequested": r.cancelled.Load()}
	if r.cancelled.Load() {
		row["cancelledBy"] = "u1"
	}
	r.reads <- r.cancelled.Load()
	return memqlengine.NewResultWithOutput([]any{row}), nil
}

// read waits for the watcher's next read of the run and answers whether the
// read saw it cancelled.
func (r *watchedRun) read(t *testing.T) bool {
	t.Helper()
	select {
	case asked := <-r.reads:
		return asked
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher took its tick and never read its run")
		return false
	}
}

// appUntilStopped stands in for the app session: it runs until the context
// the delegate opened it under is cancelled -- the runner's Wait then asks the
// machine to stop -- or until the test lets it finish.
type appUntilStopped struct {
	clock   *cancelWatchClock
	ctx     context.Context
	started chan struct{}
	finish  chan struct{}
	// stopped carries the clock's reading when the app saw its context
	// cancelled.
	stopped chan time.Duration
	// stopErr, when set, is what the app reports on being stopped, in place of
	// the runner's cancelled wait.
	stopErr error
}

func (a *appUntilStopped) admitSession(context.Context, string, memqlengine.AppDoorPin, string) *appGateRefusal {
	return nil
}

func (a *appUntilStopped) runAdmitted(ctx context.Context, _ planner.ExecutorRequest, _ planner.ProgressCallback) (planner.ExecutorResult, error) {
	a.ctx = ctx
	close(a.started)
	out := planner.ExecutorResult{Output: map[string]any{"sessionId": "v1:worker:appSession:s1"}}
	select {
	case <-ctx.Done():
		a.stopped <- a.clock.elapsed()
		if a.stopErr != nil {
			return out, fmt.Errorf("cockpit-app: claude-code run failed: %w", a.stopErr)
		}
		return out, fmt.Errorf("cockpit-app: claude-code run failed: %w", ctx.Err())
	case <-a.finish:
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("cockpit-app: claude-code run failed: %w", err)
		}
		return out, nil
	}
}

func (a *appUntilStopped) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-a.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the session never started")
	}
}

func (a *appUntilStopped) waitStopped(t *testing.T) time.Duration {
	t.Helper()
	select {
	case at := <-a.stopped:
		return at
	case <-time.After(5 * time.Second):
		t.Fatal("the app session is still running after its run was cancelled")
		return 0
	}
}

// closed answers the terminal status the child run and its step were written
// with, "" for one never closed.
func (j *journalWrites) closed() (run, step string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, q := range j.queries {
		for _, status := range []string{"succeeded", "failed", "cancelled"} {
			if strings.HasPrefix(q, "mutation updateWorkRun(") && strings.Contains(q, `status: "`+status+`"`) {
				run = status
			}
		}
		for _, status := range []string{"done", "failed", "cancelled"} {
			if strings.HasPrefix(q, "mutation updateWorkStep(") && strings.Contains(q, `status: "`+status+`"`) {
				step = status
			}
		}
	}
	return run, step
}

// sessionLog keeps the delegate's log records.
type sessionLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *sessionLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *sessionLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r.Clone())
	return nil
}

func (l *sessionLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *sessionLog) WithGroup(string) slog.Handler      { return l }

// has reports whether a record at level says words and carries each of the
// key, value pairs in attrs.
func (l *sessionLog) has(level slog.Level, words string, attrs ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.Level != level || !strings.Contains(r.Message, words) {
			continue
		}
		found := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			found[a.Key] = a.Value.String()
			return true
		})
		matched := true
		for i := 0; i+1 < len(attrs); i += 2 {
			if found[attrs[i]] != attrs[i+1] {
				matched = false
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (l *sessionLog) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.records))
	for _, r := range l.records {
		out = append(out, r.Level.String()+" "+r.Message)
	}
	return out
}
