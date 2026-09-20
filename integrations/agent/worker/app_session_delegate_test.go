//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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

func (r *recordingExecutor) Run(_ context.Context, req planner.ExecutorRequest, _ planner.ProgressCallback) (planner.ExecutorResult, error) {
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

// A structured answer comes back as RAW JSON beside the transcript, not
// instead of it: a harness can answer the schema and still exit non-zero, and
// a session that answered no schema still produced a transcript.
func TestDelegateKeepsBothTheAnswerAndTheTranscript(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{
		"sessionId": "s", "transcript": "chatter", "result": map[string]any{"ok": true},
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
		t.Fatalf("the transcript is still the text answer, got %q", out.Content)
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
			"sessionId": "v1:worker:appSession:s9", "transcript": "got halfway",
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
	journal := workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
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
