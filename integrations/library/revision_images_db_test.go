package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/work"
)

type preparedImageEngine struct {
	memql.IntegrationEngineAccess
	calls int
}

func (e *preparedImageEngine) CallAIImage(ctx context.Context, req airoute.ResolveRequest, input memql.FleetImageRequest, key string, store func(context.Context, memql.FleetImage, airoute.Resolution) (string, error)) (memql.StoredImageAIResult, error) {
	e.calls++
	if req.Level != airoute.LevelFast || req.PromptName != "libraryRevisionImageGeneration" || input.Count != 1 || input.Width != 512 || input.Height != 512 || key == "" {
		return memql.StoredImageAIResult{}, fmt.Errorf("image routing or effect identity omitted")
	}
	receipt, err := store(ctx, memql.FleetImage{Data: attachmentPNG(), MediaType: "image/png"}, airoute.Resolution{ProviderName: "served-local-fixture", Model: "actual-image-model"})
	return memql.StoredImageAIResult{Receipt: receipt}, err
}

type preparedImageStore struct {
	f              *revisionDB
	loseReply      bool
	writes         int
	artifact       string
	appendProposal bool
}

func (s *preparedImageStore) SaveImage(ctx context.Context, w ImageAssetWrite) error {
	s.writes++
	digest := sha256.Sum256(w.Data)
	s.f.query(s.f.engine, auth.ContextWithInternalOrigin(ctx), "mutation", "recordPreparedLibraryImage", map[string]any{"fileId": w.FileID, "name": w.Name, "mimeType": w.MIME, "size": len(w.Data), "sha256": hex.EncodeToString(digest[:]), "blobUrl": "https://storage.invalid/image.png", "producedByRunId": w.RunID, "producedByStepKey": w.StepKey, "imageProvenance": w.Provenance, "source": "agent_generated"})
	row := s.f.query(s.f.engine, ctx, "mutation", "createArtifact", map[string]any{"sourceConceptRef": fileSourceRef(w.FileID), "ownerUserId": s.f.owner, "lens": "artifact", "kind": "file", "source": "agent_generated", "title": w.Name, "format": "image"})[0]
	s.artifact = memql.BareShortId(asString(row["id"]))
	if s.appendProposal {
		s.f.ai.answer.Edits[0].After += "\n\n![Illustration](../" + s.artifact + "/" + w.Name + ")\nAI-generated illustration; actual-image-model."
	}
	if s.loseReply {
		return errors.New("reply lost after image storage")
	}
	return nil
}

// Image workflows share one two-replica bootstrap, never model or document state.
// Avoid repeated DSL compilation without changing the CI time limit.
func TestDocumentRevisionImageWorkflow(t *testing.T) {
	shared := newRevisionDB(t)
	cases := []struct {
		name string
		run  func(*testing.T, *revisionDB)
	}{
		{"PreparedRevisionImagesRecoverAndPinAcrossReplicas", testPreparedRevisionImagesRecoverAndPinAcrossReplicas},
		{"RevisionDSLPlansAcquiresAndProposesAnImage", testRevisionDSLPlansAcquiresAndProposesAnImage},
		{"RevisionDSLRepairsRemoteImageSourceAndContinuesMixedBatch", testRevisionDSLRepairsRemoteImageSourceAndContinuesMixedBatch},
		{"RevisionCompletionRepairsNoopAndReusesSavedImages", testRevisionCompletionRepairsNoopAndReusesSavedImages},
		{"RevisionNoopAssessmentAndBoundedRepair", testRevisionNoopAssessmentAndBoundedRepair},
		{"RevisionPartialFeedbackRepair", testRevisionPartialFeedbackRepair},
		{"RevisionAmendmentPreservesImagePinsAcrossReplicas", testRevisionAmendmentPreservesImagePinsAcrossReplicas},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, shared.isolatedCase(t)) })
	}
}

func testPreparedRevisionImagesRecoverAndPinAcrossReplicas(t *testing.T, f *revisionDB) {
	artifact, doc := f.document("# Document\n\nKeep this paragraph.\n")
	args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Add a generated illustration.")
	request := asString(args["requestId"])
	ids, err := revisionIdentity(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	ctx := common.ContextWithRun(f.ctx, common.RunContext{RunId: ids.RunID, GoalId: ids.GoalID, OwnerUserId: f.owner, StepKey: "images/0"})
	engine := &preparedImageEngine{IntegrationEngineAccess: f.engine}
	other := &preparedImageEngine{IntegrationEngineAccess: f.other}
	f.first.engine = engine
	f.second.engine = other
	store := &preparedImageStore{f: f, loseReply: true}
	f.first.SetImageAssets(store)
	f.second.SetImageAssets(store)
	bytes := &attachmentStore{data: attachmentPNG()}
	f.first.SetBlobFetcher(bytes)
	f.second.SetBlobFetcher(bytes)
	spec := revisionImageSpec{Mode: "generate", Name: "illustration.png", Alt: "Illustrated portrait", Prompt: "An editorial illustration"}
	raw, _ := json.Marshal(spec)
	var object map[string]any
	json.Unmarshal(raw, &object)
	call := map[string]any{"requestId": request, "specification": object, "level": "fast", "width": 512, "height": 512, "reportSourceFailure": true}
	if _, err = f.first.handlePrepareRevisionImage(ctx, call, 0); err == nil {
		t.Fatal("lost storage response hidden")
	}
	if _, err = f.second.handlePrepareRevisionImage(ctx, call, 0); err != nil {
		t.Fatal(err)
	}
	if engine.calls != 1 || other.calls != 0 || store.writes != 1 {
		t.Fatal("cross-replica recovery repeated inference or storage")
	}
	outsider := common.ContextWithRun(revisionActor("other-person", auth.RoleWriter), common.RunContext{RunId: ids.RunID, GoalId: ids.GoalID, OwnerUserId: f.owner})
	if _, err = f.second.handlePrepareRevisionImage(outsider, call, 0); err == nil {
		t.Fatal("foreign owner admitted")
	}
	wrong := common.ContextWithRun(f.ctx, common.RunContext{RunId: "another-run", GoalId: ids.GoalID, OwnerUserId: f.owner})
	if _, err = f.second.handlePrepareRevisionImage(wrong, call, 0); err == nil {
		t.Fatal("foreign run admitted")
	}
	images, err := f.second.preparedRevisionImages(ctx, request, ids.RunID)
	if err != nil || len(images) != 1 {
		t.Fatalf("manifest: %v %v", images, err)
	}
	if images[0].Provenance["model"] != "actual-image-model" || !strings.Contains(asString(images[0].Provenance["attribution"]), "AI-generated") {
		t.Fatal("actual generated attribution lost")
	}
	if !strings.Contains(images[0].Reference.URI, store.artifact) {
		t.Fatal("stored image URI missing")
	}
	f.ai.answer = revisionAnswer{Summary: "Insert the illustration", Edits: []revisionReplacement{{Before: "Keep this paragraph.", After: "Keep this paragraph.\n\n![Illustrated portrait](" + images[0].Reference.URI + ")\n\nAI-generated illustration; actual-image-model.", Reason: "Requested illustration", CommentIDs: []string{note}}}}
	var wait *workstate.HumanWait
	if err = f.execute(f.other, request, false); !errors.As(err, &wait) {
		t.Fatalf("did not reach approval: %v", err)
	}
	_, _, _, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	proposal := revisionMap(approval["subject"])
	if proposal["preparedImages"] == nil {
		t.Fatal("approval did not pin actual image receipt")
	}
	if err = f.first.ValidateRevisionProposal(f.ctx, proposal); err != nil {
		t.Fatal(err)
	}
	bytes.data = append(append([]byte{}, attachmentPNG()...), 0)
	if err = f.first.ValidateRevisionProposal(f.ctx, proposal); err == nil {
		t.Fatal("changed image bytes accepted for approval")
	}
}

func testRevisionDSLPlansAcquiresAndProposesAnImage(t *testing.T, f *revisionDB) {
	artifact, doc := f.document("# Document\n\nKeep this paragraph.\n")
	args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Add a generated illustration.")
	request := asString(args["requestId"])
	f.ai.imagePlan = []revisionImageSpec{{Mode: "generate", Name: "illustration.png", Alt: "Illustrated portrait", Prompt: "An editorial illustration"}}
	f.ai.assessmentReply = "sufficient"
	f.ai.needsResearch = true
	f.ai.answer = revisionAnswer{Summary: "Insert the illustration", Edits: []revisionReplacement{{Before: "Keep this paragraph.", After: "Keep this paragraph.", Reason: "Requested illustration", CommentIDs: []string{note}}}}
	generator := &preparedImageEngine{IntegrationEngineAccess: f.other}
	f.second.engine = generator
	store := &preparedImageStore{f: f, appendProposal: true}
	f.second.SetImageAssets(store)
	f.second.SetBlobFetcher(&attachmentStore{data: attachmentPNG()})
	var wait *workstate.HumanWait
	if err := f.execute(f.other, request, false); !errors.As(err, &wait) {
		t.Fatalf("DSL image branch failed: %v", err)
	}
	if generator.calls != 1 || store.writes != 1 {
		t.Fatalf("generation=%d storage=%d", generator.calls, store.writes)
	}
	_, _, _, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	proposal := revisionMap(approval["subject"])
	if proposal["preparedImages"] == nil || !strings.Contains(asString(proposal["revisedContent"]), store.artifact) {
		t.Fatal("saved image did not reach proposal")
	}
}

func testRevisionDSLRepairsRemoteImageSourceAndContinuesMixedBatch(t *testing.T, f *revisionDB) {
	for _, secondFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(secondFailure), func(t *testing.T) {
			f := f.isolatedCase(t)
			artifact, doc := f.document("# Document\n\nKeep this paragraph.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Add a researched portrait and a generated illustration.")
			request := asString(args["requestId"])
			original := revisionImageSpec{Mode: "import", Name: "portrait.png", Alt: "Historic portrait", URL: "https://museum.example/catalog", SourceURL: "https://museum.example/catalog", Attribution: "Artist", License: "Public domain"}
			replacement := original
			replacement.URL = "https://museum.example/media/portrait.png"
			f.ai.imagePlan = []revisionImageSpec{original, {Mode: "generate", Name: "illustration.png", Alt: "Editorial illustration", Prompt: "An editorial illustration"}}
			f.ai.replacementImagePlan = []revisionImageSpec{replacement}
			f.ai.assessmentReply = "sufficient"
			f.ai.needsResearch = true
			f.ai.answer = revisionAnswer{Summary: "Requested images", Edits: []revisionReplacement{{Before: "Keep this paragraph.", After: "Keep this paragraph.", Reason: "Requested images", CommentIDs: []string{note}}}}
			generator := &preparedImageEngine{IntegrationEngineAccess: f.other}
			f.second.engine = generator
			store := &preparedImageStore{f: f, appendProposal: true}
			f.second.SetImageAssets(store)
			f.second.SetBlobFetcher(&attachmentStore{data: attachmentPNG()})
			fetches := []string{}
			f.second.imageFetch = func(ctx context.Context, source string) ([]byte, error) {
				fetches = append(fetches, source)
				if source == original.URL || secondFailure {
					return nil, unavailableImageSource("remote server returned HTTP 403")
				}
				return attachmentPNG(), nil
			}
			err := f.execute(f.other, request, false)
			if len(fetches) != 2 || fetches[0] != original.URL || fetches[1] != replacement.URL || f.ai.researchCalls.Load() != 1 || f.ai.imagePlanCalls.Load() != 2 {
				t.Fatalf("unbounded or missing repair: fetches=%v research=%d plan=%d err=%v", fetches, f.ai.researchCalls.Load(), f.ai.imagePlanCalls.Load(), err)
			}
			if !strings.Contains(f.ai.headlessScope, "403") || f.ai.researchDocument != "" {
				t.Fatal("repair lost actual failure or loaded unrelated document")
			}
			if secondFailure {
				if err == nil || !strings.Contains(err.Error(), "image source unusable:") || generator.calls != 0 || store.writes != 0 {
					t.Fatalf("second refusal was hidden or repeated: %v", err)
				}
				return
			}
			var wait *workstate.HumanWait
			if !errors.As(err, &wait) || generator.calls != 1 || store.writes != 2 {
				t.Fatalf("mixed batch did not reach approval: %v gen=%d writes=%d", err, generator.calls, store.writes)
			}
			f.first.SetBlobFetcher(&attachmentStore{data: attachmentPNG()})
			f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
			// Resume from the other replica using its own local integration state. All
			// successful acquisitions and the repair evidence are durable receipts.
			if err = f.execute(f.engine, request, true); err != nil || len(fetches) != 2 || store.writes != 2 || f.ai.researchCalls.Load() != 1 {
				t.Fatalf("resume repeated acquisition/research: %v", err)
			}
		})
	}
}

func testRevisionCompletionRepairsNoopAndReusesSavedImages(t *testing.T, f *revisionDB) {
	artifact, doc := f.document("# Document\n\nKeep this paragraph.\n")
	args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Add an illustration.")
	request := asString(args["requestId"])
	f.ai.imagePlan = []revisionImageSpec{{Mode: "generate", Name: "illustration.png", Alt: "An illustration", Prompt: "An editorial illustration"}}
	f.ai.needsResearch, f.ai.assessmentReply = true, "sufficient"
	f.ai.answer = revisionAnswer{Summary: "A rewrite is required, beyond the scope of a patch.", Edits: []revisionReplacement{}}
	generator := &preparedImageEngine{IntegrationEngineAccess: f.engine}
	f.first.engine = generator
	store := &preparedImageStore{f: f}
	f.first.SetImageAssets(store)
	f.first.SetBlobFetcher(&attachmentStore{data: attachmentPNG()})
	f.second.SetBlobFetcher(&attachmentStore{data: attachmentPNG()})
	err := f.execute(f.engine, request, false)
	if err == nil || !strings.Contains(err.Error(), "document edits incomplete:") || f.ai.calls.Load() != 2 || f.ai.repairCalls.Load() != 1 || f.ai.completionCalls.Load() != 0 {
		t.Fatalf("unfinished image edit silently succeeded or repair was unbounded: %v calls=%d repairs=%d", err, f.ai.calls.Load(), f.ai.repairCalls.Load())
	}
	ids, _, _, approval, _ := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if approval != nil || generator.calls != 1 || store.writes != 1 {
		t.Fatal("invalid proposal reached approval or repeated images")
	}
	// Reproduce the legacy false-success receipt: same source and same direction.
	// The next attempt must recover its completed asset batch, on another replica.
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "updateWorkRun", map[string]any{"runId": ids.RunID, "status": "succeeded"})
	args["requestId"] = request + "-retry"
	next := asString(args["requestId"])
	if _, err = f.second.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	images, err := f.second.preparedRevisionImages(f.ctx, next, ids.RunID)
	if err != nil || len(images) != 1 {
		t.Fatalf("lost predecessor manifest: %v %v", images, err)
	}
	f.ai.answer = revisionAnswer{Summary: "Added illustration", Edits: []revisionReplacement{{Before: "Keep this paragraph.", After: "Keep this paragraph.\n\n![Illustration](" + images[0].Reference.URI + ")\n\nAI-generated illustration; actual-image-model.", Reason: "Requested illustration", CommentIDs: []string{note}}}}
	var wait *workstate.HumanWait
	if err = f.execute(f.other, next, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	if generator.calls != 1 || store.writes != 1 || f.ai.imagePlanCalls.Load() != 1 || f.ai.appCalls.Load() != 1 {
		t.Fatal("retry regenerated assets or repeated app research")
	}
	_, _, _, approval, err = f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), next)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.first.ValidateRevisionProposal(f.ctx, revisionMap(approval["subject"])); err != nil {
		t.Fatal(err)
	}
	f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(next)})
	if err = f.execute(f.engine, next, true); err != nil {
		t.Fatal(err)
	}
	if generator.calls != 1 || store.writes != 1 {
		t.Fatal("approval repeated image acquisition")
	}
	// A different request may not smuggle this sourceRunId through the private API.
	otherArtifact, otherDoc := f.document("# Other\n")
	different, _ := f.submit(otherArtifact, otherDoc, map[string]any{"kind": "document"}, "Add an image.")
	if _, err = f.second.preparedRevisionImages(f.ctx, asString(different["requestId"]), ids.RunID); err == nil {
		t.Fatal("unrelated predecessor admitted")
	}
}

func testRevisionNoopAssessmentAndBoundedRepair(t *testing.T, f *revisionDB) {
	for _, satisfied := range []bool{true, false} {
		t.Run(fmt.Sprint(satisfied), func(t *testing.T) {
			f := f.isolatedCase(t)
			artifact, doc := f.document("# Guide\n\nOriginal claim.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Correct the claim if needed.")
			f.ai.firstAnswer = &revisionAnswer{Summary: "No changes needed", Edits: []revisionReplacement{}}
			f.ai.completionReply = "incomplete: the paragraph still repeats the original claim without the requested clarification."
			f.ai.answer = revisionAnswer{Summary: "Corrected claim", Edits: []revisionReplacement{{Before: "Original claim.", After: "Corrected claim.", Reason: "Requested correction", CommentIDs: []string{note}}}}
			if satisfied {
				f.ai.completionReply = "satisfied"
			}
			err := f.execute(f.other, asString(args["requestId"]), false)
			var wait *workstate.HumanWait
			if satisfied {
				if err != nil || f.ai.calls.Load() != 1 || f.ai.repairCalls.Load() != 0 {
					t.Fatalf("genuine no-op changed content: %v", err)
				}
			} else if !errors.As(err, &wait) || f.ai.calls.Load() != 2 || f.ai.repairCalls.Load() != 1 {
				t.Fatalf("unfinished no-op not repaired: %v", err)
			}
			expectedChecks := int32(2)
			if satisfied {
				expectedChecks = 1
			}
			if f.ai.completionCalls.Load() != expectedChecks {
				t.Fatal("no-op assessment missing or repeated")
			}
		})
	}
}

func testRevisionPartialFeedbackRepair(t *testing.T, shared *revisionDB) {
	for _, unresolved := range []bool{false, true} {
		t.Run(fmt.Sprint(unresolved), func(t *testing.T) {
			f := shared.isolatedCase(t)
			artifact, doc := f.document("# Guide\n\nFirst claim. Second claim.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Correct both claims.")
			request := asString(args["requestId"])
			// Assessment runs after the first proposal codec. Journal it so the
			// real approval gate detects stale pre-assessment attribution.
			f.ai.onCompletion = func(ctx context.Context, n int32) error {
				rc, _ := common.RunFromContext(ctx)
				return work.NewModelJournal(f.other).Record(ctx, f.owner, memql.JournaledCall{RunId: rc.RunId, StepKey: rc.StepKey, RequestHash: fmt.Sprintf("assessment-%d", n), Provider: "fixture", Model: "assessment-model", PromptRef: "libraryRevisionCompletion", Served: "live"})
			}
			f.ai.firstAnswer = &revisionAnswer{Summary: "Both corrected", Edits: []revisionReplacement{{Before: "First claim. Second claim.", After: "First corrected. Second claim.", Reason: "Correction", CommentIDs: []string{note}}}}
			f.ai.answer = revisionAnswer{Summary: "Both corrected", Edits: []revisionReplacement{{Before: "First claim. Second claim.", After: "First corrected. Second corrected.", Reason: "Both requested corrections", CommentIDs: []string{note}}}}
			f.ai.completionReply = "incomplete: Second claim remains unchanged."
			f.ai.completionDocuments = []string{"# Guide\n\nFirst corrected. Second claim.\n", "# Guide\n\nFirst corrected. Second corrected.\n"}
			if unresolved {
				f.ai.answer = *f.ai.firstAnswer
				f.ai.completionAfterRepair = f.ai.completionReply
				f.ai.completionDocuments[1] = f.ai.completionDocuments[0]
			}
			err := f.execute(f.other, request, false)
			if f.ai.calls.Load() != 2 || f.ai.completionCalls.Load() != 2 || f.ai.repairCalls.Load() != 1 {
				t.Fatalf("partial feedback was not assessed and repaired once: %v", err)
			}
			if unresolved {
				_, _, _, approval, _ := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
				if err == nil || !strings.Contains(err.Error(), "document edits incomplete:") || approval != nil {
					t.Fatalf("partial repair reached approval: %v", err)
				}
				return
			}
			var wait *workstate.HumanWait
			if !errors.As(err, &wait) {
				t.Fatal(err)
			}
			f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
			if err = f.execute(f.engine, request, true); err != nil {
				t.Fatal(err)
			}
			changed, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			if err != nil || asString(changed.backing["body"]) != f.ai.completionDocuments[1] {
				t.Fatalf("did not save the assessed repair: %v %v", changed, err)
			}
		})
	}
}

func TestPreparedImageMustBeRenderedMarkdown(t *testing.T) {
	uri := "../artifact/portrait.png"
	images := []preparedRevisionImage{{Reference: reviewAttachment{URI: uri}}}
	for _, content := range []string{uri, "[Portrait](" + uri + ")", "`![Portrait](" + uri + ")`", "```md\n![Portrait](" + uri + ")\n```", "<!-- ![Portrait](" + uri + ") -->"} {
		if len(missingPreparedImages(content, images)) != 1 {
			t.Fatalf("non-image counted as embedded: %q", content)
		}
	}
	for _, content := range []string{"![Portrait](" + uri + ")", "![Portrait][photo]\n\n[photo]: " + uri} {
		if len(missingPreparedImages(content, images)) != 0 {
			t.Fatalf("rendered image not recognized: %q", content)
		}
	}
}

// An item revision must carry the original image receipt across a replica hop,
// even though its new request has different feedback and no image-generation step.
func testRevisionAmendmentPreservesImagePinsAcrossReplicas(t *testing.T, f *revisionDB) {
	artifact, doc := f.document("# Document\n\nKeep this paragraph.\n")
	args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Add a generated illustration.")
	request := asString(args["requestId"])
	f.ai.imagePlan = []revisionImageSpec{{Mode: "generate", Name: "illustration.png", Alt: "Illustrated portrait", Prompt: "An editorial illustration"}}
	f.ai.assessmentReply = "sufficient"
	f.ai.needsResearch = true
	f.ai.answer = revisionAnswer{Summary: "Insert the illustration", Edits: []revisionReplacement{{Before: "Keep this paragraph.", After: "Keep this paragraph.", Reason: "Requested illustration", CommentIDs: []string{note}}}}
	generator := &preparedImageEngine{IntegrationEngineAccess: f.other}
	f.second.engine = generator
	store := &preparedImageStore{f: f, appendProposal: true}
	f.second.SetImageAssets(store)
	bytes := &attachmentStore{data: attachmentPNG()}
	f.first.SetBlobFetcher(bytes)
	f.second.SetBlobFetcher(bytes)
	var wait *workstate.HumanWait
	if err := f.execute(f.other, request, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	_, _, _, approval, err := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	parent := revisionMap(approval["subject"])
	images, err := decodePreparedRevisionImages(parent["preparedImages"])
	if err != nil || len(images) != 1 {
		t.Fatalf("parent images: %v %v", images, err)
	}
	items, err := revisionItems(parent)
	if err != nil || len(items) != 1 {
		t.Fatalf("parent items: %v %v", items, err)
	}
	newer := "amended-" + request
	modify := map[string]any{"requestId": request, "approvalId": wait.ApprovalID, "itemId": items[0].ID, "instruction": "Clarify the paragraph, keeping the same image.", "newRequestId": newer}
	bytes.data = append(append([]byte{}, attachmentPNG()...), 0)
	if _, err = f.first.handleModifyRevisionItem(f.ctx, modify, 0); err == nil {
		t.Fatal("modified a proposal with changed image bytes")
	}
	bytes.data = attachmentPNG()
	for _, lib := range []*Integration{f.first, f.second} {
		if _, err = lib.handleModifyRevisionItem(f.ctx, modify, 0); err != nil {
			t.Fatal(err)
		}
	}
	previousEdits, err := revisionEdits(parent["edits"])
	if err != nil {
		t.Fatal(err)
	}
	f.ai.firstAnswer = &revisionAnswer{Summary: "Claimed clarification, but the proposal was copied", Edits: previousEdits}
	f.ai.calls.Store(0)
	f.ai.completionCalls.Store(0)
	f.ai.completionReply = "incomplete: the paragraph still repeats the original claim without the requested clarification."
	f.ai.imagePlan = nil
	f.ai.needsResearch = false
	f.ai.answer = revisionAnswer{Summary: "Clarify the paragraph", Edits: []revisionReplacement{{Before: "Keep this paragraph.", After: "Keep this clarified paragraph.\n\n![Illustration](" + images[0].Reference.URI + ")\nAI-generated illustration; actual-image-model.", Reason: "Requested clarification", CommentIDs: []string{note}}}}
	if err = f.execute(f.engine, newer, false); !errors.As(err, &wait) {
		t.Fatal(err)
	}
	_, _, _, approval, err = f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), newer)
	if err != nil {
		t.Fatal(err)
	}
	proposal := revisionMap(approval["subject"])
	pinned, err := decodePreparedRevisionImages(proposal["preparedImages"])
	if err != nil || len(pinned) != 1 || !containsPreparedImage(pinned, images[0]) {
		t.Fatalf("amendment lost image pin: %v %v", pinned, err)
	}
	if err = f.second.ValidateRevisionProposal(f.ctx, proposal); err != nil {
		t.Fatal(err)
	}
	// Supplying inheritance on a different request cannot authorize that image.
	other, _ := f.submit(artifact, doc, map[string]any{"kind": "document"}, "An unrelated change.")
	forged := map[string]any{"requestId": other["requestId"], "preparedImages": pinned, "inheritedPreparedImages": pinned, "amendment": modify}
	if err = f.second.validatePreparedRevisionImages(f.ctx, forged); err == nil {
		t.Fatal("caller-supplied inheritance authorized a foreign image")
	}
	amendedItems, err := revisionItems(proposal)
	if err != nil || len(amendedItems) != 1 {
		t.Fatalf("amended items: %v %v", amendedItems, err)
	}
	decision := map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": map[string]any{"acceptedItemIds": []string{amendedItems[0].ID}, "proposalHash": workstate.ArtifactHash(proposal)}}
	// The approval gate validates the bytes again, without the preparing replica.
	bytes.data = append(append([]byte{}, attachmentPNG()...), 0)
	call, _ := langparser.RenderCall("decideApproval", decision)
	if _, err = f.other.Execute(f.ctx, "query "+call); err == nil {
		t.Fatal("approved modified image bytes")
	}
	bytes.data = attachmentPNG()
	f.query(f.other, f.ctx, "query", "decideApproval", decision)
	if err = f.execute(f.other, newer, true); err != nil {
		t.Fatal(err)
	}
	changed, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil || !strings.Contains(asString(changed.backing["body"]), images[0].Reference.URI) || !strings.Contains(asString(changed.backing["body"]), "clarified paragraph") {
		t.Fatalf("saved amendment lost content/image: %v %v", changed, err)
	}
	if f.ai.completionCalls.Load() != 2 || f.ai.repairCalls.Load() != 1 || f.ai.calls.Load() != 2 {
		t.Fatal("unchanged amendment did not receive exactly one bounded correction")
	}
	if generator.calls != 1 || store.writes != 1 {
		t.Fatalf("amendment repeated image generation/storage: %d/%d", generator.calls, store.writes)
	}
}

func TestAmendedPreparedImagesRetainsOnlyEmbeddedAssets(t *testing.T) {
	image := preparedRevisionImage{Reference: reviewAttachment{URI: "../asset/image.png"}}
	captured := map[string]any{"inheritedPreparedImages": []preparedRevisionImage{image}}
	for _, body := range []string{"Removed image.", "[link](../asset/image.png)"} {
		images, err := amendedPreparedImages(captured, body, nil)
		if err != nil || len(images) != 0 {
			t.Fatalf("removed image retained: %v %v", images, err)
		}
	}
	images, err := amendedPreparedImages(captured, "![image](../asset/image.png)", []preparedRevisionImage{image})
	if err != nil || len(images) != 1 {
		t.Fatalf("image duplicated: %v %v", images, err)
	}
}
