//go:build agent

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/id"
)

// integrations_skills_capture.go -- the Library half of capture.
//
// ===========================================================================
// IT LIVES IN app/ BECAUSE THIS IS WHERE THE TWO HALVES MEET
// ===========================================================================
// Filing bytes in the Library is two things: a blob upload through the
// storage client the transport built, and a `createLibraryFile` row through
// the engine. `component/server`'s EngineLibraryStore owns the second and the
// FileUploader owns the first; `integrations/skills` owns neither and should
// not import `component/server`, which is a bff/agent transport package.
//
// So the seam is an interface on the skills side and this adapter is its one
// implementation -- the same shape `newLibraryAnalyzer` uses for the analysis
// pass, and for the same reason.
//
// WITHOUT IT, CAPTURE REFUSES BY NAME (`capture_not_wired`) rather than
// quietly doing nothing: a capture that silently succeeded and filed nothing
// would leave a skill pointing at a path on somebody's machine forever, which
// is the exact failure capture exists to prevent.

// libraryArtifactWriter files a captured script in the caller's Library.
type libraryArtifactWriter struct {
	store    *server.EngineLibraryStore
	uploader server.FileUploader
	bucket   string
}

// WriteArtifact stores the bytes and writes the file row, then answers with
// the FILE id.
//
// THE FILE ID, NOT THE ARTIFACT ID, and that is deliberate. The artifact index
// row is promoted asynchronously by the `indexFileOnCreate` automation, so
// waiting for it here would make a capture's latency depend on an automation
// round trip -- and `runScript`'s reader resolves either id (a file id is
// recognised by its concept prefix), so the extra wait would buy nothing. A
// skill's `scripts[]` entry naming a file id is as content-addressable as one
// naming an artifact id.
func (w libraryArtifactWriter) WriteArtifact(ctx context.Context, name, mimeType string, data []byte) (string, error) {
	if w.store == nil {
		return "", fmt.Errorf("the Library store is not wired on this node")
	}
	if w.uploader == nil || strings.TrimSpace(w.bucket) == "" {
		return "", fmt.Errorf("object storage is not configured on this node, so the bytes cannot be stored")
	}
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil || strings.TrimSpace(access.UserId) == "" {
		// The row's owner is the row's only reader. A capture written under a
		// blank actor is one nobody can run and nobody can find.
		return "", fmt.Errorf("a captured script needs a caller to own it")
	}
	userId := strings.TrimSpace(access.UserId)

	fileId := id.NewShortId()
	// The same object path the upload route uses, so one bucket has one
	// layout whichever writer put the bytes there.
	object := fmt.Sprintf("library/%s/%s/%s", userId, fileId, name)
	blobUrl, err := w.uploader.Upload(ctx, w.bucket, object, data, mimeType)
	if err != nil {
		return "", fmt.Errorf("storing the bytes: %w", err)
	}
	sum := sha256.Sum256(data)
	if err := w.store.CreateFile(ctx, server.LibraryFileCreateParams{
		FileId:   fileId,
		Name:     name,
		MimeType: mimeType,
		Size:     len(data),
		Sha256:   hex.EncodeToString(sum[:]),
		BlobUrl:  blobUrl,
		// DERIVED, not `uploaded`: nobody uploaded this. A run found it on a
		// surface and the platform copied it, which is what that enum member
		// is for -- and a Library reader filtering for what they themselves
		// put there should not be shown it.
		Source: "derived",
		Format: "text",
		Summary: fmt.Sprintf(
			"A script captured from a run and filed under the skill that used it."),
	}); err != nil {
		return "", fmt.Errorf("recording the file: %w", err)
	}
	return fileId, nil
}
