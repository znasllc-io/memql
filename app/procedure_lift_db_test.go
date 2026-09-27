package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// procedure_lift_db_test.go -- the lift end to end against a REAL engine and a
// REAL database (epic memql#5408, gaps G2, G3, G4, G7, G9, G15).
//
// The unit tests drive a recording engine, which accepts any row. Four things
// only a database can say: that the procedure payload -- an object of objects,
// with dotted keys -- passes the construct's schema; that the owned reads the
// lift makes with a CANONICAL owner id (the form a run row stores) answer the
// owner's rows; that Gate 1, installed the way app/ installs it, passes the
// rendered source; and that the second lift of an unchanged corpus finds the
// first construct and writes nothing.
//
// The recordings are written by the REAL session writer -- the path a
// delegated app session takes -- so the corpus is the shape production
// writes, arguments on the observation and fingerprint on the first step.

const procDBHelloDigest = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

func TestProcedureLiftDB_RecordedSessionsBecomeAProcedureOnTheLadder(t *testing.T) {
	e := workTemplateDBEngine(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := "dbtest-proc-" + strings.ReplaceAll(id.NewShortId(), "-", "")
	ownerCtx := auth.ContextWithUserActor(context.Background(), owner)
	statement := "Write the greeting file " + owner
	sig := work.GoalSignature(statement, []string{"file"})

	writer := workspine.NewSessionWriter(e, logger)
	var recordingRuns []string
	for n, file := range []string{"a.txt", "b.txt"} {
		recordingRuns = append(recordingRuns, recordSession(t, e, writer, ownerCtx, owner, statement, sig, file, n))
	}

	integ := procedure.New(e, logger)
	integ.SetCompiler(&CognitionEngineAdapter{Engine: e})
	res, err := integ.LearnFromRun(ownerCtx, recordingRuns[1])
	if err != nil {
		t.Fatalf("LearnFromRun: %v", err)
	}
	if !res.Accepted || res.Lift != procedure.LiftCreated {
		t.Fatalf("the two recordings lifted nothing: %+v", res)
	}
	if res.Rung != work.RungShadow {
		t.Fatalf("rung = %s, want shadow: the real Gate 1 must pass the rendered source (%+v)", res.Rung, res)
	}

	name := "learnedProcedure_" + sig[:12] + "_l1"
	row := procedureRow(t, e, ownerCtx, name)
	if got := row["procedureHash"]; got != res.ProcedureHash {
		t.Fatalf("stored procedureHash %v, want %s", got, res.ProcedureHash)
	}
	if row["ladder"] != "shadow" || row["goalSignature"] != sig {
		t.Fatalf("stored ladder %v / goalSignature %v", row["ladder"], row["goalSignature"])
	}
	p, err := procedure.DecodeProcedure(row["procedure"])
	if err != nil {
		t.Fatalf("the stored payload does not decode: %v", err)
	}
	if p.InputMap["s0.command.7"] != "file" || len(p.Steps) != 2 || p.Title != statement {
		t.Fatalf("stored payload: inputMap %v, %d steps, title %q", p.InputMap, len(p.Steps), p.Title)
	}
	if got := p.Expect[1].Contents; len(got) != 1 || got[0].Digest != procDBHelloDigest || got[0].Path != "out/report.txt" {
		t.Fatalf("the write's expectation = %+v, want the report's digest from the Library row", p.Expect[1])
	}
	if p.RecordedFrom.Model != "claude-sonnet-4-6" || p.RecordedFrom.Effort != "high" || p.RecordedFrom.App != "claude-code" {
		t.Fatalf("recordedFrom = %+v, want the app's report read off the recording runs", p.RecordedFrom)
	}
	prec, err := procedure.DecodePreconditions(row["preconditions"])
	if err != nil || prec.Tools["mkdir"] == "" {
		t.Fatalf("stored preconditions = %+v (%v), want the tools the commands use", prec, err)
	}
	if found := procedureConstructsForSignature(t, e, ownerCtx, sig); len(found) != 1 || found[0] != row["id"] {
		t.Fatalf("procedureConstructsForGoalSignature = %v, want exactly the lifted construct %v", found, row["id"])
	}

	// THE SAME CORPUS AGAIN, the way the completion trigger arrives after it
	// has borrowed the event's owner: as that owner, through the owned read.
	// The lift finds the construct it wrote and writes nothing.
	again, err := integ.LearnFromRun(ownerCtx, recordingRuns[0])
	if err != nil {
		t.Fatalf("LearnFromRun as the borrowed owner: %v", err)
	}
	if again.Lift != procedure.LiftUnchanged || again.ConstructId != row["id"] || again.ProcedureHash != res.ProcedureHash {
		t.Fatalf("second lift = %+v, want the first construct, unchanged", again)
	}
	if after := procedureRow(t, e, ownerCtx, name); after["id"] != row["id"] || after["procedureHash"] != row["procedureHash"] {
		t.Fatalf("the construct moved: %v -> %v", row["id"], after["id"])
	}
}

// recordSession writes one delegated session the way production does: the
// goal's run, then the session writer opening a recording run that inherits
// the goal's signature, two actions, and the close.
func recordSession(t *testing.T, e *memql.MemQLEngine, w *workspine.SessionWriter, ownerCtx context.Context,
	owner, statement, sig, file string, n int) string {
	t.Helper()
	internal := auth.ContextWithInternalOrigin(ownerCtx)
	goalId, parentRun := id.NewShortId(), id.NewShortId()
	templateMutation(t, e, internal, "createWorkGoal", map[string]any{
		"goalId": goalId, "statement": statement, "origin": "user", "input": map[string]any{"file": file},
	})
	templateMutation(t, e, internal, "createWorkRun", map[string]any{
		"runId": parentRun, "goalId": goalId, "automationName": "someTemplate", "templateFingerprint": "fp",
		"status": "running", "startedAt": "2026-09-23T10:00:00Z", "goalSignature": sig,
		"variables": map[string]any{"file": file},
	})

	session := "v1:worker:appSession:sess" + strings.ReplaceAll(id.NewShortId(), "-", "")[:8]
	runId, err := w.OpenRecording(ownerCtx, workerservice.RecordingOpen{
		SessionId: session, OwnerUserId: owner, App: "claude-code", Workspace: "/w/project",
		ParentRunId: parentRun,
	})
	if err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}

	// The report the session wrote, filed content-addressed the way the
	// recorder files it.
	fileId := id.NewShortId()
	templateMutation(t, e, ownerCtx, "createLibraryFile", map[string]any{
		"fileId": fileId, "name": "report.txt." + session[len(session)-8:] + ".toolu_write", "mimeType": "text/plain",
		"size": 6, "sha256": procDBHelloDigest, "blobUrl": "memory://report", "source": "app_session",
	})

	exit := 0
	fingerprint := map[string]any{
		"platform": map[string]any{"os": "darwin", "arch": "arm64"},
		"tools": []any{
			map[string]any{"name": "mkdir", "version": "mkdir (GNU coreutils) 9.4"},
			map[string]any{"name": "echo", "version": "echo 9.4"},
		},
		"cwd": "/w/project", "cwdEntries": 0,
	}
	if err := w.RecordAction(ownerCtx, workerservice.RecordedAction{
		SessionId: session, OwnerUserId: owner, RunId: runId, Seq: 0, Fingerprint: fingerprint,
		Action: workerservice.ActionEvent{
			Id: "toolu_exec_" + file, Seq: 1, Tool: "exec", Cwd: "/w/project", ExitCode: &exit,
			Args:         map[string]any{"command": "mkdir -p out && echo hello > " + file},
			ResultDigest: "sha256:cockpit-exec", ResultType: "string",
		},
	}); err != nil {
		t.Fatalf("RecordAction exec: %v", err)
	}
	if err := w.RecordAction(ownerCtx, workerservice.RecordedAction{
		SessionId: session, OwnerUserId: owner, RunId: runId, Seq: 1, ContentRefs: []string{fileId},
		Action: workerservice.ActionEvent{
			Id: "toolu_write_" + file, Seq: 2, Tool: "fs_write", Cwd: "/w/project",
			Args:         map[string]any{"file_path": "/w/project/out/report.txt", "content": "hello\n"},
			ResultDigest: "sha256:cockpit-write", ResultType: "string",
		},
	}); err != nil {
		t.Fatalf("RecordAction write: %v", err)
	}
	if err := w.CloseRecording(ownerCtx, workerservice.RecordingClose{
		SessionId: session, OwnerUserId: owner, RunId: runId, Seq: 2, Status: workerservice.AppSessionStatusEnded,
		Model: "claude-sonnet-4-6", Effort: "high", RecordedActions: 2,
	}); err != nil {
		t.Fatalf("CloseRecording: %v", err)
	}
	return runId
}

func procedureRow(t *testing.T, e *memql.MemQLEngine, ownerCtx context.Context, name string) map[string]any {
	t.Helper()
	res, err := e.Execute(ownerCtx, `query procedureConstructByName(name: "`+name+`")`)
	if err != nil {
		t.Fatalf("procedureConstructByName: %v", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("procedureConstructByName answered %d rows, want the one construct", len(rows))
	}
	return rows[0]
}

func procedureConstructsForSignature(t *testing.T, e *memql.MemQLEngine, ownerCtx context.Context, sig string) []any {
	t.Helper()
	res, err := e.Execute(ownerCtx, `query procedureConstructsForGoalSignature(goalSignature: "`+sig+`")`)
	if err != nil {
		t.Fatalf("procedureConstructsForGoalSignature: %v", err)
	}
	var out []any
	for _, r := range memql.MaterializeRows(res) {
		out = append(out, r["id"])
	}
	return out
}
