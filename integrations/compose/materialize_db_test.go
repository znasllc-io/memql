package compose

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	work "github.com/znasllc-io/memql/integrations/work"
)

func materializeDBEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "materializer lifecycle", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return e
}

// gatedWork is the work spine as a node builds it: its write gates on a pool of
// their own (database.NewGatePool, as the node's database opens GateDB;
// memql#6022), never on the engine's.
func gatedWork(t *testing.T, e *memql.MemQLEngine) *work.Integration {
	t.Helper()
	w := work.New(e, e.Logger)
	w.SetGateDB(composeGatePool(t))
	return w
}

// composeGatePool opens one replica's write-gate pool.
func composeGatePool(t *testing.T) func() *sql.DB {
	t.Helper()
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	return func() *sql.DB { return gates }
}

func TestMaterializeDB_SharedCompositionKeepsSourcesPrivate(t *testing.T) {
	e := materializeDBEngine(t)
	i := New(e, e.Logger)
	i.SetGoalOpener(gatedWork(t, e))
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, member := "compose-owner-"+suffix, "compose-member-"+suffix
	account, group := "compose-account-"+suffix, "compose-group-"+suffix
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: owner})
	for _, user := range []string{owner, member} {
		payload, _ := json.Marshal(map[string]any{"displayName": user, "primaryEmail": user + "@example.test", "role": "writer", "active": true})
		if _, err := e.Execute(seed, fmt.Sprintf(`insert("v1:identity:user", id=%q, payload=%s)`, user, payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		fmt.Sprintf(`insert("v1:accounts:account", id=%q, payload={"name": "Shared composition", "status": "active", "domainStatus": "unverified"})`, account),
		"mutation " + call("writeGroup", map[string]any{"groupId": group, "name": "Shared composition", "kind": "account", "accountId": account, "status": "active"}),
		"mutation " + call("writeGroupMembership", map[string]any{"membershipId": group + "-" + member, "groupId": group, "userId": member, "accountId": account, "origin": "added", "status": "active"}),
	} {
		if _, err := e.Execute(seed, query); err != nil {
			t.Fatal(err)
		}
	}
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	privateSource := "source-" + suffix
	secret := "private-source-marker-" + suffix
	if err := i.store().createComposition(ctx, map[string]any{"compositionId": privateSource, "name": "Private source", "statement": secret, "format": "txt"}); err != nil {
		t.Fatal(err)
	}
	a := materializeDraft()
	a.AccountIds = []string{account}
	a.Sources = []SourceRef{{Kind: KindQuery, Ref: call("compositionById", map[string]any{"compositionId": privateSource})}}
	accepted, err := i.materialize(ctx, owner, "", a)
	if err != nil {
		t.Fatal(err)
	}
	compositionId := stringOf(accepted["compositionId"])
	memberCtx := auth.ContextWithUserActor(context.Background(), member)
	readRaw := func(ctx context.Context, concept string) []map[string]any {
		t.Helper()
		rows, err := i.store().query(ctx, fmt.Sprintf("concept==%q && row.id==%q", concept, compositionId))
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	countConcept := func(rows []map[string]any, concept string) int {
		count := 0
		for _, row := range rows {
			if row["concept"] == concept {
				count++
			}
		}
		return count
	}
	shared := readRaw(memberCtx, "v1:compose:composition")
	if countConcept(shared, "v1:compose:composition") != 1 {
		t.Fatalf("account member should see the shared composition: %+v", shared)
	}
	encoded, _ := json.Marshal(shared)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), `"request"`) {
		t.Fatalf("raw shared composition exposes captured private input: %s", encoded)
	}
	if hidden := readRaw(memberCtx, "v1:compose:compositionInput"); len(hidden) != 0 {
		t.Fatalf("account member can browse private input: %+v", hidden)
	}
	if hidden, err := i.store().compositionInputById(memberCtx, compositionId); err != nil || hidden != nil {
		t.Fatalf("account member input query: %+v %v", hidden, err)
	}
	owned := readRaw(ctx, "v1:compose:compositionInput")
	encoded, _ = json.Marshal(owned)
	if countConcept(owned, "v1:compose:compositionInput") != 1 || !strings.Contains(string(encoded), secret) {
		t.Fatalf("owner cannot recover the saved source snapshot: %s", encoded)
	}
}

// The accepting and executing engines share only Postgres. This exercises the
// actual template, argument binding, owner tier, work journal and terminal run
// transition, which the in-memory pipeline tests cannot prove.
func TestMaterializeDB_AcceptThenAdoptOnAnotherEngine(t *testing.T) {
	accepting := materializeDBEngine(t)
	executing := materializeDBEngine(t)
	accept := New(accepting, accepting.Logger)
	accept.SetGoalOpener(gatedWork(t, accepting))
	worker := New(executing, executing.Logger)
	if err := executing.RegisterIntegration(worker); err != nil {
		t.Fatal(err)
	}
	upload := &materializeUploader{}
	worker.SetUploader(upload, "files")
	loader := automations.NewLoader(automations.LoaderOptions{Logger: executing.Logger})
	auto, err := loader.LoadByName("materializeFile")
	if err != nil || auto == nil {
		t.Fatalf("template: %v", err)
	}
	exec := automations.NewExecutor(automations.ExecutorOptions{Engine: executing, Logger: executing.Logger, StepRegistry: steps.NewRegistry()})
	defer exec.Close()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("providerFailure=%v", fail), func(t *testing.T) {
			owner := fmt.Sprintf("materializer-db-%d", time.Now().UnixNano())
			ctx := auth.ContextWithUserActor(context.Background(), owner)
			worker.SetComposer(materializeComposerFunc(func(ctx context.Context, req ComposeRequest) (ComposeReply, error) {
				rc, ok := common.RunFromContext(ctx)
				if !ok || rc.GoalId == "" || rc.RunId == "" || rc.StepKey == "" {
					t.Fatalf("provider lost run scope: %+v", rc)
				}
				if fail {
					return ComposeReply{}, fmt.Errorf("provider unavailable")
				}
				return ComposeReply{Draft: pure.Draft{Body: "A persisted result"}}, nil
			}))
			accepted, err := accept.materialize(ctx, owner, "", materializeDraft())
			if err != nil {
				t.Fatal(err)
			}
			compositionId, goalId, runId := stringOf(accepted["compositionId"]), stringOf(accepted["goalId"]), stringOf(accepted["runId"])
			row, err := worker.store().compositionById(ctx, compositionId)
			if err != nil || row == nil {
				t.Fatalf("composition missing before dispatch: %v", err)
			}
			if row["status"] != "draft" || row["goalId"] != goalId || row["runId"] != runId {
				t.Fatalf("accept did not persist matching identities before returning: %v", row)
			}
			journal, err := automations.LoadRunJournal(ctx, executing, runId)
			if err != nil {
				t.Fatal(err)
			}
			if journal.AutomationName != "materializeFile" || journal.Status != "running" {
				t.Fatalf("materializer sent request to compiler: %+v", journal)
			}
			// Dispatch rebuilds context from the persisted journal. Its RunId is
			// bare while goal and owner references retain canonical identities.
			ctx, err = auth.ContextWithPersistedOwner(context.Background(), journal.OwnerUserId, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx = common.ContextWithRun(ctx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode})
			result, runErr := exec.ExecuteAdopted(ctx, auto, automations.RunAdoption{RunId: runId, Variables: journal.Variables, Journal: journal})
			journal, err = automations.LoadRunJournal(ctx, executing, runId)
			if err != nil {
				t.Fatal(err)
			}
			row, err = worker.store().compositionById(ctx, compositionId)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if row["status"] != "failed" || journal.Status != "failed" {
					t.Fatalf("provider failure did not close composition and run: composition=%v journal=%+v result=%+v err=%v", row, journal, result, runErr)
				}
				return
			}
			if runErr != nil || row["status"] != "ready" || journal.Status != "succeeded" {
				t.Fatalf("actual template failed: composition=%v journal=%+v result=%+v err=%v", row, journal, result, runErr)
			}
			file, err := worker.store().libraryFileById(ctx, stringOf(row["outputFileId"]))
			if err != nil || file == nil {
				t.Fatalf("file missing: %v", err)
			}
			if file["producedByRunId"] != runId {
				t.Fatalf("file missing Nexus provenance: %v", file)
			}

			index, err := loader.LoadByName("indexFileOnCreate")
			if err != nil || index == nil {
				t.Fatalf("file promotion template: %v", err)
			}
			ownerCtx := auth.ContextWithUserActor(context.Background(), owner)
			promoted, promoteErr := exec.ExecuteWithEvent(ownerCtx, index, "file-created", &events.Event{Topic: "graph.node.created.v1:library:file", Payload: file})
			if promoteErr != nil || promoted.Status != "completed" {
				t.Fatalf("file index promotion failed: %+v %v", promoted, promoteErr)
			}
			indexed, err := worker.store().query(ownerCtx, "query "+call("libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": file["id"]}))
			if err != nil || len(indexed) != 1 || indexed[0]["producedByRunId"] != runId {
				t.Fatalf("Nexus cannot find producing run on file index: %+v %v", indexed, err)
			}
			// Source snapshots are internal. The ordinary owner query never sends
			// gathered source payloads to a browser watching composition state.
			if _, leaked := row["request"]; leaked {
				t.Fatal("public shape exposes internal request snapshot")
			}
			outsider := auth.ContextWithUserActor(context.Background(), owner+"-stranger")
			hidden, err := worker.store().compositionById(outsider, compositionId)
			if err != nil {
				t.Fatal(err)
			}
			if hidden != nil {
				t.Fatal("stranger can read the composition")
			}
			if !strings.Contains(string(upload.bytes), "A persisted result") {
				t.Fatal("file does not contain composed result")
			}
		})
	}
}

// Client organizations and identity billing accounts are different concepts.
// A fake account row cannot catch accidentally querying the billing registry.
func TestEmailMaterializeDBUsesClientOrganizationRegistry(t *testing.T) {
	e := materializeDBEngine(t)
	i := New(e, e.Logger)
	i.SetGoalOpener(&directMaterializeGoal{})
	suffix := fmt.Sprintf("email-org-%d", time.Now().UnixNano())
	owner, account := suffix+"-owner", suffix+"-client"
	ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
	if _, err := e.Execute(ctx, "mutation "+call("createClientAccount", map[string]any{"accountId": account, "name": "Email client"})); err != nil {
		t.Fatal(err)
	}
	// No identity.account with this id exists. The business organization is
	// sufficient; creating a billing account must never be a hidden prerequisite.
	a := materializeArgs{Name: "Client welcome", Statement: "Compose a welcome email", Format: pure.FormatJSON, OutputKind: "email_template", AccountIds: []string{account}}
	result, err := i.materialize(ctx, owner, "", a)
	if err != nil || result["compositionId"] == "" {
		t.Fatalf("client organization refused: %v %v", result, err)
	}
	if _, err := e.Execute(ctx, "mutation "+call("archiveClientAccount", map[string]any{"accountId": account})); err != nil {
		t.Fatal(err)
	}
	if _, err := i.materialize(ctx, owner, "", a); err == nil {
		t.Fatal("archived organization accepted an email composition")
	}
}

// waitComposition polls a composition until it reaches status and returns that
// row, failing with the last row it saw. A run's writes land on the engine that
// executed it, so a reader on another replica asks until it sees them.
func waitComposition(t *testing.T, i *Integration, ctx context.Context, compositionId, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last map[string]any
	for {
		row, err := i.store().compositionById(ctx, compositionId)
		if err != nil {
			t.Fatal(err)
		}
		if row != nil {
			last = row
			if stringOf(row["status"]) == status {
				return row
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("composition %s did not reach %q within 20s; last row: %v", compositionId, status, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// requestOutcome is what a request left behind once its run finished -- the empty file a
// blank or described request is filed as, or the one the composer wrote.
type requestOutcome struct {
	row     map[string]any
	file    map[string]any
	upload  *materializeUploader
	journal *automations.RunJournal
}

// materializeBlankDB drives one blank request of the given format through the
// real engine (see materializeRequestDB).
func materializeBlankDB(t *testing.T, format pure.Format) requestOutcome {
	t.Helper()
	return materializeRequestDB(t, materializeArgs{Name: "Blank", Format: format})
}

// libraryFileSeed is a Library file the owner already has when the request is made.
// The size and the digest are what the file's row says; the bytes behind it are whatever
// the accepting node's source downloader answers with, if the request reads them.
type libraryFileSeed struct {
	id, name string
	size     int
	sha256   string // "" leaves the digest off the row
}

// requestSetup is what a request driven through materializeRequestDB needs around it.
type requestSetup struct {
	asCaller func(e *memql.MemQLEngine, owner string, args *materializeArgs) context.Context
	files    []libraryFileSeed
	reader   SourceDownloader // for the node that accepts the request
	composer Composer         // for the node that executes it
}

type requestOption func(*requestSetup)

// withLibraryFiles seeds Library files under the request's owner, through the calls the
// composer files its own output with, before the request is made.
func withLibraryFiles(files ...libraryFileSeed) requestOption {
	return func(s *requestSetup) { s.files = append(s.files, files...) }
}

// withSourceDownloader gives the node that accepts the request a way to read references.
func withSourceDownloader(reader SourceDownloader) requestOption {
	return func(s *requestSetup) { s.reader = reader }
}

// withComposer gives the node that executes the request a composer.
func withComposer(composer Composer) requestOption {
	return func(s *requestSetup) { s.composer = composer }
}

// materializeRequestDB drives one request through the real engine the way two replicas
// would: one accepts it (and opens the goal), another adopts the run and executes it.
// Opening a goal dispatches nothing by itself -- the run's own `running` event is what
// reaches an agent replica -- so a test that only accepts would wait on a composition
// nothing is working on.
//
// The request is made the way a person's client makes it: outside any work run, as an
// actor with no run on its context.
//
// By default no composer is wired on the executing node, on purpose: a request that is
// filed blank must never need a model, and a stray call would fail the run loudly. A test
// of a request the composer writes gives it one with withComposer, and the files such a
// request reads are seeded with withLibraryFiles and read through withSourceDownloader.
func materializeRequestDB(t *testing.T, args materializeArgs, options ...requestOption) requestOutcome {
	t.Helper()
	var setup requestSetup
	for _, option := range options {
		option(&setup)
	}
	accepting := materializeDBEngine(t)
	executing := materializeDBEngine(t)
	accept := New(accepting, accepting.Logger)
	accept.SetGoalOpener(work.New(accepting, accepting.Logger))
	if setup.reader != nil {
		accept.SetSourceDownloader(setup.reader)
	}
	worker := New(executing, executing.Logger)
	if err := executing.RegisterIntegration(worker); err != nil {
		t.Fatal(err)
	}
	upload := &materializeUploader{}
	worker.SetUploader(upload, "files")
	if setup.composer != nil {
		worker.SetComposer(setup.composer)
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: executing.Logger})
	auto, err := loader.LoadByName("materializeFile")
	if err != nil || auto == nil {
		t.Fatalf("template: %v", err)
	}
	exec := automations.NewExecutor(automations.ExecutorOptions{Engine: executing, Logger: executing.Logger, StepRegistry: steps.NewRegistry()})
	t.Cleanup(exec.Close)

	owner := fmt.Sprintf("compose-blank-%s-%d", args.Format, time.Now().UnixNano())
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	for _, file := range setup.files {
		// The calls the composer files its own output with: the key is held to the owner's
		// own library/{owner}/ prefix, and the file is ready once it is written.
		row := map[string]any{
			"fileId": file.id, "name": file.name, "mimeType": "application/octet-stream", "size": file.size,
			"blobUrl": "https://storage.test/files/library/" + owner + "/" + file.id + "/" + file.name,
			"source":  "uploaded", "format": "other", "sha256": file.sha256,
		}
		if file.sha256 == "" {
			delete(row, "sha256")
		}
		if err := accept.store().createLibraryFile(ctx, row); err != nil {
			t.Fatalf("seeding %s: %v", file.name, err)
		}
		if err := accept.store().setLibraryFileReady(ctx, file.id, "Seeded for the test."); err != nil {
			t.Fatalf("marking %s ready: %v", file.name, err)
		}
	}
	if setup.asCaller != nil {
		ctx = setup.asCaller(accepting, owner, &args)
	}
	args.RequestId = "r-" + owner
	accepted, err := accept.materialize(ctx, owner, "", args)
	if err != nil {
		t.Fatal(err)
	}
	compositionId, goalId, runId := stringOf(accepted["compositionId"]), stringOf(accepted["goalId"]), stringOf(accepted["runId"])

	// The same request again -- a double click, a retried call -- is the same
	// composition, on the real engine's reads and not just the fake's.
	again, err := accept.materialize(ctx, owner, "", args)
	if err != nil {
		t.Fatal(err)
	}
	if stringOf(again["compositionId"]) != compositionId || stringOf(again["goalId"]) != goalId || stringOf(again["runId"]) != runId {
		t.Fatalf("a repeated request id started another composition: first=%v again=%v", accepted, again)
	}

	journal, err := automations.LoadRunJournal(ctx, executing, runId)
	if err != nil {
		t.Fatal(err)
	}
	if journal.AutomationName != "materializeFile" || journal.Status != "running" {
		t.Fatalf("a blank request did not open the materializer's own run: %+v", journal)
	}
	runCtx, err := auth.ContextWithPersistedOwner(context.Background(), journal.OwnerUserId, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx = common.ContextWithRun(runCtx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode})
	result, runErr := exec.ExecuteAdopted(runCtx, auto, automations.RunAdoption{RunId: runId, Variables: journal.Variables, Journal: journal})
	if runErr != nil {
		t.Fatalf("the blank run failed: result=%+v err=%v", result, runErr)
	}
	row := waitComposition(t, worker, runCtx, compositionId, "ready")
	journal, err = automations.LoadRunJournal(runCtx, executing, runId)
	if err != nil {
		t.Fatal(err)
	}
	file, err := worker.store().libraryFileById(runCtx, stringOf(row["outputFileId"]))
	if err != nil {
		t.Fatal(err)
	}
	return requestOutcome{row: row, file: file, upload: upload, journal: journal}
}

// The real engine accepts everything a blank file writes: the composition row
// (no models, no embedded provenance, no source package), the one-byte Library
// file and the work run that finishes around them.
func TestMaterializeDB_BlankMarkdownIsReadyWithOneFile(t *testing.T) {
	out := materializeBlankDB(t, pure.FormatMarkdown)
	if stringOf(out.row["outputFileId"]) == "" || stringOf(out.row["sourceFileId"]) != "" {
		t.Fatalf("a blank file is one file with no source package: %v", out.row)
	}
	if out.upload.calls != 1 || string(out.upload.bytes) != "\n" {
		t.Fatalf("blank db run uploaded %q in %d uploads, want a single newline once", out.upload.bytes, out.upload.calls)
	}
	if out.row["provenanceNote"] != "Created blank. Its history is kept on its composition, not in the file." {
		t.Fatalf("blank composition note = %q", out.row["provenanceNote"])
	}
	if out.file == nil || out.file["name"] != "Blank.md" || out.file["status"] != "ready" || runCountOf(out.file["size"]) != 1 {
		t.Fatalf("the filed blank markdown is not a ready one-byte Blank.md: %v", out.file)
	}
	if out.journal.Status != "succeeded" {
		t.Fatalf("the blank run did not succeed: %+v", out.journal)
	}
}

// Text and CSV blanks are zero bytes. A required size of 0 and an empty blob are
// the two things only the real engine and the real filing can vouch for.
func TestMaterializeDB_BlankTextIsReadyAsAZeroByteFile(t *testing.T) {
	out := materializeBlankDB(t, pure.FormatText)
	if out.upload.calls != 1 || len(out.upload.bytes) != 0 {
		t.Fatalf("blank text uploaded %q in %d uploads, want one empty file", out.upload.bytes, out.upload.calls)
	}
	// A size the row does not carry, or carries as something else, is not zero:
	// runCountOf answers 0 for both, which would make this check pass on a file
	// the engine never sized.
	size, err := filedSize(out.file)
	if err != nil || size != 0 {
		t.Fatalf("the filed blank text does not have a size of exactly 0: %d, %v (%v)", size, err, out.file)
	}
	if out.file == nil || out.file["name"] != "Blank.txt" || out.file["status"] != "ready" {
		t.Fatalf("the filed blank text is not a ready empty Blank.txt: %v", out.file)
	}
	if out.journal.Status != "succeeded" {
		t.Fatalf("the blank run did not succeed: %+v", out.journal)
	}
}

// A person's description reaches the real engine as a saved request, and every
// replica must read it the same way: that it is filed blank is part of what the
// adopting replica loads from the database, not something the accepting replica
// remembered. The executing node has no composer, so a described request that
// went to the model would fail the run instead of passing.
func TestMaterializeDB_DescribedMarkdownIsReadyAsABlankFile(t *testing.T) {
	statement := "A one-page launch brief for the autumn release"
	out := materializeRequestDB(t, materializeArgs{Name: "Launch brief", Format: pure.FormatMarkdown, Statement: statement})
	if stringOf(out.row["outputFileId"]) == "" || stringOf(out.row["sourceFileId"]) != "" {
		t.Fatalf("a described file is one file with no source package: %v", out.row)
	}
	if out.upload.calls != 1 || string(out.upload.bytes) != "\n" {
		t.Fatalf("described db run uploaded %q in %d uploads, want a single newline once", out.upload.bytes, out.upload.calls)
	}
	// The description stays on the record: it is what the person's client hands to
	// the revision workflow, and what is left if the client never does.
	if out.row["statement"] != statement {
		t.Fatalf("composition statement = %q, want the request's own", out.row["statement"])
	}
	if out.row["provenanceNote"] != "Created blank. Its history is kept on its composition, not in the file." {
		t.Fatalf("described composition note = %q", out.row["provenanceNote"])
	}
	if out.file == nil || out.file["name"] != "Launch brief.md" || out.file["status"] != "ready" || runCountOf(out.file["size"]) != 1 || out.file["summary"] != "Created blank." {
		t.Fatalf("the filed file is not a ready one-byte Launch brief.md summarised as created blank: %v", out.file)
	}
	if out.journal.Status != "succeeded" {
		t.Fatalf("the described run did not succeed: %+v", out.journal)
	}
}

// A described request whose picked file is one the Library attaches is filed blank, and
// nothing it names is read: the accepting node here has a source downloader that would
// fail the test if it were asked, and the request says `content: true` all the same, as
// the Materializer sends every picked Library file. The path is decided on the file's row,
// which the real engine hands back with its name and size.
func TestMaterializeDB_DescribedMarkdownWithAMarkdownFileReadsNothingAndIsFiledBlank(t *testing.T) {
	statement := "A one-page launch brief for the autumn release"
	reader := &referenceReader{data: []byte("# Meeting notes\n\nShip on the first Tuesday.")}
	notes := libraryFileSeed{id: fmt.Sprintf("notes-%d", time.Now().UnixNano()), name: "notes.md", size: 2048}
	out := materializeRequestDB(t, materializeArgs{
		Name: "Launch brief", Format: pure.FormatMarkdown, Statement: statement,
		Sources: []SourceRef{{Kind: KindLibraryFile, Ref: notes.id, Label: "Meeting notes", Content: true}},
	}, withLibraryFiles(notes), withSourceDownloader(reader))
	if reader.calls != 0 {
		t.Fatalf("a described request with a Markdown file read %d reference(s); it is filed blank and reads none", reader.calls)
	}
	if out.upload.calls != 1 || string(out.upload.bytes) != "\n" {
		t.Fatalf("described db run uploaded %q in %d uploads, want a single newline once", out.upload.bytes, out.upload.calls)
	}
	if out.row["statement"] != statement || out.row["provenanceNote"] != "Created blank. Its history is kept on its composition, not in the file." {
		t.Fatalf("the composition lost its statement or did not say it was created blank: %v", out.row)
	}
	// The file is the composition's provenance, so it stays named in its sources.
	sources, _ := json.Marshal(out.row["sources"])
	if !strings.Contains(string(sources), `"ref":"`+notes.id+`"`) {
		t.Fatalf("the picked file is not named in the composition's sources: %s", sources)
	}
	if out.file == nil || out.file["name"] != "Launch brief.md" || out.file["status"] != "ready" || runCountOf(out.file["size"]) != 1 || out.file["summary"] != "Created blank." {
		t.Fatalf("the filed file is not a ready one-byte Launch brief.md summarised as created blank: %v", out.file)
	}
	if out.journal.Status != "succeeded" {
		t.Fatalf("the described run did not succeed: %+v", out.journal)
	}
}

// A described request with a spreadsheet among its files goes to the composer, which reads
// it: the accepting node reads the file once, and the node that executes the run writes the
// document from that snapshot with the spreadsheet's text in front of it.
func TestMaterializeDB_DescribedMarkdownWithACSVIsWrittenByTheComposer(t *testing.T) {
	statement := "A one-page summary of the regional totals"
	table := "region,total\nnorth,40\nsouth,60\n"
	reader := &referenceReader{data: []byte(table)}
	numbers := libraryFileSeed{id: fmt.Sprintf("numbers-%d", time.Now().UnixNano()), name: "numbers.csv", size: len(table), sha256: (pure.Result{Bytes: reader.data}).SHA256()}
	var asked []ComposeRequest
	composer := materializeComposerFunc(func(_ context.Context, r ComposeRequest) (ComposeReply, error) {
		asked = append(asked, r)
		return ComposeReply{Draft: pure.Draft{Body: "North sold 40 and south sold 60."}}, nil
	})
	out := materializeRequestDB(t, materializeArgs{
		Name: "Regional totals", Format: pure.FormatMarkdown, Statement: statement,
		Sources: []SourceRef{{Kind: KindLibraryFile, Ref: numbers.id, Label: "Numbers", Content: true}},
	}, withLibraryFiles(numbers), withSourceDownloader(reader), withComposer(composer))
	if reader.calls != 1 {
		t.Fatalf("the spreadsheet is read once, when the request is accepted; it was read %d times", reader.calls)
	}
	if len(asked) != 1 || asked[0].Statement != statement || len(asked[0].Sources) != 1 || len(asked[0].Sources[0].Files) != 1 || asked[0].Sources[0].Files[0].Text != table {
		t.Fatalf("the executing node's composer was not asked to write from the statement and the spreadsheet's text: %+v", asked)
	}
	if !strings.Contains(string(out.upload.bytes), "North sold 40 and south sold 60.") || string(out.upload.bytes) == "\n" {
		t.Fatalf("the uploaded file must hold the composer's draft, got %q", out.upload.bytes)
	}
	if out.row["statement"] != statement || stringOf(out.row["sourceFileId"]) != "" {
		t.Fatalf("the composition lost its statement, or filed a source package for a Markdown file: %v", out.row)
	}
	if out.file == nil || out.file["name"] != "Regional totals.md" || out.file["status"] != "ready" || out.file["summary"] == "Created blank." {
		t.Fatalf("the filed file is not a ready Regional totals.md that was written: %v", out.file)
	}
	if out.journal.Status != "succeeded" {
		t.Fatalf("the composed run did not succeed: %+v", out.journal)
	}
}

// Every other format with an empty document is filed as one on the real engine,
// as Markdown and text are above: one Library file holding exactly what the blank
// writer writes, named with the format's extension once, beside a ready
// composition that reached no model and embedded nothing.
func TestMaterializeDB_BlankHTMLCSVAndJSONAreReadyWithOneFile(t *testing.T) {
	for _, tc := range []struct {
		format pure.Format
		name   string
	}{
		{pure.FormatHTML, "Blank.html"},
		{pure.FormatCSV, "Blank.csv"},
		{pure.FormatJSON, "Blank.json"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			want, err := pure.RenderBlank(tc.format, "Blank")
			if err != nil {
				t.Fatal(err)
			}
			out := materializeBlankDB(t, tc.format)
			if out.upload.calls != 1 || string(out.upload.bytes) != string(want.Bytes) {
				t.Fatalf("blank %s uploaded %q in %d uploads, want %q once", tc.format, out.upload.bytes, out.upload.calls, want.Bytes)
			}
			size, err := filedSize(out.file)
			if err != nil || size != len(want.Bytes) {
				t.Fatalf("the filed blank %s does not have a size of exactly %d: %d, %v (%v)", tc.format, len(want.Bytes), size, err, out.file)
			}
			if out.file["name"] != tc.name || out.file["status"] != "ready" || out.file["summary"] != "Created blank." {
				t.Fatalf("the filed blank %s is not a ready %s summarised as created blank: %v", tc.format, tc.name, out.file)
			}
			if stringOf(out.row["outputFileId"]) == "" || stringOf(out.row["sourceFileId"]) != "" {
				t.Fatalf("a blank file is one file with no source package: %v", out.row)
			}
			if out.row["provenanceEmbedded"] != false || out.row["provenanceNote"] != "Created blank. Its history is kept on its composition, not in the file." {
				t.Fatalf("blank %s composition provenance = %v / %q", tc.format, out.row["provenanceEmbedded"], out.row["provenanceNote"])
			}
			if models, _ := out.row["modelsUsed"].([]any); len(models) != 0 {
				t.Fatalf("a blank %s reached a model: %v", tc.format, out.row["modelsUsed"])
			}
			if out.journal.Status != "succeeded" {
				t.Fatalf("the blank %s run did not succeed: %+v", tc.format, out.journal)
			}
		})
	}
}

// A FOLDER IS CHECKED AGAINST THE LIBRARY ITSELF. The unit tests stand a map in
// for libraryFolderById; this asks the shipped query on the real engine, as the
// caller, for each way a folder cannot be filed into -- archived (here on another
// replica, which the accepting node has never asked about), somebody else's, no
// such folder -- and through both ways a folder reaches the Materializer today: a
// request that names one, and a recipe run, which files where the recipe was
// saved to file. Nothing is written for a refused one, and a live folder is filed
// into.
func TestMaterializeDB_RefusesAFolderTheCallerCannotUse(t *testing.T) {
	accepting := materializeDBEngine(t)
	other := materializeDBEngine(t)
	accept := New(accepting, accepting.Logger)
	accept.SetGoalOpener(work.New(accepting, accepting.Logger))
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, stranger := "compose-folder-owner-"+suffix, "compose-folder-stranger-"+suffix
	ownerCtx := auth.ContextWithUserActor(context.Background(), owner)
	mutate := func(e *memql.MemQLEngine, ctx context.Context, name string, args map[string]any) {
		t.Helper()
		if _, err := e.Execute(ctx, "mutation "+call(name, args)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	live, archived, theirs := "folder-live-"+suffix, "folder-archived-"+suffix, "folder-theirs-"+suffix
	mutate(accepting, ownerCtx, "createLibraryFolder", map[string]any{"folderId": live, "name": "Reports"})
	mutate(accepting, ownerCtx, "createLibraryFolder", map[string]any{"folderId": archived, "name": "Old reports"})
	mutate(other, ownerCtx, "archiveLibraryFolder", map[string]any{"folderId": archived})
	mutate(accepting, auth.ContextWithUserActor(context.Background(), stranger), "createLibraryFolder", map[string]any{"folderId": theirs, "name": "Not yours"})

	for name, folderId := range map[string]string{"archived": archived, "someone else's": theirs, "no such folder": "folder-none-" + suffix} {
		a := materializeArgs{Name: "Notes", Format: pure.FormatMarkdown, FolderId: folderId, RequestId: "r-" + strings.ReplaceAll(name, " ", "-") + "-" + suffix}
		if _, err := accept.materialize(ownerCtx, owner, "", a); !errors.Is(err, errFolderUnavailable) {
			t.Fatalf("%s folder: refusal = %v, want %q", name, err, errFolderUnavailable)
		}
		written, err := accept.store().compositionById(ownerCtx, stableMaterializeID("request", memql.BareShortId(owner), a.RequestId))
		if err != nil || written != nil {
			t.Fatalf("%s folder: refused, but a composition was written: %v (%v)", name, written, err)
		}
	}

	filed, err := accept.materialize(ownerCtx, owner, "", materializeArgs{Name: "Notes", Format: pure.FormatMarkdown, FolderId: live, RequestId: "r-live-" + suffix})
	if err != nil {
		t.Fatalf("a live folder of the caller's own was refused: %v", err)
	}
	row, err := accept.store().compositionById(ownerCtx, stringOf(filed["compositionId"]))
	if err != nil || row == nil || !sameRowID(stringOf(row["folderId"]), live) {
		t.Fatalf("the composition does not file into the live folder: %v (%v)", row, err)
	}

	recipeId := "compose-folder-recipe-" + suffix
	mutate(accepting, ownerCtx, "createComposeRecipe", map[string]any{"recipeId": recipeId, "name": "Weekly notes", "description": "The week's notes", "format": "markdown", "folderId": archived})
	if _, err := accept.handleRunRecipe(ownerCtx, map[string]any{"recipeId": recipeId}, 0); !errors.Is(err, errFolderUnavailable) {
		t.Fatalf("a recipe saved to file into an archived folder ran: refusal = %v, want %q", err, errFolderUnavailable)
	}
}

// A blank email on the real engine: the layout resolved when the request is
// accepted travels in the saved request to the replica that adopts the run,
// which files it as the email without a model (the executing node has no
// composer). The organization is a real client account the caller can read.
func TestMaterializeDB_BlankEmailFromABuiltinLayout(t *testing.T) {
	asOrganizationOwner := func(e *memql.MemQLEngine, owner string, args *materializeArgs) context.Context {
		ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
		account := owner + "-client"
		if _, err := e.Execute(ctx, "mutation "+call("createClientAccount", map[string]any{"accountId": account, "name": "Email client"})); err != nil {
			t.Fatal(err)
		}
		args.AccountIds = []string{account}
		return ctx
	}
	out := materializeRequestDBAs(t, materializeArgs{Name: "Welcome", Format: pure.FormatJSON, OutputKind: "email_template", LayoutId: "builtin:confirmation"}, asOrganizationOwner)
	want, ok := emaildoc.Layout("builtin:confirmation")
	if !ok {
		t.Fatal("no confirmation layout")
	}
	wantBytes, err := want.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if out.upload.calls != 1 || string(out.upload.bytes) != string(wantBytes) {
		t.Fatalf("the blank email uploaded %d times:\n%s\nwant the confirmation layout itself", out.upload.calls, out.upload.bytes)
	}
	doc, err := emaildoc.Parse(out.upload.bytes)
	if err != nil || doc.Layout != "builtin:confirmation" {
		t.Fatalf("the filed email is not the confirmation layout: %q %v", doc.Layout, err)
	}
	if out.file == nil || out.file["name"] != "Welcome.email.json" || out.file["status"] != "ready" || out.file["summary"] != "Created from an email layout." {
		t.Fatalf("the filed email is not a ready Welcome.email.json created from its layout: %v", out.file)
	}
	if stringOf(out.row["sourceFileId"]) != "" {
		t.Fatalf("a blank email files no source package: %v", out.row)
	}
	if models, _ := out.row["modelsUsed"].([]any); len(models) != 0 {
		t.Fatalf("a blank email reached a model: %v", out.row["modelsUsed"])
	}
	if out.journal.Status != "succeeded" {
		t.Fatalf("the blank email run did not succeed: %+v", out.journal)
	}
}

// AN ORGANIZATION'S LAYOUT IS READ ON THE REAL ENGINE. The unit tests stand a
// map in for emailLayoutById; this asks the shipped query, as the person, behind
// the organization boundary, and compares the organization the engine answers
// with (in whatever form it answers it) with the one the email is for. A layout
// of the email's organization is filed as the email; one of another of their
// organizations, and an archived one, are refused before anything is written.
func TestMaterializeDB_EmailFromAnOrganizationLayout(t *testing.T) {
	letter, ok := emaildoc.Layout("builtin:letter")
	if !ok {
		t.Fatal("no letter layout")
	}
	letter.Subject = "Our house letter"
	raw, err := letter.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var layout string
	saveLayout := func(e *memql.MemQLEngine, ctx context.Context, layoutId, account string) {
		t.Helper()
		// What campaignSaveLayout writes once it has checked the organization and
		// the revision: the mutation is @serverOnly, so it runs with internal origin.
		if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call("createEmailLayout", map[string]any{
			"layoutId": layoutId, "accountId": account, "name": "House letter", "document": document,
			"versionTime": time.Now().UTC().Format(time.RFC3339Nano),
		})); err != nil {
			t.Fatalf("createEmailLayout: %v", err)
		}
	}
	out := materializeRequestDBAs(t, materializeArgs{Name: "Welcome", Format: pure.FormatJSON, OutputKind: "email_template"},
		func(e *memql.MemQLEngine, owner string, args *materializeArgs) context.Context {
			ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
			account := owner + "-client"
			if _, err := e.Execute(ctx, "mutation "+call("createClientAccount", map[string]any{"accountId": account, "name": "Email client"})); err != nil {
				t.Fatal(err)
			}
			layout = owner + "-layout"
			saveLayout(e, ctx, layout, account)
			args.AccountIds, args.LayoutId = []string{account}, "org:"+layout
			return ctx
		})
	doc, err := emaildoc.Parse(out.upload.bytes)
	if err != nil {
		t.Fatalf("the filed email is not a valid email document: %v", err)
	}
	if doc.Layout != "org:"+layout || doc.Subject != "Our house letter" {
		t.Fatalf("filed layout %q with subject %q, want the organization's layout", doc.Layout, doc.Subject)
	}
	if out.journal.Status != "succeeded" || out.file == nil || out.file["name"] != "Welcome.email.json" {
		t.Fatalf("the email from an organization layout did not finish as one ready file: %+v %v", out.journal, out.file)
	}

	// The refusals, made by the same kind of caller on a fresh engine.
	e := materializeDBEngine(t)
	accept := New(e, e.Logger)
	accept.SetGoalOpener(work.New(e, e.Logger))
	owner := fmt.Sprintf("compose-layout-refusals-%d", time.Now().UnixNano())
	ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
	mine, theirs := owner+"-client", owner+"-other-client"
	for _, account := range []string{mine, theirs} {
		if _, err := e.Execute(ctx, "mutation "+call("createClientAccount", map[string]any{"accountId": account, "name": "Email client"})); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere, archived := owner+"-elsewhere", owner+"-archived"
	saveLayout(e, ctx, elsewhere, theirs)
	saveLayout(e, ctx, archived, mine)
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call("updateEmailLayout", map[string]any{
		"layoutId": archived, "status": "archived", "versionTime": time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
	})); err != nil {
		t.Fatalf("archiving the layout: %v", err)
	}
	for _, c := range []struct{ layoutId, want string }{
		{"org:" + elsewhere, "compose: that layout belongs to another organization"},
		{"org:" + archived, "compose: that layout is archived; restore it first"},
		{"org:" + owner + "-no-such-layout", "compose: that layout is not readable by you"},
	} {
		a := materializeArgs{Name: "Welcome", Format: pure.FormatJSON, OutputKind: "email_template", AccountIds: []string{mine}, LayoutId: c.layoutId, RequestId: "r-" + strings.TrimPrefix(c.layoutId, "org:")}
		if _, err := accept.materialize(ctx, owner, "", a); err == nil || err.Error() != c.want {
			t.Fatalf("%s: refusal = %v, want %q", c.layoutId, err, c.want)
		}
		written, err := accept.store().compositionById(ctx, stableMaterializeID("request", memql.BareShortId(owner), a.RequestId))
		if err != nil || written != nil {
			t.Fatalf("%s: refused, but a composition was written: %v (%v)", c.layoutId, written, err)
		}
	}
}

// A LAYOUT IS ITS ORGANIZATION'S, and the organization boundary on the real
// engine decides who reads one. A member of the layout's organization writes an
// email into it; a member of another organization, asking for an email of their
// own organization, cannot read it at all and is told so exactly as if it did
// not exist. Membership is the engine's own: a group of the organization, with
// grant resolution installed, as the Email app's layout tests set it up.
func TestMaterializeDB_AnOrganizationLayoutIsReadOnlyByItsMembers(t *testing.T) {
	e := materializeDBEngine(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	e.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })
	accept := New(e, e.Logger)
	accept.SetGoalOpener(&directMaterializeGoal{})

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, colleague, rival := "compose-org-owner-"+suffix, "compose-org-colleague-"+suffix, "compose-org-rival-"+suffix
	ours, theirs := "compose-org-ours-"+suffix, "compose-org-theirs-"+suffix
	actor := func(user string, role auth.Role) context.Context {
		return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role}), &auth.TokenInfo{Subject: user})
	}
	ownerCtx := actor(owner, auth.RoleOwner)
	seed := auth.ContextWithInternalOrigin(ownerCtx)
	exec := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := e.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", firstConstruct(q), err)
		}
	}
	for user, role := range map[string]string{owner: "owner", colleague: "writer", rival: "writer"} {
		payload, _ := json.Marshal(map[string]any{"displayName": user, "primaryEmail": user + "@example.test", "role": role, "active": true})
		exec(seed, `insert("v1:identity:user", id=`+langparser.QuoteString(user)+`, payload=`+string(payload)+`)`)
	}
	for member, account := range map[string]string{colleague: ours, rival: theirs} {
		exec(ownerCtx, "mutation "+call("createClientAccount", map[string]any{"accountId": account, "name": "Email client"}))
		group := "g-" + account
		exec(seed, "mutation "+call("writeGroup", map[string]any{"groupId": group, "name": account, "kind": "account", "accountId": account, "status": "active"}))
		exec(seed, "mutation "+call("writeGroupMembership", map[string]any{"membershipId": group + "-" + member, "groupId": group, "userId": member, "accountId": account, "origin": "added", "status": "active"}))
	}
	letter, _ := emaildoc.Layout("builtin:letter")
	letter.Subject = "Our house letter"
	raw, err := letter.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	layout, theirLayout := "compose-org-layout-"+suffix, "compose-org-their-layout-"+suffix
	for id, account := range map[string]string{layout: ours, theirLayout: theirs} {
		exec(seed, "mutation "+call("createEmailLayout", map[string]any{"layoutId": id, "accountId": account, "name": "House letter", "document": document,
			"versionTime": time.Now().UTC().Format(time.RFC3339Nano)}))
	}

	// A member of the layout's organization writes an email into it.
	member := actor(colleague, auth.RoleWriter)
	mine := materializeArgs{Name: "Welcome", Format: pure.FormatJSON, OutputKind: "email_template", AccountIds: []string{ours}, LayoutId: "org:" + layout, RequestId: "r-member-" + suffix}
	if _, err := accept.materialize(member, colleague, "", mine); err != nil {
		t.Fatalf("a member could not write an email into their organization's layout: %v", err)
	}
	input, err := accept.store().compositionInputById(member, stableMaterializeID("request", memql.BareShortId(colleague), mine.RequestId))
	if err != nil || input == nil {
		t.Fatalf("the member's request was not saved: %v %v", input, err)
	}
	encoded, err := json.Marshal(input["request"])
	if err != nil {
		t.Fatal(err)
	}
	var saved executionRequest
	if err := json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	if doc, err := savedEmailLayout(saved); err != nil || doc.Layout != "org:"+layout || doc.Subject != "Our house letter" {
		t.Fatalf("the member's request does not carry the organization's layout: %q %q %v", doc.Layout, doc.Subject, err)
	}

	// A member of another organization cannot read it, for an email of their own.
	outsider := actor(rival, auth.RoleWriter)
	theirsArgs := materializeArgs{Name: "Welcome", Format: pure.FormatJSON, OutputKind: "email_template", AccountIds: []string{theirs}, LayoutId: "org:" + layout, RequestId: "r-rival-" + suffix}
	if _, err := accept.materialize(outsider, rival, "", theirsArgs); err == nil || err.Error() != "compose: that layout is not readable by you" {
		t.Fatalf("a member of another organization: refusal = %v, want the layout to be unreadable", err)
	}
	if written, err := accept.store().compositionById(outsider, stableMaterializeID("request", memql.BareShortId(rival), theirsArgs.RequestId)); err != nil || written != nil {
		t.Fatalf("refused, but a composition was written: %v (%v)", written, err)
	}
	// What refused them is the boundary between the organizations: a layout of
	// their own organization they use like the member above.
	ownArgs := theirsArgs
	ownArgs.LayoutId, ownArgs.RequestId = "org:"+theirLayout, "r-rival-own-"+suffix
	if _, err := accept.materialize(outsider, rival, "", ownArgs); err != nil {
		t.Fatalf("a member could not use a layout of their own organization: %v", err)
	}
}

// materializeRequestDBAs prepares organization-scoped requests before acceptance.
func materializeRequestDBAs(t *testing.T, args materializeArgs, asCaller func(e *memql.MemQLEngine, owner string, args *materializeArgs) context.Context) requestOutcome {
	t.Helper()
	return materializeRequestDB(t, args, func(s *requestSetup) { s.asCaller = asCaller })
}
