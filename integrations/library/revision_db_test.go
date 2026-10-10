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
	firstAnswer           *revisionAnswer
	completionReply       string
	completionAfterRepair string
	completionDocuments   []string
	onCompletion          func(context.Context, int32) error
	completionCalls       atomic.Int32
	repairCalls           atomic.Int32
	calls                 atomic.Int32
	researchCalls         atomic.Int32
	retainedEvidenceCalls atomic.Int32
	appCalls              atomic.Int32
	needsResearch         bool
	imagePlan             []revisionImageSpec
	replacementImagePlan  []revisionImageSpec
	imagePlanCalls        atomic.Int32
	imagePlanPrevious     string
	imagePlanSchema       any
	parallelResearch      bool
	assessmentReply       string
	assessmentError       error
	assessmentCalls       atomic.Int32
	headlessScope         string
	researchDocument      string
	researchPassages      string
	editDocument          string
	appStarted            chan struct{}
	headlessStarted       chan struct{}
	expectedReference     string
	appError              error
	answer                revisionAnswer
}

func (*revisionAI) IntegrationName() string { return "agents" }
func (a *revisionAI) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "ensureForGoal", Handler: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return reviewResult(map[string]any{"agentId": "review-test-agent"})
		}},
		{Name: "runAgentTurn", Handler: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			data := revisionMap(args["data"])
			if a.expectedReference != "" && !strings.Contains(asString(data["references"]), a.expectedReference) {
				return nil, fmt.Errorf("reference contents did not reach the DSL model step")
			}
			if args["templateId"] != "libraryRevisionResearch" || data["document"] == nil || data["passages"] == nil {
				return nil, fmt.Errorf("DSL omitted evidence stage context")
			}
			a.researchCalls.Add(1)
			if !a.parallelResearch && a.appCalls.Load() == 0 {
				return nil, fmt.Errorf("local research started before the app attempt")
			}
			a.headlessScope = asString(data["gaps"])
			a.researchDocument = asString(data["document"])
			a.researchPassages = asString(data["passages"])
			if a.headlessStarted != nil {
				close(a.headlessStarted)
				select {
				case <-a.appStarted:
				case <-time.After(3 * time.Second):
					return nil, fmt.Errorf("independent researchers did not overlap")
				}
			}
			var priorEvidence []string
			raw, _ := json.Marshal(data["priorEvidence"])
			if err := json.Unmarshal(raw, &priorEvidence); err != nil {
				return nil, fmt.Errorf("research prompt contains receipt envelopes instead of evidence: %w", err)
			}
			if strings.Contains(strings.Join(priorEvidence, "\n"), "Independent app evidence") {
				a.retainedEvidenceCalls.Add(1)
			}
			return reviewResult(map[string]any{"reply": "Evidence report for the selected feedback."})
		}},
		{Name: "invokePrompt", Handler: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			data := revisionMap(args["data"])
			if args["templateId"] == "libraryRevisionCompletion" {
				n := a.completionCalls.Add(1)
				if len(a.completionDocuments) > 0 && (int(n) > len(a.completionDocuments) || data["document"] != a.completionDocuments[n-1]) {
					return nil, fmt.Errorf("completion check did not assess the actual proposed document: %v", data["document"])
				}
				if a.onCompletion != nil {
					if err := a.onCompletion(ctx, n); err != nil {
						return nil, err
					}
				}
				reply := a.completionReply
				if a.repairCalls.Load() > 0 {
					reply = a.completionAfterRepair
				}
				if reply == "" {
					reply = "satisfied"
				}
				return reviewResult(map[string]any{"reply": reply})
			}
			if a.expectedReference != "" && !strings.Contains(asString(data["references"]), a.expectedReference) {
				return nil, fmt.Errorf("reference contents did not reach the DSL model step")
			}
			if args["templateId"] == "libraryRevisionIntent" {
				intent := "edit"
				if len(a.imagePlan) > 0 {
					return reviewResult(map[string]any{"reply": "images"})
				}
				if a.needsResearch {
					intent = "research"
				}
				if a.parallelResearch {
					intent = "parallel"
				}
				return reviewResult(map[string]any{"reply": intent})
			}
			if args["templateId"] == "libraryRevisionImagePlan" {
				a.imagePlanPrevious = asString(data["previous"])
				a.imagePlanSchema = args["responseSchema"]
				plan := a.imagePlan
				if a.imagePlanCalls.Add(1) > 1 && a.replacementImagePlan != nil {
					plan = a.replacementImagePlan
				}
				raw, _ := json.Marshal(map[string]any{"images": plan, "limitations": ""})
				return reviewResult(map[string]any{"reply": string(raw)})
			}
			if args["templateId"] == "libraryRevisionAppResearch" {
				a.appCalls.Add(1)
				if a.appStarted != nil {
					close(a.appStarted)
					select {
					case <-a.headlessStarted:
					case <-time.After(3 * time.Second):
						return nil, fmt.Errorf("independent researchers did not overlap")
					}
				}
				if a.appError != nil {
					return nil, a.appError
				}
				return reviewResult(map[string]any{"reply": "Independent app evidence with a second source."})
			}
			if args["templateId"] == "libraryRevisionEvidence" {
				a.assessmentCalls.Add(1)
				if a.appCalls.Load() == 0 || !strings.Contains(asString(data["appEvidence"]), "Independent app evidence") {
					return nil, fmt.Errorf("assessment ran without the app's returned evidence")
				}
				if a.assessmentError != nil {
					return nil, a.assessmentError
				}
				reply := a.assessmentReply
				if reply == "" {
					reply = "Verify the original claim against the cited primary source."
				}
				return reviewResult(map[string]any{"reply": reply})
			}
			a.editDocument = asString(data["document"])
			passages, valid := data["passages"].(string)
			expectedEvidence := "Editorial change:"
			if a.needsResearch {
				expectedEvidence = "Evidence report for the selected feedback."
				if a.assessmentReply == "sufficient" && !a.parallelResearch {
					expectedEvidence = "Independent app evidence"
				}
			}
			if (args["templateId"] != "libraryRevisionPassages" && args["templateId"] != "libraryRevisionItem") || !strings.Contains(asString(data["evidence"]), expectedEvidence) || !valid || !json.Valid([]byte(passages)) || data["document"] == nil {
				return nil, fmt.Errorf("DSL lost the review prompt or captured feedback")
			}
			if a.needsResearch && a.appError == nil && !strings.Contains(asString(data["evidence"]), "Independent app evidence") {
				return nil, fmt.Errorf("app evidence missing from reconciliation")
			}
			if a.needsResearch && a.appError != nil && !strings.Contains(asString(data["evidence"]), "App research was unavailable") {
				return nil, fmt.Errorf("app failure disguised as research")
			}
			if schema := revisionMap(args["responseSchema"]); schema["type"] != "object" || schema["additionalProperties"] != false || args["progress"] != true {
				return nil, fmt.Errorf("revision analysis lost its strict edit protocol or public lifecycle")
			}
			count := a.calls.Add(1)
			answer := a.answer
			if count == 1 && a.firstAnswer != nil {
				answer = *a.firstAnswer
			}
			if asString(data["recovery"]) != "" {
				a.repairCalls.Add(1)
				if a.completionCalls.Load() > 0 && !strings.Contains(asString(data["recovery"]), a.completionReply) {
					return nil, fmt.Errorf("repair lost the independent check's concrete findings")
				}
			}
			expectedReview := a.completionReply
			if expectedReview == "" {
				expectedReview = "satisfied"
			}
			if args["templateId"] == "libraryRevisionItem" && (a.completionCalls.Load() == 0 || data["reviewNotes"] != expectedReview) {
				return nil, fmt.Errorf("amendment did not receive its independent preflight assessment")
			}
			body, _ := json.Marshal(answer)
			return reviewResult(map[string]any{"reply": string(body)})
		}}}
}

type revisionDB struct {
	t             *testing.T
	ai            *revisionAI
	first, second *Integration
	engine, other *memql.MemQLEngine
	templates     map[*memql.MemQLEngine]*automations.Automation
	ctx           context.Context
	owner         string
	resetCase     func()
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
	// Each replica compiles its own immutable definition once, as an installed
	// service does. Recompiling the entire DSL tree for every execute/resume
	// wastes this suite's CI budget; journals and executors remain per call.
	f.templates = make(map[*memql.MemQLEngine]*automations.Automation)
	for _, engine := range []*memql.MemQLEngine{f.engine, f.other} {
		loader := automations.NewLoader(automations.LoaderOptions{Logger: engine.Logger})
		auto, err := loader.LoadByName(documentRevisionTemplate)
		if err != nil || auto == nil {
			t.Fatalf("template: %v", err)
		}
		f.templates[engine] = auto
	}
	first, second := *f.first, *f.second
	f.resetCase = func() {
		*f.ai = revisionAI{}
		*f.first, *f.second = first, second
	}
	return f
}

// Reuse only immutable engine/DSL setup within one sequential test group.
// Each case gets new ownership, rows and model/adapter state. Registered
// handlers still point at these restored integration instances.
func (f *revisionDB) isolatedCase(t *testing.T) *revisionDB {
	t.Helper()
	f.resetCase()
	fresh := *f
	fresh.t = t
	fresh.owner = fmt.Sprintf("revision-%d", time.Now().UnixNano())
	fresh.ctx = revisionActor(fresh.owner, auth.RoleWriter)
	return &fresh
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
	auto := f.templates[engine]
	if auto == nil {
		f.t.Fatal("replica has no compiled revision template")
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
		opts := &automations.ResumeOptions{}
		if journal.Rerun != nil {
			journal, opts, err = automations.PrepareRerun(journal, nil, auto)
			if err != nil {
				f.t.Fatal(err)
			}
		}
		_, err = executor.ResumeFrom(ctx, journal, auto, opts)
	} else {
		_, err = executor.ExecuteAdopted(ctx, auto, automations.RunAdoption{RunId: journal.RunId, Variables: journal.Variables, Journal: journal})
	}
	return err
}

// Revision, image, history, attachment and lookup cases mount the same immutable
// DSL on two independent replicas.
// Bootstrap them once under this parent; each case owns fresh document/run IDs
// and model state. Do not use a package-global fixture or parallel subtests.
func TestDocumentRevisionWorkflow(t *testing.T) {
	shared := newRevisionDB(t)
	cases := []struct {
		name string
		run  func(*testing.T, *revisionDB)
	}{
		{"HistoryBranchesAcrossReplicas", testHistoryBranchesAcrossReplicas},
		{"HistoryForkReadsOwnedFileBytes", testHistoryForkReadsOwnedFileBytes},
		{"PersonalDocumentNotesNeverEnterAIRevision", testPersonalDocumentNotesNeverEnterAIRevision},
		{"ArtifactForFileResolvesBareReceiptAcrossEngines", testArtifactForFileResolvesBareReceiptAcrossEngines},
		{"ReviewAttachmentsAcrossReplicas", testReviewAttachmentsAcrossReplicas},
		{"PreparedRevisionImagesRecoverAndPinAcrossReplicas", testPreparedRevisionImagesRecoverAndPinAcrossReplicas},
		{"RevisionDSLPlansAcquiresAndProposesAnImage", testRevisionDSLPlansAcquiresAndProposesAnImage},
		{"RevisionDSLRepairsImagePlanBeforeEffects", testRevisionDSLRepairsImagePlanBeforeEffects},
		{"RevisionDSLRepairsOverlappingImageEdits", testRevisionDSLRepairsOverlappingImageEdits},
		{"RevisionDSLRepairsRemoteImageSourceAndContinuesMixedBatch", testRevisionDSLRepairsRemoteImageSourceAndContinuesMixedBatch},
		{"RevisionCompletionRepairsNoopAndReusesSavedImages", testRevisionCompletionRepairsNoopAndReusesSavedImages},
		{"RevisionNoopAssessmentAndBoundedRepair", testRevisionNoopAssessmentAndBoundedRepair},
		{"RevisionPartialFeedbackRepair", testRevisionPartialFeedbackRepair},
		{"RevisionAmendmentPreservesImagePinsAcrossReplicas", testRevisionAmendmentPreservesImagePinsAcrossReplicas},
		{"AppliedFeedbackCannotBeDeletedAcrossReplicas", testAppliedFeedbackCannotBeDeletedAcrossReplicas},
		{"DeleteDocumentAnnotationsAcrossReplicas", testDeleteDocumentAnnotationsAcrossReplicas},
		{"DeleteProposedFeedbackInvalidatesStaleReview", testDeleteProposedFeedbackInvalidatesStaleReview},
		{"DocumentRevisionAnalyzesThenApprovesAndAppliesAcrossReplicas", testDocumentRevisionAnalyzesThenApprovesAndAppliesAcrossReplicas},
		{"DocumentResearchCombinesReportsAndContinuesAfterAppQuotaFailure", testDocumentResearchCombinesReportsAndContinuesAfterAppQuotaFailure},
		{"DocumentResearchRouting", testDocumentResearchRouting},
		{"DocumentResearchScopesContext", testDocumentResearchScopesContext},
		{"DocumentRetryReusesCompletedAppEvidence", testDocumentRetryReusesCompletedAppEvidence},
		{"DocumentRevisionRecoveryOrdersByRequestAndRechecksAccess", testDocumentRevisionRecoveryOrdersByRequestAndRechecksAccess},
		{"DocumentRevisionRefusesStaleApprovalAndAllowsDecline", testDocumentRevisionRefusesStaleApprovalAndAllowsDecline},
		{"DocumentRevisionRecoversAfterHistoryWriteBeforeHeadMove", testDocumentRevisionRecoversAfterHistoryWriteBeforeHeadMove},
		{"DocumentRevisionItemsModifyAndAcceptSubsetAcrossReplicas", testDocumentRevisionItemsModifyAndAcceptSubsetAcrossReplicas},
		{"DocumentRevisionStatusCarriesRetryAcrossReplicas", testDocumentRevisionStatusCarriesRetryAcrossReplicas},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, shared.isolatedCase(t))
		})
	}
}

func testDocumentRevisionAnalyzesThenApprovesAndAppliesAcrossReplicas(t *testing.T, f *revisionDB) {
	for _, tc := range []struct {
		name, source, quote, feedback, want string
		extension, section, whole           bool
		edits                               []revisionReplacement
	}{
		{name: "document-wide name correction", whole: true, source: "# Guide\n\n**Alice** opens the workshop.\n\n## Close\n\nThank Alice at the end.\n", feedback: "Replace Alice with Morgan throughout.", want: "# Guide\n\n**Morgan** opens the workshop.\n\n## Close\n\nThank Morgan at the end.\n", edits: []revisionReplacement{{Before: "**Alice**", After: "**Morgan**", Reason: "Correct the name"}, {Before: "Thank Alice", After: "Thank Morgan", Reason: "Correct the second reference"}}},
		{name: "requested full rewrite", whole: true, source: "# Workshop\n\nAlice prepares tools.\n", feedback: "Rewrite as a first-person checklist.", want: "# Workshop checklist\n\n- I prepare the tools.\n", edits: []revisionReplacement{{Before: "# Workshop\n\nAlice prepares tools.\n", After: "# Workshop checklist\n\n- I prepare the tools.\n", Reason: "Apply the requested perspective and structure"}}},
		{name: "precise rephrase", source: "# Guide\n\nKeep the opening. This bit is verbose. Keep the ending.\n", quote: "This bit is verbose.", feedback: "Rephrase only this sentence.", want: "# Guide\n\nKeep the opening. This is clear. Keep the ending.\n", edits: []revisionReplacement{{Before: "This bit is verbose.", After: "This is clear.", Reason: "Shorten the selected sentence"}}},
		{name: "delete", source: "# Guide\n\nKeep this. Remove this. Keep that.\n", quote: "Remove this.", feedback: "Delete this sentence.", want: "# Guide\n\nKeep this. Keep that.\n", edits: []revisionReplacement{{Before: "Remove this. ", After: "", Reason: "Remove the selected sentence"}}},
		{name: "move and expand", source: "# Guide\n\nMove this example. Keep the intro.\n\n## Examples\n\nExisting example.\n", quote: "Move this example.", feedback: "Move this into Examples and explain it.", want: "# Guide\n\nKeep the intro.\n\n## Examples\n\nExisting example.\n\nMoved example, with an explanation.\n", edits: []revisionReplacement{{Before: "Move this example. ", After: "", Reason: "Remove from intro"}, {Before: "Existing example.", After: "Existing example.\n\nMoved example, with an explanation.", Reason: "Move into Examples and explain"}}},
		{name: "extend existing section", source: "# Guide\n\n## Examples\n\nAn existing example.\n\n## Next\n\nKeep this.\n", quote: "Examples", section: true, feedback: "Add a practical exercise.", want: "# Guide\n\n## Examples\n\nAn existing example.\n\nTry a practical exercise.\n\n## Next\n\nKeep this.\n", edits: []revisionReplacement{{Before: "An existing example.", After: "An existing example.\n\nTry a practical exercise.", Reason: "Extend the selected section"}}},
		{name: "extend selected passage", source: "# Guide\n\nKeep the introduction. An existing example. Keep the conclusion.\n", quote: "An existing example.", feedback: "Add a practical exercise here.", want: "# Guide\n\nKeep the introduction. An existing example. Try a practical exercise. Keep the conclusion.\n", edits: []revisionReplacement{{Before: "An existing example.", After: "An existing example. Try a practical exercise.", Reason: "Expand the selected passage"}}},
		{name: "extend imported document", source: "# Imported note\n\nOriginal content.\n", extension: true, feedback: "Add next steps.", want: "# Imported note\n\nOriginal content.\n\n## Next steps\n\nTry the example.\n", edits: []revisionReplacement{{Before: "Original content.", After: "Original content.\n\n## Next steps\n\nTry the example.", Reason: "Extend with requested next steps"}}},
		{name: "extend schedule after lunch", source: "# Workshop\n\n## Schedule\n\n- 12:00 PM: Lunch break\n\n## Activities\n\nRead an excerpt.\n", extension: true, feedback: "Extend the workshop after the lunch break with time for asking questions.", want: "# Workshop\n\n## Schedule\n\n- 12:00 PM: Lunch break\n- 1:00 PM: Questions and discussion\n\n## Activities\n\nRead an excerpt.\n", edits: []revisionReplacement{{Before: "- 12:00 PM: Lunch break", After: "- 12:00 PM: Lunch break\n- 1:00 PM: Questions and discussion", Reason: "Add question time after lunch in the schedule"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact, doc := f.document(tc.source)
			anchor := map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": tc.quote, "sourceQuote": strings.Split(tc.source, "\n")[2]}
			if tc.whole {
				anchor = map[string]any{"kind": "document"}
			}
			if tc.extension {
				anchor = map[string]any{"kind": "document-end"}
			}
			if tc.section {
				anchor["scope"] = "section"
			}
			args, note := f.submit(artifact, doc, anchor, tc.feedback)
			request := asString(args["requestId"])
			recovered := f.query(f.other, f.ctx, "query", "libraryDocumentReview", map[string]any{"artifactId": artifact})
			if len(recovered) != 1 || recovered[0]["requestId"] != request {
				t.Fatalf("new editor on another replica cannot recover the review: %v", recovered)
			}
			f.ai.answer = revisionAnswer{Summary: tc.name, Edits: tc.edits}
			for n := range f.ai.answer.Edits {
				f.ai.answer.Edits[n].CommentIDs = []string{note}
			}
			calls := f.ai.calls.Load()
			ids, captured, run, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
			if err != nil || run["status"] != "running" || approval != nil {
				t.Fatalf("analysis did not start before approval: %v %v", run, err)
			}
			if tc.section {
				passages, err := revisionPassages(captured)
				if err != nil || len(passages) != 1 || passages[0].Comments[0]["scope"] != "section" || passages[0].Comments[0]["kind"] != "feedback" {
					t.Fatalf("section context lost or forced into an extension: %v %v", passages, err)
				}
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
			decision := map[string]any{"approvalId": memql.BareShortId(asString(approval["id"])), "decision": "approved", "answer": f.accepted(request)}
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

func testDocumentResearchCombinesReportsAndContinuesAfterAppQuotaFailure(t *testing.T, f *revisionDB) {
	for _, appErr := range []error{nil, errors.New("Claude Code: You've hit your weekly limit (429)"), errors.New("Codex unavailable; Claude Code weekly limit"), context.DeadlineExceeded} {
		t.Run(fmt.Sprint(appErr), func(t *testing.T) {
			f.ai.calls.Store(0)
			f.ai.appCalls.Store(0)
			f.ai.researchCalls.Store(0)
			f.ai.retainedEvidenceCalls.Store(0)
			f.ai.needsResearch, f.ai.appError = true, appErr
			artifact, doc := f.document("# Research\n\nOriginal claim.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Research and verify this claim.")
			f.ai.answer = revisionAnswer{Summary: "Verified claim", Edits: []revisionReplacement{{Before: "Original claim.", After: "Verified claim.", Reason: "Based on retrieved evidence", CommentIDs: []string{note}}}}
			request := asString(args["requestId"])
			var wait *workstate.HumanWait
			if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
				t.Fatalf("research did not reach ordinary edit approval: %v", err)
			}
			ids, _, run, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
			if err != nil || run["status"] != "waiting" || approval["kind"] != "planReview" {
				t.Fatalf("optional app failure parked the goal: %v %v %v", run, approval, err)
			}
			if f.ai.researchCalls.Load() != 1 || f.ai.appCalls.Load() != 1 || f.ai.calls.Load() != 1 {
				t.Fatal("research did not execute exactly once per branch")
			}
			f.query(f.other, f.ctx, "query", "decideApproval", map[string]any{"approvalId": memql.BareShortId(asString(approval["id"])), "decision": "approved", "answer": f.accepted(request)})
			if err := f.execute(f.other, request, true); err != nil {
				t.Fatalf("resume lost parallel results: %v", err)
			}
			changed, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			if err != nil || changed.backing["body"] != "# Research\n\nVerified claim.\n" {
				t.Fatalf("goal did not finish: %v %v", changed, err)
			}
			if f.ai.researchCalls.Load() != 1 || f.ai.appCalls.Load() != 1 {
				t.Fatal("approval spent research quota again")
			}
			if appErr != nil {
				rows := f.query(f.other, f.ctx, "query", "workStepsForOwnerRun", map[string]any{"runId": ids.RunID})
				recorded := false
				for _, row := range rows {
					if row["status"] == "failed" && strings.Contains(asString(row["errorMessage"]), appErr.Error()) {
						recorded = true
					}
				}
				if !recorded {
					t.Fatal("optional app failure disappeared from the durable journal")
				}
			}
		})
	}
}
func testDocumentRevisionRecoveryOrdersByRequestAndRechecksAccess(t *testing.T, f *revisionDB) {
	artifact, doc := f.document("# Guide\n\nOriginal content.\n")
	anchor := map[string]any{"kind": "document-end"}
	old, note := f.submit(artifact, doc, anchor, "Add next steps.")
	latest, _ := f.submit(artifact, doc, anchor, "Add practice questions.")
	// Finishing analysis on an older request creates newer row versions. Those
	// writes must not hide a request admitted later on another replica.
	f.ai.answer = revisionAnswer{Summary: "Next steps", Edits: []revisionReplacement{{Before: "Original content.", After: "Original content.\n\nTry an example.", Reason: "Add next steps", CommentIDs: []string{note}}}}
	var wait *workstate.HumanWait
	if err := f.execute(f.engine, asString(old["requestId"]), false); !errors.As(err, &wait) {
		t.Fatalf("older analysis: %v", err)
	}
	rows := f.query(f.other, f.ctx, "query", "libraryDocumentReview", map[string]any{"artifactId": artifact})
	if len(rows) != 1 || rows[0]["requestId"] != latest["requestId"] {
		t.Fatalf("older row update hid latest request: %v", rows)
	}
	head := f.query(f.other, f.ctx, "query", "workDocumentRevisionRequest", map[string]any{"artifactId": memql.BareShortId(artifact)})
	if len(head) != 1 || len(head[0]) != 1 || head[0]["requestId"] != latest["requestId"] {
		t.Fatalf("recovery must project only the latest request identity: %v", head)
	}
	if _, err := f.second.handleDocumentReview(revisionActor("outsider", auth.RoleWriter), map[string]any{"artifactId": artifact}, 0); err == nil {
		t.Fatal("outsider recovered a private document review")
	}
	rows = f.query(f.other, revisionActor("outsider", auth.RoleWriter), "query", "workDocumentRevisionRequest", map[string]any{"artifactId": memql.BareShortId(artifact)})
	if len(rows) != 0 {
		t.Fatal("outsider discovered another person's review receipt")
	}
}

func testDocumentRevisionRefusesStaleApprovalAndAllowsDecline(t *testing.T, f *revisionDB) {
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

func testDocumentRevisionRecoversAfterHistoryWriteBeforeHeadMove(t *testing.T, f *revisionDB) {
	source := "# Recovery\n\nOriginal text.\n"
	artifact, doc := f.document(source)
	args, note := f.submit(artifact, doc, map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": "Original text.", "sourceQuote": "Original text."}, "Clarify this")
	f.ai.answer = revisionAnswer{Summary: "Clarify", Edits: []revisionReplacement{{Before: "Original text.", After: "Clear text.", Reason: "Clarify", CommentIDs: []string{note}}}}
	request := asString(args["requestId"])
	var wait *workstate.HumanWait
	if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	f.query(f.other, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
	ids, _, _, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	proposal := revisionMap(approval["subject"])
	// Inject the real durable boundary: history committed, backing write absent.
	latest, _, err := f.first.latestVersion(f.ctx, doc.source)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.first.appendVersion(f.ctx, appendArgs{versionAt: nextDocumentTime(doc.backing, latest), versionId: "revision-" + ids.RunID, documentId: doc.source, versionNumber: doc.version + 1, content: asString(proposal["revisedContent"]), authorKind: "assistant", note: "Clarify", parentVersionId: stringField(latest, "id"), producedByRunId: ids.RunID, partitionId: stringField(doc.backing, "partitionId")}); err != nil {
		t.Fatal(err)
	}
	// Reconcile the bounded apply operation on another replica. The separate
	// workflow tests above exercise journal continuation at the approval gate.
	ctx := common.ContextWithRun(f.ctx, common.RunContext{RunId: ids.RunID, GoalId: ids.GoalID, OwnerUserId: f.owner})
	for _, lib := range []*Integration{f.second, f.first} {
		if _, err = lib.handleExecuteDocumentRevision(ctx, map[string]any{"requestId": request}, 0); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil || current.version != doc.version+1 || current.backing["body"] != "# Recovery\n\nClear text.\n" {
		t.Fatalf("did not recover the single version: %+v %v", current, err)
	}
	if f.ai.calls.Load() != 1 {
		t.Fatal("recovery called the model again")
	}
}

func (f *revisionDB) accepted(request string) map[string]any {
	f.t.Helper()
	status := f.query(f.other, f.ctx, "query", "libraryDocumentRevisionStatus", map[string]any{"requestId": request})[0]
	items, err := revisionItems(revisionMap(status["proposal"]))
	if err != nil {
		f.t.Fatal(err)
	}
	ids := []string{}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return map[string]any{"acceptedItemIds": ids, "proposalHash": status["proposalHash"]}
}

func testDocumentRevisionItemsModifyAndAcceptSubsetAcrossReplicas(t *testing.T, f *revisionDB) {
	f.ai.needsResearch = true
	source := "# Plan\n\nFirst paragraph.\n\nSecond paragraph.\n"
	artifact, doc := f.document(source)
	a, noteA := f.submit(artifact, doc, map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": "First paragraph.", "sourceQuote": "First paragraph."}, "Clarify the first paragraph")
	_, noteB := f.submit(artifact, doc, map[string]any{"kind": "markdown", "startLine": 4, "endLine": 5, "quote": "Second paragraph.", "sourceQuote": "Second paragraph."}, "Clarify the second paragraph")
	a["requestId"] = "combined-" + fmt.Sprint(time.Now().UnixNano())
	a["commentIds"] = []string{noteA, noteB}
	if _, err := f.first.handleRequestDocumentRevision(f.ctx, a, 0); err != nil {
		t.Fatal(err)
	}
	request := asString(a["requestId"])
	recordModel := func(request, model string) {
		t.Helper()
		ids, err := revisionIdentity(f.ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if err = work.NewModelJournal(f.engine).Record(f.ctx, f.owner, memql.JournaledCall{RunId: ids.RunID, StepKey: "analysis", RequestHash: request, Provider: "fixture", Model: model, PromptRef: "libraryRevisionPassages", Served: "live"}); err != nil {
			t.Fatal(err)
		}
	}
	recordModel(request, "initial-model")
	f.ai.answer = revisionAnswer{Summary: "Two changes", Edits: []revisionReplacement{{Before: "First paragraph.", After: "First clear paragraph.", Reason: "Clarify first", CommentIDs: []string{noteA}}, {Before: "Second paragraph.", After: "Second clear paragraph.", Reason: "Clarify second", CommentIDs: []string{noteB}}}}
	var wait *workstate.HumanWait
	if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	_, _, _, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	proposal := revisionMap(approval["subject"])
	items, err := revisionItems(proposal)
	if err != nil || len(items) != 2 {
		t.Fatalf("items: %v %v", items, err)
	}
	inherited := revisionMap(revisionMap(proposal["attribution"])["items"])[items[1].ID]
	invalid, _ := langparser.RenderCall("decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": map[string]any{"acceptedItemIds": []string{"invented"}, "proposalHash": workstate.ArtifactHash(proposal)}})
	if _, err = f.other.Execute(f.ctx, "query "+invalid); err == nil {
		t.Fatal("invalid item decision accepted")
	}
	newer := "modified-" + fmt.Sprint(time.Now().UnixNano())
	args := map[string]any{"requestId": request, "approvalId": wait.ApprovalID, "itemId": items[0].ID, "instruction": "Research the first passage and use the evidence", "newRequestId": newer}
	for _, lib := range []*Integration{f.first, f.second} {
		if _, err = lib.handleModifyRevisionItem(f.ctx, args, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.second.ValidateRevisionProposal(f.ctx, proposal); err == nil {
		t.Fatal("superseded proposal still applicable")
	}
	recordModel(newer, "replacement-model")
	f.ai.answer = revisionAnswer{Summary: "A researched first passage", Edits: []revisionReplacement{{Before: "First paragraph.", After: "First verified paragraph.", Reason: "Verified against fixture evidence", CommentIDs: []string{noteA}}}}
	if err = f.execute(f.other, newer, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	_, _, _, approval, err = f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), newer)
	if err != nil {
		t.Fatal(err)
	}
	proposal = revisionMap(approval["subject"])
	modified, err := revisionItems(proposal)
	if err != nil || len(modified) != 2 {
		t.Fatalf("modified items: %v %v", modified, err)
	}
	if modified[1].ID != items[1].ID {
		t.Fatal("unmodified proposal item was regenerated")
	}
	attributed := revisionMap(revisionMap(proposal["attribution"])["items"])
	if workstate.ArtifactHash(revisionMap(inherited)) != workstate.ArtifactHash(revisionMap(attributed[items[1].ID])) {
		t.Fatal("unchanged item lost its original model attribution")
	}
	modelJSON, _ := json.Marshal(attributed[modified[0].ID])
	if !strings.Contains(string(modelJSON), "replacement-model") || strings.Contains(string(modelJSON), "initial-model") {
		t.Fatalf("replacement attribution: %s", modelJSON)
	}
	decision := map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": map[string]any{"acceptedItemIds": []string{modified[0].ID}, "proposalHash": workstate.ArtifactHash(proposal)}}
	f.query(f.other, f.ctx, "query", "decideApproval", decision)
	f.query(f.engine, f.ctx, "query", "decideApproval", decision)
	if err = f.execute(f.engine, newer, true); err != nil {
		t.Fatal(err)
	}
	changed, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if changed.backing["body"] != "# Plan\n\nFirst verified paragraph.\n\nSecond paragraph.\n" {
		t.Fatalf("declined paragraph changed: %q", changed.backing["body"])
	}
	if f.ai.researchCalls.Load() != 2 || f.ai.calls.Load() != 2 {
		t.Fatal("evidence or edits reran on approval")
	}
}

func testDocumentRevisionStatusCarriesRetryAcrossReplicas(t *testing.T, f *revisionDB) {
	artifact, doc := f.document("# Guide\n\nOriginal content.\n")
	request, _ := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Expand the guide.")
	ids, _ := revisionIdentity(f.ctx, asString(request["requestId"]))
	resumeAt := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "updateWorkRun", map[string]any{
		"runId": ids.RunID, "status": "waiting", "waitingOn": map[string]any{"kind": "retry", "resumeAt": resumeAt, "reason": "internal provider detail"}, "spent": map[string]any{"retries": 2}, "errorMessage": "idle ceiling",
	})
	progressCtx := common.ContextWithRun(f.ctx, common.RunContext{RunId: ids.RunID, GoalId: ids.GoalID, StepKey: "analysis", OwnerUserId: f.owner})
	if err := f.engine.RecordWorkProgress(progressCtx, memql.WorkEvent{ID: "ai-draft", Kind: "draft", Text: `{"edits":[{"after":"First section`, Phase: "paused"}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.second.handleDocumentRevisionStatus(f.ctx, map[string]any{"requestId": request["requestId"]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(rows[0].Payload, &payload)
	if draft := revisionMap(payload["draft"]); draft["phase"] != "paused" || !strings.Contains(asString(draft["text"]), "First section") {
		t.Fatalf("cross-replica partial draft lost: %v", draft)
	}
	waiting := revisionMap(payload["waitingOn"])
	if payload["status"] != "waiting" || waiting["kind"] != "retry" || waiting["resumeAt"] != resumeAt || payload["retryCount"] != float64(2) {
		t.Fatalf("retry metadata lost: %+v", payload)
	}
	if waiting["reason"] != nil {
		t.Fatal("status unnecessarily copied internal wait details")
	}
	if payload["approvalId"] != "" {
		t.Fatal("retry appeared to be document approval")
	}
}

func testDocumentResearchRouting(t *testing.T, f *revisionDB) {
	for _, tc := range []struct {
		name, assessment string
		assessmentError  error
		parallel         bool
		localCalls       int32
	}{
		{name: "sufficient app evidence avoids local research", assessment: "sufficient", localCalls: 0},
		{name: "missing source support gets targeted research", assessment: "Read the source abstract to verify the arsenic capacity.", localCalls: 1},
		{name: "ambiguous assessment falls back", assessment: "probably sufficient", localCalls: 1},
		{name: "blank assessment falls back", assessment: "  ", localCalls: 1},
		{name: "unavailable assessment falls back", assessmentError: context.DeadlineExceeded, localCalls: 1},
		{name: "independent corroboration overlaps both researchers", assessment: "sufficient", parallel: true, localCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.ai.calls.Store(0)
			f.ai.appCalls.Store(0)
			f.ai.researchCalls.Store(0)
			f.ai.assessmentCalls.Store(0)
			f.ai.needsResearch, f.ai.parallelResearch = true, tc.parallel
			f.ai.assessmentReply, f.ai.assessmentError = tc.assessment, tc.assessmentError
			f.ai.appStarted, f.ai.headlessStarted = nil, nil
			if tc.parallel {
				f.ai.appStarted, f.ai.headlessStarted = make(chan struct{}), make(chan struct{})
			}
			artifact, doc := f.document("# Research\n\nOriginal claim.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Research and verify this claim.")
			f.ai.answer = revisionAnswer{Summary: "Verified claim", Edits: []revisionReplacement{{Before: "Original claim.", After: "Verified claim.", Reason: "Evidence", CommentIDs: []string{note}}}}
			var wait *workstate.HumanWait
			request := asString(args["requestId"])
			if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
				t.Fatalf("research did not reach review: %v", err)
			}
			if f.ai.appCalls.Load() != 1 || f.ai.assessmentCalls.Load() != 1 || f.ai.researchCalls.Load() != tc.localCalls {
				t.Fatalf("wrong route: app=%d assessment=%d local=%d", f.ai.appCalls.Load(), f.ai.assessmentCalls.Load(), f.ai.researchCalls.Load())
			}
			if tc.localCalls > 0 && !tc.parallel && tc.assessmentError == nil && strings.TrimSpace(tc.assessment) != "" && f.ai.headlessScope != tc.assessment {
				t.Fatalf("gap plan was lost: %q", f.ai.headlessScope)
			}
			f.query(f.other, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
			if err := f.execute(f.other, request, true); err != nil {
				t.Fatalf("cross-replica approval failed: %v", err)
			}
			if f.ai.appCalls.Load() != 1 || f.ai.assessmentCalls.Load() != 1 || f.ai.researchCalls.Load() != tc.localCalls {
				t.Fatal("approval repeated research")
			}
		})
	}
}

func testDocumentRetryReusesCompletedAppEvidence(t *testing.T, f *revisionDB) {
	f.ai.needsResearch = true
	artifact, doc := f.document("# Research\n\nOriginal claim.\n")
	args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Research and verify this claim.")
	f.ai.answer = revisionAnswer{Summary: "Verified claim", Edits: []revisionReplacement{{Before: "Original claim.", After: "Verified claim.", Reason: "Evidence", CommentIDs: []string{note}}}}
	request := asString(args["requestId"])
	var wait *workstate.HumanWait
	if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	ids, _, _, _, err := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise a same-run parallel-stage retry on a different replica first.
	// The engine re-enters both branches; DSL reuses the completed app receipt.
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "updateWorkRun", map[string]any{
		"runId": ids.RunID, "status": "running", "waitingOn": map[string]any{},
		"rerun": map[string]any{"requestId": "same-run-retry", "reason": "rerun", "stepKey": "evidence"},
	})
	if err = f.execute(f.other, request, true); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	if f.ai.appCalls.Load() != 1 || f.ai.researchCalls.Load() != 2 {
		t.Fatalf("same-run retry repeated app research: app=%d headless=%d rows=%v", f.ai.appCalls.Load(), f.ai.researchCalls.Load(), f.query(f.other, f.ctx, "query", "workCompletedStepsForOwnerRuns", map[string]any{"runIds": []string{ids.RunID}, "stepKey": "evidence.app/report"}))
	}
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "updateWorkRun", map[string]any{"runId": ids.RunID, "status": "waiting", "cancelRequested": true, "errorMessage": "cancelled: the person asked this work to stop"})
	args["requestId"] = "retry-" + fmt.Sprint(time.Now().UnixNano())
	if _, err = f.second.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	next := asString(args["requestId"])
	_, captured, _, _, err := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), next)
	if err != nil || !strings.Contains(fmt.Sprint(captured["previousRunIds"]), ids.RunID) {
		t.Fatalf("retry lost its validated predecessor: %v %v", captured, err)
	}
	if err = f.execute(f.other, next, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	if f.ai.appCalls.Load() != 1 || f.ai.researchCalls.Load() != 3 {
		t.Fatalf("retry repeated subscription research or skipped fresh headless research: app=%d headless=%d", f.ai.appCalls.Load(), f.ai.researchCalls.Load())
	}
	if f.ai.retainedEvidenceCalls.Load() != 3 {
		t.Fatal("new attempt did not give its researcher the existing source leads")
	}
	// An interrupted retry has no app/report receipt of its own. A later
	// attempt must still find the original completed evidence.
	nextIDs, _, _, _, _ := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), next)
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "updateWorkRun", map[string]any{"runId": nextIDs.RunID, "status": "cancelled"})
	args["requestId"] = "third-" + fmt.Sprint(time.Now().UnixNano())
	if _, err = f.second.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	if err = f.execute(f.other, asString(args["requestId"]), false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	if f.ai.appCalls.Load() != 1 || f.ai.retainedEvidenceCalls.Load() != 4 {
		t.Fatal("intervening attempt hid the completed app evidence")
	}
	// Changed direction is new work, even against the same unchanged file.
	args["requestId"] = "different-" + fmt.Sprint(time.Now().UnixNano())
	args["instruction"] = "Investigate a different question."
	if _, err = f.second.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	_, different, _, _, err := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), asString(args["requestId"]))
	if err != nil || len(fmt.Sprint(different["previousRunIds"])) > 2 {
		t.Fatalf("new feedback inherited old evidence: %v %v", different, err)
	}
}

func testDocumentResearchScopesContext(t *testing.T, f *revisionDB) {
	for _, scope := range []string{"selection", "document", "end"} {
		t.Run(scope, func(t *testing.T) {
			source := "# Research\n\nSelected claim.\n\n# Unrelated\n\n" + strings.Repeat("Unrelated material. ", 1500) + "\nEnd context."
			anchor := map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "sourceQuote": "Selected claim.", "quote": "Selected claim."}
			if scope == "document" {
				anchor = map[string]any{"kind": "document"}
			}
			if scope == "end" {
				anchor = map[string]any{"kind": "document-end", "prefix": "Unrelated material. "}
			}
			f.ai.needsResearch = true
			f.ai.appCalls.Store(0)
			f.ai.researchCalls.Store(0)
			artifact, doc := f.document(source)
			args, note := f.submit(artifact, doc, anchor, "Verify the selected claim using sources.")
			f.ai.answer = revisionAnswer{Summary: "Verified", Edits: []revisionReplacement{{Before: "Selected claim.", After: "Verified claim.", Reason: "Source", CommentIDs: []string{note}}}}
			if scope == "end" {
				f.ai.answer.Edits[0].Before = "End context."
				f.ai.answer.Edits[0].After = "End context.\n\nAdditional verified claim.\n"
			}
			var wait *workstate.HumanWait
			if err := f.execute(f.engine, asString(args["requestId"]), false); !errors.As(err, &wait) {
				t.Fatal(err)
			}
			if f.ai.researchCalls.Load() != 1 || f.ai.editDocument != source {
				t.Fatal("research tools or complete editing source lost")
			}
			if scope == "end" {
				if f.ai.researchDocument != source {
					t.Fatal("end-of-document feedback lost its context")
				}
			} else {
				if f.ai.researchDocument != "" {
					t.Fatal("research duplicated the full document")
				}
				if scope == "selection" && strings.Contains(f.ai.researchPassages, "Unrelated material.") {
					t.Fatal("unrelated prose entered selected research")
				}
				if scope == "document" && !strings.Contains(f.ai.researchPassages, "Unrelated material.") {
					t.Fatal("whole-document feedback lost content")
				}
			}
			if !strings.Contains(f.ai.researchPassages, "Verify the selected claim using sources.") {
				t.Fatal("human feedback was truncated")
			}
		})
	}
}
