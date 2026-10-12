package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	work "github.com/znasllc-io/memql/integrations/work"
)

// State lives in the engine seam, so a second integration represents a different
// replica with none of the first replica's local state.
type materializeEngine struct {
	t              *testing.T
	compositions   map[string]map[string]any
	inputs         map[string]map[string]any
	files          map[string]map[string]any
	folders        map[string]map[string]any // the Library folders the caller can READ, by id; any other id answers no row
	folderReads    []bool                    // for each libraryFolderById, whether it asked for a fresh read
	templateReads  []bool                    // for each composeTemplateById, whether it asked for a fresh read
	calls          []string
	source         map[string]any
	account        map[string]any
	recipe         map[string]any
	template       map[string]any
	failReady      bool
	failFileReady  bool
	fileReadyError string
	afterFile      func()

	// cachedCompositions is what THIS node's result cache still holds for a
	// composition another replica has changed since: a compositionById read that
	// is not fresh is answered from here when it names one. compositionReads
	// records, for each compositionById, whether it asked for a fresh read.
	cachedCompositions map[string]map[string]any
	compositionReads   []bool
}

func newMaterializeEngine(t *testing.T) *materializeEngine {
	return &materializeEngine{t: t, compositions: map[string]map[string]any{}, inputs: map[string]map[string]any{}, files: map[string]map[string]any{}, folders: map[string]map[string]any{}}
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
	case "saveCompositionRecipe":
		if auth.OriginFromContext(ctx) != auth.OriginInternal {
			return nil, fmt.Errorf("missing internal origin")
		}
		row = e.inputs[stringOf(args["compositionId"])]
		if row == nil {
			return nil, fmt.Errorf("missing input")
		}
		maps.Copy(row, args)
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
		if name == "compositionById" {
			fresh := memql.FreshReadFromContext(ctx)
			e.compositionReads = append(e.compositionReads, fresh)
			if cached, ok := e.cachedCompositions[stringOf(args["compositionId"])]; ok && !fresh {
				row = cached
			}
		}
	case "createLibraryFile":
		// @serverOnly, as the engine holds it: blobUrl is a storage path the
		// node's credential reads back, so an unstamped write is refused.
		if auth.OriginFromContext(ctx) != auth.OriginInternal {
			return nil, fmt.Errorf("missing internal origin")
		}
		row = maps.Clone(args)
		row["ownerUserId"] = ac.UserId
		e.files[stringOf(args["fileId"])] = row
		if e.afterFile != nil {
			e.afterFile()
		}
	case "setLibraryFileStatus":
		if e.failFileReady {
			if e.fileReadyError != "" {
				return nil, fmt.Errorf("%s", e.fileReadyError)
			}
			return nil, fmt.Errorf("file receipt write interrupted")
		}
		row = e.files[stringOf(args["fileId"])]
		if row == nil {
			return nil, fmt.Errorf("file missing")
		}
		maps.Copy(row, args)
	case "libraryFileById":
		row = e.files[stringOf(args["fileId"])]
	case "libraryFolderById":
		e.folderReads = append(e.folderReads, memql.FreshReadFromContext(ctx))
		row = e.folders[stringOf(args["folderId"])]
	case "sourceRows":
		row = e.source
	case "clientAccountById":
		row = e.account
	case "composeRecipeById":
		row = e.recipe
	case "composeTemplateById":
		e.templateReads = append(e.templateReads, memql.FreshReadFromContext(ctx))
		row = e.template
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

func (u *materializeUploader) Upload(_ context.Context, container, name string, data []byte, _ string) (string, error) {
	u.calls++
	u.bytes = append([]byte(nil), data...)
	// The shape the Azure client returns: account host, then container, then
	// the object -- which is what the Library's key check reads.
	return "https://storage.test/" + container + "/" + name, nil
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

func TestMaterializeTransientFailureResumesOnAnotherReplica(t *testing.T) {
	for _, message := range []string{"context deadline exceeded", "the local runtime stopped producing output past the idle ceiling", "connection reset by peer", "materializer: invalid draft: unexpected EOF"} {
		t.Run(message, func(t *testing.T) {
			i, e, u := materializeFixture(t)
			i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
				return ComposeReply{}, fmt.Errorf("%s", message)
			}))
			_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
			if err == nil || strings.Contains(err.Error(), terminalFailureCode) {
				t.Fatalf("transient error was swallowed or made terminal: %v", err)
			}
			for _, row := range e.compositions {
				if row["status"] != "draft" {
					t.Fatalf("resumable composition was closed: %v", row)
				}
			}
			worker := New(e, nil)
			worker.SetUploader(u, "files")
			worker.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
				return ComposeReply{Draft: pure.Draft{Body: "Recovered report"}}, nil
			}))
			out, err := worker.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
			if err != nil || len(e.compositions) != 1 || len(e.files) != 1 || u.calls != 1 {
				t.Fatalf("recovery lost its identity or duplicated an effect: out=%v err=%v uploads=%d", out, err, u.calls)
			}
			if row := e.compositions[stringOf(out["compositionId"])]; row["status"] != "ready" {
				t.Fatalf("recovered file did not finish: %v", row)
			}
		})
	}
}

func TestMaterializeTerminalFailureDoesNotCallModelAgain(t *testing.T) {
	i, e, u := materializeFixture(t)
	i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		return ComposeReply{}, fmt.Errorf("invalid document draft")
	}))
	_, _ = i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	worker := New(e, nil)
	worker.SetUploader(u, "files")
	worker.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		t.Fatal("terminal composition was retried")
		return ComposeReply{}, nil
	}))
	if _, err := worker.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft()); err == nil || !strings.Contains(err.Error(), terminalFailureCode) {
		t.Fatalf("terminal state was ignored: %v", err)
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
		return ComposeReply{}, fmt.Errorf("invalid document draft")
	}))
	_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft())
	if err == nil {
		t.Fatal("the failure was swallowed")
	}
	if !strings.Contains(err.Error(), "composition_failed") {
		t.Fatalf("error = %q, want composition_failed for an invalid document", err)
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

// A STOP IS DECIDED ON A FRESH READ. With two replicas the run that makes a file
// usually finishes on the other one, and this node's cached copy of the
// composition can still say `composing` after it is ready. Deciding on that copy
// would mark a ready composition cancelled -- erasing that it made a file that is
// still there -- and the person's client would report a stop for a file that
// exists.
func TestCancelDecidesOnAFreshReadOfTheComposition(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.compositions["c1"] = map[string]any{"compositionId": "c1", "status": "ready", "outputFileId": "f1", "goalId": "g1", "ownerUserId": "u-alice"}
	e.cachedCompositions = map[string]map[string]any{"c1": {"compositionId": "c1", "status": "composing", "goalId": "g1", "ownerUserId": "u-alice"}}

	nodes, err := i.handleCancel(materializeContext(), map[string]any{"compositionId": "c1", "reason": "Stopped by the person"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.compositionReads) != 1 || !e.compositionReads[0] {
		t.Fatalf("the composition must be read once, fresh, before deciding the stop: %v", e.compositionReads)
	}
	var reply map[string]any
	if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &reply) != nil {
		t.Fatalf("cancel answered %v", nodes)
	}
	if reply["cancelled"] != false || reply["status"] != "ready" {
		t.Fatalf("a composition that had finished was reported as stopped: %v", reply)
	}
	if e.compositions["c1"]["status"] != "ready" {
		t.Fatalf("a ready composition was marked %v", e.compositions["c1"]["status"])
	}
	for _, call := range e.calls {
		if call == "updateCompositionState" || call == "cancelGoal" {
			t.Fatalf("a finished composition was still stopped (%s): %v", call, e.calls)
		}
	}
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

func TestBriefOnlyRecipeStartsMaterializerWithSavedClient(t *testing.T) {
	i, e, _ := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	e.account = map[string]any{"id": "client-a", "status": "active"}
	e.recipe = map[string]any{"id": "brief-recipe", "name": "Welcome", "format": "json", "outputKind": "email_template", "description": "Create a welcoming email for our new subscribers.", "accountIds": []any{"client-a"}}
	if _, err := i.handleRunRecipe(materializeContext(), map[string]any{"recipeId": "brief-recipe"}, 0); err != nil {
		t.Fatal(err)
	}
	if opener.calls != 1 || opener.goal.AutomationName != "materializeFile" || len(opener.goal.AccountIds) != 1 || opener.goal.AccountIds[0] != "client-a" {
		t.Fatalf("recipe lost its scope or work runtime: %+v", opener)
	}
	for _, input := range e.inputs {
		raw, err := json.Marshal(input["request"])
		if err != nil {
			t.Fatal(err)
		}
		var saved executionRequest
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Args.Statement != e.recipe["description"] || saved.Args.OutputKind != "email_template" {
			t.Fatalf("recipe changed the saved brief or output kind: %+v", saved.Args)
		}
	}
	delete(e.recipe, "description")
	if _, err := i.handleRunRecipe(materializeContext(), map[string]any{"recipeId": "brief-recipe"}, 0); err == nil || !strings.Contains(err.Error(), "brief") {
		t.Fatalf("empty recipe started work: %v", err)
	}
	if opener.calls != 1 {
		t.Fatal("empty recipe opened another goal")
	}
}

func TestCompoundOutputRecoveryRetainsRecipeAndSourceZIPAcrossReplicas(t *testing.T) {
	for _, interruptFileReceipt := range []bool{false, true} {
		t.Run(fmt.Sprint(interruptFileReceipt), func(t *testing.T) {
			i, e, u := materializeFixture(t)
			drafts := 0
			i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
				drafts++
				return ComposeReply{Draft: pure.Draft{Title: "Report", Body: "Exact source"}}, nil
			}))
			args := materializeDraft()
			args.Format = pure.FormatPDF
			e.fileReadyError = "connection reset by peer"
			e.failFileReady = interruptFileReceipt
			e.failReady = !interruptFileReceipt
			if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", args); err == nil {
				t.Fatal("injected receipt failure was ignored")
			}
			e.failFileReady = false
			e.failReady = false
			sibling := New(e, nil)
			sibling.SetUploader(u, "files")
			sibling.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
				t.Fatal("retry regenerated the captured recipe")
				return ComposeReply{}, nil
			}))
			out, err := sibling.materialize(nestedMaterializeContext(), "u-alice", "", args)
			if err != nil {
				t.Fatal(err)
			}
			if drafts != 1 || out["sourceFileId"] == "" || out["sourceFileId"] == nil || len(e.files) != 2 {
				t.Fatalf("incomplete compound recovery: %v drafts=%d files=%d", out, drafts, len(e.files))
			}
			uploads := u.calls
			again, err := sibling.materialize(nestedMaterializeContext(), "u-alice", "", args)
			if err != nil {
				t.Fatal(err)
			}
			if again["sourceFileId"] != out["sourceFileId"] || u.calls != uploads {
				t.Fatalf("completed compound delivery repeated: %v", again)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Blank files and request ids (epic #6023, task #6024)
// ---------------------------------------------------------------------------

func blankArgs(format pure.Format) materializeArgs {
	return materializeArgs{Name: "Notes", Format: format}
}

func TestMaterializeBlankMarkdownFilesAnEmptyDocumentWithoutAModelCall(t *testing.T) {
	i, e, u := materializeFixture(t)
	i.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		t.Fatal("a blank request reached the model")
		return ComposeReply{}, nil
	}))
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(pure.FormatMarkdown))
	if err != nil {
		t.Fatal(err)
	}
	// The pipeline answers with the filed output; the composition row is where
	// "ready" is recorded, so that is what is read.
	row := e.compositions[stringOf(out["compositionId"])]
	if row["status"] != "ready" || stringOf(row["outputFileId"]) == "" || out["outputFileId"] != row["outputFileId"] {
		t.Fatalf("blank composition did not complete: result=%v row=%v", out, row)
	}
	if u.calls != 1 || string(u.bytes) != "\n" {
		t.Fatalf("blank markdown must be a single newline so the editor shows its empty page, got %q after %d uploads", u.bytes, u.calls)
	}
	// The file carries no provenance, so the row says why in the blank file's own
	// sentence rather than leaving "not embedded" unexplained.
	if row["provenanceEmbedded"] != false || row["provenanceNote"] != "Created blank. Its history is kept on its composition, not in the file." {
		t.Fatalf("blank composition provenance = embedded %v, note %q", row["provenanceEmbedded"], row["provenanceNote"])
	}
	if len(e.files) != 1 {
		t.Fatalf("a blank file is one file, got %d", len(e.files))
	}
	for _, f := range e.files {
		if f["name"] != "Notes.md" {
			t.Fatalf("blank markdown filename = %v, want Notes.md", f["name"])
		}
	}
}

func TestMaterializeBlankFormatsRenderTheirEmptyShape(t *testing.T) {
	cases := map[pure.Format]string{
		pure.FormatText:     "",
		pure.FormatCSV:      "",
		pure.FormatJSON:     "{}\n",
		pure.FormatHTML:     "<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>Notes</title></head><body></body></html>\n",
		pure.FormatMarkdown: "\n",
	}
	for format, want := range cases {
		t.Run(string(format), func(t *testing.T) {
			i, e, u := materializeFixture(t)
			if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(format)); err != nil {
				t.Fatalf("%s: %v", format, err)
			}
			// An empty want would also match a file nobody wrote, so the upload
			// itself is part of the claim. A blank file has no editable recipe
			// worth rebuilding, so csv, json and html file no source package.
			if u.calls != 1 || len(e.files) != 1 {
				t.Fatalf("%s: blank filed %d files with %d uploads, want exactly the one", format, len(e.files), u.calls)
			}
			if string(u.bytes) != want {
				t.Fatalf("%s blank bytes = %q, want %q", format, u.bytes, want)
			}
		})
	}
}

func TestMaterializeRefusesBlankDocxAndPdfBeforeWritingAnything(t *testing.T) {
	for _, format := range []pure.Format{pure.FormatDOCX, pure.FormatPDF} {
		for name, ctx := range map[string]context.Context{"in a run": nestedMaterializeContext(), "standalone": materializeContext()} {
			i, e, u := materializeFixture(t)
			_, err := i.materialize(ctx, "u-alice", "", blankArgs(format))
			if err == nil || !strings.Contains(err.Error(), "A blank file can be Markdown, text, HTML, CSV or JSON.") {
				t.Fatalf("%s %s: want blank refusal, got %v", format, name, err)
			}
			if len(e.compositions) != 0 || len(e.inputs) != 0 || len(e.calls) != 0 || u.calls != 0 {
				t.Fatalf("%s %s: refusal wrote rows: %v %v calls=%v uploads=%d", format, name, e.compositions, e.inputs, e.calls, u.calls)
			}
		}
	}
}

// A docx or pdf with something to compose from is an ordinary request. The
// refusal is about having nothing to put in them, not about the formats.
func TestMaterializeStillWritesDocxAndPdfWhenThereIsContent(t *testing.T) {
	for _, format := range []pure.Format{pure.FormatDOCX, pure.FormatPDF} {
		i, _, u := materializeFixture(t)
		a := materializeDraft()
		a.Format = format
		if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", a); err != nil {
			t.Fatalf("%s with a draft: %v", format, err)
		}
		if u.calls == 0 {
			t.Fatalf("%s with a draft filed nothing", format)
		}
	}
}

// ---------------------------------------------------------------------------
// The folder a file is filed into (epic #6023, task #6029)
// ---------------------------------------------------------------------------
//
// folderId is a person's choice or another app's claim, and creating the file
// stores whatever string it is given: nothing downstream asks whether the caller
// can see that folder. A file filed under a folder they cannot see is a file they
// cannot find, and one filed into the Bin is hidden by the very act of filing it.
// So the request is refused, before anything is written, when the caller cannot
// read the folder or it has been archived.

const folderUnavailable = "That folder isn't available. Choose another folder."

func folderArgs() materializeArgs {
	a := blankArgs(pure.FormatMarkdown)
	a.FolderId = "folder-1"
	return a
}

func TestMaterializeRefusesAFolderTheCallerCannotUseBeforeWritingAnything(t *testing.T) {
	for name, folder := range map[string]map[string]any{
		"a folder the caller cannot read": nil,
		"a folder in the Bin":             {"id": "folder-1", "name": "Old reports", "archived": true},
	} {
		for where, ctx := range map[string]context.Context{"in a run": nestedMaterializeContext(), "standalone": materializeContext()} {
			i, e, u := materializeFixture(t)
			if folder != nil {
				e.folders["folder-1"] = folder
			}
			_, err := i.materialize(ctx, "u-alice", "", folderArgs())
			// The sentence is the person's: the OS shows it as it is.
			if err == nil || err.Error() != folderUnavailable {
				t.Fatalf("%s %s: want the folder refusal verbatim, got %v", name, where, err)
			}
			if len(e.compositions) != 0 || len(e.inputs) != 0 || len(e.files) != 0 || u.calls != 0 {
				t.Fatalf("%s %s: refusal wrote rows: %v %v %v uploads=%d", name, where, e.compositions, e.inputs, e.files, u.calls)
			}
			// Reads only: a step looks for the composition it may already have made, and
			// the folder is read. No composition, input, goal or file follows.
			for _, made := range e.calls {
				if made != "libraryFolderById" && made != "compositionExecutionById" {
					t.Fatalf("%s %s: the refusal went on to %s: %v", name, where, made, e.calls)
				}
			}
		}
	}
}

func TestMaterializeFilesIntoAFolderTheCallerCanUse(t *testing.T) {
	for name, folder := range map[string]map[string]any{
		"an active folder": {"id": "folder-1", "name": "Reports", "archived": false},
		// A folder from before the field existed has no `archived` at all, and is not in the Bin.
		"a folder with no archived field": {"id": "folder-1", "name": "Reports"},
	} {
		i, e, _ := materializeFixture(t)
		e.folders["folder-1"] = folder
		out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", folderArgs())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		row := e.compositions[stringOf(out["compositionId"])]
		if row["status"] != "ready" || row["folderId"] != "folder-1" {
			t.Fatalf("%s: the composition did not file into the folder: %v", name, row)
		}
		if len(e.files) != 1 {
			t.Fatalf("%s: want one file, got %d", name, len(e.files))
		}
		for _, f := range e.files {
			if f["folderId"] != "folder-1" {
				t.Fatalf("%s: the file was filed under %v, want folder-1", name, f["folderId"])
			}
		}
	}
}

// The folder is read when one is named and not otherwise, as the caller (the
// fixture refuses a read under any other actor), and FRESH: a folder archived a
// moment ago on another replica must not be filed into because this node's cache
// has not heard.
func TestMaterializeReadsTheFolderFreshAndOnlyWhenOneIsNamed(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.folders["folder-1"] = map[string]any{"id": "folder-1", "name": "Reports"}
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", folderArgs()); err != nil {
		t.Fatal(err)
	}
	if len(e.folderReads) != 1 || !e.folderReads[0] {
		t.Fatalf("the folder must be read once, fresh: %v", e.folderReads)
	}

	none, ne, _ := materializeFixture(t)
	if _, err := none.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(pure.FormatMarkdown)); err != nil {
		t.Fatal(err)
	}
	if len(ne.folderReads) != 0 {
		t.Fatalf("a request that names no folder read one: %v", ne.folderReads)
	}
}

// A repeat finds the composition its first call made. If the folder has been
// archived since, that repeat must still answer with the composition: the first
// call was accepted, the file exists, and a refusal now would only hide it.
func TestMaterializeRepeatedRequestDoesNotAskAboutTheFolderAgain(t *testing.T) {
	i, e, _ := materializeFixture(t)
	i.SetGoalOpener(&directMaterializeGoal{})
	e.folders["folder-1"] = map[string]any{"id": "folder-1", "name": "Reports"}
	a := folderArgs()
	a.RequestId = "req-1"
	first, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	e.folders["folder-1"]["archived"] = true
	again, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatalf("a repeat of an accepted request was refused: %v", err)
	}
	if again["compositionId"] != first["compositionId"] || len(e.compositions) != 1 || len(e.folderReads) != 1 {
		t.Fatalf("the repeat did not return the first composition without a second folder read: first=%v again=%v rows=%d reads=%v", first, again, len(e.compositions), e.folderReads)
	}
}

// A TEMPLATE THE CALLER HOLDS IS NOT NECESSARILY ONE THAT CAN MAKE THE FILE.
//
// composeTemplateById answers any template its owner holds, and the render step used to apply
// whatever it was given: an archived one (a binding somebody retired) and one that makes another
// kind of file than the request asked for (a PDF template under a Markdown file) were applied
// without a word. So the request is refused, before anything is written, when its template is
// archived or makes another format. A template that names no format fits any, as it always did.

const templateArchived = "That template is archived. Restore it or choose another."

func templateArgs(format pure.Format) materializeArgs {
	a := materializeDraft()
	a.Format = format
	a.TemplateId = "template-1"
	return a
}

func TestMaterializeRefusesAnArchivedTemplateBeforeWritingAnything(t *testing.T) {
	for where, ctx := range map[string]context.Context{"in a run": nestedMaterializeContext(), "standalone": materializeContext()} {
		i, e, u := materializeFixture(t)
		e.template = map[string]any{"id": "template-1", "name": "House style", "format": "markdown", "archived": true}
		_, err := i.materialize(ctx, "u-alice", "", templateArgs(pure.FormatMarkdown))
		// The sentence is the person's: the OS shows it as it is.
		if err == nil || err.Error() != templateArchived {
			t.Fatalf("%s: want the archived-template refusal verbatim, got %v", where, err)
		}
		if len(e.compositions) != 0 || len(e.inputs) != 0 || len(e.files) != 0 || u.calls != 0 {
			t.Fatalf("%s: refusal wrote rows: %v %v %v uploads=%d", where, e.compositions, e.inputs, e.files, u.calls)
		}
		// Reads only: a step looks for the composition it may already have made, and the template is read.
		for _, made := range e.calls {
			if made != "composeTemplateById" && made != "compositionExecutionById" {
				t.Fatalf("%s: the refusal went on to %s: %v", where, made, e.calls)
			}
		}
	}
}

// The refusal names the KIND of file the person asked for (the words of the Materializer's kind choice), not
// the format it is made in: a package is HTML pages and an email template is JSON, and neither is what they chose.
func TestMaterializeRefusesATemplateThatMakesAnotherKindOfFile(t *testing.T) {
	for name, tc := range map[string]struct {
		template   string
		request    pure.Format
		deployable string
		email      bool
		want       string
	}{
		"a PDF template under a Markdown file":      {template: "pdf", request: pure.FormatMarkdown, want: "That template makes PDF files. Choose a template for Markdown."},
		"a web page template under a PDF":           {template: "html", request: pure.FormatPDF, want: "That template makes Web page files. Choose a template for PDF."},
		"a Word template under a spreadsheet":       {template: "docx", request: pure.FormatCSV, want: "That template makes Word document files. Choose a template for Spreadsheet (CSV)."},
		"a text template under data":                {template: "txt", request: pure.FormatJSON, want: "That template makes Plain text files. Choose a template for Data (JSON)."},
		"a Markdown template under a static site":   {template: "markdown", request: pure.FormatHTML, deployable: "static", want: "That template makes Markdown files. Choose a template for Static site package."},
		"a PDF template under a single-page app":    {template: "pdf", request: pure.FormatHTML, deployable: "spa", want: "That template makes PDF files. Choose a template for Single-page app package."},
		"a text template under a storefront":        {template: "txt", request: pure.FormatHTML, deployable: "shopify_storefront", want: "That template makes Plain text files. Choose a template for Shopify storefront package."},
		"a Markdown template under an email":        {template: "markdown", request: pure.FormatJSON, email: true, want: "That template makes Markdown files. Choose a template for Email template."},
		"a web page template under an email":        {template: "html", request: pure.FormatJSON, email: true, want: "That template makes Web page files. Choose a template for Email template."},
		"a template of a format this app can't say": {template: "audio", request: pure.FormatMarkdown, want: "That template makes audio files. Choose a template for Markdown."},
	} {
		for where, ctx := range map[string]context.Context{"in a run": nestedMaterializeContext(), "standalone": materializeContext()} {
			i, e, u := materializeFixture(t)
			e.template = map[string]any{"id": "template-1", "name": "House style", "format": tc.template}
			a := templateArgs(tc.request)
			a.DeployableKind = tc.deployable
			if tc.email {
				e.account = map[string]any{"id": "client", "status": "active"}
				a.OutputKind, a.AccountIds = "email_template", []string{"client"}
			}
			_, err := i.materialize(ctx, "u-alice", "", a)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("%s %s: want %q, got %v", name, where, tc.want, err)
			}
			if len(e.compositions) != 0 || len(e.inputs) != 0 || len(e.files) != 0 || u.calls != 0 {
				t.Fatalf("%s %s: refusal wrote rows: %v %v %v uploads=%d", name, where, e.compositions, e.inputs, e.files, u.calls)
			}
		}
	}
}

func TestMaterializeAcceptsATemplateThatCanMakeTheFile(t *testing.T) {
	for name, tc := range map[string]struct {
		row        map[string]any
		request    pure.Format
		deployable string
	}{
		"a template of the same format":                   {row: map[string]any{"format": "markdown"}, request: pure.FormatMarkdown},
		"a template that has no format at all":            {row: map[string]any{}, request: pure.FormatPDF},
		"a template whose format is empty":                {row: map[string]any{"format": ""}, request: pure.FormatCSV},
		"a template that is not archived":                 {row: map[string]any{"format": "markdown", "archived": false}, request: pure.FormatMarkdown},
		"a web page template, for a package's HTML pages": {row: map[string]any{"format": "html"}, request: pure.FormatHTML, deployable: "static"},
	} {
		// Standalone with a goal opener: the request is accepted and nothing runs, which is where the template is checked.
		i, e, _ := materializeFixture(t)
		i.SetGoalOpener(&directMaterializeGoal{})
		e.template = map[string]any{"id": "template-1", "name": "House style"}
		for key, value := range tc.row {
			e.template[key] = value
		}
		a := templateArgs(tc.request)
		a.DeployableKind = tc.deployable
		out, err := i.materialize(materializeContext(), "u-alice", "", a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if row := e.compositions[stringOf(out["compositionId"])]; row == nil || row["templateId"] != "template-1" {
			t.Fatalf("%s: the composition does not name the template: %v", name, row)
		}
	}
}

// The template is read when one is named and not otherwise, as the caller (the fixture refuses a read under
// any other actor), and FRESH: a template archived a moment ago on another replica must not be applied
// because this node's cache has not heard.
func TestMaterializeReadsTheTemplateFreshAndOnlyWhenOneIsNamed(t *testing.T) {
	i, e, _ := materializeFixture(t)
	i.SetGoalOpener(&directMaterializeGoal{})
	e.template = map[string]any{"id": "template-1", "name": "House style"}
	if _, err := i.materialize(materializeContext(), "u-alice", "", templateArgs(pure.FormatMarkdown)); err != nil {
		t.Fatal(err)
	}
	if len(e.templateReads) != 1 || !e.templateReads[0] {
		t.Fatalf("the template must be read once, fresh: %v", e.templateReads)
	}

	none, ne, _ := materializeFixture(t)
	none.SetGoalOpener(&directMaterializeGoal{})
	if _, err := none.materialize(materializeContext(), "u-alice", "", materializeDraft()); err != nil {
		t.Fatal(err)
	}
	if len(ne.templateReads) != 0 {
		t.Fatalf("a request that names no template read one: %v", ne.templateReads)
	}
}

// A repeat finds the composition its first call made. If the template has been archived since, that repeat
// must still answer with the composition: the first call was accepted and a refusal now would only hide it.
func TestMaterializeRepeatedRequestDoesNotAskAboutTheTemplateAgain(t *testing.T) {
	i, e, _ := materializeFixture(t)
	i.SetGoalOpener(&directMaterializeGoal{})
	e.template = map[string]any{"id": "template-1", "name": "House style", "format": "markdown"}
	a := templateArgs(pure.FormatMarkdown)
	a.RequestId = "req-1"
	first, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	e.template["archived"] = true
	again, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatalf("a repeat of an accepted request was refused: %v", err)
	}
	if again["compositionId"] != first["compositionId"] || len(e.compositions) != 1 || len(e.templateReads) != 1 {
		t.Fatalf("the repeat did not return the first composition without a second template read: first=%v again=%v rows=%d reads=%v", first, again, len(e.compositions), e.templateReads)
	}
}

// A template the caller cannot read is not this check's to refuse: the render step has always said so
// ("that template is not readable by you"), and the composition it fails is the record of it.
func TestMaterializeLeavesAnUnreadableTemplateToTheRenderStep(t *testing.T) {
	i, e, _ := materializeFixture(t)
	_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", templateArgs(pure.FormatMarkdown))
	if err == nil || !strings.Contains(err.Error(), "that template is not readable by you") {
		t.Fatalf("an unreadable template: want the render step's refusal, got %v", err)
	}
	for _, row := range e.compositions {
		if row["status"] != "failed" {
			t.Fatalf("the composition was not failed by the render step: %v", row)
		}
	}
	if len(e.compositions) != 1 {
		t.Fatalf("want the failed composition as the record, got %d rows", len(e.compositions))
	}
}

func TestParseMaterializeArgsStripsAMatchingExtensionAndRefusesPaths(t *testing.T) {
	a, err := parseMaterializeArgs(map[string]any{"name": "Plan.md", "format": "markdown"})
	if err != nil || a.Name != "Plan" {
		t.Fatalf("name = %q err = %v, want Plan", a.Name, err)
	}
	if _, err := parseMaterializeArgs(map[string]any{"name": "a/b", "format": "markdown"}); err == nil {
		t.Fatal("a name with a slash must be refused")
	}

	for _, tc := range []struct{ name, format, want string }{
		{"Plan.MD", "markdown", "Plan"},       // case does not matter
		{"Plan.markdown", "markdown", "Plan"}, // the format's own name is an extension people type too
		{"Plan.md ", "markdown", "Plan"},      // the name is trimmed before and after
		{"notes.txt", "txt", "notes"},         // txt's extension is its name
		{"data.CSV", "csv", "data"},
		{"page.html", "html", "page"},
		{"Plan.txt", "markdown", "Plan.txt"}, // a different format's extension is part of the name
		{"v1.2 notes", "markdown", "v1.2 notes"},
		{"Plan.md.md", "markdown", "Plan.md"}, // only one trailing extension is the format's
	} {
		got, err := parseMaterializeArgs(map[string]any{"name": tc.name, "format": tc.format})
		if err != nil || got.Name != tc.want {
			t.Fatalf("name %q as %s = %q (err %v), want %q", tc.name, tc.format, got.Name, err, tc.want)
		}
	}

	// A person reads these refusals where they typed the name (the OS shows the
	// cluster's sentence beside the control), so each is exactly one sentence that
	// says what to do, with no package name in front of it.
	for _, name := range []string{"a\\b", "/etc/passwd", "folder/Plan.md", "Notes 10/11"} {
		_, err := parseMaterializeArgs(map[string]any{"name": name, "format": "markdown"})
		if err == nil || err.Error() != "Enter a name without slashes." {
			t.Fatalf("name %q carries a slash: refusal = %v, want %q", name, err, "Enter a name without slashes.")
		}
	}
	// Stripping must not turn a name into nothing, and a name that was nothing to
	// begin with is the same refusal.
	for _, tc := range []struct{ name, format string }{
		{".md", "markdown"}, {".MARKDOWN", "markdown"}, {" .md ", "markdown"}, {".csv", "csv"}, {"", "markdown"}, {"   ", "txt"},
	} {
		_, err := parseMaterializeArgs(map[string]any{"name": tc.name, "format": tc.format})
		if err == nil || err.Error() != "Enter a name for the file." {
			t.Fatalf("name %q as %s has no stem: refusal = %v, want %q", tc.name, tc.format, err, "Enter a name for the file.")
		}
	}
}

func TestParseMaterializeArgsReadsTheRequestId(t *testing.T) {
	a, err := parseMaterializeArgs(map[string]any{"name": "Plan", "format": "markdown", "requestId": "  req-1 "})
	if err != nil || a.RequestId != "req-1" {
		t.Fatalf("requestId = %q err = %v, want req-1", a.RequestId, err)
	}
	a, err = parseMaterializeArgs(map[string]any{"name": "Plan", "format": "markdown", "requestId": "   "})
	if err != nil || a.RequestId != "" {
		t.Fatalf("a blank requestId must read as none, got %q (err %v)", a.RequestId, err)
	}
}

// A name that has no extension to strip, and a request id that is absent, must
// leave the identity of an in-run materialization exactly as it was: the
// composition id of a step is derived from its arguments, so a field that
// serialised as an empty string would give every step already in flight a new
// id and file its output twice on the next replay. (A name that DOES end in its
// format's extension is keyed by its cleaned stem, which re-keys such a step
// once; parseMaterializeArgs says why that is accepted.)
func TestMaterializeArgsWithoutARequestIdSerialiseAsBefore(t *testing.T) {
	raw, err := json.Marshal(materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "RequestId") {
		t.Fatalf("an absent request id changed the saved request, and with it every in-run composition id: %s", raw)
	}
}

// legacyMaterializeArgs is materializeArgs exactly as it was before RequestId
// existed: same fields, same order, same tags. A nested materialization's
// composition id is a hash of its arguments' JSON, so this is the shape every
// step already in flight was keyed by.
type legacyMaterializeArgs struct {
	Name           string
	Statement      string
	Format         pure.Format
	OutputKind     string
	Sources        []SourceRef
	Draft          string
	SectionKeys    []string `json:",omitempty"`
	TemplateId     string
	FolderId       string
	AccountIds     []string
	DeployableKind string
	RecipeId       string
	Ceilings       map[string]any
}

// A nested request that carries no request id must be keyed exactly as it was
// before the field existed. If the new field serialised even as an empty
// string, every run in flight at the upgrade would compute a different
// composition id on its next replay or retry, miss the composition it had
// already written, and file its output a second time.
func TestMaterializeNestedCompositionIdIsUnchangedByTheRequestIdField(t *testing.T) {
	a := materializeDraft()
	a.Sources = []SourceRef{{Kind: KindQuery, Ref: "sourceRows()", Label: "Invoices"}}
	a.SectionKeys = nil
	a.AccountIds = []string{"client-a"}
	a.FolderId = "folder-1"
	a.Ceilings = map[string]any{"maxRetries": 2}
	before, err := json.Marshal(legacyMaterializeArgs{
		Name: a.Name, Statement: a.Statement, Format: a.Format, OutputKind: a.OutputKind, Sources: a.Sources,
		Draft: a.Draft, SectionKeys: a.SectionKeys, TemplateId: a.TemplateId, FolderId: a.FolderId,
		AccountIds: a.AccountIds, DeployableKind: a.DeployableKind, RecipeId: a.RecipeId, Ceilings: a.Ceilings,
	})
	if err != nil {
		t.Fatal(err)
	}
	// nestedMaterializeContext() runs as run "v1:work:run:nexus", step "file".
	want := stableMaterializeID(memql.BareShortId("v1:work:run:nexus"), "file", string(before))

	i, e, _ := materializeFixture(t)
	e.source = map[string]any{"id": "invoice-1", "amount": 100}
	e.folders["folder-1"] = map[string]any{"id": "folder-1", "name": "Reports"}
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	if out["compositionId"] != want {
		t.Fatalf("the composition id of a nested request changed: got %v, want %s (what the arguments' JSON hashed to before RequestId existed)", out["compositionId"], want)
	}
}

func TestMaterializeSameRequestIdReturnsTheSameComposition(t *testing.T) {
	i, e, u := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	a := blankArgs(pure.FormatMarkdown)
	a.RequestId = "req-1"
	first, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	if first["compositionId"] != again["compositionId"] || len(e.compositions) != 1 || opener.calls != 1 {
		t.Fatalf("repeat started a second composition: first=%v again=%v rows=%d opens=%d uploads=%d", first, again, len(e.compositions), opener.calls, u.calls)
	}
	// The repeat answers with where the first one went, not with an empty shell.
	for _, key := range []string{"goalId", "runId", "format", "status"} {
		if stringOf(again[key]) == "" || again[key] != stringOf(e.compositions[stringOf(first["compositionId"])][key]) {
			t.Fatalf("the repeat's %s = %v, want the composition's own", key, again[key])
		}
	}
	if len(e.inputs) != 1 {
		t.Fatalf("the repeat captured a second input snapshot: %d", len(e.inputs))
	}
}

func TestMaterializeWithoutARequestIdStartsANewCompositionEachTime(t *testing.T) {
	i, e, _ := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	a := blankArgs(pure.FormatMarkdown)
	first, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	if first["compositionId"] == second["compositionId"] || len(e.compositions) != 2 || opener.calls != 2 {
		t.Fatalf("two requests with no request id must be two compositions: first=%v second=%v rows=%d opens=%d", first, second, len(e.compositions), opener.calls)
	}
}

// The request id is the caller's own, so it can only ever name that caller's
// request: the same text from another person, or a different text from the same
// person, is another composition. Were it global, one person's id would return
// (or collide with) somebody else's file.
func TestMaterializeRequestIdIsScopedToTheCallerAndToTheRequest(t *testing.T) {
	i, e, _ := materializeFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	a := blankArgs(pure.FormatMarkdown)
	a.RequestId = "shared-id"
	mine, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	// The engine fake admits one actor, so the second caller is a second
	// derivation rather than a second session: what matters is the id.
	theirs, err := i.materialize(materializeContext(), "u-bob", "", a)
	if err != nil {
		t.Fatal(err)
	}
	a.RequestId = "another-id"
	another, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[any]bool{mine["compositionId"]: true, theirs["compositionId"]: true, another["compositionId"]: true}
	if len(ids) != 3 || len(e.compositions) != 3 || opener.calls != 3 {
		t.Fatalf("request ids collided: mine=%v theirs=%v another=%v rows=%d", mine, theirs, another, len(e.compositions))
	}
}

// A request id only makes a start idempotent. Inside a run the composition
// already has a stable identity of its own (run, step, arguments), and that
// must stay the identity the replay machinery finds.
func TestMaterializeRequestIdDoesNotChangeAnInRunComposition(t *testing.T) {
	i, e, _ := materializeFixture(t)
	a := materializeDraft()
	a.RequestId = "req-in-run"
	first, err := i.materialize(nestedMaterializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := New(e, nil).materialize(nestedMaterializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	if first["compositionId"] != again["compositionId"] || len(e.compositions) != 1 {
		t.Fatalf("an in-run replay with a request id found another composition: first=%v again=%v rows=%d", first, again, len(e.compositions))
	}
}

func TestBlankMeansThereIsNothingToComposeFrom(t *testing.T) {
	if !blankArgs(pure.FormatMarkdown).blank() {
		t.Fatal("a name and a format alone are a blank request")
	}
	// Where the file goes and the limits its run keeps are not what it is made from.
	where := blankArgs(pure.FormatMarkdown)
	where.FolderId, where.AccountIds, where.Ceilings, where.Draft = "folder-1", []string{"account-1"}, map[string]any{"maxRetries": 1}, "  \n "
	if !where.blank() {
		t.Fatal("a folder, account tags, ceilings and a whitespace-only draft are still a blank request")
	}
	for name, with := range map[string]func(*materializeArgs){
		"statement":    func(a *materializeArgs) { a.Statement = "Write the agenda" },
		"draft":        func(a *materializeArgs) { a.Draft = "Some words" },
		"section keys": func(a *materializeArgs) { a.SectionKeys = []string{"chapter-1"} },
		"sources":      func(a *materializeArgs) { a.Sources = []SourceRef{{Kind: KindQuery, Ref: "sourceRows()"}} },
		"template":     func(a *materializeArgs) { a.TemplateId = "template-1" },
		"recipe":       func(a *materializeArgs) { a.RecipeId = "recipe-1" },
		"output kind":  func(a *materializeArgs) { a.OutputKind = "email_template" },
		"deployable":   func(a *materializeArgs) { a.DeployableKind = "spa" },
	} {
		a := blankArgs(pure.FormatMarkdown)
		with(&a)
		if a.blank() {
			t.Fatalf("a request with %s is not blank", name)
		}
	}
}

// The extension is added once, on filing, and a path is refused before anything
// is written -- through the capability, the way a caller meets both.
func TestMaterializeCapabilityCleansTheNameBeforeFiling(t *testing.T) {
	i, e, _ := materializeFixture(t)
	if _, err := i.handleMaterialize(nestedMaterializeContext(), map[string]any{"name": "Plan.md", "format": "markdown"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(e.files) != 1 || len(e.compositions) != 1 {
		t.Fatalf("one request made %d files and %d compositions", len(e.files), len(e.compositions))
	}
	for _, f := range e.files {
		if f["name"] != "Plan.md" {
			t.Fatalf("filed name = %v, want Plan.md and never Plan.md.md", f["name"])
		}
	}
	for _, row := range e.compositions {
		if row["name"] != "Plan" {
			t.Fatalf("composition name = %v, want the stem Plan", row["name"])
		}
	}

	refused, refusedEngine, _ := materializeFixture(t)
	if _, err := refused.handleMaterialize(nestedMaterializeContext(), map[string]any{"name": "notes/Plan", "format": "markdown"}, 0); err == nil || err.Error() != "Enter a name without slashes." {
		t.Fatalf("a name with a slash: refusal = %v, want %q", err, "Enter a name without slashes.")
	}
	if _, err := refused.handleMaterialize(nestedMaterializeContext(), map[string]any{"name": ".md", "format": "markdown"}, 0); err == nil || err.Error() != "Enter a name for the file." {
		t.Fatalf("a name that is only the extension: refusal = %v, want %q", err, "Enter a name for the file.")
	}
	if len(refusedEngine.calls) != 0 {
		t.Fatalf("the refused name still reached the engine: %v", refusedEngine.calls)
	}
}

// failOnce fails the first call of one construct, the way a store that blinked
// does, and passes everything else through.
type failOnce struct {
	*materializeEngine
	construct string
	message   string
	fired     bool
}

func (f *failOnce) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if name, _ := materializeCall(f.t, query); name == f.construct && !f.fired {
		f.fired = true
		return nil, errors.New(f.message)
	}
	return f.materializeEngine.Execute(ctx, query)
}

// A blank request that is interrupted AFTER its render was captured (the
// snapshot a sibling replica resumes from) must still come back blank. The
// resumed pass finds a saved recipe, so it restores that recipe instead of
// taking the blank branch; the blank-ness therefore has to travel on the recipe
// itself rather than in a flag only the first pass set -- otherwise the person
// who asked for an empty page is handed a file with a front-matter block and a
// heading, and the editor never shows its empty-document page.
func TestMaterializeBlankRecoveryStillFilesTheBlankDocument(t *testing.T) {
	e := newMaterializeEngine(t)
	flaky := &failOnce{materializeEngine: e, construct: "createLibraryFile", message: "connection reset by peer"}
	u := &materializeUploader{}
	first := New(flaky, nil)
	first.SetUploader(u, "files")
	if _, err := first.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(pure.FormatMarkdown)); err == nil || strings.Contains(err.Error(), terminalFailureCode) {
		t.Fatalf("the interruption must stay resumable, got %v", err)
	}
	for _, row := range e.compositions {
		if row["status"] != "draft" {
			t.Fatalf("an interrupted blank request must stay resumable: %v", row)
		}
	}
	if len(e.files) != 0 {
		t.Fatalf("the interruption happened before filing, but %d files exist", len(e.files))
	}

	sibling := New(flaky, nil)
	sibling.SetUploader(u, "files")
	sibling.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		t.Fatal("resuming a blank request reached the model")
		return ComposeReply{}, nil
	}))
	out, err := sibling.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(pure.FormatMarkdown))
	if err != nil {
		t.Fatal(err)
	}
	if string(u.bytes) != "\n" {
		t.Fatalf("the resumed blank request filed %q, want the single newline an editor opens as an empty page", u.bytes)
	}
	if row := e.compositions[stringOf(out["compositionId"])]; row["status"] != "ready" || len(e.files) != 1 {
		t.Fatalf("the resumed blank request did not finish with its one file: row=%v files=%d", row, len(e.files))
	}
}

// The extension is added once. parseMaterializeArgs drops a matching one from
// what a person typed, and outputFileName is the second line for a name that
// reaches materialize another way (a recipe run names its own output).
func TestOutputFileNameAddsTheExtensionOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format pure.Format
		want   string
	}{
		{"Plan", pure.FormatMarkdown, "Plan.md"},
		{"Plan.md", pure.FormatMarkdown, "Plan.md"},
		{"Plan.MD", pure.FormatMarkdown, "Plan.MD"},
		{"notes", pure.FormatText, "notes.txt"},
		{"notes.txt", pure.FormatText, "notes.txt"},
		{"data", pure.FormatCSV, "data.csv"},
		{"data.csv", pure.FormatCSV, "data.csv"},
	} {
		if got := outputFileName(tc.name, tc.format, ""); got != tc.want {
			t.Fatalf("outputFileName(%q, %s) = %q, want %q", tc.name, tc.format, got, tc.want)
		}
	}
}

// A stem longer than 120 bytes is cut, and the cut is on a character. It used to be at byte 120 wherever
// that fell, so a name whose 120th byte was in the middle of a character was filed with the start of that
// character at its end: a file name that is not text, which a client shows with a replacement character.
// The cut is now at the last character that ends at or below the limit.
func TestSanitiseFileStemIsCutOnACharacterNotInsideOne(t *testing.T) {
	const e, ri, face = "\u00e9", "\u65e5", "\U0001F600" // 2, 3 and 4 bytes
	repeat := strings.Repeat
	for name, tc := range map[string]struct{ typed, want string }{
		"exactly the limit is kept whole":                {repeat("a", 120), repeat("a", 120)},
		"one over the limit is cut":                      {repeat("a", 121), repeat("a", 120)},
		"two-byte characters ending on the limit":        {repeat(e, 60), repeat(e, 60)},
		"two-byte characters cut on the limit":           {repeat(e, 61), repeat(e, 60)},
		"a two-byte character across the limit":          {"a" + repeat(e, 61), "a" + repeat(e, 59)},
		"a three-byte character across the limit":        {"ab" + repeat(ri, 50), "ab" + repeat(ri, 39)},
		"a three-byte character one byte earlier":        {"abcd" + repeat(ri, 50), "abcd" + repeat(ri, 38)},
		"four-byte characters ending on the limit":       {repeat(face, 100), repeat(face, 30)},
		"a four-byte character across the limit (1)":     {"a" + repeat(face, 40), "a" + repeat(face, 29)},
		"a four-byte character across the limit (2)":     {"ab" + repeat(face, 40), "ab" + repeat(face, 29)},
		"a four-byte character across the limit (3)":     {"abc" + repeat(face, 40), "abc" + repeat(face, 29)},
		"the space the cut leaves at the end is trimmed": {repeat("a", 118) + " " + repeat(e, 5), repeat("a", 118)},
		"bytes that are not text become U+FFFD whole":    {repeat("\xff", 100), repeat("\ufffd", 40)},
	} {
		got := sanitiseFileStem(tc.typed)
		if got != tc.want {
			t.Errorf("%s: sanitiseFileStem(%d bytes) = %q (%d bytes), want %q (%d bytes)", name, len(tc.typed), got, len(got), tc.want, len(tc.want))
		}
		if !utf8.ValidString(got) || len(got) > maxFileStemBytes {
			t.Errorf("%s: the stem is %d bytes and valid = %v: it must be text of at most %d bytes", name, len(got), utf8.ValidString(got), maxFileStemBytes)
		}
	}

	// Whatever character falls across the limit, and wherever in the last four bytes it starts, the stem
	// is text, within the limit, and the longest such: it gives up only the bytes of the one character.
	for _, character := range []string{e, ri, face} {
		for lead := 0; lead < 8; lead++ {
			typed := repeat("a", lead) + repeat(character, 80)
			got := sanitiseFileStem(typed)
			if !utf8.ValidString(got) || len(got) > maxFileStemBytes || len(got) <= maxFileStemBytes-len(character) {
				t.Errorf("%d plain bytes then %q x 80: stem is %d bytes (valid = %v), want a whole number of characters within %d bytes and more than %d",
					lead, character, len(got), utf8.ValidString(got), maxFileStemBytes, maxFileStemBytes-len(character))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Described Markdown: filed blank, and the person's client hands it on
// (epic #6023, task #6025)
// ---------------------------------------------------------------------------
//
// A Markdown request that only DESCRIBES the document is filed as the empty
// page. The description is not lost: it stays on the composition, and the
// person's own client gives it to the Library's revision workflow, which a work
// run is forbidden to do on a person's behalf (integrations/library/revision.go).
// That is why these requests are made the way a person's client makes them --
// accepted outside any run by one replica, executed by another that adopts the
// run it opened -- and not through nestedMaterializeContext.

const describedStatement = "A one-page launch brief for the autumn release"

// describedArgs is a Markdown file asked for by describing it: a statement and
// nothing to compose from.
func describedArgs() materializeArgs {
	return materializeArgs{Name: "Launch brief", Format: pure.FormatMarkdown, Statement: describedStatement}
}

// forbidComposer is a composer that fails the test when the model is reached.
func forbidComposer(t *testing.T) materializeComposerFunc {
	return func(context.Context, ComposeRequest) (ComposeReply, error) {
		t.Helper()
		t.Fatal("the model was reached for a file that is filed blank")
		return ComposeReply{}, nil
	}
}

// acceptAsAPerson sends a request the way a person's client does: outside any
// work run. The replica that accepts opens the goal and returns; nothing is
// dispatched from there, so the saved request waits for a replica to adopt the
// run. It returns the composition and the input the adopting run is given.
func acceptAsAPerson(t *testing.T, accepting *Integration, a materializeArgs) (string, map[string]any) {
	t.Helper()
	opener := &directMaterializeGoal{}
	accepting.SetGoalOpener(opener)
	out, err := accepting.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	return stringOf(out["compositionId"]), opener.goal.Input
}

// adoptTheRun executes an accepted request on the given replica, under the run
// the accepting replica's goal opened.
func adoptTheRun(replica *Integration, input map[string]any) error {
	ctx := common.ContextWithRun(materializeContext(), common.RunContext{RunId: "v1:work:run:direct", GoalId: "v1:work:goal:direct", StepKey: "compose", OwnerUserId: "u-alice"})
	_, err := replica.handleExecute(ctx, input, 0)
	return err
}

// anotherReplica is a second node over the same engine: none of the first
// node's local state, and the composer the caller gives it.
func anotherReplica(e Engine, u *materializeUploader, composer Composer) *Integration {
	r := New(e, nil)
	r.SetUploader(u, "files")
	if composer != nil {
		r.SetComposer(composer)
	}
	return r
}

func TestDescribedMarkdownIsFiledBlankWithoutAModelCall(t *testing.T) {
	i, e, u := materializeFixture(t)
	i.SetComposer(forbidComposer(t))
	compositionId, input := acceptAsAPerson(t, i, describedArgs())

	// The description is on record the moment the request is accepted, before
	// anything has run: if the client never hands it on, it is still there.
	if row := e.compositions[compositionId]; row["statement"] != describedStatement || row["status"] != "draft" {
		t.Fatalf("an accepted request must already carry its statement: %v", row)
	}

	if err := adoptTheRun(anotherReplica(e, u, forbidComposer(t)), input); err != nil {
		t.Fatal(err)
	}
	row := e.compositions[compositionId]
	if row["status"] != "ready" || stringOf(row["outputFileId"]) == "" {
		t.Fatalf("the described request did not complete: %v", row)
	}
	if row["statement"] != describedStatement {
		t.Fatalf("the statement on the composition = %q, want the request's own", row["statement"])
	}
	// One newline: no front matter, no heading, so the editor shows its empty page
	// and its "Describe document" call to action.
	if u.calls != 1 || string(u.bytes) != "\n" {
		t.Fatalf("described markdown must be filed as a single newline, got %q after %d uploads", u.bytes, u.calls)
	}
	if row["provenanceEmbedded"] != false || row["provenanceNote"] != "Created blank. Its history is kept on its composition, not in the file." {
		t.Fatalf("provenance = embedded %v, note %q", row["provenanceEmbedded"], row["provenanceNote"])
	}
	if len(e.files) != 1 {
		t.Fatalf("a described file is one file with no source package, got %d", len(e.files))
	}
	if f := e.files[stringOf(row["outputFileId"])]; f["name"] != "Launch brief.md" {
		t.Fatalf("filed name = %v, want Launch brief.md", f["name"])
	}
}

func TestDescribedMarkdownWithLibrarySourcesStillFilesBlank(t *testing.T) {
	i, e, u := materializeFixture(t)
	// A reference the person attached, which their client hands on with the
	// description. It resolves like any other source.
	e.files["notes-file"] = map[string]any{"name": "meeting-notes.md", "mimeType": "text/markdown"}
	a := describedArgs()
	a.Sources = []SourceRef{{Kind: KindLibraryFile, Ref: "notes-file", Label: "Meeting notes"}}
	compositionId, input := acceptAsAPerson(t, i, a)

	if err := adoptTheRun(anotherReplica(e, u, forbidComposer(t)), input); err != nil {
		t.Fatal(err)
	}
	row := e.compositions[compositionId]
	if row["status"] != "ready" || u.calls != 1 || string(u.bytes) != "\n" {
		t.Fatalf("described markdown with a source must still be filed as a single newline: row=%v bytes=%q uploads=%d", row, u.bytes, u.calls)
	}
	// The source is the composition's provenance, so it stays on the record.
	recorded, _ := json.Marshal(row["sources"])
	if !strings.Contains(string(recorded), `"ref":"notes-file"`) || !strings.Contains(string(recorded), `"label":"Meeting notes"`) {
		t.Fatalf("the source was dropped from the composition: %s", recorded)
	}
	// The attached file and the one new file; nothing else is filed for a source.
	if len(e.files) != 2 || stringOf(row["sourceFileId"]) != "" {
		t.Fatalf("a described file with sources files only its one output: files=%d sourceFileId=%v", len(e.files), row["sourceFileId"])
	}
}

// THE PATH IS DECIDED BEFORE ANY CONTENT IS READ. A described file is filed empty, so
// nothing it names is read: not when the request is accepted (it used to be, for a
// provenance the empty file never used, and a reference nobody could read refused a
// request that needed none of them) and not on the replica that executes the run.
func TestDescribedMarkdownReadsNoReferenceContent(t *testing.T) {
	data := []byte("Launch notes: ship on the first Tuesday")
	notes := func() map[string]any {
		return map[string]any{"name": "launch-notes.md", "blobUrl": "library/u-alice/reference-file/launch-notes.md", "ownerUserId": "u-alice", "sha256": (pure.Result{Bytes: data}).SHA256(), "size": len(data)}
	}
	described := func() materializeArgs {
		a := describedArgs()
		a.Sources = []SourceRef{{Kind: KindLibraryFile, Ref: "reference-file", Label: "Launch notes", Content: true}}
		return a
	}

	t.Run("a node with no way to read references accepts it", func(t *testing.T) {
		i, e, u := materializeFixture(t)
		e.files["reference-file"] = notes()
		compositionId, input := acceptAsAPerson(t, i, described())
		if err := adoptTheRun(anotherReplica(e, u, forbidComposer(t)), input); err != nil {
			t.Fatal(err)
		}
		if row := e.compositions[compositionId]; row["status"] != "ready" || string(u.bytes) != "\n" {
			t.Fatalf("the described request did not file blank: row=%v bytes=%q", row, u.bytes)
		}
	})

	t.Run("a node that can read them reads nothing, at accept or on the replica that executes it", func(t *testing.T) {
		i, e, u := materializeFixture(t)
		reader := &referenceReader{data: data}
		i.SetSourceDownloader(reader)
		e.files["reference-file"] = notes()
		compositionId, input := acceptAsAPerson(t, i, described())
		if reader.calls != 0 {
			t.Fatalf("a described request reads no reference, but the accepting node read one %d time(s)", reader.calls)
		}
		saved, _ := json.Marshal(e.inputs[compositionId]["request"])
		if strings.Contains(string(saved), "ship on the first Tuesday") {
			t.Fatalf("the saved input holds the text of a file nothing read: %s", saved)
		}
		var snapshot executionRequest
		if err := json.Unmarshal(saved, &snapshot); err != nil || !snapshot.Described || len(snapshot.Resolved) != 1 || len(snapshot.Resolved[0].Files) != 0 {
			t.Fatalf("the saved input is not a described request that read nothing: described=%v resolved=%+v (%v)", snapshot.Described, snapshot.Resolved, err)
		}

		if err := adoptTheRun(anotherReplica(e, u, forbidComposer(t)), input); err != nil {
			t.Fatal(err)
		}
		if reader.calls != 0 || string(u.bytes) != "\n" || e.compositions[compositionId]["status"] != "ready" {
			t.Fatalf("the empty file should need no reference read: reads=%d bytes=%q row=%v", reader.calls, u.bytes, e.compositions[compositionId])
		}
		// The file is still the composition's provenance: it is named in its sources.
		recorded, _ := json.Marshal(e.compositions[compositionId]["sources"])
		if !strings.Contains(string(recorded), `"ref":"reference-file"`) {
			t.Fatalf("the picked file is not among the composition's sources: %s", recorded)
		}
	})
}

// A file the Library cannot attach to a review comment cannot go to the revision workflow,
// so the request goes to the composer, which reads it -- every picked file is used, and
// none is dropped. Only now is a reference's content read: once, when the request is
// accepted, and the replica that executes the run composes from that snapshot.
func TestDescribedMarkdownWithAFileTheLibraryCannotAttachIsWrittenByTheComposer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		size     int // what the file's row says; 0 says what the bytes are
		archived bool
	}{
		{name: "numbers.csv", text: "region,total\nnorth,40\nsouth,60\n"},
		{name: "data.json", text: `{"region": "north", "total": 40}`},
		{name: "notes.txt", text: "Plain notes on the autumn launch"},
		{name: "page.html", text: "<h1>Autumn launch</h1>"},
		{name: "long-notes.md", text: "# Notes that are too long for a comment", size: 40960},
		{name: "old-notes.md", text: "# Notes from the Bin", archived: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, e, u := materializeFixture(t)
			data := []byte(tc.text)
			reader := &referenceReader{data: data}
			i.SetSourceDownloader(reader)
			size := tc.size
			if size == 0 {
				size = len(data)
			}
			e.files["reference-file"] = map[string]any{"name": tc.name, "blobUrl": "library/u-alice/reference-file/" + tc.name, "ownerUserId": "u-alice", "sha256": (pure.Result{Bytes: data}).SHA256(), "size": size, "archived": tc.archived}
			a := describedArgs()
			a.Sources = []SourceRef{{Kind: KindLibraryFile, Ref: "reference-file", Label: tc.name, Content: true}}
			compositionId, input := acceptAsAPerson(t, i, a)
			if reader.calls != 1 {
				t.Fatalf("the composer reads the file, once, when the request is accepted; it was read %d times", reader.calls)
			}

			var asked []ComposeRequest
			worker := anotherReplica(e, u, materializeComposerFunc(func(_ context.Context, r ComposeRequest) (ComposeReply, error) {
				asked = append(asked, r)
				return ComposeReply{Draft: pure.Draft{Body: "Written from the file"}}, nil
			}))
			if err := adoptTheRun(worker, input); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || asked[0].Statement != describedStatement || len(asked[0].Sources) != 1 || len(asked[0].Sources[0].Files) != 1 || asked[0].Sources[0].Files[0].Text != tc.text {
				t.Fatalf("the composer was not asked to write from the statement and the file's text: %+v", asked)
			}
			if !strings.Contains(string(u.bytes), "Written from the file") || string(u.bytes) == "\n" {
				t.Fatalf("a request with a file the Library cannot attach must carry the composed document, got %q", u.bytes)
			}
			if row := e.compositions[compositionId]; row["status"] != "ready" || row["statement"] != describedStatement {
				t.Fatalf("the request did not complete with its statement: %v", row)
			}
			if reader.calls != 1 {
				t.Fatalf("the replica that executed the run read the file again (%d reads in all)", reader.calls)
			}
		})
	}
}

// A picked file that nobody can read refuses the start on the composed path, as an
// unreadable reference always has -- and the refusal comes before anything is written.
func TestAPickedFileNobodyCanReadRefusesTheStart(t *testing.T) {
	i, e, _ := materializeFixture(t)
	reader := &referenceReader{data: []byte("never read")}
	i.SetSourceDownloader(reader)
	i.SetGoalOpener(&directMaterializeGoal{})
	a := describedArgs()
	a.Sources = []SourceRef{{Kind: KindLibraryFile, Ref: "nobody-readable", Label: "Meeting notes", Content: true}}
	_, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err == nil || !strings.Contains(err.Error(), `reference "Meeting notes" is not readable by you`) {
		t.Fatalf("a file no row answers for must refuse the start, got %v", err)
	}
	if len(e.compositions) != 0 || len(e.inputs) != 0 || len(e.files) != 0 || reader.calls != 0 {
		t.Fatalf("the refused start wrote something or read something: compositions=%d inputs=%d files=%d reads=%d", len(e.compositions), len(e.inputs), len(e.files), reader.calls)
	}
}

// A work run keeps the composer for a described request, so the composer reads what the run
// picked: the path is the composed one, and the composed path reads its contents.
func TestDescribedMarkdownInsideARunReadsItsReferences(t *testing.T) {
	i, e, u := materializeFixture(t)
	data := []byte("Launch notes: ship on the first Tuesday")
	reader := &referenceReader{data: data}
	i.SetSourceDownloader(reader)
	e.files["reference-file"] = map[string]any{"name": "launch-notes.md", "blobUrl": "library/u-alice/reference-file/launch-notes.md", "ownerUserId": "u-alice", "sha256": (pure.Result{Bytes: data}).SHA256(), "size": len(data)}
	var asked []ComposeRequest
	i.SetComposer(materializeComposerFunc(func(_ context.Context, r ComposeRequest) (ComposeReply, error) {
		asked = append(asked, r)
		return ComposeReply{Draft: pure.Draft{Body: "Written from the notes"}}, nil
	}))
	a := describedArgs()
	a.Sources = []SourceRef{{Kind: KindLibraryFile, Ref: "reference-file", Label: "Launch notes", Content: true}}
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", a); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || len(asked) != 1 || len(asked[0].Sources) != 1 || len(asked[0].Sources[0].Files) != 1 || asked[0].Sources[0].Files[0].Text != string(data) {
		t.Fatalf("an in-run request is written by the composer from the file it read: reads=%d asked=%+v", reader.calls, asked)
	}
	if !strings.Contains(string(u.bytes), "Written from the notes") || string(u.bytes) == "\n" {
		t.Fatalf("an in-run described request was filed blank: %q", u.bytes)
	}
}

func TestDescribedMarkdownWithATemplateStillUsesTheComposer(t *testing.T) {
	i, e, u := materializeFixture(t)
	e.template = map[string]any{"id": "template-1", "name": "House style"}
	a := describedArgs()
	a.TemplateId = "template-1"
	compositionId, input := acceptAsAPerson(t, i, a)

	// The person asked for a template, and only the composer renders through one.
	var asked []ComposeRequest
	worker := anotherReplica(e, u, materializeComposerFunc(func(_ context.Context, r ComposeRequest) (ComposeReply, error) {
		asked = append(asked, r)
		return ComposeReply{Draft: pure.Draft{Body: "Written through the template"}}, nil
	}))
	if err := adoptTheRun(worker, input); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].TemplateName != "House style" || asked[0].Statement != describedStatement {
		t.Fatalf("the composer was not asked to write through the template: %+v", asked)
	}
	if !strings.Contains(string(u.bytes), "Written through the template") || string(u.bytes) == "\n" {
		t.Fatalf("a described request with a template must carry the composed document, got %q", u.bytes)
	}
	if row := e.compositions[compositionId]; row["status"] != "ready" {
		t.Fatalf("the templated request did not complete: %v", row)
	}
}

// "Write a report from these records" is what a person asks the OS composer for
// when they pick rows and describe the document. The revision workflow can read
// the files they attach and nothing else, so filing that request blank would
// throw the records away: it keeps the composer, which reads them.
func TestDescribedMarkdownFromDataSourcesStillUsesTheComposer(t *testing.T) {
	for name, sources := range map[string][]SourceRef{
		"a query":                    {{Kind: KindQuery, Ref: "sourceRows()", Label: "Invoices"}},
		"a library file and a query": {{Kind: KindLibraryFile, Ref: "notes-file", Label: "Notes"}, {Kind: KindQuery, Ref: "sourceRows()", Label: "Invoices"}},
	} {
		i, e, u := materializeFixture(t)
		e.source = map[string]any{"id": "invoice-1", "amount": 100}
		e.files["notes-file"] = map[string]any{"name": "meeting-notes.md", "mimeType": "text/markdown"}
		a := describedArgs()
		a.Sources = sources
		compositionId, input := acceptAsAPerson(t, i, a)

		var asked []ComposeRequest
		worker := anotherReplica(e, u, materializeComposerFunc(func(_ context.Context, r ComposeRequest) (ComposeReply, error) {
			asked = append(asked, r)
			return ComposeReply{Draft: pure.Draft{Body: "Written from the invoices"}}, nil
		}))
		if err := adoptTheRun(worker, input); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(asked) != 1 || asked[0].Statement != describedStatement || len(asked[0].Sources) != len(sources) {
			t.Fatalf("%s: the composer was not asked to write from the statement and every source: %+v", name, asked)
		}
		if got := asked[0].Sources[len(sources)-1]; len(got.Rows) != 1 {
			t.Fatalf("%s: the composer did not receive the query's rows: %+v", name, got)
		}
		if !strings.Contains(string(u.bytes), "Written from the invoices") || string(u.bytes) == "\n" {
			t.Fatalf("%s: a request with data sources must carry the composed document, got %q", name, u.bytes)
		}
		if row := e.compositions[compositionId]; row["status"] != "ready" || row["statement"] != describedStatement {
			t.Fatalf("%s: the request did not complete with its statement: %v", name, row)
		}
	}
}

// The same recovery Task 1 pins for a blank request, for a described one: the
// resumed pass finds a saved recipe and restores it, so the blank-ness has to be
// on that recipe -- and the pass that restores it is not the pass that decided.
func TestDescribedMarkdownRecoveredFromItsRecipeStaysBlank(t *testing.T) {
	e := newMaterializeEngine(t)
	flaky := &failOnce{materializeEngine: e, construct: "createLibraryFile", message: "connection reset by peer"}
	u := &materializeUploader{}
	compositionId, input := acceptAsAPerson(t, anotherReplica(flaky, u, nil), describedArgs())

	err := adoptTheRun(anotherReplica(flaky, u, forbidComposer(t)), input)
	if err == nil || strings.Contains(err.Error(), terminalFailureCode) {
		t.Fatalf("the interruption must stay resumable, got %v", err)
	}
	if row := e.compositions[compositionId]; row["status"] != "draft" || len(e.files) != 0 {
		t.Fatalf("an interrupted described request must stay resumable with nothing filed: row=%v files=%d", row, len(e.files))
	}
	// What the next replica restores: the recipe the first pass saved before it
	// tried to file, marked blank, in a request that still knows it was described.
	saved, err := json.Marshal(e.inputs[compositionId]["request"])
	if err != nil {
		t.Fatal(err)
	}
	var snapshot executionRequest
	if err := json.Unmarshal(saved, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Recipe == nil || !snapshot.Recipe.Blank || !snapshot.Described {
		t.Fatalf("the interrupted pass did not leave a blank recipe to resume from: described=%v recipe=%+v", snapshot.Described, snapshot.Recipe)
	}

	if err := adoptTheRun(anotherReplica(flaky, u, forbidComposer(t)), input); err != nil {
		t.Fatal(err)
	}
	if string(u.bytes) != "\n" {
		t.Fatalf("the resumed described request filed %q, want the single newline an editor opens as an empty page", u.bytes)
	}
	row := e.compositions[compositionId]
	if row["status"] != "ready" || len(e.files) != 1 || row["statement"] != describedStatement {
		t.Fatalf("the resumed described request did not finish with its one file and its statement: row=%v files=%d", row, len(e.files))
	}
}

// The blank file is half of a hand-off, and only a person's client makes the
// other half. A call made INSIDE a work run -- the planner's file delivery, an
// agent's composeFile -- says "write this" in its statement and has no client
// to give the description to the revision workflow, which refuses a run that
// tries. Filing it blank would end the run with an empty document and nobody to
// write it, so it keeps the composer.
func TestDescribedMarkdownInsideARunStillUsesTheComposer(t *testing.T) {
	i, e, u := materializeFixture(t)
	var asked []ComposeRequest
	i.SetComposer(materializeComposerFunc(func(_ context.Context, r ComposeRequest) (ComposeReply, error) {
		asked = append(asked, r)
		return ComposeReply{Draft: pure.Draft{Body: "Written from the statement"}}, nil
	}))
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", describedArgs())
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Statement != describedStatement {
		t.Fatalf("an in-run described request must be written by the composer: %+v", asked)
	}
	if !strings.Contains(string(u.bytes), "Written from the statement") || string(u.bytes) == "\n" {
		t.Fatalf("an in-run described request was filed blank: %q", u.bytes)
	}
	if row := e.compositions[stringOf(out["compositionId"])]; row["status"] != "ready" {
		t.Fatalf("the in-run request did not complete: %v", row)
	}
}

// fromARun is true for a context that names a run and for nothing else. That it
// is the Library's own test for "a person is not making this call" is true by
// construction (both read common.RunFromContext) and cannot be seen from here:
// this package never calls the Library.
func TestFromARunIsTrueOnlyForAContextThatNamesARun(t *testing.T) {
	if fromARun(materializeContext()) {
		t.Fatal("a call with no run on its context is a person's")
	}
	if !fromARun(nestedMaterializeContext()) {
		t.Fatal("a call inside a work run is not a person's")
	}
	noRunId := common.ContextWithRun(materializeContext(), common.RunContext{GoalId: "v1:work:goal:nexus", StepKey: "file", OwnerUserId: "u-alice"})
	if fromARun(noRunId) {
		t.Fatal("a run context with no run id is a person's: the Library lets it through")
	}
	// A run with no goal is still a run: it is not nested (its composition has no
	// goal to attach to) but the Library still refuses it, so there is still no
	// client to hand a description on.
	noGoal := common.ContextWithRun(materializeContext(), common.RunContext{RunId: "v1:work:run:orphan", StepKey: "file", OwnerUserId: "u-alice"})
	if !fromARun(noGoal) {
		t.Fatal("a run that names no goal is still a run, and not a person's")
	}
}

// filedSize reads the size of a filed file's row, strictly: a row with no size,
// or one that is not a whole number of bytes, is an error and not zero.
// runCountOf answers 0 for all of those, which is right for a counter that starts
// at nothing and wrong for a check that a file was sized at nothing.
func filedSize(file map[string]any) (int, error) {
	raw, ok := file["size"]
	if !ok {
		return 0, errors.New("the file row carries no size")
	}
	switch n := raw.(type) {
	case int:
		if n >= 0 {
			return n, nil
		}
	case int64:
		if n >= 0 {
			return int(n), nil
		}
	case float64:
		if n >= 0 && n == float64(int(n)) {
			return int(n), nil
		}
	case json.Number:
		if v, err := n.Int64(); err == nil && v >= 0 {
			return int(v), nil
		}
	}
	return 0, fmt.Errorf("the file row's size %v (%T) is not a whole number of bytes", raw, raw)
}

func TestFiledSizeIsReadStrictly(t *testing.T) {
	for name, tc := range map[string]struct {
		row  map[string]any
		want int
	}{
		"zero as a decoded number": {map[string]any{"size": float64(0)}, 0},
		"one as a decoded number":  {map[string]any{"size": float64(1)}, 1},
		"an int":                   {map[string]any{"size": 7}, 7},
		"an int64":                 {map[string]any{"size": int64(9)}, 9},
		"a json number":            {map[string]any{"size": json.Number("12")}, 12},
	} {
		if got, err := filedSize(tc.row); err != nil || got != tc.want {
			t.Errorf("%s: filedSize = %d, %v; want %d", name, got, err, tc.want)
		}
	}
	for name, row := range map[string]map[string]any{
		"no row at all":         nil,
		"no size":               {"name": "Blank.txt"},
		"a null size":           {"size": nil},
		"a string":              {"size": "0"},
		"a fraction":            {"size": 1.5},
		"a negative number":     {"size": float64(-1)},
		"a negative int":        {"size": -3},
		"a bad json number":     {"size": json.Number("x")},
		"a type it cannot read": {"size": []int{0}},
	} {
		if got, err := filedSize(row); err == nil {
			t.Errorf("%s was read as %d; a size that is not there or not a number must be an error", name, got)
		}
	}
}

func TestDescribedMarkdownMeansAStatementAndNothingToComposeFrom(t *testing.T) {
	notes, spec := pickedFile("meeting-notes.md", 2048), pickedFile("spec.md", 4096)
	if !describedArgs().describedMarkdown(nil) {
		t.Fatal("a Markdown name and statement alone are a described request")
	}
	// Where the document goes and the limits its run keeps are not what makes it a
	// description. Nor are the files it is about: Markdown files and pictures the Library
	// can attach are references the revision workflow can read, and they are passed on with
	// the description. A whitespace-only draft is no draft.
	around := describedArgs()
	specRef := spec.Ref
	specRef.Content = true
	around.Sources = []SourceRef{notes.Ref, specRef}
	around.FolderId, around.AccountIds, around.Ceilings, around.Draft, around.RequestId = "folder-1", []string{"account-1"}, map[string]any{"maxRetries": 1}, " \n ", "req-1"
	if !around.describedMarkdown([]Resolved{notes, spec}) {
		t.Fatal("Markdown files, a folder, account tags, ceilings, a request id and a whitespace-only draft do not stop a request being described")
	}
	for name, with := range map[string]func(*materializeArgs){
		"no statement":   func(a *materializeArgs) { a.Statement = "" },
		"html":           func(a *materializeArgs) { a.Format = pure.FormatHTML },
		"text":           func(a *materializeArgs) { a.Format = pure.FormatText },
		"csv":            func(a *materializeArgs) { a.Format = pure.FormatCSV },
		"json":           func(a *materializeArgs) { a.Format = pure.FormatJSON },
		"docx":           func(a *materializeArgs) { a.Format = pure.FormatDOCX },
		"pdf":            func(a *materializeArgs) { a.Format = pure.FormatPDF },
		"a draft":        func(a *materializeArgs) { a.Draft = "Some words" },
		"section keys":   func(a *materializeArgs) { a.SectionKeys = []string{"chapter-1"} },
		"a template":     func(a *materializeArgs) { a.TemplateId = "template-1" },
		"a recipe":       func(a *materializeArgs) { a.RecipeId = "recipe-1" },
		"an output kind": func(a *materializeArgs) { a.OutputKind = "email_template" },
		"a deployable":   func(a *materializeArgs) { a.DeployableKind = "spa" },
	} {
		a := describedArgs()
		with(&a)
		if a.describedMarkdown(nil) || a.describedMarkdown([]Resolved{notes}) {
			t.Fatalf("a request with %s is not a described Markdown request", name)
		}
	}
	// The revision workflow reads Markdown and images. It cannot read the graph, so a
	// request with records to write from has something of its own to compose from, and
	// only the composer can read it. One data source among the files is enough to keep the
	// composer: the files would go to the revision workflow and the records could not.
	query := Resolved{Ref: SourceRef{Kind: KindQuery, Ref: "sourceRows()"}, Rows: []map[string]any{{"id": "invoice-1"}}, Count: 1}
	conceptRow := Resolved{Ref: SourceRef{Kind: KindConceptRow, Ref: "invoice-1"}, Rows: []map[string]any{{"id": "invoice-1"}}, Count: 1}
	for name, resolved := range map[string][]Resolved{
		"a query":                     {query},
		"a concept row":               {conceptRow},
		"a Markdown file and a query": {notes, query},
		"a query and a Markdown file": {query, notes},
	} {
		a := describedArgs()
		a.Sources = make([]SourceRef, 0, len(resolved))
		for _, r := range resolved {
			a.Sources = append(a.Sources, r.Ref)
		}
		if a.describedMarkdown(resolved) {
			t.Fatalf("a request with %s is not a described Markdown request", name)
		}
	}
	// A request is blank or described, never both: blank has no words at all.
	if describedArgs().blank() || blankArgs(pure.FormatMarkdown).describedMarkdown(nil) {
		t.Fatal("blank and described requests overlap")
	}
}

// The Library lists a file by its summary, and "Materialized from no sources"
// reads as a fault on a file that was never going to have any.
func TestBlankAndDescribedFilesAreSummarisedAsCreatedBlank(t *testing.T) {
	// Each request files exactly one file: a summary read off a file that was never
	// filed, or off a second file that is not the one asked for, proves nothing.
	i, e, _ := materializeFixture(t)
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(pure.FormatMarkdown)); err != nil {
		t.Fatal(err)
	}
	if len(e.files) != 1 {
		t.Fatalf("a blank request filed %d files, want exactly one", len(e.files))
	}
	for _, f := range e.files {
		if f["summary"] != "Created blank." {
			t.Fatalf("a blank file's summary = %q, want Created blank.", f["summary"])
		}
	}

	described, de, du := materializeFixture(t)
	compositionId, input := acceptAsAPerson(t, described, describedArgs())
	if err := adoptTheRun(anotherReplica(de, du, nil), input); err != nil {
		t.Fatal(err)
	}
	if len(de.files) != 1 {
		t.Fatalf("a described request filed %d files, want exactly one", len(de.files))
	}
	if f := de.files[stringOf(de.compositions[compositionId]["outputFileId"])]; f["summary"] != "Created blank." {
		t.Fatalf("a described file's summary = %q, want Created blank.", f["summary"])
	}

	// An ordinary file keeps saying what it was made from.
	ordinary, oe, _ := materializeFixture(t)
	if _, err := ordinary.materialize(nestedMaterializeContext(), "u-alice", "", materializeDraft()); err != nil {
		t.Fatal(err)
	}
	if len(oe.files) != 1 {
		t.Fatalf("an ordinary request filed %d files, want exactly one", len(oe.files))
	}
	for _, f := range oe.files {
		if f["summary"] != "Materialized from no sources." {
			t.Fatalf("an ordinary file's summary = %q, want Materialized from no sources.", f["summary"])
		}
	}
}
