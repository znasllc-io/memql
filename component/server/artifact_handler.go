package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// artifact_handler.go -- the Library's two byte-bearing HTTP routes
// (memql#4341), the pair design D1 approves:
//
//	POST /artifacts                 -- upload any file, owned by the caller
//	GET  /artifacts/{id}/content    -- export any artifact: a file's bytes, or
//	                                   a note / generated output / memory's body
//
// WHY HTTP AT ALL. Both are inside the exception CLAUDE.md already records for
// multipart ("file uploads map poorly to gRPC"), and the owner approved them
// explicitly in this epic's brainstorm -- the same way memql#3713's bundle
// route was approved. The alternatives were measured and rejected in the
// design record (docs/superpowers/specs/2026-08-22-library-artifacts-and-deployables-design.md,
// D1): riding the attachment routes would rest the Library on a pack-owned
// space concept the engine does not declare and inherit its 25 MB cap and
// 18-entry MIME allowlist, and chunked upload on the gRPC stream would make
// every CI tool that wants to hand MemQL a file link a MemQL client.
//
// WHAT MAKES THIS NOT A SECOND ATTACHMENT HANDLER. AttachmentHandler is
// SPACE-scoped: it gates on owning the space named in the path and writes a
// row keyed to it. This one is USER-scoped -- there is no path-borne container
// to check, the actor owns what they upload, and every read resolves under the
// caller's own actor so per-row authorization is the only gate. The multipart
// shape, the streamed-then-stored order, and the 404-on-deny download posture
// are deliberately borrowed from it; the ownership model is not.
//
// NO SIGNED URLS, EVER. blobUrl is a storage PATH, not something a browser can
// fetch, and the download route never redirects to storage -- it re-resolves
// the row under the caller's actor and streams the bytes through the bff. The
// design says so in as many words (D9, and the sha256 field's own description
// on the concept: "a DEDUP HINT and an integrity check -- NEVER an access
// key"). A redirect would move authorization from the graph to whoever holds a
// URL.

const (
	// libraryFormFileKey is the multipart field carrying the bytes.
	libraryFormFileKey = "file"
	// libraryFormNameKey optionally overrides the part's own filename.
	libraryFormNameKey = "name"
	// libraryFormLabelsKey optionally carries a comma-separated label list,
	// applied to the promoted artifact index row through the SAME builtin the
	// portal's label editor and the agent tool use.
	libraryFormLabelsKey = "labels"

	// DefaultLibraryMaxUploadBytes is the cap when MEMQL_LIBRARY_MAX_UPLOAD_BYTES
	// is unset. 256 MB, sized so a site bundle fits (design 3.4) -- ten times
	// the attachment cap, because a Library file is not a chat attachment and
	// the deployables path publishes from one.
	DefaultLibraryMaxUploadBytes int64 = 256 * 1024 * 1024

	// libraryMultipartMemory is the in-memory buffering threshold handed to
	// ParseMultipartForm; larger parts spill to temp files, which is why
	// handleUpload defers MultipartForm.RemoveAll. Same value the attachment
	// handler uses (32 MB) -- deliberately NOT scaled with the cap above, or a
	// 256 MB upload would be a 256 MB resident buffer.
	libraryMultipartMemory = 32 * 1024 * 1024

	// libraryUploadFramingAllowance is the slack the WHOLE-BODY guard gets on
	// top of the file cap.
	//
	// The cap names a FILE ("a single POST /artifacts upload"), and a multipart
	// request carries framing on top of it: a boundary per part, the
	// Content-Disposition and Content-Type headers, and the optional name and
	// labels fields. Without the allowance a file of exactly
	// MEMQL_LIBRARY_MAX_UPLOAD_BYTES bytes could never be uploaded, which makes
	// the documented number quietly untrue by a few hundred bytes -- measured:
	// a 1024-byte file in a 1024-byte body is a 413.
	//
	// So the enforcement is two-layer, and each layer means a different thing:
	// MaxBytesReader bounds the REQUEST (cap + allowance) so an unbounded body
	// cannot be streamed at this process at all, and the header.Size check plus
	// the LimitReader bound the FILE at exactly the cap. Both answer 413.
	// 1 MB is generous for framing and negligible against the 256 MB default.
	libraryUploadFramingAllowance = 1 << 20

	// libraryFileConcept is the concept whose canonical node id the promotion
	// automation writes into v1:library:artifact.sourceConceptRef.
	libraryFileConcept = "v1:library:file"

	// libraryStatusReady / libraryStatusFailed are the two terminal lifecycle
	// values this handler itself writes. `analyzing` belongs to the analysis
	// pass (memql#4342), which owns everything after the hand-off.
	libraryStatusReady  = "ready"
	libraryStatusFailed = "failed"

	// libraryFormatOther is the format an unrecognised MIME resolves to. It is
	// also the discriminator for "store it opaquely": a file whose format is
	// `other` goes straight to ready with no text extracted and no chunks.
	libraryFormatOther = "other"

	// libraryMaxFileNameRunes bounds the stored name. It is the Content-
	// Disposition filename and the last segment of the blob path, so an
	// unbounded one is both a storage-key problem and a header problem.
	libraryMaxFileNameRunes = 200
)

// LibraryMaxUploadBytesEnv is the operator knob for the upload cap.
//
// Named as a constant AND read through it (rather than composed from a prefix
// and a struct field the way the MEMQL_SERVER_* family is) so a plain
// `grep -rn MEMQL_LIBRARY_MAX_UPLOAD_BYTES` finds the read. That family's own
// comment in env.go records what the composed spelling cost: the variable was
// filed as dead because no grep could find it, and envscan could not attribute
// it either.
const LibraryMaxUploadBytesEnv = "MEMQL_LIBRARY_MAX_UPLOAD_BYTES"

// LibraryMaxUploadBytes resolves the upload cap from the environment, falling
// back to DefaultLibraryMaxUploadBytes.
//
// A value that is set and unparseable, or non-positive, falls back to the
// default rather than to "no limit": an unbounded upload route is the one
// outcome a misconfigured cap must never produce.
func LibraryMaxUploadBytes() int64 {
	raw, ok := os.LookupEnv(LibraryMaxUploadBytesEnv)
	if !ok {
		return DefaultLibraryMaxUploadBytes
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return DefaultLibraryMaxUploadBytes
	}
	return n
}

// LibraryFileCreateParams is what the upload route hands createLibraryFile.
//
// There is deliberately NO Status and NO OwnerUserId field. Both are stamped
// server-side by the mutation (`status: "stored"`, `ownerUserId:
// actor.userId`) and the concept marks ownerUserId @serverSet, so a Go caller
// naming either is a load-time rejection rather than a convention. Status in
// particular is load-bearing beyond this call: indexFileOnCreate filters on
// `payload.status == "stored"` so it promotes exactly once, because
// graph.node.created fires on EVERY write.
type LibraryFileCreateParams struct {
	FileId   string
	Name     string
	MimeType string
	Size     int
	Sha256   string
	BlobUrl  string
	Source   string // uploaded | exported | agent_generated | derived
	Format   string // markdown | document | pdf | spreadsheet | image | text | conversation | other
	Summary  string
}

// LibraryFileStatusParams advances a file through the analysis lifecycle.
// Every field but Status is optional and an absent one is NOT written, so a
// later transition cannot blank a summary an earlier one recorded.
type LibraryFileStatusParams struct {
	FileId          string
	Status          string // stored | analyzing | ready | failed
	Summary         string
	EmbeddingStatus string // none | partial | complete
	FailureReason   string
}

// LibraryArtifactRow is the artifact index projection the export route needs:
// which backing row to read, and the metadata to name the download.
type LibraryArtifactRow struct {
	ID               string
	Kind             string
	SourceConceptRef string
	Title            string
	Format           string
	MimeType         string
	Summary          string
}

// LibraryFileRow is the v1:library:file projection the download path needs.
type LibraryFileRow struct {
	ID       string
	Name     string
	MimeType string
	Size     int
	BlobUrl  string
	Status   string
}

// LibraryExportBody is a text-bearing backing row rendered for download.
// Markdown picks between text/markdown and text/plain, and therefore between
// the .md and .txt extension on the derived filename.
type LibraryExportBody struct {
	Title    string
	Body     string
	Markdown bool
}

// LibraryStore is the graph seam for both routes, declared entirely in this
// package's own types.
//
// An interface rather than the engine itself for the reason every other
// handler in this package takes one (FileUploader, AttachmentStore,
// BundlePublisher): component/server is a tiered module with its own go.mod
// (memql#3228), and a test must be able to drive the handler without standing
// up an engine. EngineLibraryStore below is the production implementation, and
// every one of its calls runs under the CALLER's actor -- none of these five
// DSL constructs is @serverOnly, and all of them are owner-gated, so per-row
// authorization is what decides the answer rather than anything in Go.
type LibraryStore interface {
	// CreateFile runs createLibraryFile. The mutation stamps ownerUserId from
	// actor.userId and status from nothing at all, so the row is the caller's
	// by construction.
	CreateFile(ctx context.Context, p LibraryFileCreateParams) error
	// SetFileStatus runs setLibraryFileStatus (a read-merge; absent fields are
	// not written).
	SetFileStatus(ctx context.Context, p LibraryFileStatusParams) error
	// ArtifactForFile resolves the promoted index row for a file through
	// libraryArtifactBySourceConceptRef and returns its id, or "" when the
	// promotion has not landed yet.
	ArtifactForFile(ctx context.Context, fileId string) (string, error)
	// AddArtifactLabel runs the libraryAddArtifactLabel builtin. Idempotent.
	AddArtifactLabel(ctx context.Context, artifactId, label string) error
	// Artifact reads one index row by id under the caller's actor. Returns nil
	// (no error) when the caller may not see it or it does not exist -- the
	// download route cannot tell those apart, and must not.
	Artifact(ctx context.Context, artifactId string) (*LibraryArtifactRow, error)
	// File reads one v1:library:file row under the caller's actor, by the
	// canonical ref the artifact carries. Returns nil when absent or denied.
	File(ctx context.Context, fileRef string) (*LibraryFileRow, error)
	// ExportBody reads a text-bearing backing row (note / generated output /
	// memory) under the caller's actor. Returns nil when the kind has no
	// exportable body, or the row is absent or denied.
	ExportBody(ctx context.Context, kind, sourceConceptRef string) (*LibraryExportBody, error)
}

// LibraryAnalysisRequest is the whole of what the analysis pass needs to run.
//
// Data is the bytes as uploaded, already read into memory and bounded by the
// cap, so the pass does not have to fetch them back out of blob storage for
// the common case. BlobUrl is carried too, because a pass that runs later (a
// retry, a re-index) has to.
type LibraryAnalysisRequest struct {
	FileId      string // BARE short id -- the value createLibraryFile was given
	ArtifactId  string // the promoted index row, or "" if promotion had not landed
	OwnerUserId string
	Name        string
	MimeType    string
	Format      string
	Size        int
	Sha256      string
	BlobUrl     string
	Data        []byte
}

// LibraryAnalyzer is the seam memql#4342's analysis pass registers into.
//
// THE CONTRACT, because the halves are owned by different code:
//
//   - This handler owns everything up to and including a durable row in
//     `stored`. It calls AnalyzeFile exactly once, from a DETACHED goroutine,
//     with a context already carrying the file owner's actor
//     (auth.ContextWithUserActor) -- so the pass's own writes land owned by
//     the same person as the file, and HTTP request cancellation does not kill
//     the work.
//   - The analyzer owns every status transition from that point: `analyzing`
//     while it runs, then `ready` or `failed` with a reason. Never a silent
//     partial. It also owns chunking and embedding.
//   - The handler NEVER calls it for a file whose format resolved to `other`
//     (an unrecognised MIME). Those go straight to `ready` with no chunks,
//     which is what the concept's status field documents. Nor does it call it
//     when no analyzer is wired at all, for the same reason: a row must not be
//     left in `stored` forever waiting for something that does not exist.
//
// AnalyzeFile returns nothing on purpose. It is already running detached, so
// there is no caller left to hand an error to; the place a failure has to be
// recorded is the row's own failureReason, which is the field the person who
// uploaded the file can actually see.
type LibraryAnalyzer interface {
	AnalyzeFile(ctx context.Context, req LibraryAnalysisRequest)
}

// ArtifactUploadResponse is the 201 body: the two ids the caller needs to find
// what it just created, on both sides of the promotion.
type ArtifactUploadResponse struct {
	ArtifactId string `json:"artifactId"`
	FileId     string `json:"fileId"`
}

// ArtifactHandlerOptions configures an ArtifactHandler.
type ArtifactHandlerOptions struct {
	Logger     *slog.Logger
	Bucket     string
	Uploader   FileUploader
	Downloader FileDownloader
	Store      LibraryStore
	// Analyzer is optional. Absent, every stored file goes straight to ready.
	Analyzer LibraryAnalyzer
	// MaxUploadBytes overrides LibraryMaxUploadBytes(); zero means "read the
	// environment". Tests set it; production does not.
	MaxUploadBytes int64
	// PromotionWait / PromotionPoll bound the wait for indexFileOnCreate to
	// land the artifact index row. Zero takes the defaults below.
	PromotionWait time.Duration
	PromotionPoll time.Duration
}

// ArtifactHandler serves POST /artifacts and GET /artifacts/{id}/content.
type ArtifactHandler struct {
	logger         *slog.Logger
	bucket         string
	uploader       FileUploader
	downloader     FileDownloader
	store          LibraryStore
	analyzer       LibraryAnalyzer
	maxUploadBytes int64
	promotionWait  time.Duration
	promotionPoll  time.Duration
}

var _ http.Handler = (*ArtifactHandler)(nil)

const (
	defaultPromotionWait = 3 * time.Second
	defaultPromotionPoll = 50 * time.Millisecond
)

// NewArtifactHandler creates an ArtifactHandler.
func NewArtifactHandler(opts ArtifactHandlerOptions) *ArtifactHandler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBytes := opts.MaxUploadBytes
	if maxBytes <= 0 {
		maxBytes = LibraryMaxUploadBytes()
	}
	wait := opts.PromotionWait
	if wait <= 0 {
		wait = defaultPromotionWait
	}
	poll := opts.PromotionPoll
	if poll <= 0 {
		poll = defaultPromotionPoll
	}
	return &ArtifactHandler{
		logger:         logger,
		bucket:         opts.Bucket,
		uploader:       opts.Uploader,
		downloader:     opts.Downloader,
		store:          opts.Store,
		analyzer:       opts.Analyzer,
		maxUploadBytes: maxBytes,
		promotionWait:  wait,
		promotionPoll:  poll,
	}
}

// ServeHTTP dispatches the two routes. Registered by app/transport_artifacts.go
// on every path server.ArtifactPaths() returns.
func (h *ArtifactHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && isArtifactCollectionPath(r.URL.Path):
		h.handleUpload(w, r)
	case r.Method == http.MethodGet:
		h.handleContent(w, r)
	case r.Method == http.MethodPost:
		http.Error(w, "not found", http.StatusNotFound)
	default:
		methodNotAllowed(w, r, http.MethodGet+", "+http.MethodPost)
	}
}

// ---------------------------------------------------------------------------
// POST /artifacts
// ---------------------------------------------------------------------------

func (h *ArtifactHandler) handleUpload(w http.ResponseWriter, r *http.Request) {
	// The actor has to resolve to a USER, not merely to a bearer: ownerUserId
	// is stamped from actor.userId and the blob path is keyed on it, so a
	// credential with no user behind it has nowhere to put the bytes. Refusing
	// here rather than letting the mutation stamp "" is the difference between
	// a 401 and an unreachable, unowned row.
	access, _ := auth.AccessFromContext(r.Context())
	userId := ""
	if access != nil {
		userId = strings.TrimSpace(access.UserId)
	}
	if strings.TrimSpace(auth.ActorFromContext(r.Context())) == "" || userId == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.store == nil {
		http.Error(w, "library store not configured", http.StatusInternalServerError)
		return
	}

	// Cap the WHOLE request body before ParseMultipartForm reads a byte of it
	// (the site_bundle_handler.go pattern). header.Size is a claim by the
	// client; this is the enforcement. The allowance is what keeps the cap
	// meaning "a file of this many bytes" rather than "a request of this many
	// bytes" -- see libraryUploadFramingAllowance. The file itself is bounded
	// at exactly the cap a few lines below.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes+libraryUploadFramingAllowance)

	if err := r.ParseMultipartForm(libraryMultipartMemory); err != nil {
		if isRequestTooLarge(err) {
			h.tooLarge(w)
			return
		}
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}

	file, header, err := r.FormFile(libraryFormFileKey)
	if err != nil {
		http.Error(w, "file field is required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	if header.Size > h.maxUploadBytes {
		h.tooLarge(w)
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, h.maxUploadBytes+1))
	if err != nil {
		if isRequestTooLarge(err) {
			h.tooLarge(w)
			return
		}
		h.logger.Error("read library upload", "error", err)
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	if int64(len(data)) > h.maxUploadBytes {
		h.tooLarge(w)
		return
	}

	name := sanitizeLibraryFileName(firstNonBlank(r.FormValue(libraryFormNameKey), header.Filename))
	mimeType := resolveLibraryMIME(header.Header.Get("Content-Type"), data)
	format := LibraryFormatForMIME(mimeType)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	fileId := id.NewShortId()
	// The storage path the concept documents, verbatim:
	// library/{userId}/{fileId}/{name}.
	objectName := fmt.Sprintf("library/%s/%s/%s", userId, fileId, name)

	ctx := r.Context()

	// --- the bytes, then the row. Never the other way round. ---
	//
	// A row written before the bytes are durable is a row that claims a
	// blobUrl nothing answers. When storage refuses (or is not configured at
	// all) the row is still written -- the owner has to be able to SEE that
	// their upload failed and why, which is what failureReason exists for --
	// but it is written and then immediately marked failed, and the response
	// says so. What is never written is a `local://` placeholder in `stored`,
	// which is the attachment handler's degraded shape and reads to every
	// consumer as a successfully stored file.
	blobUrl := objectName
	storageErr := ""
	storageStatus := 0
	switch {
	case h.uploader == nil || h.bucket == "":
		storageErr = "object storage is not configured on this node, so the bytes were not stored " +
			"(set MEMQL_AZURE_BLOB_CONTAINER and MEMQL_AZURE_STORAGE_CONNECTION_STRING)"
		storageStatus = http.StatusServiceUnavailable
	default:
		stored, upErr := h.uploader.Upload(ctx, h.bucket, objectName, data, mimeType)
		if upErr != nil {
			h.logger.Error("upload library file to blob storage", "error", upErr, "fileId", fileId)
			storageErr = "object storage refused the upload, so the bytes were not stored"
			storageStatus = http.StatusBadGateway
		} else if strings.TrimSpace(stored) != "" {
			blobUrl = stored
		}
	}

	if err := h.store.CreateFile(ctx, LibraryFileCreateParams{
		FileId:   fileId,
		Name:     name,
		MimeType: mimeType,
		Size:     len(data),
		Sha256:   digest,
		BlobUrl:  blobUrl,
		Source:   "uploaded",
		Format:   format,
	}); err != nil {
		h.logger.Error("create library file row", "error", err, "fileId", fileId)
		http.Error(w, fmt.Sprintf("failed to create library file: %v", err), http.StatusInternalServerError)
		return
	}

	if storageErr != "" {
		if err := h.store.SetFileStatus(ctx, LibraryFileStatusParams{
			FileId:        fileId,
			Status:        libraryStatusFailed,
			FailureReason: storageErr,
		}); err != nil {
			h.logger.Error("mark library file failed", "error", err, "fileId", fileId)
		}
		http.Error(w, storageErr, storageStatus)
		return
	}

	// --- promotion, then labels, then the analysis hand-off ---
	//
	// indexFileOnCreate promotes the file into the index off the
	// graph.node.created event, which is asynchronous, so the artifact id is
	// WAITED for rather than derived. Deriving it -- concat("artifact-",
	// hash(ref)) -- would put a copy of a DSL expression in Go, and the copy
	// would be the thing that is wrong the day the expression changes.
	artifactId := h.waitForPromotion(ctx, fileId)
	if artifactId == "" {
		// The labels go with it, and that is worth naming rather than leaving
		// as a silent consequence: they are applied to the INDEX row, which is
		// the row that does not exist yet. The file is fine and the artifact
		// will appear; the labels this request carried will not be on it.
		h.logger.Warn("library artifact promotion not visible yet; returning the file id alone",
			"fileId", fileId, "waited", h.promotionWait,
			"labelsDropped", len(splitList(r.FormValue(libraryFormLabelsKey))))
	} else {
		h.applyLabels(ctx, artifactId, r.FormValue(libraryFormLabelsKey))
	}

	h.startAnalysis(userId, artifactId, LibraryFileCreateParams{
		FileId:   fileId,
		Name:     name,
		MimeType: mimeType,
		Size:     len(data),
		Sha256:   digest,
		BlobUrl:  blobUrl,
		Format:   format,
	}, data)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ArtifactUploadResponse{ArtifactId: artifactId, FileId: fileId})
}

// tooLarge answers the cap. 413, with the limit named -- a caller that cannot
// see the number can only bisect for it.
func (h *ArtifactHandler) tooLarge(w http.ResponseWriter) {
	http.Error(w, fmt.Sprintf("file too large: max %d bytes (%s)", h.maxUploadBytes, LibraryMaxUploadBytesEnv),
		http.StatusRequestEntityTooLarge)
}

// waitForPromotion polls libraryArtifactBySourceConceptRef until
// indexFileOnCreate has landed the index row, or the budget runs out.
//
// A bounded wait rather than an unbounded one, and an empty answer rather than
// an error: the file row exists and is the caller's either way, and the
// artifact WILL appear. Failing the upload because an automation was slow
// would throw away durable bytes over a scheduling detail.
func (h *ArtifactHandler) waitForPromotion(ctx context.Context, fileId string) string {
	deadline := time.Now().Add(h.promotionWait)
	for {
		artifactId, err := h.store.ArtifactForFile(ctx, fileId)
		if err != nil {
			h.logger.Warn("resolve library artifact for file", "error", err, "fileId", fileId)
			return ""
		}
		if strings.TrimSpace(artifactId) != "" {
			return strings.TrimSpace(artifactId)
		}
		if !time.Now().Add(h.promotionPoll).Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(h.promotionPoll):
		}
	}
}

// applyLabels puts each comma-separated label on the promoted index row
// through libraryAddArtifactLabel -- the SAME builtin the portal's label
// editor and the agent tool call, so a label applied at upload is
// indistinguishable from one applied later. Idempotent, and best-effort: a
// label that fails to write is logged, never a reason to fail an upload whose
// bytes are already durable.
func (h *ArtifactHandler) applyLabels(ctx context.Context, artifactId, raw string) {
	for _, label := range splitList(raw) {
		if err := h.store.AddArtifactLabel(ctx, artifactId, label); err != nil {
			h.logger.Warn("apply library upload label", "error", err,
				"artifactId", artifactId, "label", label)
		}
	}
}

// startAnalysis hands off to memql#4342's pass, or closes the lifecycle here.
//
// The detached context carries the OWNER's actor, not the request's: the pass
// writes chunks and status transitions that must land owned by the same person
// as the file, and a background context with no actor at all would stamp "".
// context.Background() rather than r.Context() so the client hanging up does
// not cancel work whose bytes are already stored.
func (h *ArtifactHandler) startAnalysis(userId, artifactId string, p LibraryFileCreateParams, data []byte) {
	ctx := auth.ContextWithUserActor(context.Background(), userId)

	if h.analyzer == nil || p.Format == libraryFormatOther {
		// Opaque, or nothing to analyze with. Either way the file is as
		// finished as it is going to get, and leaving it in `stored` would
		// read as "analysis pending" forever.
		if err := h.store.SetFileStatus(ctx, LibraryFileStatusParams{
			FileId:          p.FileId,
			Status:          libraryStatusReady,
			EmbeddingStatus: "none",
		}); err != nil {
			h.logger.Warn("mark library file ready", "error", err, "fileId", p.FileId)
		}
		return
	}

	req := LibraryAnalysisRequest{
		FileId:      p.FileId,
		ArtifactId:  artifactId,
		OwnerUserId: userId,
		Name:        p.Name,
		MimeType:    p.MimeType,
		Format:      p.Format,
		Size:        p.Size,
		Sha256:      p.Sha256,
		BlobUrl:     p.BlobUrl,
		Data:        data,
	}
	go h.analyzer.AnalyzeFile(ctx, req)
}

// ---------------------------------------------------------------------------
// GET /artifacts/{id}/content
// ---------------------------------------------------------------------------

func (h *ArtifactHandler) handleContent(w http.ResponseWriter, r *http.Request) {
	artifactId, ok := parseArtifactContentPath(r.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if strings.TrimSpace(auth.ActorFromContext(r.Context())) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.store == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()

	// EVERY refusal below this line is a 404, and that is the whole posture.
	// The reads run under the caller's actor against owner-gated,
	// tier-declaring concepts, so "you may not see it" and "it is not there"
	// come back from the graph as the same empty result -- and the response
	// must not reintroduce the distinction the authorization model just
	// erased. Same reasoning the attachment download records.
	artifact, err := h.store.Artifact(ctx, artifactId)
	if err != nil {
		h.logger.Error("library artifact lookup failed", "error", err, "artifactId", artifactId)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if artifact == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if artifact.Kind == "file" {
		h.serveFileBytes(w, r, artifact)
		return
	}
	h.serveRenderedBody(w, r, artifact)
}

// serveFileBytes streams a file artifact's stored bytes. No redirect: the
// bytes come through the bff, after the backing row has been admitted for this
// caller in its own right.
func (h *ArtifactHandler) serveFileBytes(w http.ResponseWriter, r *http.Request, artifact *LibraryArtifactRow) {
	ctx := r.Context()

	row, err := h.store.File(ctx, artifact.SourceConceptRef)
	if err != nil {
		h.logger.Error("library file lookup failed", "error", err,
			"artifactId", artifact.ID, "sourceConceptRef", artifact.SourceConceptRef)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if row == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.downloader == nil {
		http.Error(w, "file not available for download", http.StatusNotFound)
		return
	}
	data, err := h.downloader.DownloadURL(ctx, row.BlobUrl)
	if err != nil {
		h.logger.Warn("library file bytes unavailable", "error", err, "fileId", row.ID,
			"status", row.Status)
		http.Error(w, "file not available", http.StatusNotFound)
		return
	}

	mimeType := strings.TrimSpace(row.MimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	name := sanitizeLibraryFileName(firstNonBlank(row.Name, artifact.Title))
	writeDownload(w, mimeType, name, data)
}

// serveRenderedBody exports a note / generated output / memory as a text
// download with a filename derived from its title (design D9 -- one route
// exports the whole Library).
func (h *ArtifactHandler) serveRenderedBody(w http.ResponseWriter, r *http.Request, artifact *LibraryArtifactRow) {
	body, err := h.store.ExportBody(r.Context(), artifact.Kind, artifact.SourceConceptRef)
	if err != nil {
		h.logger.Error("library export body lookup failed", "error", err,
			"artifactId", artifact.ID, "kind", artifact.Kind)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if body == nil {
		// Covers both "the row is absent or not yours" and "this kind has no
		// exportable body" (a todo, a calendar event, a live source). One
		// answer for both, for the reason handleContent gives.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	mimeType := "text/plain; charset=utf-8"
	ext := ".txt"
	if body.Markdown {
		mimeType = "text/markdown; charset=utf-8"
		ext = ".md"
	}
	name := exportFileName(firstNonBlank(body.Title, artifact.Title), ext)
	writeDownload(w, mimeType, name, []byte(body.Body))
}

// writeDownload writes the three headers every export carries plus the bytes.
func writeDownload(w http.ResponseWriter, mimeType, fileName string, data []byte) {
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// paths, names and MIME
// ---------------------------------------------------------------------------

// isArtifactCollectionPath reports whether the path IS the collection --
// /artifacts, or a base-prefixed spelling of it, with or without a trailing
// slash. Anything deeper is not an upload target.
func isArtifactCollectionPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) > 0 && parts[len(parts)-1] == "artifacts"
}

// parseArtifactContentPath parses /artifacts/{artifactId}/content, tolerating a
// leading base prefix like /api.
//
// Segment-walking rather than segmentBetween because the suffix is a whole
// segment two positions along, not the end of the path, and because the id may
// contain colons (a canonical node id does) but never a slash. "content" must
// be the LAST segment: /artifacts/{id}/content/anything is not this route.
func parseArtifactContentPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p != "artifacts" {
			continue
		}
		if i+2 != len(parts)-1 {
			return "", false
		}
		artifactId := strings.TrimSpace(parts[i+1])
		if artifactId == "" || parts[i+2] != "content" {
			return "", false
		}
		return artifactId, true
	}
	return "", false
}

// LibraryFileConceptRef composes the canonical node id that indexFileOnCreate
// writes into v1:library:artifact.sourceConceptRef for a file.
//
// Composing an id in Go is normally the anti-pattern the identifier
// conventions name, and this is the narrow case where it is not: the value is
// not an id being handed to a client, it is the automation's own idempotency
// KEY, and the only way to ask "which index row is this file's" is to name it.
// What is NOT re-derived here is the artifact id itself -- that is
// concat("artifact-", hash(ref)) in the DSL and stays there, resolved through
// libraryArtifactBySourceConceptRef.
//
// Idempotent: an already-canonical value is returned unchanged, so a caller
// that has the ref rather than the bare id cannot double-prefix it.
func LibraryFileConceptRef(fileId string) string {
	fileId = strings.TrimSpace(fileId)
	if fileId == "" {
		return ""
	}
	if strings.HasPrefix(fileId, libraryFileConcept+":") {
		return fileId
	}
	return libraryFileConcept + ":" + fileId
}

// sanitizeLibraryFileName reduces a client-supplied name to something safe to
// use as the last segment of a storage key AND as a Content-Disposition
// filename -- the two places it lands.
//
// Directory components are stripped (both separators, because a Windows client
// sends backslashes), control characters and double quotes are dropped (the
// header quotes the value), leading/trailing dots go (so "." and ".." cannot
// survive), and the result is bounded by RUNES rather than bytes so the trim
// cannot split a codepoint.
func sanitizeLibraryFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.Trim(strings.TrimSpace(b.String()), ".")
	if name == "" {
		return "upload"
	}
	runes := []rune(name)
	if len(runes) > libraryMaxFileNameRunes {
		name = string(runes[:libraryMaxFileNameRunes])
	}
	return name
}

// exportFileName derives a download filename from an artifact title.
// Whitespace collapses to hyphens so the name survives a shell without
// quoting; everything else goes through sanitizeLibraryFileName.
func exportFileName(title, ext string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "artifact"
	}
	title = strings.Join(strings.Fields(title), "-")
	name := sanitizeLibraryFileName(title)
	if strings.HasSuffix(strings.ToLower(name), ext) {
		return name
	}
	return name + ext
}

// resolveLibraryMIME normalizes the declared content type, and sniffs the
// leading bytes when the client sent none.
//
// ANY type is accepted -- there is no allowlist, which is one of the reasons
// the Library does not ride the attachment routes. Sniffing is a fallback for
// an ABSENT header, not a second opinion about a present one: a client that
// says what it is sending is believed, because it knows things the first 512
// bytes do not.
func resolveLibraryMIME(declared string, data []byte) string {
	if mt := normalizeMIME(declared); mt != "" {
		return mt
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	if mt := normalizeMIME(http.DetectContentType(data)); mt != "" {
		return mt
	}
	return "application/octet-stream"
}

// normalizeMIME lowercases a media type and strips its parameters.
func normalizeMIME(raw string) string {
	mt := strings.TrimSpace(raw)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return strings.ToLower(mt)
}

// libraryFormatByMIME is the MIME -> v1:library:file.format table.
//
// It lives in GO rather than in the promotion automation because an automation
// step argument has no conditional form -- the concept's own format field
// documents exactly that, and says the row is where the answer is recorded
// once. Exported through LibraryFormatForMIME so the analysis pass classifies
// a sniffed type the same way the upload route classified a declared one.
var libraryFormatByMIME = map[string]string{
	"text/markdown":   "markdown",
	"text/x-markdown": "markdown",

	"application/pdf": "pdf",

	"application/msword": "document",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "document",
	"application/vnd.oasis.opendocument.text":                                 "document",
	"application/rtf": "document",
	"text/rtf":        "document",

	"text/csv":                  "spreadsheet",
	"application/csv":           "spreadsheet",
	"text/tab-separated-values": "spreadsheet",
	"application/vnd.ms-excel":  "spreadsheet",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "spreadsheet",
	"application/vnd.oasis.opendocument.spreadsheet":                    "spreadsheet",

	"application/json": "text",
	"application/xml":  "text",
}

// LibraryFormatForMIME derives the v1:library:file.format enum value from a
// MIME type, defaulting to "other" -- the metadata-only card, and the
// discriminator for "store it opaquely, analyze nothing".
func LibraryFormatForMIME(mimeType string) string {
	mt := normalizeMIME(mimeType)
	if format, ok := libraryFormatByMIME[mt]; ok {
		return format
	}
	if strings.HasPrefix(mt, "image/") {
		return "image"
	}
	if strings.HasPrefix(mt, "text/") {
		return "text"
	}
	return libraryFormatOther
}

// firstNonBlank returns the first argument that is not blank once trimmed.
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// isRequestTooLarge reports whether an error is MaxBytesReader's cap being
// hit. errors.As for the typed form, plus the string the multipart reader
// produces when it wraps the read error into its own -- checking only the type
// leaves the ParseMultipartForm path reporting "invalid multipart form" for an
// oversized body, which is a 400 for what is a 413.
func isRequestTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}

// ---------------------------------------------------------------------------
// the engine-backed store
// ---------------------------------------------------------------------------

// EngineLibraryStore implements LibraryStore by calling the DSL constructs
// memql#4340 declared, through the same MemQLExecutor seam
// EngineAttachmentStore uses.
//
// EVERY CALL RUNS UNDER THE CALLER'S ACTOR. None of these constructs is
// @serverOnly and all of them are owner-gated over concepts that declare
// @rowAuthz(owner="ownerUserId", clusterOwner), so authorization is the
// engine's answer and not something this type re-implements. That is why the
// download path can treat an empty result as 404 without a separate ownership
// probe: an empty result IS the refusal.
type EngineLibraryStore struct {
	engine MemQLExecutor
}

var _ LibraryStore = (*EngineLibraryStore)(nil)

// NewEngineLibraryStore creates a LibraryStore backed by a MemQL engine.
func NewEngineLibraryStore(engine MemQLExecutor) *EngineLibraryStore {
	return &EngineLibraryStore{engine: engine}
}

func (s *EngineLibraryStore) exec(ctx context.Context, fn string, args map[string]any) (any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	q, err := dslCall(fn, args)
	if err != nil {
		return nil, err
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("execute %s: %w", fn, err)
	}
	return res, nil
}

// CreateFile runs createLibraryFile.
//
// Only the fields the mutation DECLARES are passed. An argument it does not
// declare is not refused -- rejectUnknownArgs is gated behind the MCP boundary
// -- it is silently discarded, so sending `status` or `ownerUserId` here would
// look like it worked and write nothing (memql#3626, memql#4258).
func (s *EngineLibraryStore) CreateFile(ctx context.Context, p LibraryFileCreateParams) error {
	args := map[string]any{
		"fileId":   p.FileId,
		"name":     p.Name,
		"mimeType": p.MimeType,
		"size":     p.Size,
		"sha256":   p.Sha256,
		"blobUrl":  p.BlobUrl,
		"source":   p.Source,
	}
	if f := strings.TrimSpace(p.Format); f != "" {
		args["format"] = f
	}
	if sm := strings.TrimSpace(p.Summary); sm != "" {
		args["summary"] = sm
	}
	_, err := s.exec(ctx, "createLibraryFile", args)
	return err
}

// SetFileStatus runs setLibraryFileStatus. Absent optional fields are omitted
// rather than sent blank: the mutation read-merges, so an omitted field keeps
// whatever an earlier transition wrote and a blank one would erase it.
func (s *EngineLibraryStore) SetFileStatus(ctx context.Context, p LibraryFileStatusParams) error {
	args := map[string]any{
		"fileId": p.FileId,
		"status": p.Status,
	}
	if v := strings.TrimSpace(p.Summary); v != "" {
		args["summary"] = v
	}
	if v := strings.TrimSpace(p.EmbeddingStatus); v != "" {
		args["embeddingStatus"] = v
	}
	if v := strings.TrimSpace(p.FailureReason); v != "" {
		args["failureReason"] = v
	}
	_, err := s.exec(ctx, "setLibraryFileStatus", args)
	return err
}

// ArtifactForFile resolves the promoted index row through
// libraryArtifactBySourceConceptRef.
func (s *EngineLibraryStore) ArtifactForFile(ctx context.Context, fileId string) (string, error) {
	ref := LibraryFileConceptRef(fileId)
	if ref == "" {
		return "", fmt.Errorf("fileId is required")
	}
	res, err := s.exec(ctx, "libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": ref})
	if err != nil {
		return "", err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return "", nil
	}
	return rowString(rows[0], "id"), nil
}

// AddArtifactLabel runs the libraryAddArtifactLabel builtin -- the same one
// the portal's label editor and the artifactAddLabel agent tool call.
//
// A BUILTIN IS NOT CALLED THE WAY A MUTATION IS, and this is the one place in
// this file where that matters. `libraryAddArtifactLabel(artifactId: "x",
// label: "y")` -- the bare named-args form every other call site here uses --
// is refused by the engine with "requires a JSON object argument"; the builtin
// surface takes `builtin <name>(k: v, ...)` (or the object-literal
// `<name>({...})`, which the function surface has rejected since memql#2335).
// This is exactly the class of defect that ships green when a handler suite
// records query strings and parses none of them, so
// TestLibraryStoreCallSitesResolveThroughTheRealEngine runs every rendered
// call site, this one included, through the real front end.
func (s *EngineLibraryStore) AddArtifactLabel(ctx context.Context, artifactId, label string) error {
	artifactId = strings.TrimSpace(artifactId)
	label = strings.TrimSpace(label)
	if artifactId == "" || label == "" {
		return fmt.Errorf("artifactId and label are required")
	}
	q, err := dslBuiltinCall("libraryAddArtifactLabel", map[string]any{
		"artifactId": artifactId,
		"label":      label,
	})
	if err != nil {
		return err
	}
	if s == nil || s.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	if _, err := s.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("execute libraryAddArtifactLabel: %w", err)
	}
	return nil
}

// dslBuiltinCall renders `builtin name(k: v, ...)` -- the builtin invocation
// form, the same spelling sdk/go/client's generated builders emit. The
// argument rendering is dslCall's, so the JSON-escaped quoting that keeps a
// value from breaking out of its literal is shared rather than re-derived.
func dslBuiltinCall(fn string, args map[string]any) (string, error) {
	call, err := dslCall(fn, args)
	if err != nil {
		return "", err
	}
	return "builtin " + call, nil
}

// Artifact reads one index row through libraryArtifactById.
func (s *EngineLibraryStore) Artifact(ctx context.Context, artifactId string) (*LibraryArtifactRow, error) {
	artifactId = strings.TrimSpace(artifactId)
	if artifactId == "" {
		return nil, fmt.Errorf("artifactId is required")
	}
	res, err := s.exec(ctx, "libraryArtifactById", map[string]any{"artifactId": artifactId})
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &LibraryArtifactRow{
		ID:               rowString(r, "id"),
		Kind:             rowString(r, "kind"),
		SourceConceptRef: rowString(r, "sourceConceptRef"),
		Title:            rowString(r, "title"),
		Format:           rowString(r, "format"),
		MimeType:         rowString(r, "mimeType"),
		Summary:          rowString(r, "summary"),
	}, nil
}

// File reads one v1:library:file row through libraryFileById.
func (s *EngineLibraryStore) File(ctx context.Context, fileRef string) (*LibraryFileRow, error) {
	fileRef = strings.TrimSpace(fileRef)
	if fileRef == "" {
		return nil, nil
	}
	res, err := s.exec(ctx, "libraryFileById", map[string]any{"fileId": fileRef})
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &LibraryFileRow{
		ID:       rowString(r, "id"),
		Name:     rowString(r, "name"),
		MimeType: rowString(r, "mimeType"),
		Size:     rowInt(r, "size"),
		BlobUrl:  rowString(r, "blobUrl"),
		Status:   rowString(r, "status"),
	}, nil
}

// ExportBody reads a text-bearing backing row for the export route.
//
// THE KIND DECIDES THE READ, and the set is closed on purpose. `file` is
// handled by File above; `todo`, `calendar_event`, `live_source` and
// `document` have no body this route can render, so they return nil and the
// handler answers 404 -- the same answer a denied row gets, which is what
// keeps the response from distinguishing "not yours" from "nothing to export".
func (s *EngineLibraryStore) ExportBody(ctx context.Context, kind, sourceConceptRef string) (*LibraryExportBody, error) {
	ref := strings.TrimSpace(sourceConceptRef)
	if ref == "" {
		return nil, nil
	}

	var (
		fn     string
		argKey string
	)
	switch kind {
	case "note":
		fn, argKey = "noteById", "noteId"
	case "generated_output":
		fn, argKey = "generatedOutputById", "outputId"
	case "memory":
		fn, argKey = "memoryById", "memoryId"
	default:
		return nil, nil
	}

	res, err := s.exec(ctx, fn, map[string]any{argKey: ref})
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]

	switch kind {
	case "note":
		// A note's body is freeform text the portal renders as markdown, so it
		// exports as markdown.
		return &LibraryExportBody{
			Title:    rowString(r, "title"),
			Body:     rowString(r, "body"),
			Markdown: true,
		}, nil
	case "generated_output":
		// createGeneratedOutput defaults format to "markdown", so most of
		// these are; a producer that said otherwise is believed.
		markdown := rowString(r, "format") == "markdown" ||
			normalizeMIME(rowString(r, "mimeType")) == "text/markdown"
		return &LibraryExportBody{
			Title:    rowString(r, "title"),
			Body:     rowString(r, "body"),
			Markdown: markdown,
		}, nil
	default: // memory
		// A memory's content is a fact / preference / instruction sentence,
		// not a document -- plain text, not markdown.
		return &LibraryExportBody{
			Title:    rowString(r, "title"),
			Body:     rowString(r, "content"),
			Markdown: false,
		}, nil
	}
}

// rowString reads a string field off a materialized row.
func rowString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// rowInt reads an int field off a materialized row, tolerating the float64 a
// JSON round-trip produces.
func rowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}
