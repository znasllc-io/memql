package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	workstate "github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// session_test.go -- the app-session recording writer (epic memql#5396,
// task memql#5398).
//
// Every assertion here is about one of three things, and all three are
// SILENT when they break: the rows, the internal-origin stamp (without which
// every @serverOnly write is refused with one WARN nothing above it hears),
// and the borrowed owner actor (without which the row is written under a
// blank actor and readable by nobody, including the operator asking what an
// agent did on their machine).

const (
	testSessionId = "v1:worker:appSession:s1"
	testRunId     = "v1:work:run:r1"
	testOwner     = "v1:identity:user:alice"
)

func newSessionWriter(t *testing.T) (*SessionWriter, *recordingEngine) {
	t.Helper()
	eng := newRecordingEngine()
	w := NewSessionWriter(eng, testLogger())
	w.SetNow(func() time.Time { return testNow })
	return w, eng
}

func anAction(seq uint64, tool string) workerservice.ActionEvent {
	exit := 0
	return workerservice.ActionEvent{
		Id:           "toolu_" + tool,
		Seq:          seq,
		Tool:         tool,
		Args:         map[string]any{"command": "ls -la"},
		Cwd:          "/work",
		ExitCode:     &exit,
		ResultDigest: "sha256:abc",
		ResultType:   "string",
	}
}

// TestASessionOfNActionsWritesNStepsAndNObservations -- the epic's headline
// acceptance. Before this, one session was one step.
func TestASessionOfNActionsWritesNStepsAndNObservations(t *testing.T) {
	w, eng := newSessionWriter(t)
	ctx := context.Background()

	const n = 4
	for i := 0; i < n; i++ {
		err := w.RecordAction(ctx, workerservice.RecordedAction{
			SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
			Seq: i, Action: anAction(uint64(i+1), "exec"),
		})
		if err != nil {
			t.Fatalf("RecordAction %d: %v", i, err)
		}
	}

	steps := eng.callsTo("createWorkStep")
	obs := eng.callsTo("createWorkObservation")
	if len(steps) != n || len(obs) != n {
		t.Fatalf("got %d steps and %d observations, want %d of each (calls: %s)",
			len(steps), len(obs), n, eng.summary())
	}
	for i, c := range steps {
		args := c.Args(t)
		if got := args["seq"]; got != float64(i) {
			t.Errorf("step %d seq = %v, want %d -- the client orders by seq", i, got, i)
		}
		if got := args["stepType"]; got != "exec" {
			t.Errorf("step %d stepType = %v, want exec", i, got)
		}
		if got := args["runId"]; got != testRunId {
			t.Errorf("step %d runId = %v, want the recording run", i, got)
		}
	}
}

// TestEveryRecordingWriteBorrowsTheOwnerAndStampsInternalOrigin. Both are
// silent failures: an unstamped @serverOnly write is refused with a WARN, and
// an unowned row is readable by nobody.
func TestEveryRecordingWriteBorrowsTheOwnerAndStampsInternalOrigin(t *testing.T) {
	w, eng := newSessionWriter(t)
	ctx := context.Background()

	if err := w.RecordAction(ctx, workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 0, Action: anAction(1, "exec"),
	}); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	if err := w.RecordGap(ctx, workerservice.RecordedGap{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		AfterSeq: 1, BeforeSeq: 4, Missing: 2,
	}); err != nil {
		t.Fatalf("RecordGap: %v", err)
	}
	if err := w.CloseRecording(ctx, workerservice.RecordingClose{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 1, Status: "ended", RecordedActions: 1, DroppedActions: 2,
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}

	calls := eng.recorded()
	if len(calls) == 0 {
		t.Fatal("nothing was written at all")
	}
	for _, c := range calls {
		if c.Origin != auth.OriginInternal {
			t.Errorf("%s ran at origin %v, want internal -- a @serverOnly write on client "+
				"origin is refused with one WARN and the row is simply never written", c.Name(), c.Origin)
		}
		if c.Actor != testOwner {
			t.Errorf("%s ran as %q, want the session owner -- a row written under a blank actor "+
				"is readable by nobody", c.Name(), c.Actor)
		}
	}
}

// TestAGapWritesANoteObservation. The loss has to be visible: a procedure
// lifted from an incomplete recording would otherwise be lifted as if it were
// complete.
func TestAGapWritesANoteObservation(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.RecordGap(context.Background(), workerservice.RecordedGap{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		AfterSeq: 2, BeforeSeq: 5, Missing: 2,
	}); err != nil {
		t.Fatalf("RecordGap: %v", err)
	}
	c := eng.callTo(t, "createWorkObservation")
	args := c.Args(t)
	if args["kind"] != "note" {
		t.Errorf("kind = %v, want note", args["kind"])
	}
	content, _ := args["content"].(string)
	if !strings.Contains(content, "2") {
		t.Errorf("content = %q, want it to name how many actions are missing", content)
	}
	data, _ := args["data"].(map[string]any)
	if data["missing"] != float64(2) || data["afterSeq"] != float64(2) || data["beforeSeq"] != float64(5) {
		t.Errorf("data = %v, want the gap bracketed", data)
	}
}

// TestTheObservationCarriesTheActionVerbatim -- design D5, "actions verbatim".
func TestTheObservationCarriesTheActionVerbatim(t *testing.T) {
	w, eng := newSessionWriter(t)
	action := anAction(7, "fs_write")
	action.Args = map[string]any{"path": "/work/a.txt", "content": "hello"}
	action.IsError = true
	action.Error = "permission denied"

	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 3, Action: action, ContentRefs: []string{"v1:library:file:f1"},
		ContentOmitted: []string{"/work/big.bin: above the per-file cap"},
	}); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	data := eng.callTo(t, "createWorkObservation").Args(t)["data"].(map[string]any)
	for key, want := range map[string]any{
		"appActionId":  "toolu_fs_write",
		"sessionId":    testSessionId,
		"seq":          float64(7),
		"cwd":          "/work",
		"exitCode":     float64(0),
		"isError":      true,
		"resultDigest": "sha256:abc",
		"resultType":   "string",
	} {
		if data[key] != want {
			t.Errorf("data[%q] = %v, want %v", key, data[key], want)
		}
	}
	if refs, _ := data["contentRefs"].([]any); len(refs) != 1 || refs[0] != "v1:library:file:f1" {
		t.Errorf("contentRefs = %v", data["contentRefs"])
	}
	if om, _ := data["contentOmitted"].([]any); len(om) != 1 {
		t.Errorf("contentOmitted = %v", data["contentOmitted"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(data["args"].(string)), &args); err != nil {
		t.Fatalf("args did not round-trip as JSON: %v", err)
	}
	if args["content"] != "hello" || args["path"] != "/work/a.txt" {
		t.Errorf("args = %v, want the call verbatim", args)
	}
}

// TestAnAbsentExitCodeIsAbsentFromTheObservation. A file write has no exit
// code; writing 0 would report it as a command that succeeded.
func TestAnAbsentExitCodeIsAbsentFromTheObservation(t *testing.T) {
	w, eng := newSessionWriter(t)
	action := anAction(1, "fs_write")
	action.ExitCode = nil
	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 0, Action: action,
	}); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	data := eng.callTo(t, "createWorkObservation").Args(t)["data"].(map[string]any)
	if _, present := data["exitCode"]; present {
		t.Errorf("exitCode = %v, want the key ABSENT for an action that reported none", data["exitCode"])
	}
}

// TestTheFingerprintIsWrittenOnTheStepThatCarriesIt -- once, on the session's
// first step (design D16).
func TestTheFingerprintIsWrittenOnTheStepThatCarriesIt(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 0, Action: anAction(1, "exec"),
		Fingerprint: map[string]any{"cwdDigest": "d1"},
	}); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	c := eng.callTo(t, "updateWorkStep")
	fp, _ := c.Args(t)["fingerprint"].(map[string]any)
	if fp["cwdDigest"] != "d1" {
		t.Errorf("fingerprint = %v, want the environment on the receipt", c.Args(t)["fingerprint"])
	}
}

// TestCloseWritesTheAppAnswerStepAndClosesTheRun.
func TestCloseWritesTheAppAnswerStepAndClosesTheRun(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.CloseRecording(context.Background(), workerservice.RecordingClose{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 9, Status: "ended", Answer: []byte(`{"ok":true}`),
		RecordedActions: 9, DroppedActions: 0,
		TranscriptFileId: "v1:library:file:t1",
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}
	step := eng.callTo(t, "createWorkStep").Args(t)
	if step["stepType"] != "app_answer" {
		t.Errorf("stepType = %v, want app_answer", step["stepType"])
	}
	if step["seq"] != float64(9) {
		t.Errorf("seq = %v, want 9", step["seq"])
	}
	run := eng.callTo(t, "updateWorkRun").Args(t)
	if run["status"] != "succeeded" {
		t.Errorf("run status = %v, want succeeded for an ended session", run["status"])
	}
	summary, _ := run["summary"].(map[string]any)
	if summary["recordedActions"] != float64(9) || summary["droppedActions"] != float64(0) {
		t.Errorf("summary = %v, want the recording's own accounting", summary)
	}
	if summary["transcriptFileId"] != "v1:library:file:t1" {
		t.Errorf("summary = %v, want the transcript named on the run", summary)
	}
}

// TestAFailedSessionClosesTheRunFailed. A run that burned an hour of
// somebody's subscription and then failed still happened, and reporting it
// succeeded would make a lift treat a broken procedure as a good one.
func TestAFailedSessionClosesTheRunFailed(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.CloseRecording(context.Background(), workerservice.RecordingClose{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 1, Status: "failed", ExitCode: 2, ErrorMessage: "app exited 2",
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}
	if got := eng.callTo(t, "updateWorkRun").Args(t)["status"]; got != "failed" {
		t.Errorf("run status = %v, want failed", got)
	}
	if got := eng.callTo(t, "updateWorkStep").Args(t)["status"]; got != "failed" {
		t.Errorf("app_answer step status = %v, want failed", got)
	}
}

// TestARecordingWithNoRunIsRefused. Every observation read is scoped by run,
// so a row with no run belongs to nothing, is reachable through no query, and
// is invisible to the retention sweep -- it would accumulate forever.
func TestARecordingWithNoRunIsRefused(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, Seq: 0, Action: anAction(1, "exec"),
	}); err == nil {
		t.Fatal("an action with no run id was accepted")
	}
	if len(eng.recorded()) != 0 {
		t.Fatalf("it wrote anyway: %s", eng.summary())
	}
}

// TestARecordingWithNoOwnerIsRefused -- the workspace_owner_unresolved rule
// (memql#4354). A row written under a blank actor is readable by nobody,
// including the operator answering "what did this agent do".
func TestARecordingWithNoOwnerIsRefused(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, RunId: testRunId, Seq: 0, Action: anAction(1, "exec"),
	}); err == nil {
		t.Fatal("an action with no owner was accepted")
	}
	if len(eng.recorded()) != 0 {
		t.Fatalf("it wrote anyway: %s", eng.summary())
	}
}

// TestOpenRecordingKeepsARunTheCallerAlreadyOpened. Epic memql#5391's
// delegate opens the subrun and stamps childRunId on the delegating step;
// opening a SECOND one here would leave that pointer aimed at a run holding
// one step and no actions -- the "points at nothing" failure the delegate's
// own comment warns about.
func TestOpenRecordingKeepsARunTheCallerAlreadyOpened(t *testing.T) {
	w, eng := newSessionWriter(t)
	runId, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code", RunId: testRunId,
	})
	if err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	if runId != testRunId {
		t.Fatalf("OpenRecording returned %q, want the caller's run", runId)
	}
	if calls := eng.callsTo("createWorkRun"); len(calls) != 0 {
		t.Fatalf("it opened a second run: %s", eng.summary())
	}
}

// TestOpenRecordingOpensARunWhenNobodyDid -- the delegated-task path, where no
// step handed the session over.
func TestOpenRecordingOpensARunWhenNobodyDid(t *testing.T) {
	w, eng := newSessionWriter(t)
	runId, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code",
		Prompt: "do the thing",
	})
	if err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	if runId == "" {
		t.Fatal("OpenRecording returned no run")
	}
	run := eng.callTo(t, "createWorkRun").Args(t)
	if run["runId"] != runId {
		t.Errorf("createWorkRun wrote %v, want %v", run["runId"], runId)
	}
	if run["status"] != "running" {
		t.Errorf("run status = %v, want running", run["status"])
	}
	eng.callTo(t, "createWorkGoal")
}

// TestOpeningARunStampsTheDelegatingStep. Without it the parent step points
// at nothing and a reader has nowhere to go.
func TestOpeningARunStampsTheDelegatingStep(t *testing.T) {
	w, eng := newSessionWriter(t)
	runId, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "codex",
		ParentRunId: "v1:work:run:parent", ParentStepId: "v1:work:step:parent",
	})
	if err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	args := eng.callTo(t, "updateWorkStep").Args(t)
	if args["stepId"] != "v1:work:step:parent" || args["childRunId"] != runId {
		t.Errorf("updateWorkStep args = %v, want childRunId on the delegating step", args)
	}
}

// TestAnObservationStoresTwoHundredKilobytesOfArgumentsWhole -- issue #5397's
// acceptance, and the reason the 8 KiB cap went.
//
// An action is only reproducible from its arguments. The cap was a bound on
// how much of a call to keep, and integrations/planner's authoring capture
// rendered a truncated argument set as an EMPTY OBJECT -- so the largest
// calls, the ones most worth having, lost their arguments silently.
func TestAnObservationStoresTwoHundredKilobytesOfArgumentsWhole(t *testing.T) {
	w, eng := newSessionWriter(t)
	body := strings.Repeat("x", 200<<10)
	action := anAction(1, "fs_write")
	action.Args = map[string]any{"path": "/work/big.md", "content": body}

	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 0, Action: action,
	}); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	data := eng.callTo(t, "createWorkObservation").Args(t)["data"].(map[string]any)
	if data["argsTruncated"] == true {
		t.Fatal("200 KiB of arguments were truncated -- the ceiling is meant to sit above real work")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(data["args"].(string)), &args); err != nil {
		t.Fatalf("args did not round-trip: %v", err)
	}
	if got := len(args["content"].(string)); got != len(body) {
		t.Fatalf("content stored as %d bytes, want %d whole", got, len(body))
	}
}

// TestArgumentsAboveTheCeilingKeepAReference. The backstop must leave the
// action reproducible: a truncated argument set with no pointer to the whole
// one reproduces a DIFFERENT call, which is the failure the ceiling exists to
// bound rather than to cause.
func TestArgumentsAboveTheCeilingKeepAReference(t *testing.T) {
	w, eng := newSessionWriter(t)
	action := anAction(1, "fs_write")
	action.Args = map[string]any{"content": strings.Repeat("y", 300<<10)}

	if err := w.RecordAction(context.Background(), workerservice.RecordedAction{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 0, Action: action, ArgsRef: "v1:library:file:spill",
	}); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	data := eng.callTo(t, "createWorkObservation").Args(t)["data"].(map[string]any)
	if data["argsTruncated"] != true {
		t.Error("argsTruncated was not recorded for arguments above the ceiling")
	}
	if data["argsRef"] != "v1:library:file:spill" {
		t.Errorf("argsRef = %v, want the Library file the arguments spilled to", data["argsRef"])
	}
}

// TestAnOpenedRecordingRunCarriesItsParentsGoalSignature (epic memql#5408, gap
// G2). Procedure learning mines the recordings of ONE goal signature; a
// recording run the writer opens itself -- the delegated-task path -- must
// inherit its delegating run's signature, name that run, and carry the
// parent's variables, or it belongs to no corpus and no goal input can be
// mapped onto a parameter. The parent is read under the OWNER's actor, since
// the composite tier answers zero rows and no error to anybody else.
//
// The control is the same open with no parent: nothing is read, and the run
// carries neither a parent nor variables -- but it still carries a goal
// signature, derived from the session's own statement (main's delegated-task
// rule), so a parentless session is still learnable. The PARENT's signature
// wins whenever there is a parent, because it covers the goal's input names
// and the statement-derived one cannot.
func TestAnOpenedRecordingRunCarriesItsParentsGoalSignature(t *testing.T) {
	w, eng := newSessionWriter(t)
	eng.reply("workRunForOwner", map[string]any{
		"id":            "v1:work:run:parent",
		"goalSignature": "sig-parent",
		"variables":     map[string]any{"day": "2026-09-04"},
	})
	if _, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code",
		ParentRunId: "v1:work:run:parent", ParentStepId: "v1:work:step:parent",
	}); err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	read := eng.callTo(t, "workRunForOwner")
	if read.Actor != testOwner {
		t.Errorf("the parent was read as %q, want the owner %q", read.Actor, testOwner)
	}
	run := eng.callTo(t, "createWorkRun").Args(t)
	if run["goalSignature"] != "sig-parent" {
		t.Errorf("goalSignature = %v, want the parent's", run["goalSignature"])
	}
	if run["parentRunId"] != "v1:work:run:parent" {
		t.Errorf("parentRunId = %v, want the delegating run", run["parentRunId"])
	}
	if vars, _ := run["variables"].(map[string]any); vars["day"] != "2026-09-04" {
		t.Errorf("variables = %v, want the parent's", run["variables"])
	}

	w2, eng2 := newSessionWriter(t)
	if _, err := w2.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code",
	}); err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	if len(eng2.callsTo("workRunForOwner")) != 0 {
		t.Error("a recording with no parent read a parent anyway")
	}
	control := eng2.callTo(t, "createWorkRun").Args(t)
	for _, absent := range []string{"parentRunId", "variables"} {
		if _, present := control[absent]; present {
			t.Errorf("a recording with no parent wrote %s = %v", absent, control[absent])
		}
	}
	if sig, _ := control["goalSignature"].(string); sig == "" || sig == "sig-parent" {
		t.Errorf("a recording with no parent must carry its statement's own signature, got %v", control["goalSignature"])
	}
}

// TestTheCloseCarriesTheReportedModelAndEffortOnTheSummary (epic memql#5408,
// design D9). A learned procedure's provenance is read back from the
// recording run's summary, written in the same call that moves the run to
// `succeeded` -- the transition that fires the lift. What the app did NOT say
// stays absent: an empty string on the summary would read as a report.
func TestTheCloseCarriesTheReportedModelAndEffortOnTheSummary(t *testing.T) {
	w, eng := newSessionWriter(t)
	if err := w.CloseRecording(context.Background(), workerservice.RecordingClose{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId,
		Seq: 3, Status: "ended", Model: "claude-sonnet-4-6", Effort: "high",
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}
	summary, _ := eng.callTo(t, "updateWorkRun").Args(t)["summary"].(map[string]any)
	if summary["model"] != "claude-sonnet-4-6" || summary["effort"] != "high" {
		t.Errorf("summary = %v, want the app's reported model and effort", summary)
	}

	w2, eng2 := newSessionWriter(t)
	if err := w2.CloseRecording(context.Background(), workerservice.RecordingClose{
		SessionId: testSessionId, OwnerUserId: testOwner, RunId: testRunId, Seq: 3, Status: "ended",
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}
	silent, _ := eng2.callTo(t, "updateWorkRun").Args(t)["summary"].(map[string]any)
	for _, absent := range []string{"model", "effort"} {
		if _, present := silent[absent]; present {
			t.Errorf("an app that reported nothing left %s = %v on the summary", absent, silent[absent])
		}
	}
}

// TestARecordingInheritsTheGoalsInputAndNotTheReplaysVariables (epic
// memql#5408). A goal a learned procedure serves runs replayLearnedProcedure,
// and compile lays procedureConstructId over the goal's input to say which
// procedure. When the replay hands the goal back to the app, the repair
// session's recording is opened from that goal run -- and the lift reads a
// recording's variables AS THE GOAL'S INPUT: it maps parameters onto them and
// lists their keys as the procedure's inputs. A recording that kept
// procedureConstructId would teach it an input nobody gave. The goal's own
// input riding along is the control: without it, dropping every variable would
// pass.
func TestARecordingInheritsTheGoalsInputAndNotTheReplaysVariables(t *testing.T) {
	w, eng := newSessionWriter(t)
	eng.reply("workRunForOwner", map[string]any{
		"id": "v1:work:run:goal", "goalSignature": "sig-goal",
		"variables": map[string]any{"day": "2026-09-04", "procedureConstructId": "v1:authoring:construct:p1"},
	})
	if _, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code",
		ParentRunId: "v1:work:run:goal", ParentStepId: "v1:work:step:goal-replay",
	}); err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	vars, _ := eng.callTo(t, "createWorkRun").Args(t)["variables"].(map[string]any)
	if _, leaked := vars["procedureConstructId"]; leaked {
		t.Errorf("variables = %v: the recording inherited the replay's procedureConstructId, which the lift would read as a goal input", vars)
	}
	if vars["day"] != "2026-09-04" {
		t.Errorf("variables = %v, want the goal's own input inherited", vars)
	}
}

// TestAnUnreadableParentLeavesTheRecordingItsOwnStatementsSignature: a parent
// that cannot be read costs the recording its PARENT's signature, not its
// signature -- it keeps the one built from its own statement -- and the
// warning has to say that, or an operator reading it goes looking for an
// unsigned run that does not exist.
func TestAnUnreadableParentLeavesTheRecordingItsOwnStatementsSignature(t *testing.T) {
	eng := newRecordingEngine()
	eng.refuse("workRunForOwner", errors.New("database unavailable"))
	var logs bytes.Buffer
	w := NewSessionWriter(eng, slog.New(slog.NewTextHandler(&logs, nil)))
	w.SetNow(func() time.Time { return testNow })
	if _, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code", Prompt: "Summarise the ledger",
		ParentRunId: "v1:work:run:goal",
	}); err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	run := eng.callTo(t, "createWorkRun").Args(t)
	if want := workstate.GoalSignature("Summarise the ledger", nil); run["goalSignature"] != want {
		t.Errorf("goalSignature = %v, want %s, the signature of the recording's own statement", run["goalSignature"], want)
	}
	if _, present := run["variables"]; present {
		t.Errorf("variables = %v, from a parent nobody read", run["variables"])
	}
	warning := logs.String()
	if !strings.Contains(warning, "could not read the delegating run") {
		t.Fatalf("no warning about the unreadable parent was logged: %q", warning)
	}
	if strings.Contains(warning, "no goal signature") || !strings.Contains(warning, "its own statement") {
		t.Errorf("the warning %q does not say what was written: the recording keeps its own statement's signature", warning)
	}
}
