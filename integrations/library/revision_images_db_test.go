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
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
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
func TestPreparedRevisionImagesRecoverAndPinAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
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
	call := map[string]any{"requestId": request, "specification": object, "level": "fast", "width": 512, "height": 512}
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

func TestRevisionDSLPlansAcquiresAndProposesAnImage(t *testing.T) {
	f := newRevisionDB(t)
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
