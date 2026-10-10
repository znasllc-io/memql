package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

type libraryImageAssets struct {
	engine   server.MemQLExecutor
	store    *server.EngineLibraryStore
	uploader server.FileUploader
	bucket   string
}

// Called under the image operation's shared gate. CreateVerifiedStream never
// overwrites an object, verifies readback and reconciles an uncertain upload.
func (s *libraryImageAssets) SaveImage(ctx context.Context, w library.ImageAssetWrite) error {
	ac, ok := auth.AccessFromContext(ctx)
	subject, hasSubject := auth.SubjectFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || !hasSubject || !auth.CapableFor(ctx, subject, auth.VerbCreate, auth.ResourceData) {
		return fmt.Errorf("you do not have permission to save this image")
	}
	uploader, ok := s.uploader.(pipelineVerifiedUploader)
	if !ok || s.bucket == "" {
		return fmt.Errorf("verified image storage is unavailable")
	}
	if len(w.Data) == 0 || len(w.Data) > 8<<20 || w.FileID == "" || w.RunID == "" {
		return fmt.Errorf("image bytes or run identity are invalid")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	digestBytes := sha256.Sum256(w.Data)
	digest := hex.EncodeToString(digestBytes[:])
	current, err := s.store.File(ctx, server.LibraryFileConceptRef(w.FileID))
	if err != nil {
		return err
	}
	if current != nil {
		if current.Archived || current.Sha256 != digest || current.Name != w.Name {
			return fmt.Errorf("stored image differs from this acquisition")
		}
		return nil
	}
	files, versions, sessions, err := s.store.StorageFootprint(ctx)
	if err != nil {
		return err
	}
	if files+versions+sessions+int64(len(w.Data)) > server.LibraryUserQuotaBytes() {
		return fmt.Errorf("the image would exceed your Library storage quota")
	}
	object := fmt.Sprintf("library/%s/%s/%s/%s", memql.BareShortId(ac.UserId), w.FileID, digest, w.Name)
	receipt, err := uploader.CreateVerifiedStream(ctx, s.bucket, object, bytes.NewReader(w.Data), int64(len(w.Data)), digest, w.MIME)
	if err != nil {
		return fmt.Errorf("could not store image: %w", err)
	}
	if receipt.URL == "" || receipt.ETag == "" || receipt.SHA256 != digest || receipt.Size != int64(len(w.Data)) {
		return fmt.Errorf("image storage returned an incomplete receipt")
	}
	source := "agent_generated"
	if w.Provenance["mode"] == "import" {
		source = "derived"
	}
	call, err := langparser.RenderCall("recordPreparedLibraryImage", map[string]any{"source": source, "fileId": w.FileID, "name": w.Name, "mimeType": w.MIME, "size": len(w.Data), "sha256": digest, "blobUrl": receipt.URL, "imageProvenance": w.Provenance, "producedByRunId": w.RunID, "producedByStepKey": w.StepKey})
	if err != nil {
		return err
	}
	_, writeErr := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	// Reconcile the row even after an uncertain response; never overwrite it.
	current, err = s.store.File(ctx, server.LibraryFileConceptRef(w.FileID))
	if err != nil {
		return err
	}
	if current == nil {
		if writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("image storage did not confirm the file")
	}
	if current.Archived || current.Sha256 != digest || current.BlobUrl != receipt.URL {
		return fmt.Errorf("stored image receipt differs from uploaded bytes")
	}
	// Compare structured metadata, not JSON key order or model-claimed ids.
	actual, _ := json.Marshal(current.ImageProvenance)
	expected, _ := json.Marshal(w.Provenance)
	if string(actual) != string(expected) {
		return fmt.Errorf("stored image provenance differs from its acquisition")
	}
	return nil
}
