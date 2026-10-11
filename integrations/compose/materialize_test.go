package compose

import (
	"context"
	"encoding/json"
	"errors"
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
	t              *testing.T
	compositions   map[string]map[string]any
	inputs         map[string]map[string]any
	files          map[string]map[string]any
	calls          []string
	source         map[string]any
	account        map[string]any
	recipe         map[string]any
	failReady      bool
	failFileReady  bool
	fileReadyError string
	afterFile      func()
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
	case "createLibraryFile":
		row = maps.Clone(args)
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
	case "sourceRows":
		row = e.source
	case "clientAccountById":
		row = e.account
	case "composeRecipeById":
		row = e.recipe
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

	for _, name := range []string{"a\\b", "/etc/passwd", "folder/Plan.md"} {
		if _, err := parseMaterializeArgs(map[string]any{"name": name, "format": "markdown"}); err == nil {
			t.Fatalf("name %q carries a folder path and must be refused", name)
		}
	}
	if _, err := parseMaterializeArgs(map[string]any{"name": "a/b", "format": "markdown"}); err == nil || !strings.Contains(err.Error(), "enter a file name without a folder path") {
		t.Fatalf("the path refusal must say what to do: %v", err)
	}
	// Stripping must not turn a name into nothing.
	if _, err := parseMaterializeArgs(map[string]any{"name": ".md", "format": "markdown"}); err == nil {
		t.Fatal("a name that is only the extension has no stem and must be refused")
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
// id and file its output twice on the next replay.
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
	if _, err := refused.handleMaterialize(nestedMaterializeContext(), map[string]any{"name": "notes/Plan", "format": "markdown"}, 0); err == nil {
		t.Fatal("a name with a folder path was accepted")
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
