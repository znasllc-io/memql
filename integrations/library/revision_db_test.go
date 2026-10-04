package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	composeint "github.com/znasllc-io/memql/integrations/compose"
	"github.com/znasllc-io/memql/integrations/work"
)

type revisionComposer struct{ calls atomic.Int32 }

func (c *revisionComposer) Compose(_ context.Context, r composeint.ComposeRequest) (composeint.ComposeReply, error) {
	c.calls.Add(1)
	if r.Draft != "# Heading\n\nA paragraph." || !strings.Contains(r.Statement, "Clarify this paragraph") {
		return composeint.ComposeReply{}, fmt.Errorf("lost the captured input")
	}
	return composeint.ComposeReply{Draft: pure.Draft{Title: "Revised", Body: "# Heading\n\nA clearer paragraph."}}, nil
}

type revisionUpload struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (u *revisionUpload) Upload(_ context.Context, _, name string, body []byte, _ string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	return "https://blob.test/" + name, nil
}

func TestDocumentRevisionRequiresExactHumanApprovalAcrossReplicas(t *testing.T) {
	available, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		dbtest.Unreachable(t, "document revisions", dbtest.DSN(), nil)
		return
	}
	if _, err = memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	composer := &revisionComposer{}
	uploader := &revisionUpload{}
	open := func() (*Integration, *memql.MemQLEngine) {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		lib := NewIntegration(e, func() *sql.DB { return db.DB })
		w := work.New(e, e.Logger, func() *bun.DB { return db })
		lib.SetReviewGoals(w)
		materializer := composeint.New(e, e.Logger)
		materializer.SetUploader(uploader, "files")
		materializer.SetComposer(composer)
		for _, integration := range []memql.IntegrationProvider{lib, w, materializer} {
			if err = e.RegisterIntegration(integration); err != nil {
				t.Fatal(err)
			}
		}
		return lib, e
	}
	first, e := open()
	second, other := open()
	owner := fmt.Sprintf("revision-%d", time.Now().UnixNano())
	actor := func(user string, role auth.Role) context.Context {
		return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: role})
	}
	ctx := actor(owner, auth.RoleWriter)
	query := func(engine *memql.MemQLEngine, ctx context.Context, kind, name string, args map[string]any) []map[string]any {
		t.Helper()
		call, err := langparser.RenderCall(name, args)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := engine.Execute(ctx, kind+" "+call)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return extractRows(raw)
	}
	source := owner + "-document"
	query(e, ctx, "mutation", "createGeneratedOutput", map[string]any{"outputId": source, "title": "Document", "body": "# Heading\n\nA paragraph.", "format": "markdown", "source": "user_created"})
	artifact := query(e, ctx, "mutation", "createArtifact", map[string]any{"sourceConceptRef": source, "ownerUserId": owner, "lens": "artifact", "kind": "generated_output", "source": "user_created", "title": "Document", "format": "markdown"})[0]
	artifactID := asString(artifact["id"])
	doc, err := first.reviewDocument(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := first.handleAddDocumentComment(ctx, map[string]any{"artifactId": artifactID, "expectedVersion": 0, "expectedRevision": doc.revision, "body": "Clarify this paragraph", "requestId": "revision-feedback", "anchor": map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": "paragraph", "sourceQuote": "A paragraph."}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var comment map[string]any
	if err = json.Unmarshal(rows[0].Payload, &comment); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"artifactId": artifactID, "expectedVersion": 0, "expectedRevision": doc.revision, "commentIds": []string{asString(comment["commentId"])}, "instruction": "Use plain language", "requestId": "revision-first-request"}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, lib := range []*Integration{first, second} {
		wg.Add(1)
		go func(lib *Integration) {
			defer wg.Done()
			_, err := lib.handleRequestDocumentRevision(ctx, args, 0)
			failures <- err
		}(lib)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if composer.calls.Load() != 0 {
		t.Fatal("AI ran before approval")
	}
	receipt, proposal, run, approval, err := second.revisionRequest(memql.ContextWithFreshRead(ctx), asString(args["requestId"]))
	if err != nil {
		t.Fatal(err)
	}
	if run["status"] != "waiting" || approval["decision"] != "" || proposal["content"] != "# Heading\n\nA paragraph." {
		t.Fatalf("bad pending request: %v %v", run, approval)
	}
	executeCtx := common.ContextWithRun(ctx, common.RunContext{RunId: receipt.RunID, GoalId: receipt.GoalID, OwnerUserId: owner, Mode: common.RunModeLive, StepKey: "revise"})
	if _, err = second.handleExecuteDocumentRevision(executeCtx, map[string]any{"requestId": args["requestId"]}, 0); err == nil {
		t.Fatal("unapproved run executed")
	}
	if _, err = second.handleDocumentRevisionStatus(actor(owner+"-outsider", auth.RoleWriter), map[string]any{"requestId": args["requestId"]}, 0); err == nil {
		t.Fatal("outsider read private proposal")
	}
	for concept, id := range map[string]string{"goal": receipt.GoalID, "run": receipt.RunID, "approval": receipt.ApprovalID} {
		if _, err = e.Execute(ctx, fmt.Sprintf(`insert("v1:work:%s", id=%q, payload={"decision":"approved"})`, concept, id)); err == nil || !strings.Contains(err.Error(), "internal origin") {
			t.Fatalf("raw review forgery %s: %v", concept, err)
		}
	}
	// Revoked write capability is rechecked even when document reads still work.
	if err = second.ValidateRevisionProposal(actor(owner, auth.RoleReader), proposal); err == nil {
		t.Fatal("reader accepted for revision")
	}
	query(other, ctx, "query", "decideApproval", map[string]any{"approvalId": receipt.ApprovalID, "decision": "approved"})
	// Repeated approval returns its receipt without duplicating the run.
	recovered := query(e, ctx, "query", "decideApproval", map[string]any{"approvalId": receipt.ApprovalID, "decision": "approved"})
	if len(recovered) != 1 || recovered[0]["recovered"] != true {
		t.Fatalf("lost decision receipt: %v", recovered)
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: other.Logger})
	auto, err := loader.LoadByName(documentRevisionTemplate)
	if err != nil || auto == nil {
		t.Fatalf("template: %v", err)
	}
	journal, err := automations.LoadRunJournal(ctx, other, receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct execution on a fresh replica from the persisted authority,
	// whose relationship owner uses the canonical representation.
	restored, err := auth.ContextWithPersistedOwner(context.Background(), "v1:identity:user:"+owner, revisionMap(run["executionAuthority"]), auth.NewIdentityResolver(auth.QueryRunnerFunc(func(context.Context, string) (any, error) { return map[string]any{"role": "writer"}, nil }), nil))
	if err != nil {
		t.Fatal(err)
	}
	executeCtx = common.ContextWithRun(restored, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode, StepKey: "revise"})
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: other, Logger: other.Logger, StepRegistry: steps.NewRegistry()})
	defer executor.Close()
	result, err := executor.ExecuteAdopted(executeCtx, auto, automations.RunAdoption{RunId: journal.RunId, Variables: journal.Variables, Journal: journal})
	if err != nil || result.Status != "completed" {
		t.Fatalf("approved template: %+v %v", result, err)
	}
	if composer.calls.Load() != 1 || len(uploader.bodies) != 1 {
		t.Fatalf("AI calls=%d uploaded=%d", composer.calls.Load(), len(uploader.bodies))
	}
	unchanged, err := first.reviewDocument(memql.ContextWithFreshRead(ctx), artifactID)
	if err != nil || unchanged.revision != doc.revision || unchanged.backing["body"] != "# Heading\n\nA paragraph." {
		t.Fatalf("original overwritten: %v %v", unchanged, err)
	}
	// The request stays the same after completion; no new AI or work identity.
	if _, err = first.handleRequestDocumentRevision(ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	if composer.calls.Load() != 1 {
		t.Fatal("request retry spent another model call")
	}
	// A new proposal approved before an intervening save must fail before AI.
	args["requestId"] = "revision-second-request"
	if _, err = first.handleRequestDocumentRevision(ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	secondReceipt, secondProposal, _, _, err := second.revisionRequest(ctx, asString(args["requestId"]))
	if err != nil {
		t.Fatal(err)
	}
	query(e, ctx, "query", "decideApproval", map[string]any{"approvalId": secondReceipt.ApprovalID, "decision": "approved"})
	args["requestId"] = "revision-pending-request"
	if _, err = first.handleRequestDocumentRevision(ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	pendingReceipt, _, _, _, err := second.revisionRequest(ctx, asString(args["requestId"]))
	if err != nil {
		t.Fatal(err)
	}
	args["requestId"] = "revision-second-request"
	if _, err = first.handleEditDocument(ctx, map[string]any{"documentId": source, "content": "# New content", "expectedVersion": 0, "expectedRevision": doc.revision}, 0); err != nil {
		t.Fatal(err)
	}
	declineCall, _ := langparser.RenderCall("decideApproval", map[string]any{"approvalId": pendingReceipt.ApprovalID, "decision": "approved"})
	if _, err = other.Execute(ctx, "query "+declineCall); err == nil || !strings.Contains(err.Error(), "document changed") {
		t.Fatalf("source changed before approval was admitted: %v", err)
	}
	query(other, ctx, "query", "decideApproval", map[string]any{"approvalId": pendingReceipt.ApprovalID, "decision": "rejected"})
	if err = second.ValidateRevisionProposal(ctx, secondProposal); err == nil {
		t.Fatal("stale source accepted")
	}
	staleCtx := common.ContextWithRun(ctx, common.RunContext{RunId: secondReceipt.RunID, GoalId: secondReceipt.GoalID, OwnerUserId: owner, Mode: common.RunModeLive, StepKey: "revise"})
	if _, err = second.handleExecuteDocumentRevision(staleCtx, map[string]any{"requestId": args["requestId"]}, 0); err == nil {
		t.Fatal("changed source executed")
	}
	if composer.calls.Load() != 1 {
		t.Fatal("changed source spent AI")
	}
}
