package work

// session.go -- the writer that turns a delegated app session into rows of
// the work spine (epic memql#5396, task memql#5398, design D2, D5 and D12).
//
// ============================================================================
// WHY IT IS HERE AND NOT IN component/worker
// ============================================================================
// Every write below is an @serverOnly mutation, and `auth.OriginFromContext`
// answers OriginClient for any context nobody stamped -- so an unstamped write
// is REFUSED with one WARN nothing above it hears. The stamp is allowlisted
// per PACKAGE, and this package is on that list for exactly this family of
// writes. component/worker declares the seam (SessionRecorder) and this
// implements it, so the dependency points worker -> work and there is no
// cycle: the same shape ToolInvocationRecorder has, for the same reason.
//
// ============================================================================
// THE TWO WRITERS AND THE ONE RUN
// ============================================================================
// A session's steps are written from two places. The replica holding the
// session decodes the cockpit's action events; the MCP node writes an `mcp`
// step when the app calls back into MemQL, which is a DIFFERENT REPLICA with
// no access to the first one's memory. Both write into ONE run -- the subrun
// the delegating step's childRunId points at -- because a reader following
// that pointer must find the whole session and not half of it.
//
// SEQ IS ALLOCATED BY THE CALLER, not here, and that is deliberate. The two
// writers know different things: the replica holding the session is the only
// writer of ACTIONS and can count them in memory, while the MCP node has to
// read the count off the session row, which is the only state both can see.
// Putting the allocation here would force the cheap side to pay the expensive
// side's read.
//
// ============================================================================
// NOTHING HERE MAY FAIL THE SESSION
// ============================================================================
// A recording is a record of work, not the work. A session that ran on
// somebody's machine and spent their subscription must not be reported as
// failed because a step row did not land. Errors are RETURNED so the caller
// can log them; the caller never refuses the run on one. The two exceptions
// are the preconditions -- no run and no owner -- which are refused BEFORE
// anything is written, because a row written without either is unreachable
// rather than merely missing.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// appSessionTemplate names a recording run's template. It matches the name
// epic memql#5391's delegate opens its child run under, so a run this writer
// opens and a run the delegate opened read as the same kind of thing in the
// Work app's feed.
const appSessionTemplate = "appSession"

// SessionWriter records an app session's actions.
//
// It satisfies workerservice.SessionRecorder. The MCP node's recorder uses the
// same type through the same methods, which is what keeps one account of what
// an app did rather than two that can disagree.
type SessionWriter struct {
	store  *store
	logger *slog.Logger
	now    func() time.Time
}

var _ workerservice.SessionRecorder = (*SessionWriter)(nil)

// NewSessionWriter builds the writer over the engine.
func NewSessionWriter(engine Engine, logger *slog.Logger) *SessionWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionWriter{store: &store{engine: engine}, logger: logger, now: func() time.Time {
		return time.Now().UTC()
	}}
}

// SetNow makes the clock injectable, so a rendered timestamp is an assertable
// value in a test.
func (w *SessionWriter) SetNow(now func() time.Time) {
	if w != nil && now != nil {
		w.now = now
	}
}

// OpenRecording returns the run the session's actions are recorded into.
//
// A run the CALLER already opened is kept as-is and nothing is written. That
// is the ordinary path since epic memql#5391: the delegate opens the subrun
// and stamps `childRunId` on the delegating step, and opening a second one
// here would leave that pointer aimed at a run holding one step and no
// actions -- the "points at nothing" failure the delegate's own comment warns
// about.
//
// With no run, one is opened here and the delegating step is stamped, which is
// the delegated-task path: nothing handed a step over, so nothing opened a run
// for it.
func (w *SessionWriter) OpenRecording(ctx context.Context, r workerservice.RecordingOpen) (string, error) {
	owner := strings.TrimSpace(r.OwnerUserId)
	if owner == "" {
		return "", fmt.Errorf("work: an app session with no owner cannot be recorded; a row written under a blank actor is readable by nobody")
	}
	if runId := strings.TrimSpace(r.RunId); runId != "" {
		return runId, nil
	}

	now := w.now().UTC()
	// The GOAL is keyed to the SESSION, so a session re-attached later is a
	// second run of one goal rather than a second goal -- the same split the
	// journal draws, and what makes the Work app's feed read as one thing per
	// session.
	goalId := deriveRecordingId("goal", r.SessionId, "")
	runId := deriveRecordingId("run", r.SessionId, now.Format(time.RFC3339Nano))

	statement := "run this in " + firstNonBlank(r.App, "a local app")
	if p := strings.TrimSpace(r.Prompt); p != "" {
		statement = p
	}
	if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkGoal", map[string]any{
		"goalId":       goalId,
		"statement":    statement,
		"origin":       "system",
		"requestedVia": "api",
		"input":        map[string]any{"app": r.App, "sessionId": r.SessionId},
	})); err != nil {
		return "", fmt.Errorf("work: open the recording's goal: %w", err)
	}
	if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkRun", map[string]any{
		"runId":               runId,
		"goalId":              goalId,
		"automationName":      appSessionTemplate,
		"templateFingerprint": appSessionTemplate,
		"triggeredBy":         "system",
		"mode":                "live",
		"status":              "running",
		"nodeId":              selfNodeId(),
		"startedAt":           now.Format(time.RFC3339),
		"input": map[string]any{
			"app":          r.App,
			"sessionId":    r.SessionId,
			"workspace":    r.Workspace,
			"parentRunId":  r.ParentRunId,
			"parentStepId": r.ParentStepId,
		},
	})); err != nil {
		return "", fmt.Errorf("work: open the recording run: %w", err)
	}

	// A FAILURE HERE IS A WARNING. The session is about to run on somebody's
	// machine either way; losing the back-pointer costs a reader a journey,
	// and refusing the work would cost them the work.
	if stepId := strings.TrimSpace(r.ParentStepId); stepId != "" {
		if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("updateWorkStep", map[string]any{
			"stepId":     stepId,
			"childRunId": runId,
		})); err != nil {
			w.logger.Warn("app session recording: could not record childRunId on the delegating step",
				"step_id", stepId, "child_run_id", runId, "error", err)
		}
	}
	return runId, nil
}

// RecordAction writes one action as a step and an observation.
//
// TWO ROWS, NOT ONE, and the split is the spine's: the STEP is what happened
// and is what a later lift reads to build a procedure; the OBSERVATION is the
// evidence, carrying the arguments whole, the result digest and the Library
// ids of whatever the action read or wrote. A step alone could not be
// replayed; an observation alone would not be in the run's order.
func (w *SessionWriter) RecordAction(ctx context.Context, r workerservice.RecordedAction) error {
	owner, err := recordingOwner(r.OwnerUserId, r.RunId, "an action")
	if err != nil {
		return err
	}

	stepType := workerservice.StepTypeForTool(r.Action.Tool)
	key := actionStepKey(r.Action)
	stepId := deriveRecordingId("step", r.RunId, key)
	started := r.StartedAt
	if started.IsZero() {
		started = w.now().UTC()
	}
	finished := r.FinishedAt
	if finished.IsZero() {
		finished = started
	}

	if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkStep", map[string]any{
		"stepId":         stepId,
		"runId":          r.RunId,
		"key":            key,
		"seq":            r.Seq,
		"stepType":       stepType,
		"kind":           actionKind(stepType),
		"call":           map[string]any{"construct": "app", "name": r.Action.Tool},
		"status":         "running",
		"attempt":        1,
		"idempotencyKey": r.RunId + ":" + key + ":1",
		"startedAt":      started.Format(time.RFC3339),
	})); err != nil {
		return fmt.Errorf("work: write the action step: %w", err)
	}

	receipt := map[string]any{
		"stepId":            stepId,
		"status":            actionStatus(r.Action),
		"result":            actionResult(r.Action),
		"resultFingerprint": r.Action.ResultDigest,
		"finishedAt":        finished.Format(time.RFC3339),
		"durationMs":        int(finished.Sub(started).Milliseconds()),
	}
	if len(r.Fingerprint) > 0 {
		// Written ONCE, on the step that carried it -- the session's first.
		// It is the initiation set a later replay is compared against.
		receipt["fingerprint"] = r.Fingerprint
	}
	if r.Action.IsError {
		receipt["errorCode"] = "app_action_failed"
		receipt["errorMessage"] = r.Action.Error
	}
	if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("updateWorkStep", receipt)); err != nil {
		return fmt.Errorf("work: write the action step's receipt: %w", err)
	}

	return w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkObservation", map[string]any{
		"observationId": newRowId("v1:work:observation"),
		"runId":         r.RunId,
		"stepKey":       key,
		"kind":          observationKindToolResult,
		"content":       actionContent(r.Action),
		"data":          actionData(r),
	}))
}

// RecordGap notes actions that never arrived.
//
// IT IS AN OBSERVATION RATHER THAN A FIELD, and the reason is that the loss
// has to be IN THE RUN'S OWN RECORD. A counter on the session row says a
// session lost something; an observation says WHERE, in the same stream a
// later lift is already reading, so a procedure built from an incomplete
// recording is built by something that had to walk past the hole.
func (w *SessionWriter) RecordGap(ctx context.Context, r workerservice.RecordedGap) error {
	owner, err := recordingOwner(r.OwnerUserId, r.RunId, "a gap")
	if err != nil {
		return err
	}
	return w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkObservation", map[string]any{
		"observationId": newRowId("v1:work:observation"),
		"runId":         r.RunId,
		"kind":          "note",
		"content": fmt.Sprintf(
			"%d action(s) of this app session were not recorded: the sequence jumped from %d to %d. "+
				"Anything lifted from this recording is incomplete.",
			r.Missing, r.AfterSeq, r.BeforeSeq),
		"data": map[string]any{
			"sessionId": r.SessionId,
			"reason":    "action_sequence_gap",
			"afterSeq":  int(r.AfterSeq),
			"beforeSeq": int(r.BeforeSeq),
			"missing":   r.Missing,
		},
	}))
}

// CloseRecording writes the app_answer step and closes the run.
//
// The structured answer is a STEP rather than only the run's outcome because
// it is the one part of the session that IS intelligence (component/work reads
// `app_answer` as reasoning), and a lift that could not see it would treat the
// whole session as deterministic.
func (w *SessionWriter) CloseRecording(ctx context.Context, r workerservice.RecordingClose) error {
	owner, err := recordingOwner(r.OwnerUserId, r.RunId, "the answer")
	if err != nil {
		return err
	}
	finished := r.FinishedAt
	if finished.IsZero() {
		finished = w.now().UTC()
	}
	succeeded := r.Status == workerservice.AppSessionStatusEnded

	const key = "app_answer"
	stepId := deriveRecordingId("step", r.RunId, key)
	if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkStep", map[string]any{
		"stepId":         stepId,
		"runId":          r.RunId,
		"key":            key,
		"seq":            r.Seq,
		"stepType":       workerservice.StepTypeAppAnswer,
		"kind":           "reasoning",
		"call":           map[string]any{"construct": "app", "name": "answer"},
		"status":         "running",
		"attempt":        1,
		"idempotencyKey": r.RunId + ":" + key + ":1",
		"startedAt":      finished.Format(time.RFC3339),
	})); err != nil {
		return fmt.Errorf("work: write the answer step: %w", err)
	}

	receipt := map[string]any{
		"stepId":     stepId,
		"status":     map[bool]string{true: "done", false: "failed"}[succeeded],
		"result":     answerResult(r),
		"finishedAt": finished.Format(time.RFC3339),
	}
	if !succeeded {
		receipt["errorCode"] = "app_session_" + r.Status
		receipt["errorMessage"] = r.ErrorMessage
	}
	if err := w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("updateWorkStep", receipt)); err != nil {
		return fmt.Errorf("work: write the answer step's receipt: %w", err)
	}

	// The run's SUMMARY carries the recording's own accounting. A reader
	// asking whether a lifted procedure can be trusted asks this first: a
	// recording that lost actions is not a recording of the whole session.
	closeRun := map[string]any{
		"runId":      r.RunId,
		"status":     map[bool]string{true: "succeeded", false: "failed"}[succeeded],
		"finishedAt": finished.Format(time.RFC3339),
		"summary": map[string]any{
			"recordedActions":  r.RecordedActions,
			"droppedActions":   r.DroppedActions,
			"transcriptFileId": r.TranscriptFileId,
			"sessionId":        r.SessionId,
		},
	}
	if succeeded {
		closeRun["outcome"] = answerResult(r)
	} else {
		closeRun["errorCode"] = "app_session_" + r.Status
		closeRun["errorMessage"] = r.ErrorMessage
	}
	return w.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("updateWorkRun", closeRun))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// recordingOwner checks the two preconditions and answers the owner to borrow.
//
// It returns the OWNER ID, not a context, and that is this package's rule
// rather than a style choice: a context built here and handed back is
// inherited by every later frame, which is how memql#2879's hole was reachable
// -- so the actor is stamped INLINE as the argument to each write, and
// TestNoMethodReturnsAContext fails the build on the other shape.
//
// Both preconditions are REFUSED rather than defaulted. A row with no run
// belongs to no run, is reachable through no query (every observation and step
// read is scoped by one) and is invisible to the retention sweep that folds a
// run's detail, so it would accumulate forever, unreachable. A row with no
// owner is readable by nobody, including the operator answering "what did this
// agent do on my machine" -- the workspace_owner_unresolved rule memql#4354
// settled.
func recordingOwner(ownerUserId, runId, what string) (string, error) {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return "", fmt.Errorf("work: %s of an app session with no owner cannot be recorded; a row written under a blank actor is readable by nobody", what)
	}
	if strings.TrimSpace(runId) == "" {
		return "", fmt.Errorf("work: %s of an app session with no run id cannot be recorded; it would belong to no run and be reachable through no query", what)
	}
	return owner, nil
}

// actionStepKey names the step. It is the app's OWN id where there is one, so
// a recorded step ties back to the line of the transcript that produced it;
// the sequence is the fallback for a harness that reports no id.
func actionStepKey(a workerservice.ActionEvent) string {
	if id := strings.TrimSpace(a.Id); id != "" {
		return "action-" + id
	}
	return fmt.Sprintf("action-%d", a.Seq)
}

// actionKind is the derived kind. It mirrors component/work.DeriveKind rather
// than calling it, because that function takes a construct registry this
// package has no reason to hold and every app-session answer is decided by the
// step type alone. TestRecordedKindsMatchDeriveKind pins the two together.
func actionKind(stepType string) string {
	if stepType == workerservice.StepTypeAppAnswer {
		return "reasoning"
	}
	return "deterministic"
}

func actionStatus(a workerservice.ActionEvent) string {
	if a.IsError {
		return "failed"
	}
	return "done"
}

// actionResult is the step's trimmed result: a DIGEST and an inferred type,
// never the result itself (design D5), so the spine stays about actions rather
// than filling with output nobody replays.
func actionResult(a workerservice.ActionEvent) map[string]any {
	out := map[string]any{"digest": a.ResultDigest, "type": a.ResultType}
	if a.ExitCode != nil {
		out["exitCode"] = *a.ExitCode
	}
	return out
}

func answerResult(r workerservice.RecordingClose) map[string]any {
	out := map[string]any{"exitCode": r.ExitCode, "status": r.Status}
	if len(r.Answer) > 0 {
		var decoded any
		if err := json.Unmarshal(r.Answer, &decoded); err == nil {
			out["answer"] = decoded
		} else {
			// Not JSON. Kept as text rather than dropped: the harness answered
			// something, and losing it because it did not parse hides the one
			// piece of evidence that says the schema was not met.
			out["answer"] = string(r.Answer)
		}
	}
	return out
}

// actionContent renders the action as the sentence the observation is embedded
// from. A JSON dump would retrieve on punctuation; what is worth recalling is
// what the app did and whether it worked.
func actionContent(a workerservice.ActionEvent) string {
	var b strings.Builder
	b.WriteString("The app ran ")
	b.WriteString(a.Tool)
	if cwd := strings.TrimSpace(a.Cwd); cwd != "" {
		b.WriteString(" in ")
		b.WriteString(cwd)
	}
	if a.IsError {
		b.WriteString(" and it failed")
		if a.Error != "" {
			b.WriteString(": ")
			b.WriteString(truncateForContent(a.Error))
		}
	} else {
		b.WriteString(" successfully")
	}
	b.WriteString(".")
	return b.String()
}

// actionData is the observation's evidence.
//
// EVERY KEY IT WRITES IS ONE THE EVENT REPORTED. An absent exitCode stays
// absent rather than becoming 0: a clean command exits 0 and a file write has
// no exit code at all, and collapsing them reports every write as a command
// that succeeded.
func actionData(r workerservice.RecordedAction) map[string]any {
	a := r.Action
	data := map[string]any{
		"tool":        a.Tool,
		"appActionId": a.Id,
		"sessionId":   r.SessionId,
		"seq":         int(a.Seq),
		"isError":     a.IsError,
	}
	if cwd := strings.TrimSpace(a.Cwd); cwd != "" {
		data["cwd"] = cwd
	}
	if a.ExitCode != nil {
		data["exitCode"] = *a.ExitCode
	}
	if d := strings.TrimSpace(a.ResultDigest); d != "" {
		data["resultDigest"] = d
	}
	if t := strings.TrimSpace(a.ResultType); t != "" {
		data["resultType"] = t
	}
	if args, truncated := encodeObservationArgs(a.Args); args != "" {
		data["args"] = args
		if truncated {
			data["argsTruncated"] = true
		}
	}
	if ref := strings.TrimSpace(r.ArgsRef); ref != "" {
		// The arguments were too large to keep inline and went to a Library
		// file. Both keys are written: `args` is the head a reader sees
		// without leaving the row, and `argsRef` is what makes the action
		// reproducible anyway.
		data["argsRef"] = ref
	}
	if len(r.ContentRefs) > 0 {
		data["contentRefs"] = r.ContentRefs
	}
	if len(r.ContentOmitted) > 0 {
		data["contentOmitted"] = r.ContentOmitted
	}
	if a.IsError && a.Error != "" {
		data["error"] = a.Error
	}
	return data
}

// deriveRecordingId makes a STABLE id for a recording row.
//
// Stable because the rows are append-only: a step written twice at one key is
// a second VERSION of one row rather than a duplicate, which is what makes a
// retry readable as a retry. Through core/id rather than crypto/sha256
// because everything under integrations/ derives its ids that way and
// TestNoSHA256InIntegrations says so -- this is an opaque row id, not a
// wire-format digest anybody compares with sha256sum.
func deriveRecordingId(kind, scope, key string) string {
	return "v1:work:" + kind + ":" + string(id.New().MustFromMap(map[string]any{
		"kind":  kind,
		"scope": scope,
		"key":   key,
	}))
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
