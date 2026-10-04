//go:build agent

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// appsession_content_store.go -- the Library half of an app session's
// recording (epic memql#5396, task memql#5400).
//
// ===========================================================================
// IT LIVES IN app/ FOR THE REASON integrations_skills_capture.go DOES
// ===========================================================================
// Filing bytes in the Library is two things: a blob upload through the
// storage client the transport built, and a `createLibraryFile` row through
// the engine. component/worker declares the ContentStore seam and cannot
// reach either -- it is its own Go module and `component/server` is a
// bff/agent transport package integrations must not import. So the seam is an
// interface on the worker side and this is its one implementation.
//
// ===========================================================================
// CONTENT-ADDRESSED, AND THAT IS THE WHOLE FEATURE
// ===========================================================================
// An agent reads the same file in three steps and writes it back twice. Under
// a naive writer that is five copies of one document in somebody's Library,
// and a later branch from a recorded step would copy them again. Here the
// digest is looked up first, so two identical contents are ONE file
// referenced twice -- which is what makes the workspace snapshot D19 branches
// from cost nothing.
//
// `v1:library:file.sha256` has carried the words "a DEDUP HINT and an
// integrity check" since the concept existed and nothing read it that way.
// `libraryFileBySha256` is that read.
//
// ===========================================================================
// NO INTERNAL-ORIGIN STAMP, DELIBERATELY
// ===========================================================================
// `createLibraryFile` and `libraryFileBySha256` are both `@actor`, not
// `@serverOnly`: a file can only ever be created for the person the call runs
// as, and the row is theirs to read. So this borrows the OWNER's actor and
// stamps nothing. Reaching for internal origin here would be the laundering
// the allowlist exists to prevent, and it would buy nothing -- the owner's
// authority is exactly the authority these two constructs want.
//
// ===========================================================================
// A FAILURE IS NEVER FATAL TO THE SESSION
// ===========================================================================
// Every refusal comes back as a ContentResult carrying WHY, and the caller
// records `contentOmitted` on the observation. The session already happened
// on somebody's machine; losing a stored copy of one file costs a reader a
// journey, and failing the run would cost them the work.

// appSessionContentStore stores an app session's file contents and prose.
type appSessionContentStore struct {
	engine   contentEngine
	store    *server.EngineLibraryStore
	uploader server.FileUploader
	bucket   string
	logger   *slog.Logger
	// maxBytes bounds one content. Zero takes the worker module's value.
	maxBytes int
}

// contentEngine is the read half: the dedup lookup and nothing else.
type contentEngine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

var _ workerservice.ContentStore = (*appSessionContentStore)(nil)

// StoreContent files one content, reusing an identical one the owner already
// has.
func (s *appSessionContentStore) StoreContent(ctx context.Context, req workerservice.ContentRequest) (workerservice.ContentResult, error) {
	owner := strings.TrimSpace(req.OwnerUserId)
	if owner == "" {
		// Refused rather than written under a blank actor: the row's owner is
		// the row's only reader, so a content filed this way is one nobody
		// can find -- including the person whose machine it came from.
		return workerservice.ContentResult{}, fmt.Errorf("a recorded content needs an owner to file it under")
	}
	if len(req.Bytes) == 0 {
		return workerservice.ContentResult{Omitted: "the content was empty"}, nil
	}

	// THE DIGEST IS COMPUTED WHETHER OR NOT THE BYTES ARE STORED. A content
	// above the cap is referenced by digest only (design D5), so this is the
	// part that must never be missing -- two runs can still be compared on
	// whether they read the same file.
	sum := sha256.Sum256(req.Bytes)
	digest := hex.EncodeToString(sum[:])

	if max := s.cap(); len(req.Bytes) > max {
		return workerservice.ContentResult{
			Sha256:  digest,
			Omitted: fmt.Sprintf("%d bytes is above the %d-byte per-content cap", len(req.Bytes), max),
		}, nil
	}
	if s.store == nil || s.uploader == nil || strings.TrimSpace(s.bucket) == "" {
		return workerservice.ContentResult{
			Sha256:  digest,
			Omitted: "this node has no object storage configured, so the bytes were not stored",
		}, nil
	}

	// THE OWNER'S OWN ACTOR, for both the read and the write. Stamped inline
	// at each call so the marked context dies there.
	if existing := s.existingFile(auth.ContextWithUserActor(ctx, owner), digest); existing != "" {
		return workerservice.ContentResult{FileId: existing, Sha256: digest, Deduplicated: true}, nil
	}

	name := sanitizeContentName(req.Name)
	fileId := id.NewShortId()
	// The same object path the upload route uses, so one bucket has one
	// layout whichever writer put the bytes there.
	object := fmt.Sprintf("library/%s/%s/%s", owner, fileId, name)
	blobUrl, err := s.uploader.Upload(ctx, s.bucket, object, req.Bytes, req.MimeType)
	if err != nil {
		return workerservice.ContentResult{Sha256: digest, Omitted: "storing the bytes failed: " + err.Error()}, nil
	}

	// Rendered here rather than through server.LibraryFileCreateParams
	// because that struct MAY NOT carry `producedBy` --
	// TestTheUploadRouteCannotStampProvenance reflects over it and fails the
	// build on a field by that name, since "absence is the answer" for a
	// human upload. This writer is the opposite case: an app produced these
	// bytes and the stamp is the fact worth having, so the call is composed
	// directly with the field the mutation already accepts.
	args := map[string]any{
		"fileId":   fileId,
		"name":     name,
		"mimeType": firstNonBlankString(req.MimeType, "application/octet-stream"),
		"size":     len(req.Bytes),
		"sha256":   digest,
		"blobUrl":  blobUrl,
		// NOT `agent_generated`. That value means an agent produced a
		// standalone deliverable; this is the raw material of a recording --
		// a file an app read or wrote while working, or the session's own
		// transcript -- and a person filtering their Library for what an
		// agent MADE them should not be shown it.
		"source": "app_session",
		"format": server.LibraryFormatForMIME(req.MimeType),
	}
	if !req.Provenance.Empty() {
		args["producedBy"] = req.Provenance.AsMap()
	}
	query, err := langparser.RenderCall("createLibraryFile", args)
	if err != nil {
		return workerservice.ContentResult{Sha256: digest, Omitted: "the file row could not be composed: " + err.Error()}, nil
	}
	if _, err := s.engine.Execute(auth.ContextWithUserActor(ctx, owner), "mutation "+query); err != nil {
		return workerservice.ContentResult{Sha256: digest, Omitted: "recording the file failed: " + err.Error()}, nil
	}
	return workerservice.ContentResult{FileId: fileId, Sha256: digest}, nil
}

// existingFile answers the owner's live file at this digest, or "".
//
// A FAILED LOOKUP STORES A SECOND COPY rather than failing, and that is the
// right way round: dedup is an optimisation and a duplicate file is a wasted
// blob, while a refusal here would lose the recording of an action that
// really happened.
func (s *appSessionContentStore) existingFile(ctx context.Context, digest string) string {
	if s.engine == nil {
		return ""
	}
	query, err := langparser.RenderCall("libraryFileBySha256", map[string]any{"sha256": digest})
	if err != nil {
		return ""
	}
	res, err := s.engine.Execute(ctx, "query "+query)
	if err != nil || res == nil {
		if err != nil && s.logger != nil {
			s.logger.Debug("app session content: the dedup read failed; storing a second copy",
				"error", err)
		}
		return ""
	}
	rows, _ := res.OutputPayload().([]any)
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fileId, _ := row["id"].(string); strings.TrimSpace(fileId) != "" {
			return fileId
		}
	}
	return ""
}

func (s *appSessionContentStore) cap() int {
	if s != nil && s.maxBytes > 0 {
		return s.maxBytes
	}
	return workerservice.MaxRecordedContentBytes
}

// sanitizeContentName bounds what reaches the Library's name field and the
// object path. Path separators and control characters are removed for the
// upload route's reason: the name is the last segment of the blob path, and a
// content whose "name" carried a slash would write outside its own prefix.
func sanitizeContentName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if cleaned == "" {
		cleaned = "content"
	}
	if len([]rune(cleaned)) > 200 {
		cleaned = string([]rune(cleaned)[:200])
	}
	return cleaned
}

func firstNonBlankString(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
