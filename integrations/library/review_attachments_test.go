package library

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func attachmentPNG() []byte {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==")
	return b
}
func TestReviewAttachmentAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
		want    string
	}{
		{"figure.PNG", attachmentPNG(), "image/png"}, {"Reference.MD", []byte("# Evidence\n"), "text/markdown"},
		{"figure.jpg", attachmentPNG(), ""}, {"script.svg", []byte("<svg/>"), ""}, {"reference.md", []byte{255}, ""}, {"reference.md", []byte{'a', 0}, ""}, {"reference.md", bytes.Repeat([]byte{'x'}, reviewMarkdownBytes+1), ""}, {"figure.png", []byte("incomplete"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reviewAttachmentMIME(tc.name, tc.content)
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid attachment admitted")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}

type attachmentStore struct{ data []byte }

func (s *attachmentStore) DownloadStreamURL(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

type attachmentVisionEngine struct {
	memql.IntegrationEngineAccess
	t     *testing.T
	calls int
}

func (e *attachmentVisionEngine) CallAIVision(_ context.Context, req airoute.ResolveRequest, prompt string, images []common.VisionContent) (memql.VisionAIResult, error) {
	e.calls++
	if len(images) != 1 || !bytes.Equal(images[0].Data, attachmentPNG()) || images[0].MimeType != "image/png" {
		e.t.Fatal("vision did not receive original image bytes")
	}
	if req.Level != airoute.LevelFast || req.PromptName != "libraryRevisionImages" || !strings.Contains(prompt, "Image 1: Figure.png") || !strings.Contains(prompt, "untrusted") {
		e.t.Fatalf("DSL vision contract lost: %s %v", prompt, req)
	}
	return memql.VisionAIResult{Text: "A one-pixel red test image; no scientific conclusion is supported."}, nil
}

func testReviewAttachmentsAcrossReplicas(t *testing.T, f *revisionDB) {
	referenceID, reference := f.document("# Reference\n\nReference content sentinel.")
	fileID := fmt.Sprintf("attachment-%d", time.Now().UnixNano())
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createLibraryFile", map[string]any{"fileId": fileID, "name": "Figure.png", "mimeType": "image/png", "size": len(attachmentPNG()), "blobUrl": "https://storage.invalid/verified.png", "source": "uploaded", "format": "image"})
	artifact := f.query(f.engine, f.ctx, "mutation", "createArtifact", map[string]any{"sourceConceptRef": fileID, "ownerUserId": f.owner, "lens": "artifact", "kind": "file", "source": "uploaded", "title": "Figure.png", "format": "image"})[0]
	imageID := asString(artifact["id"])
	store := &attachmentStore{data: attachmentPNG()}
	f.first.blobFetcher = store
	f.second.blobFetcher = store
	vision := &attachmentVisionEngine{IntegrationEngineAccess: f.other, t: t}
	f.second.engine = vision
	refs := []reviewAttachment{{ArtifactID: referenceID, Version: reference.version, Revision: reference.revision}, {ArtifactID: imageID, Version: 1, Revision: "file:1"}}
	if _, _, err := f.second.reviewAttachments(revisionActor("outsider", auth.RoleWriter), refs); err == nil {
		t.Fatal("attachment granted an outsider access")
	}
	target, doc := f.document("# Target\n\nKeep this.\n")
	args := map[string]any{"artifactId": target, "expectedVersion": doc.version, "expectedRevision": doc.revision, "anchor": map[string]any{"kind": "document"}, "body": "Add the image with a caption based on the reference.", "requestId": "comment-" + fileID, "attachments": refs}
	rows, err := f.first.handleAddDocumentComment(f.ctx, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	json.Unmarshal(rows[0].Payload, &receipt)
	comment := asString(receipt["commentId"])
	if _, err = f.second.handleAddDocumentComment(f.ctx, args, 0); err != nil {
		t.Fatalf("idempotent retry across replicas: %v", err)
	}
	args["attachments"] = refs[:1]
	if _, err = f.second.handleAddDocumentComment(f.ctx, args, 0); err == nil {
		t.Fatal("changed attachments reused a receipt")
	}
	args["attachments"] = refs
	request := "request-" + fileID
	requestArgs := map[string]any{"artifactId": target, "expectedVersion": doc.version, "expectedRevision": doc.revision, "commentIds": []string{comment}, "instruction": "", "requestId": request}
	if _, err = f.first.handleRequestDocumentRevision(f.ctx, requestArgs, 0); err != nil {
		t.Fatal(err)
	}
	f.ai.expectedReference = "Reference content sentinel."
	f.ai.answer = revisionAnswer{Summary: "Add the referenced figure", Edits: []revisionReplacement{{Before: "Keep this.", After: "Keep this.\n\n![Test figure](../" + imageID + "/Figure.png)", Reason: "Use the attached figure", CommentIDs: []string{comment}}}}
	var wait *workstate.HumanWait
	if err = f.execute(f.other, request, false); !errors.As(err, &wait) {
		t.Fatalf("attachment workflow did not reach review: %v", err)
	}
	if f.ai.researchCalls.Load() != 0 || f.ai.appCalls.Load() != 0 {
		t.Fatal("image placement entered unnecessary external research")
	}
	if vision.calls != 1 {
		t.Fatalf("vision calls: %d", vision.calls)
	}
	_, captured, _, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asString(revisionMap(approval["subject"])["revisedContent"]), "![Test figure]") {
		t.Fatal("image was not proposed")
	}
	// A changed backing blob with the same advertised revision cannot silently
	// replace the bytes the model observed, including on a different replica.
	store.data = append(append([]byte{}, attachmentPNG()...), 0)
	if err = f.first.ValidateRevisionProposal(f.ctx, captured); err == nil {
		t.Fatal("changed attachment bytes passed approval")
	}
	store.data = attachmentPNG()
	f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": memql.BareShortId(asString(approval["id"])), "decision": "approved", "answer": f.accepted(request)})
	if err = f.execute(f.engine, request, true); err != nil {
		t.Fatal(err)
	}
	changed, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), target)
	if err != nil || !strings.Contains(asString(changed.backing["body"]), "![Test figure]") {
		t.Fatalf("approved image not applied: %v", err)
	}
	if vision.calls != 1 {
		t.Fatal("resume repeated image analysis")
	}
	stale := append([]reviewAttachment{}, refs...)
	stale[0].Version++
	if _, _, err = f.second.reviewAttachments(f.ctx, stale); err == nil {
		t.Fatal("stale attachment revision admitted")
	}
}
