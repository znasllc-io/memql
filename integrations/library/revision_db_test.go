package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/work"
)

// Only the model boundary is replaced. The installed DSL, journal, authorization,
// two independent engines, approvals and version writes run against PostgreSQL.
type revisionAI struct {
	calls  atomic.Int32
	answer revisionAnswer
}

func (*revisionAI) IntegrationName() string { return "agents" }
func (a *revisionAI) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "invokePrompt", Handler: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		data := revisionMap(args["data"])
		passages, valid := data["passages"].(string)
		if args["templateId"] != "libraryRevisionPassages" || !valid || !json.Valid([]byte(passages)) || data["document"] == nil {
			return nil, fmt.Errorf("DSL lost the review prompt or captured feedback")
		}
		a.calls.Add(1)
		body, _ := json.Marshal(a.answer)
		return reviewResult(map[string]any{"reply": string(body)})
	}}}
}

type revisionDB struct {
	t             *testing.T
	ai            *revisionAI
	first, second *Integration
	engine, other *memql.MemQLEngine
	ctx           context.Context
	owner         string
}

func newRevisionDB(t *testing.T) *revisionDB {
	t.Helper()
	available, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		dbtest.Unreachable(t, "document revisions", dbtest.DSN(), nil)
		return nil
	}
	if _, err = memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	f := &revisionDB{t: t, ai: &revisionAI{}, owner: fmt.Sprintf("revision-%d", time.Now().UnixNano())}
	f.ctx = revisionActor(f.owner, auth.RoleWriter)
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
		for _, integration := range []memql.IntegrationProvider{lib, w, f.ai} {
			if err = e.RegisterIntegration(integration); err != nil {
				t.Fatal(err)
			}
		}
		return lib, e
	}
	f.first, f.engine = open()
	f.second, f.other = open()
	return f
}
func revisionActor(user string, role auth.Role) context.Context {
	return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: role})
}
func (f *revisionDB) query(e *memql.MemQLEngine, ctx context.Context, kind, name string, args map[string]any) []map[string]any {
	f.t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := e.Execute(ctx, kind+" "+call)
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return extractRows(raw)
}
func (f *revisionDB) document(source string) (string, reviewDocument) {
	f.t.Helper()
	id := fmt.Sprintf("%s-document-%d", f.owner, time.Now().UnixNano())
	f.query(f.engine, f.ctx, "mutation", "createGeneratedOutput", map[string]any{"outputId": id, "title": "Review fixture.md", "body": source, "format": "markdown", "source": "user_created"})
	artifact := f.query(f.engine, f.ctx, "mutation", "createArtifact", map[string]any{"sourceConceptRef": id, "ownerUserId": f.owner, "lens": "artifact", "kind": "generated_output", "source": "user_created", "title": "Review fixture.md", "format": "markdown"})[0]
	artifactID := asString(artifact["id"])
	doc, err := f.first.reviewDocument(f.ctx, artifactID)
	if err != nil {
		f.t.Fatal(err)
	}
	return artifactID, doc
}
func (f *revisionDB) submit(artifact string, doc reviewDocument, anchor map[string]any, body string) (map[string]any, string) {
	f.t.Helper()
	rows, err := f.first.handleAddDocumentComment(f.ctx, map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "anchor": anchor, "body": body, "requestId": "note-" + fmt.Sprint(time.Now().UnixNano())}, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var saved map[string]any
	_ = json.Unmarshal(rows[0].Payload, &saved)
	commentID := asString(saved["commentId"])
	args := map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "commentIds": []string{commentID}, "instruction": "", "requestId": "request-" + fmt.Sprint(time.Now().UnixNano())}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, lib := range []*Integration{f.first, f.second} {
		wg.Add(1)
		go func(lib *Integration) {
			defer wg.Done()
			_, err := lib.handleRequestDocumentRevision(f.ctx, args, 0)
			errs <- err
		}(lib)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return args, commentID
}
func (f *revisionDB) execute(engine *memql.MemQLEngine, request string, resume bool) error {
	f.t.Helper()
	ids, _ := revisionIdentity(f.ctx, request)
	journal, err := automations.LoadRunJournal(f.ctx, engine, ids.RunID)
	if err != nil {
		f.t.Fatal(err)
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: engine.Logger})
	auto, err := loader.LoadByName(documentRevisionTemplate)
	if err != nil || auto == nil {
		f.t.Fatalf("template: %v", err)
	}
	// No originating process's local state crosses this hop. Authority is restored
	// from the persisted run exactly as the receiving agent does.
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), "v1:identity:user:"+f.owner, journal.ExecutionAuthority, auth.NewIdentityResolver(auth.QueryRunnerFunc(func(context.Context, string) (any, error) { return map[string]any{"role": "writer"}, nil }), nil))
	if err != nil {
		f.t.Fatal(err)
	}
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode})
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: engine, Logger: engine.Logger, StepRegistry: steps.NewRegistry()})
	defer executor.Close()
	if resume {
		_, err = executor.ResumeFrom(ctx, journal, auto, &automations.ResumeOptions{})
	} else {
		_, err = executor.ExecuteAdopted(ctx, auto, automations.RunAdoption{RunId: journal.RunId, Variables: journal.Variables, Journal: journal})
	}
	return err
}
func TestDocumentRevisionAnalyzesThenApprovesAndAppliesAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	for _, tc := range []struct {
		name, source, quote, feedback, want string
		extension                           bool
		edits                               []revisionReplacement
	}{
		{name: "precise rephrase", source: "# Guide\n\nKeep the opening. This bit is verbose. Keep the ending.\n", quote: "This bit is verbose.", feedback: "Rephrase only this sentence.", want: "# Guide\n\nKeep the opening. This is clear. Keep the ending.\n", edits: []revisionReplacement{{Before: "This bit is verbose.", After: "This is clear.", Reason: "Shorten the selected sentence"}}},
		{name: "delete", source: "# Guide\n\nKeep this. Remove this. Keep that.\n", quote: "Remove this.", feedback: "Delete this sentence.", want: "# Guide\n\nKeep this. Keep that.\n", edits: []revisionReplacement{{Before: "Remove this. ", After: "", Reason: "Remove the selected sentence"}}},
		{name: "move and expand", source: "# Guide\n\nMove this example. Keep the intro.\n\n## Examples\n\nExisting example.\n", quote: "Move this example.", feedback: "Move this into Examples and explain it.", want: "# Guide\n\nKeep the intro.\n\n## Examples\n\nExisting example.\n\nMoved example, with an explanation.\n", edits: []revisionReplacement{{Before: "Move this example. ", After: "", Reason: "Remove from intro"}, {Before: "Existing example.", After: "Existing example.\n\nMoved example, with an explanation.", Reason: "Move into Examples and explain"}}},
		{name: "extend imported document", source: "# Imported note\n\nOriginal content.\n", extension: true, feedback: "Add next steps.", want: "# Imported note\n\nOriginal content.\n\n## Next steps\n\nTry the example.\n", edits: []revisionReplacement{{Before: "Original content.", After: "Original content.\n\n## Next steps\n\nTry the example.", Reason: "Extend with requested next steps"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact, doc := f.document(tc.source)
			anchor := map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": tc.quote, "sourceQuote": strings.Split(tc.source, "\n")[2]}
			if tc.extension {
				anchor = map[string]any{"kind": "document-end"}
			}
			args, note := f.submit(artifact, doc, anchor, tc.feedback)
			request := asString(args["requestId"])
			f.ai.answer = revisionAnswer{Summary: tc.name, Edits: tc.edits}
			for n := range f.ai.answer.Edits {
				f.ai.answer.Edits[n].CommentIDs = []string{note}
			}
			calls := f.ai.calls.Load()
			ids, captured, run, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
			if err != nil || run["status"] != "running" || approval != nil {
				t.Fatalf("analysis did not start before approval: %v %v", run, err)
			}
			if _, err = f.first.handleExecuteDocumentRevision(common.ContextWithRun(f.ctx, common.RunContext{RunId: ids.RunID, GoalId: ids.GoalID, OwnerUserId: f.owner}), map[string]any{"requestId": request}, 0); err == nil {
				t.Fatal("applied without approval")
			}
			var wait *workstate.HumanWait
			if err = f.execute(f.engine, request, false); !errors.As(err, &wait) {
				t.Fatalf("DSL did not wait for a person: %v", err)
			}
			if f.ai.calls.Load() != calls+1 {
				t.Fatal("analysis did not make exactly one model call")
			}
			_, _, run, approval, err = f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
			if err != nil || run["status"] != "waiting" {
				t.Fatalf("missing durable wait: %v %v", run, err)
			}
			if revisionMap(approval["subject"])["revisedContent"] != tc.want {
				t.Fatalf("proposal lost precise edits: %v", approval["subject"])
			}
			unchanged, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			if err != nil || unchanged.backing["body"] != tc.source {
				t.Fatal("changed source before approval")
			}
			if _, err = f.second.handleDocumentRevisionStatus(revisionActor("outsider", auth.RoleWriter), map[string]any{"requestId": request}, 0); err == nil {
				t.Fatal("outsider read review")
			}
			if err = f.second.ValidateRevisionProposal(revisionActor(f.owner, auth.RoleReader), captured); err == nil {
				t.Fatal("reader approved write")
			}
			decision := map[string]any{"approvalId": memql.BareShortId(asString(approval["id"])), "decision": "approved"}
			f.query(f.other, f.ctx, "query", "decideApproval", decision)
			f.query(f.engine, f.ctx, "query", "decideApproval", decision)
			if err = f.execute(f.other, request, true); err != nil {
				t.Fatalf("resume on second replica: %v", err)
			}
			if f.ai.calls.Load() != calls+1 {
				t.Fatal("resume repeated AI analysis")
			}
			changed, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			if err != nil || changed.backing["body"] != tc.want || changed.version != doc.version+1 {
				t.Fatalf("approved result not saved exactly once: body=%v version=%d err=%v", changed.backing["body"], changed.version, err)
			}
			if _, err = f.first.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
				t.Fatal(err)
			}
			status, err := f.second.handleDocumentRevisionStatus(f.ctx, map[string]any{"requestId": request}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			_ = json.Unmarshal(status[0].Payload, &payload)
			if payload["status"] != "succeeded" || revisionMap(payload["result"])["applied"] != true {
				t.Fatalf("missing completion receipt: %v", payload)
			}
		})
	}
}
func TestDocumentRevisionRefusesStaleApprovalAndAllowsDecline(t *testing.T) {
	f := newRevisionDB(t)
	source := "# Guide\n\nA paragraph."
	artifact, doc := f.document(source)
	args, note := f.submit(artifact, doc, map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": "paragraph", "sourceQuote": "A paragraph."}, "Clarify this")
	f.ai.answer = revisionAnswer{Summary: "Clarify", Edits: []revisionReplacement{{Before: "A paragraph.", After: "A clearer paragraph.", Reason: "Clarify", CommentIDs: []string{note}}}}
	request := asString(args["requestId"])
	var wait *workstate.HumanWait
	if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	if _, err := f.first.handleEditDocument(f.ctx, map[string]any{"documentId": doc.source, "content": "A concurrent edit", "expectedVersion": doc.version, "expectedRevision": doc.revision}, 0); err != nil {
		t.Fatal(err)
	}
	approvalArgs := map[string]any{"approvalId": wait.ApprovalID, "decision": "approved"}
	call, _ := langparser.RenderCall("decideApproval", approvalArgs)
	if _, err := f.other.Execute(f.ctx, "query "+call); err == nil || !strings.Contains(err.Error(), "document changed") {
		t.Fatalf("stale approval allowed: %v", err)
	}
	approvalArgs["decision"] = "rejected"
	f.query(f.other, f.ctx, "query", "decideApproval", approvalArgs)
	_, _, run, _, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil || run["status"] != "failed" {
		t.Fatalf("decline did not stop run: %v %v", run, err)
	}
}
