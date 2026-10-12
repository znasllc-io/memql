package compose

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
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

func TestMaterializeDB_SharedCompositionKeepsSourcesPrivate(t *testing.T) {
	e := materializeDBEngine(t)
	i := New(e, e.Logger)
	i.SetGoalOpener(work.New(e, e.Logger))
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
	accept.SetGoalOpener(work.New(accepting, accepting.Logger))
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

// blankOutcome is what a request that was filed blank left behind once its run
// finished.
type blankOutcome struct {
	row     map[string]any
	file    map[string]any
	upload  *materializeUploader
	journal *automations.RunJournal
}

// materializeBlankDB drives one blank request of the given format through the
// real engine (see materializeRequestDB).
func materializeBlankDB(t *testing.T, format pure.Format) blankOutcome {
	t.Helper()
	return materializeRequestDB(t, materializeArgs{Name: "Blank", Format: format})
}

// materializeRequestDB drives one request that is filed blank through the real
// engine the way two replicas would: one accepts it (and opens the goal),
// another adopts the run and executes it. Opening a goal dispatches nothing by
// itself -- the run's own `running` event is what reaches an agent replica -- so
// a test that only accepts would wait on a composition nothing is working on.
//
// The request is made the way a person's client makes it: outside any work run,
// as an actor with no run on its context.
//
// No composer is wired on the executing node on purpose: a request that is filed
// blank must never need a model, and a stray call would fail the run loudly.
func materializeRequestDB(t *testing.T, args materializeArgs) blankOutcome {
	t.Helper()
	accepting := materializeDBEngine(t)
	executing := materializeDBEngine(t)
	accept := New(accepting, accepting.Logger)
	accept.SetGoalOpener(work.New(accepting, accepting.Logger))
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
	t.Cleanup(exec.Close)

	owner := fmt.Sprintf("compose-blank-%s-%d", args.Format, time.Now().UnixNano())
	ctx := auth.ContextWithUserActor(context.Background(), owner)
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
	return blankOutcome{row: row, file: file, upload: upload, journal: journal}
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
