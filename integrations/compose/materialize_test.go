package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	work "github.com/znasllc-io/memql/integrations/work"
)

// State lives in the engine seam, so a second integration represents a different
// replica with none of the first replica's local state.
type materializeEngine struct {
	t             *testing.T
	compositions  map[string]map[string]any
	inputs        map[string]map[string]any
	files         map[string]map[string]any
	calls         []string
	source        map[string]any
	failReady     bool
	failFileReady bool
	afterFile     func()
}

func newMaterializeEngine(t *testing.T) *materializeEngine {
	return &materializeEngine{t: t, compositions: map[string]map[string]any{}, inputs: map[string]map[string]any{}, files: map[string]map[string]any{}}
}
func (e *materializeEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	name, args := materializeCall(e.t, query)
	e.calls = append(e.calls, name)
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId != "u-alice" {
		return nil, fmt.Errorf("wrong source/write actor")
	}
	var row map[string]any
	switch name {
	case "createCompositionInput":
		if auth.OriginFromContext(ctx) != auth.OriginInternal {
			return nil, fmt.Errorf("missing internal origin")
		}
		row = maps.Clone(args)
		row["ownerUserId"] = ac.UserId
		e.inputs[stringOf(args["compositionId"])] = row
	case "compositionInputById":
		row = e.inputs[stringOf(args["compositionId"])]
	case "createComposition":
		if auth.OriginFromContext(ctx) != auth.OriginInternal {
			return nil, fmt.Errorf("missing internal origin")
		}
		row = maps.Clone(args)
		row["status"] = "draft"
		row["ownerUserId"] = ac.UserId
		e.compositions[stringOf(args["compositionId"])] = row
	case "updateCompositionState":
		row = e.compositions[stringOf(args["compositionId"])]
		if row == nil {
			return nil, fmt.Errorf("composition missing")
		}
		if e.failReady && args["status"] == "ready" {
			return nil, fmt.Errorf("write interrupted")
		}
		maps.Copy(row, args)
	case "compositionById", "compositionExecutionById":
		row = e.compositions[stringOf(args["compositionId"])]
	case "createLibraryFile":
		row = maps.Clone(args)
		e.files[stringOf(args["fileId"])] = row
		if e.afterFile != nil {
			e.afterFile()
		}
	case "setLibraryFileStatus":
		if e.failFileReady {
			return nil, fmt.Errorf("file receipt write interrupted")
		}
		row = e.files[stringOf(args["fileId"])]
		if row == nil {
			return nil, fmt.Errorf("file missing")
		}
		maps.Copy(row, args)
	case "libraryFileById":
		row = e.files[stringOf(args["fileId"])]
	case "sourceRows":
		row = e.source
	case "cancelGoal":
		return memql.NewResultWithOutput(map[string]any{"runsAsked": 1}), nil
	case "createGoal":
		return memql.NewResultWithOutput(map[string]any{"goalId": "unrelated-goal", "runId": "unrelated-run"}), nil
	default:
		return nil, fmt.Errorf("unexpected call %s", query)
	}
	if row == nil {
		return memql.NewResultWithOutput([]map[string]any{}), nil
	}
	return memql.NewResultWithOutput([]map[string]any{maps.Clone(row)}), nil
}
func materializeCall(t *testing.T, q string) (string, map[string]any) {
	t.Helper()
	open := strings.IndexByte(q, '(')
	if open < 0 {
		t.Fatalf("not a call: %s", q)
	}
	name := strings.TrimSpace(q[:open])
	if p := strings.LastIndexByte(name, ' '); p >= 0 {
		name = name[p+1:]
	}
	body := q[open+1 : len(q)-1]
	parts := []string{}
	start, depth := 0, 0
	quoted, escaped := false, false
	for j, r := range body {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quoted:
			escaped = true
		case r == '"':
			quoted = !quoted
		case quoted:
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, body[start:j])
			start = j + 1
		}
	}
	parts = append(parts, body[start:])
	args := map[string]any{}
	for _, part := range parts {
		key, val, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(strings.TrimSpace(val)), &v); err != nil {
			t.Fatal(err)
		}
		args[strings.TrimSpace(key)] = v
	}
	return name, args
}

type materializeUploader struct {
	calls int
	bytes []byte
}

func (u *materializeUploader) Upload(_ context.Context, _, name string, data []byte, _ string) (string, error) {
	u.calls++
	u.bytes = append([]byte(nil), data...)
	return "https://storage.test/" + name, nil
}
func materializeContext() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-alice", Role: auth.RoleWriter})
}
func nestedMaterializeContext() context.Context {
	return common.ContextWithRun(materializeContext(), common.RunContext{RunId: "v1:work:run:nexus", GoalId: "v1:work:goal:nexus", StepKey: "file", OwnerUserId: "u-alice", Mode: common.RunModeLive})
}
func materializeFixture(t *testing.T) (*Integration, *materializeEngine, *materializeUploader) {
	e := newMaterializeEngine(t)
	u := &materializeUploader{}
	i := New(e, nil)
	i.SetUploader(u, "files")
	return i, e, u
}
func materializeDraft() materializeArgs {
	return materializeArgs{Name: "report", Format: pure.FormatMarkdown, Draft: "September report"}
}
func TestMaterializeWithinRunUsesThatRun(t *testing.T) {
	i, e, _ := materializeFixture(t)
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	if out["goalId"] != "v1:work:goal:nexus" || out["runId"] != "v1:work:run:nexus" {
		t.Fatalf("materialization escaped its owning run: %v", out)
	}
	for _, name := range e.calls {
		if name == "createGoal" {
			t.Fatal("a nested materialization opened another compiling goal")
		}
	}
}

func TestMaterializeRefusesAnUnwrittenReadyFileReceipt(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.failFileReady = true
	if result, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft()); err == nil {
		t.Fatalf("native composition claimed success without ready file receipt: %+v", result)
	}
	for _, row := range e.compositions {
		if row["status"] != "failed" {
			t.Fatalf("failed file receipt left composition successful: %+v", row)
		}
	}
}
func TestMaterializeRetryOnAnotherReplicaReturnsCompletedFile(t *testing.T) {
	i, e, u := materializeFixture(t)
	first, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	secondReplica := New(e, nil)
	secondReplica.SetUploader(u, "files")
	again, err := secondReplica.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	if again["compositionId"] != first["compositionId"] || again["outputFileId"] != first["outputFileId"] || u.calls != 1 || len(e.files) != 1 {
		t.Fatalf("replay duplicated a completed output: first=%v again=%v uploads=%d files=%d", first, again, u.calls, len(e.files))
	}
}

type directMaterializeGoal struct {
	goal  work.DirectGoal
	calls int
	err   error
}

func (d *directMaterializeGoal) OpenDirectGoal(ctx context.Context, g work.DirectGoal) (string, string, error) {
	d.calls++
	d.goal = g
	if d.err != nil {
		return "", "", d.err
	}
	if g.BeforeRun != nil {
		if err := g.BeforeRun(ctx, "v1:work:goal:direct", "v1:work:run:direct"); err != nil {
			return "", "", err
		}
	}
	return "v1:work:goal:direct", "v1:work:run:direct", nil
}

type materializeComposerFunc func(context.Context, ComposeRequest) (ComposeReply, error)

func (f materializeComposerFunc) Compose(ctx context.Context, r ComposeRequest) (ComposeReply, error) {
	return f(ctx, r)
}
func TestMaterializeStandaloneDispatchesSavedSnapshotThenReturns(t *testing.T) {
	i, e, u := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	e.source = map[string]any{"id": "invoice-1", "amount": 100}
	a := materializeDraft()
	a.Sources = []SourceRef{{Kind: KindQuery, Ref: "sourceRows()", Label: "Invoices"}}
	first, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	if opener.calls != 1 || opener.goal.AutomationName != "materializeFile" || u.calls != 0 || len(e.files) != 0 || len(e.compositions) != 1 {
		t.Fatalf("accept performed work or opened the wrong run: %v, uploads=%d", first, u.calls)
	}
	compositionId := stringOf(first["compositionId"])
	row := e.compositions[compositionId]
	if row["runId"] != "v1:work:run:direct" || opener.goal.Input["compositionId"] != compositionId {
		t.Fatal("dispatch cannot locate its composition")
	}
	// Neither engine reads nor the source snapshot live on the receiving replica.
	e.source["amount"] = 999
	worker := New(e, nil)
	worker.SetUploader(u, "files")
	worker.SetComposer(materializeComposerFunc(func(ctx context.Context, r ComposeRequest) (ComposeReply, error) {
		rc, ok := common.RunFromContext(ctx)
		if !ok || rc.RunId != "v1:work:run:direct" || rc.StepKey != "compose" {
			t.Fatalf("lost model run context: %+v", rc)
		}
		if r.Sources[0].Rows[0]["amount"] != float64(100) {
			t.Fatalf("source was re-read after dispatch: %+v", r.Sources)
		}
		return ComposeReply{Draft: pure.Draft{Body: "Snapshot report"}}, nil
	}))
	ctx := common.ContextWithRun(materializeContext(), common.RunContext{RunId: "v1:work:run:direct", GoalId: "v1:work:goal:direct", StepKey: "compose", OwnerUserId: "u-alice"})
	if _, err := worker.handleExecute(ctx, opener.goal.Input, 0); err != nil {
		t.Fatal(err)
	}
	if row["status"] != "ready" || u.calls != 1 {
		t.Fatalf("adopted run did not file output: %v", row)
	}
}
func TestMaterializeCancellationWhileComposingStopsBeforeUpload(t *testing.T) {
	i, e, u := materializeFixture(t)
	i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		for _, row := range e.compositions {
			row["status"] = "cancelled"
		}
		return ComposeReply{Draft: pure.Draft{Body: "too late"}}, nil
	}))
	_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err == nil || u.calls != 0 {
		t.Fatalf("cancelled run completed side effect: err=%v uploads=%d", err, u.calls)
	}
	for _, row := range e.compositions {
		if row["status"] != "cancelled" {
			t.Fatalf("cancellation overwritten: %v", row)
		}
	}
}
func TestMaterializeCrashAfterFilingDoesNotUploadAgain(t *testing.T) {
	i, e, u := materializeFixture(t)
	e.failReady = true
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft()); err == nil {
		t.Fatal("injected completion write failure was ignored")
	}
	e.failReady = false
	worker := New(e, nil)
	worker.SetUploader(u, "files")
	out, err := worker.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	if u.calls != 1 || len(e.files) != 1 || out["provenanceEmbedded"] != true {
		t.Fatalf("recovery repeated filing or lost provenance: %v uploads=%d", out, u.calls)
	}
}
func TestMaterializeExecuteRejectsOutsideItsOwningRun(t *testing.T) {
	i, e, _ := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	out, err := i.materialize(materializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{materializeContext(), nestedMaterializeContext()} {
		if _, err := i.handleExecute(ctx, map[string]any{"compositionId": out["compositionId"]}, 0); err == nil {
			t.Fatal("unrelated caller run executed the saved request")
		}
	}
	if len(e.files) != 0 {
		t.Fatal("unauthorized execution wrote an output")
	}
}
func TestMaterializeProviderFailureRecordsFailedComposition(t *testing.T) {
	i, e, u := materializeFixture(t)
	i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		return ComposeReply{}, fmt.Errorf("provider unavailable")
	}))
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft()); err == nil {
		t.Fatal("provider failure swallowed")
	}
	for _, row := range e.compositions {
		if row["status"] != "failed" || !strings.Contains(stringOf(row["failureReason"]), "provider unavailable") {
			t.Fatalf("failure record: %v", row)
		}
	}
	if u.calls != 0 {
		t.Fatal("failed generation uploaded a file")
	}
}

// TestAFailedCompositionSaysSoInAWordTheWorkSpineMatches is the RAISING END
// of the contract component/work's TerminalFailureCode reads. The two modules
// cannot see each other -- this one writes the terminal row and returns the
// error, and the executor that decides what the run does about it sees only
// the string the executor recorded -- so the code is a stable word carried in
// the message, asserted at both ends. TestAComposedDocumentThatFailedDoesNot-
// ParkAsWaiting is the matching end.
//
// Without it, this integration recorded the composition `failed` and then
// handed back prose. "context deadline exceeded" matched transient.timeout,
// the run parked at `waiting` on a retry, and a person watched a spinner over
// a document the database had already given up on.
func TestAFailedCompositionSaysSoInAWordTheWorkSpineMatches(t *testing.T) {
	i, e, u := materializeFixture(t)
	i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		return ComposeReply{}, fmt.Errorf("context deadline exceeded")
	}))
	_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err == nil {
		t.Fatal("the failure was swallowed")
	}
	if !strings.Contains(err.Error(), "composition_failed") {
		t.Fatalf("error = %q, want it to lead with composition_failed: without the code the work spine "+
			"reads the words \"deadline exceeded\" and parks the run on a retry against a failed row", err)
	}
	// THE ROW AND THE ERROR MUST AGREE. The code is only honest because the
	// terminal record is written before it is returned.
	for _, row := range e.compositions {
		if row["status"] != "failed" {
			t.Fatalf("composition status = %v, want failed alongside the coded error", row["status"])
		}
	}
	if u.calls != 0 {
		t.Fatal("failed generation uploaded a file")
	}
}

// A CANCELLATION IS NOT THAT CODE. Somebody asked it to stop, which the work
// spine has its own state for, and reporting a person's own click back to
// them as a terminal fault sends them to debug a step that was fine.
func TestACancelledCompositionIsNotReportedAsATerminalFailure(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.afterFile = func() {
		for _, row := range e.compositions {
			row["status"] = "cancelled"
		}
	}
	_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err == nil {
		t.Fatal("late cancellation was overwritten by completion")
	}
	if strings.Contains(err.Error(), "composition_failed") {
		t.Fatalf("error = %q: a cancellation must not carry the terminal-failure code", err)
	}
}

func TestMaterializeCancelRequestsItsWorkGoalToStop(t *testing.T) {
	i, e, _ := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	out, err := i.materialize(materializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleCancel(materializeContext(), map[string]any{"compositionId": out["compositionId"]}, 0); err != nil {
		t.Fatal(err)
	}
	for _, call := range e.calls {
		if call == "cancelGoal" {
			return
		}
	}
	t.Fatal("composition cancellation never requested cancellation of its executing work run")
}

func TestMaterializeReplayServesItsCompletedSourceRunFile(t *testing.T) {
	i, e, u := materializeFixture(t)
	first, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	ctx := common.ContextWithRun(materializeContext(), common.RunContext{RunId: "replay", GoalId: "nexus", SourceRunId: "nexus", SourceGoalId: "v1:work:goal:nexus", StepKey: "file", Mode: common.RunModeReplay, OwnerUserId: "u-alice"})
	again, err := i.materialize(ctx, "u-alice", "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	if again["outputFileId"] != first["outputFileId"] || u.calls != 1 || len(e.files) != 1 {
		t.Fatalf("replay re-executed file side effect: first=%v again=%v uploads=%d", first, again, u.calls)
	}
}

func TestMaterializeCancellationAfterFilingDoesNotOverwriteCancelled(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.afterFile = func() {
		for _, row := range e.compositions {
			row["status"] = "cancelled"
		}
	}
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft()); err == nil {
		t.Fatal("late cancellation was overwritten by completion")
	}
	for _, row := range e.compositions {
		if row["status"] != "cancelled" {
			t.Fatalf("late cancellation overwritten: %v", row)
		}
	}
}
