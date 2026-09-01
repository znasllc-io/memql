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
	"github.com/znasllc-io/memql/component/server/fileversion"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
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

	// libraryFormFolderKey optionally names the v1:library:folder the upload
	// is filed under (memql#4781, design B2). The upload route is the writer
	// that knows the target; the promotion automation forwards it onto the
	// index row, which is authoritative from then on.
	libraryFormFolderKey = "folderId"

	// libraryFormWorkerIdKey / libraryFormWorkerNameKey / libraryFormPathKey
	// carry the upload's machine provenance (memql#4781, design D5): the
	// worker registration the file came FROM, its display name, and the path
	// it occupied there. A cockpit push sends them; a browser physically
	// cannot name a machine and sends none. The id is VERIFIED against the
	// caller's own fleet before anything else happens -- an unverifiable
	// claim refuses the whole upload, because a silently-dropped field would
	// render as "uploaded here", which is a lie.
	libraryFormWorkerIdKey   = "uploadedFromWorkerId"
	libraryFormWorkerNameKey = "uploadedFromWorkerName"
	libraryFormPathKey       = "uploadedFromPath"

	// libraryFormTargetKey names the artifact this upload is a NEW VERSION of
	// (epic memql#4806, design D7). Absent is the ordinary case: a fresh
	// upload, minting a file row and an artifact of its own. Present makes
	// this a supersede -- the artifact keeps its id, its filing and its
	// labels, the Files list still shows one row, and the outgoing head is
	// frozen as a version row first.
	//
	// The person NAMES the target, which is the whole identity story: a
	// browser upload carries no honest machine or path identity, and guessing
	// by filename would silently merge two different files (#4721's D5
	// reasoning, unchanged). The KEY-MATCHED half landed in epic memql#4783
	// and resolves to this same seam: resolveKeyedVersionTarget runs only when
	// this form key is absent, because a person naming a row outranks a key
	// that might disagree with them.
	libraryFormTargetKey = "targetArtifactId"

	// libraryLinkStateSynced is the ONE link state the engine writes (epic
	// memql#4783). It is stamped on any push that named a (machine, path),
	// because at the instant those exact bytes arrived the copy did equal its
	// origin. The other two -- "stale" and "origin_gone" -- are answers only
	// something looking at the origin can give, so the cockpit's verify lane
	// reports them through setLibraryFileLinkState and nothing here invents
	// one.
	libraryLinkStateSynced = "synced"

	// libraryVersionQueryKey selects which version of a file the content
	// route serves (design D8). Absent means the head, which is what every
	// caller written before versions existed sends. A QUERY PARAMETER rather
	// than a new path because the front-door path set is GENERATED
	// (memql#3703) and a new path shape would change it; both spellings live
	// under the one /artifacts/{id}/content rule that already exists.
	libraryVersionQueryKey = "version"

	// DefaultLibraryMaxUploadBytes is the cap when MEMQL_LIBRARY_MAX_UPLOAD_BYTES
	// is unset. 4 GiB since memql#4782 (design D9): the chunked session path
	// carries big files -- videos are the named case -- in bounded 16 MiB
	// pieces, so the per-FILE cap no longer implies a same-sized request
	// body anywhere; the front-door body allowance stays ~48m and bounds the
	// individual REQUESTS. Still an operator knob, and a client's declared
	// size stays a claim the commit verifies.
	DefaultLibraryMaxUploadBytes int64 = 4 << 30

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

	// libraryKindFile is the artifact index's kind for a file-backed row --
	// the discriminator the content route branches on and the only kind that
	// carries upload versions.
	libraryKindFile = "file"

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
	// FolderId is the initial filing (memql#4781, design B2) -- forwarded to
	// the index by the promotion automation, after which the index copy is
	// authoritative. Blank = root.
	FolderId string
	// UploadedFrom* is the verified machine provenance (design D5). The
	// handler has already checked the registration belongs to the caller and
	// resolved the NAME from the registration row itself before these are
	// set; blank means no claim was made, never a dropped one.
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
}

// LibraryWorkerRef is the slice of a worker registration the provenance
// check needs: that it exists in the CALLER's own fleet, and what the fleet
// calls it.
type LibraryWorkerRef struct {
	ID   string
	Name string
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

// LibraryFileRow is the v1:library:file projection the download path needs --
// and, since epic memql#4806, the projection a SUPERSEDE freezes.
//
// The version fields are here rather than on a second read because the
// supersede has already read this row under the caller's actor to verify the
// target, and the snapshot it writes must be the OUTGOING head's own facts:
// its name, its bytes, its hash, its provenance and the moment it arrived. A
// second read could see a different head.
type LibraryFileRow struct {
	ID       string
	Name     string
	MimeType string
	Size     int
	BlobUrl  string
	Status   string
	// Sha256 is blank when nothing has measured it yet (a chunked upload
	// whose analysis pass has not streamed the blob). Never "no hash exists".
	Sha256  string
	Format  string
	Summary string
	// VersionNumber is 1 for every file uploaded before versions existed:
	// those rows have no member at all, and the reader that turned absence
	// into 0 would render "v0" on most of the Library.
	VersionNumber int
	// VersionUploadedAt is when THIS head's bytes arrived. Deliberately not
	// the row's createdAt, which an append-only update moves on every status
	// transition.
	VersionUploadedAt      string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
	// LinkState is "" for a file with no origin link -- a browser upload has
	// none to have. It is never a fourth state: "we do not track this file"
	// and "we track it and it is fine" are different answers (epic memql#4783).
	LinkState     string
	LinkCheckedAt string
	// Archived is the soft delete (memql#4340). libraryFileByUploadedFrom
	// filters it out, so a re-push never lands in the Bin; libraryFileById
	// does NOT, because a caller asking about a specific id deserves the
	// honest answer and this field is how they get it.
	Archived bool
}

// LibraryFileVersionRow is one superseded version, as the history read and
// the ?version={n} resolve return it.
type LibraryFileVersionRow struct {
	ID                     string
	FileId                 string
	VersionNumber          int
	Name                   string
	MimeType               string
	Size                   int
	Sha256                 string
	BlobUrl                string
	Format                 string
	Summary                string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
	UploadedAt             string
	SupersededAt           string
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
	// FileByUploadedFrom resolves the LIVE file a machine pushed from a given
	// path -- the (machine, path) key the watched-folder backup versions on
	// (epic memql#4783). Nil when nothing matches, which is the ordinary
	// answer for a first push and for every browser upload.
	FileByUploadedFrom(ctx context.Context, workerId, path string) (*LibraryFileRow, error)
	// ExportBody reads a text-bearing backing row (note / generated output /
	// memory) under the caller's actor. Returns nil when the kind has no
	// exportable body, or the row is absent or denied.
	ExportBody(ctx context.Context, kind, sourceConceptRef string) (*LibraryExportBody, error)
	// OwnedWorker resolves a worker registration IN THE CALLER'S OWN FLEET
	// (memql#4781, design D5): the read runs under the caller's actor, so
	// "not yours" and "not there" come back as the same nil -- which is the
	// whole verification. The returned Name is the fleet's own label for the
	// machine (displayName when the owner set one, else the reported
	// hostname), so an upload's provenance label can never disagree with the
	// fleet page.
	OwnedWorker(ctx context.Context, workerId string) (*LibraryWorkerRef, error)
	// StorageFootprint sums the CALLER's Library storage (memql#4782,
	// design C4): stored file bytes -- archived included, retention is real
	// -- the bytes of every SUPERSEDED version (epic memql#4806, design D9:
	// superseding destroys nothing, so those bytes are as real as a head's),
	// and the declared sizes of their open upload sessions. All three reads
	// are deliberately unbounded (a truncated page fails the quota OPEN),
	// and all three run under the caller's actor.
	StorageFootprint(ctx context.Context) (fileBytes, versionBytes, sessionBytes int64, err error)

	// FileVersion resolves ONE superseded version by (fileId, versionNumber)
	// under the caller's actor. Returns nil when absent or denied -- the
	// content route cannot tell those apart, and must not.
	FileVersion(ctx context.Context, fileId string, versionNumber int) (*LibraryFileVersionRow, error)

	// SupersedeFile freezes the outgoing head as a version row and then moves
	// the head onto new bytes (epic memql#4806). ONE method because the ORDER
	// is a design decision rather than a caller's choice: a crash between the
	// two writes may duplicate a version, never lose one. Runs the two
	// @serverOnly mutations through component/server/fileversion.
	SupersedeFile(ctx context.Context, snap LibraryVersionSnapshot, head LibraryHeadMove) error

	// RestampFileArtifact re-versions the artifact index row from its backing
	// file, so a new version's name, format and watermark reach the Library
	// list immediately rather than whenever the analysis pass finishes.
	// Idempotent and information-free; best-effort at every call site.
	RestampFileArtifact(ctx context.Context, fileId string) error

	// SetFileLinkState records how a file's copy stands against the machine it
	// was pushed from (epic memql#4783). The upload route calls it with
	// "synced" whenever a push named a (machine, path), because at the instant
	// those exact bytes arrived the copy DID equal the origin; the cockpit's
	// verify lane reports every later transition.
	SetFileLinkState(ctx context.Context, fileId, state string) error
}

// LibraryVersionSnapshot is the OUTGOING head, frozen. Mirrors
// fileversion.Snapshot field for field, declared here because
// component/server's seam is stated entirely in this package's own types --
// the same reason LibraryFileCreateParams exists beside createLibraryFile.
type LibraryVersionSnapshot struct {
	VersionId              string
	FileId                 string
	VersionNumber          int
	Name                   string
	MimeType               string
	Size                   int64
	Sha256                 string
	BlobUrl                string
	Format                 string
	Summary                string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
	UploadedAt             string
}

// LibraryHeadMove is the new bytes a superseded file's head moves onto.
type LibraryHeadMove struct {
	FileId                 string
	VersionNumber          int
	Name                   string
	MimeType               string
	Size                   int64
	Sha256                 string
	BlobUrl                string
	Format                 string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
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
// what it just created, on both sides of the promotion, and which version it
// landed as.
//
// VersionNumber is 1 for a fresh upload and N for a supersede, so a client
// that named a target can say "Version 3 uploaded" without a second read.
// Always present rather than omitempty: a missing field would read as "this
// build does not do versions", and every upload this build takes has one.
type ArtifactUploadResponse struct {
	ArtifactId    string `json:"artifactId"`
	FileId        string `json:"fileId"`
	VersionNumber int    `json:"versionNumber"`
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
	// Sessions + Blocks wire the chunked upload path (memql#4782). Either
	// absent answers 501 on the session routes; the one-shot route is
	// unaffected.
	Sessions UploadSessionStore
	Blocks   BlockStore
	// Streamer is the constant-memory download seam. Absent, the content
	// route keeps the buffered DownloadURL path and serves no Range.
	Streamer StreamDownloader
	// MaxUploadBytes overrides LibraryMaxUploadBytes(); zero means "read the
	// environment". Tests set it; production does not.
	MaxUploadBytes int64
	// UserQuotaBytes overrides LibraryUserQuotaBytes(); zero means "read the
	// environment"; negative disables the quota entirely (tests only).
	UserQuotaBytes int64
	// ChunkSizeBytes overrides LibraryChunkSizeBytes. Tests set it small;
	// production takes the constant.
	ChunkSizeBytes int64
	// PromotionWait / PromotionPoll bound the wait for indexFileOnCreate to
	// land the artifact index row. Zero takes the defaults below.
	PromotionWait time.Duration
	PromotionPoll time.Duration
}

// ArtifactHandler serves the Library's byte routes: the one-shot upload,
// the chunked session family, and the content export.
type ArtifactHandler struct {
	logger         *slog.Logger
	bucket         string
	uploader       FileUploader
	downloader     FileDownloader
	store          LibraryStore
	analyzer       LibraryAnalyzer
	sessions       UploadSessionStore
	blocks         BlockStore
	streamer       StreamDownloader
	maxUploadBytes int64
	userQuotaBytes int64
	chunkSizeBytes int64
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
	quota := opts.UserQuotaBytes
	if quota == 0 {
		quota = LibraryUserQuotaBytes()
	}
	chunkSize := opts.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = LibraryChunkSizeBytes
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
		sessions:       opts.Sessions,
		blocks:         opts.Blocks,
		streamer:       opts.Streamer,
		maxUploadBytes: maxBytes,
		userQuotaBytes: quota,
		chunkSizeBytes: chunkSize,
		promotionWait:  wait,
		promotionPoll:  poll,
	}
}

// ServeHTTP dispatches the route family. Registered by
// app/transport_artifacts.go on every path server.ArtifactPaths() returns
// -- the session routes live UNDER the /artifacts prefix, so the same
// Ingress rules and the same mux registrations carry them; PUT joined the
// registration for the chunk route (memql#4782).
func (h *ArtifactHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && isUploadInitPath(path):
		h.handleUploadInit(w, r)
	case r.Method == http.MethodPost:
		if uploadId, ok := parseUploadCompletePath(path); ok {
			h.handleUploadComplete(w, r, uploadId)
			return
		}
		if isArtifactCollectionPath(path) {
			h.handleUpload(w, r)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	case r.Method == http.MethodPut:
		if uploadId, n, ok := parseUploadChunkPath(path); ok {
			h.handleUploadChunk(w, r, uploadId, n)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	case r.Method == http.MethodGet:
		if uploadId, ok := parseUploadSessionPath(path); ok {
			h.handleUploadInventory(w, r, uploadId)
			return
		}
		h.handleContent(w, r)
	default:
		methodNotAllowed(w, r, http.MethodGet+", "+http.MethodPost+", "+http.MethodPut)
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

	ctx := r.Context()

	// --- the version target, resolved BEFORE any byte is stored (D7) ---
	//
	// Named by the person, never guessed: a browser upload carries no honest
	// machine or path identity, and matching by filename would silently merge
	// two different files. Absent is the ordinary case and leaves everything
	// below exactly as it was.
	targetArtifact, head, status, msg := h.resolveVersionTarget(ctx, strings.TrimSpace(r.FormValue(libraryFormTargetKey)))
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	// --- provenance, verified BEFORE any byte is stored (memql#4781, D5) ---
	//
	// A claim that fails verification refuses the WHOLE upload: dropping the
	// field and continuing would store a file that renders as "uploaded
	// here", which is a lie, and a row written before the refusal would be an
	// upload the response denies. The check runs under the caller's own
	// actor, so "not your machine" and "no such machine" are one empty
	// answer -- deliberately, for the same reason the download route's
	// refusals are 404s.
	folderId := strings.TrimSpace(r.FormValue(libraryFormFolderKey))
	workerId := strings.TrimSpace(r.FormValue(libraryFormWorkerIdKey))
	workerName := strings.TrimSpace(r.FormValue(libraryFormWorkerNameKey))
	fromPath := strings.TrimSpace(r.FormValue(libraryFormPathKey))
	resolvedName, provStatus, provMsg := h.verifyProvenance(ctx, workerId, workerName, fromPath)
	if provStatus != 0 {
		http.Error(w, provMsg, provStatus)
		return
	}
	workerName = resolvedName

	// --- the KEYED version target (epic memql#4783, design E) ---
	//
	// Only when the person named nothing: an explicit targetArtifactId is a
	// person pointing at a row, and a key that disagreed with them would
	// silently write somewhere else. AFTER verifyProvenance, so the machine
	// half of the key has been checked against the caller's own fleet before
	// it is used to find anything.
	if targetArtifact == nil {
		keyedArtifact, keyedHead, keyedStatus, keyedMsg := h.resolveKeyedVersionTarget(ctx, workerId, fromPath)
		if keyedStatus != 0 {
			http.Error(w, keyedMsg, keyedStatus)
			return
		}
		if keyedHead != nil {
			targetArtifact, head = keyedArtifact, keyedHead
		}
	}

	// The quota (memql#4782, design C4): stored bytes plus open sessions'
	// declared sizes plus THIS file. Checked before any byte reaches
	// storage, like every other refusal on this path.
	if quotaStatus, quotaMsg := h.checkQuota(ctx, int64(len(data))); quotaStatus != 0 {
		http.Error(w, quotaMsg, quotaStatus)
		return
	}

	fileId := id.NewShortId()
	// The storage path the concept documents, verbatim:
	// library/{userId}/{fileId}/{name}.
	objectName := fmt.Sprintf("library/%s/%s/%s", userId, fileId, name)
	if head != nil {
		// A new version keeps the file's identity -- same row, same artifact,
		// same folder, same labels -- and lands at a path no version has ever
		// used, which is what makes "superseding never touches stored bytes"
		// true by construction rather than by care (design D6).
		fileId = head.ID
		objectName = libraryVersionObjectName(userId, fileId, name)
	}

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

	// --- STORAGE FAILED, AND THE FILE IS ALREADY THERE ---
	//
	// A supersede whose bytes did not land must not touch the head. The row
	// exists, holds the PREVIOUS version, and is downloadable; failing the
	// request and changing nothing is the honest outcome. (A fresh upload
	// still writes its row and marks it failed, below -- there the owner has
	// nothing else to look at, and failureReason is the only place the
	// failure can be seen.)
	if head != nil && storageErr != "" {
		http.Error(w, storageErr, storageStatus)
		return
	}

	newVersion := 1
	if head != nil {
		// --- the supersede: freeze the outgoing head, then move it (D3) ---
		newVersion = head.VersionNumber + 1
		if err := h.store.SupersedeFile(ctx,
			LibraryVersionSnapshot{
				VersionId:     fileversion.DerivedVersionId(fileId, head.VersionNumber),
				FileId:        fileId,
				VersionNumber: head.VersionNumber,
				Name:          head.Name,
				MimeType:      head.MimeType,
				Size:          int64(head.Size),
				Sha256:        head.Sha256,
				BlobUrl:       head.BlobUrl,
				Format:        head.Format,
				Summary:       head.Summary,
				// The outgoing version's OWN provenance, frozen with it. A
				// file first pushed from a laptop and then replaced from a
				// browser has one version that names the laptop and one that
				// names nothing, which is what actually happened.
				UploadedFromWorkerId:   head.UploadedFromWorkerId,
				UploadedFromWorkerName: head.UploadedFromWorkerName,
				UploadedFromPath:       head.UploadedFromPath,
				UploadedAt:             head.VersionUploadedAt,
			},
			LibraryHeadMove{
				FileId:                 fileId,
				VersionNumber:          newVersion,
				Name:                   name,
				MimeType:               mimeType,
				Size:                   int64(len(data)),
				Sha256:                 digest,
				BlobUrl:                blobUrl,
				Format:                 format,
				UploadedFromWorkerId:   workerId,
				UploadedFromWorkerName: workerName,
				UploadedFromPath:       fromPath,
			}); err != nil {
			h.logger.Error("supersede library file", "error", err, "fileId", fileId,
				"artifactId", targetArtifact.ID, "version", newVersion)
			http.Error(w, fmt.Sprintf("failed to record the new version: %v", err), http.StatusInternalServerError)
			return
		}
	} else if err := h.store.CreateFile(ctx, LibraryFileCreateParams{
		FileId:   fileId,
		Name:     name,
		MimeType: mimeType,
		Size:     len(data),
		Sha256:   digest,
		BlobUrl:  blobUrl,
		Source:   "uploaded",
		Format:   format,
		FolderId: folderId,
		// workerName is the registration's own label by this point (or the
		// form's, only when the registration carries none): verified above,
		// before the bytes moved.
		UploadedFromWorkerId:   workerId,
		UploadedFromWorkerName: workerName,
		UploadedFromPath:       fromPath,
	}); err != nil {
		h.logger.Error("create library file row", "error", err, "fileId", fileId)
		http.Error(w, fmt.Sprintf("failed to create library file: %v", err), http.StatusInternalServerError)
		return
	}

	// --- the origin link, stamped from the strongest evidence there is ---
	//
	// A push that named a (machine, path) is a copy that equalled its origin
	// at the moment those exact bytes arrived, so "synced" here is a fact
	// rather than an assumption -- and it is what makes the Files app's link
	// states work before any watcher exists to report on them. Re-stamped on
	// a supersede too: a re-push is what clears a "stale".
	//
	// BEST EFFORT, like RestampFileArtifact. The upload succeeded; a file that
	// is stored and unlabelled is a smaller problem than a 500 telling
	// somebody their bytes did not land when they did.
	if workerId != "" && fromPath != "" {
		if err := h.store.SetFileLinkState(ctx, fileId, libraryLinkStateSynced); err != nil {
			h.logger.Warn("stamp library file link state", "error", err, "fileId", fileId)
		}
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
	//
	// A SUPERSEDE WAITS FOR NOTHING: the artifact was resolved before a byte
	// moved, and no promotion runs -- the head update deliberately does not
	// re-enter `stored`, which is what stops indexFileOnCreate re-firing and
	// wiping the labels (design D4). It is re-stamped instead, so the new
	// version's name and format reach the list now rather than whenever the
	// analysis pass finishes.
	if head != nil {
		h.restampAfterSupersede(ctx, fileId, targetArtifact.ID)
		h.applyLabels(ctx, targetArtifact.ID, r.FormValue(libraryFormLabelsKey))
		h.startAnalysis(userId, targetArtifact.ID, LibraryFileCreateParams{
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
		_ = json.NewEncoder(w).Encode(ArtifactUploadResponse{
			ArtifactId: targetArtifact.ID, FileId: fileId, VersionNumber: newVersion,
		})
		return
	}

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
	_ = json.NewEncoder(w).Encode(ArtifactUploadResponse{
		ArtifactId: artifactId, FileId: fileId, VersionNumber: 1,
	})
}

// ---------------------------------------------------------------------------
// versions (epic memql#4806)
// ---------------------------------------------------------------------------

// resolveVersionTarget runs design D7's three gates on a targetArtifactId,
// before a byte moves, all under the CALLER's own actor. A blank target is
// the ordinary case and answers (nil, nil, 0, "") -- a fresh upload.
//
// FAIL FAST IS THE POINT, on both routes. The one-shot path has already read
// the body by the time it gets here, but the chunked path calls this at INIT:
// discovering a foreign target after somebody has streamed gigabytes is the
// outcome this ordering exists to prevent.
//
// The refusals are deliberately different shapes, because they are different
// facts. An unresolvable target is 404 -- "not yours" and "not there" come
// back from the graph as the same empty result and the response must not
// reintroduce the distinction authorization just erased, which is the posture
// the whole download route already takes. A non-file target is 400 and NAMES
// THE FLOW that does version documents: the person asking is not wrong, they
// are in the wrong place.
// resolveKeyedVersionTarget resolves the version target from the PROVENANCE
// KEY rather than from a named artifact -- the half memql#4779's sibling
// comment on libraryFormTargetKey promised would live in epic memql#4783, and
// it resolves to this same seam.
//
// The two halves answer the same question from opposite ends and neither can
// do the other's job. A browser NAMES the target, because it has no honest
// machine or path identity to key on and matching by filename would silently
// merge two different files. A cockpit KEYS on (machine, path), because it has
// no artifact id to name -- a watcher pushes a file that changed on disk and
// has never held the id of the row that file became.
//
// It is a NO-OP wherever the key is absent or matches nothing, which is the
// ordinary case: a first push, and every browser upload. A nil head means "a
// new file", exactly as it did before this existed.
//
// The caller has ALREADY verified the worker id against the caller's own
// fleet by the time this runs, so a matched row was pushed from a machine this
// person owns. The read is under their actor besides, which is what makes the
// key safe to trust: it can only ever find their own file.
func (h *ArtifactHandler) resolveKeyedVersionTarget(ctx context.Context, workerId, fromPath string) (*LibraryArtifactRow, *LibraryFileRow, int, string) {
	if strings.TrimSpace(workerId) == "" || strings.TrimSpace(fromPath) == "" {
		return nil, nil, 0, ""
	}
	head, err := h.store.FileByUploadedFrom(ctx, workerId, fromPath)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, "could not resolve the file this path was last pushed as"
	}
	if head == nil {
		return nil, nil, 0, ""
	}
	artifactId, err := h.store.ArtifactForFile(ctx, head.ID)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, "could not resolve the Library row for this file"
	}
	if strings.TrimSpace(artifactId) == "" {
		// The file exists and its promotion has not landed. Versioning it
		// anyway would move the head under an index row that is about to be
		// written from the OLD bytes, so this push becomes a new file instead
		// -- a duplicate is recoverable and a head/index disagreement is not.
		return nil, nil, 0, ""
	}
	artifact, err := h.store.Artifact(ctx, artifactId)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, "could not read the Library row for this file"
	}
	if artifact == nil || artifact.Kind != libraryKindFile {
		return nil, nil, 0, ""
	}
	head.VersionNumber = headVersionNumber(head.VersionNumber)
	return artifact, head, 0, ""
}

func (h *ArtifactHandler) resolveVersionTarget(ctx context.Context, targetArtifactId string) (*LibraryArtifactRow, *LibraryFileRow, int, string) {
	targetArtifactId = strings.TrimSpace(targetArtifactId)
	if targetArtifactId == "" {
		return nil, nil, 0, ""
	}
	artifact, err := h.store.Artifact(ctx, targetArtifactId)
	if err != nil {
		h.logger.Error("resolve version target", "error", err, "artifactId", targetArtifactId)
		return nil, nil, http.StatusInternalServerError, "lookup failed"
	}
	if artifact == nil {
		return nil, nil, http.StatusNotFound, "not found"
	}
	if artifact.Kind != libraryKindFile {
		return nil, nil, http.StatusBadRequest, fmt.Sprintf(
			"%q is a %s, and only files carry upload versions. A document is versioned by editing it, "+
				"which appends a version through the document history rather than through this route.",
			targetArtifactId, artifact.Kind)
	}
	head, err := h.store.File(ctx, artifact.SourceConceptRef)
	if err != nil {
		h.logger.Error("resolve version target file", "error", err,
			"artifactId", targetArtifactId, "sourceConceptRef", artifact.SourceConceptRef)
		return nil, nil, http.StatusInternalServerError, "lookup failed"
	}
	if head == nil {
		return nil, nil, http.StatusNotFound, "not found"
	}
	// The BARE id, taken from the artifact's own canonical sourceConceptRef
	// rather than from the row's projected id: every write below targets a
	// file by the bare id createLibraryFile was given, and the ref is the
	// value the promotion wrote, so it is the one spelling that cannot
	// depend on how a read chose to render an id.
	head.ID = strings.TrimPrefix(artifact.SourceConceptRef, libraryFileConcept+":")
	if head.ID == "" {
		return nil, nil, http.StatusNotFound, "not found"
	}
	// ABSENT IS VERSION 1, normalised HERE rather than trusted from the read.
	// Every file uploaded before this epic carries no versionNumber at all,
	// and a supersede that took that as 0 would freeze the outgoing head as
	// "version 0" and move the head to 1 -- renumbering a file nobody
	// touched. The store normalises too; this is the point of USE, which is
	// where the rule has to hold for any store.
	head.VersionNumber = headVersionNumber(head.VersionNumber)
	return artifact, head, 0, ""
}

// libraryVersionObjectName composes the storage path for a version after the
// first: library/{userId}/{fileId}/{key}/{name}.
//
// The key is a FRESH short id per upload attempt, not the version number, and
// that is a correctness decision rather than a style one. Two supersedes of
// one file racing each other both read the same head, so both would compute
// the same version number -- and the second Upload would overwrite the
// first's bytes at a shared path, leaving a head row whose name, size and
// hash describe a different file's content. A per-attempt key makes every
// upload's bytes untouchable by every other, which is what the durability
// invariant actually needs; the version NUMBER is a fact about a row, and it
// lives on the row.
//
// (The race still costs one of the two uploads its place as the head -- the
// later head write wins and the loser's bytes are left unreferenced. That is
// a lost upload, which is visible, rather than a corrupted one, which is not.)
func libraryVersionObjectName(userId, fileId, name string) string {
	return fmt.Sprintf("library/%s/%s/%s/%s", userId, fileId, id.NewShortId(), name)
}

// restampAfterSupersede pushes the new version's facts onto the artifact
// index row. Best-effort and logged, never fatal: the bytes are stored and
// the head has moved by the time this runs, so a failed re-stamp costs a
// stale title in the list until the analysis pass's own re-stamp lands --
// which is a delay, not a loss.
func (h *ArtifactHandler) restampAfterSupersede(ctx context.Context, fileId, artifactId string) {
	if err := h.store.RestampFileArtifact(ctx, fileId); err != nil {
		h.logger.Warn("re-stamp artifact after supersede", "error", err,
			"fileId", fileId, "artifactId", artifactId)
	}
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

	// Opaque files short-circuit to ready -- EXCEPT when the hash is still
	// absent (a chunked upload, memql#4782 D10): those go to the pass even
	// though nothing is extractable, because the pass is what streams the
	// committed blob once and stamps sha256. With no analyzer wired at all,
	// ready-without-hash is the honest degraded state and the row says
	// nothing false -- absent means "not measured".
	if h.analyzer == nil || (p.Format == libraryFormatOther && p.Sha256 != "") {
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

	if artifact.Kind == libraryKindFile {
		h.serveFileBytes(w, r, artifact)
		return
	}
	// A version selector on a non-file kind is a 400 rather than a silent
	// fall-through to the rendered body: the caller asked for something this
	// artifact does not have, and serving the current text instead would
	// answer a different question than the one asked.
	if strings.TrimSpace(r.URL.Query().Get(libraryVersionQueryKey)) != "" {
		http.Error(w, fmt.Sprintf(
			"%q is a %s, and only files carry upload versions.", artifact.ID, artifact.Kind),
			http.StatusBadRequest)
		return
	}
	h.serveRenderedBody(w, r, artifact)
}

// requestedVersion reads the ?version={n} selector (design D8).
//
// Absent is the head, which is what every caller written before versions
// existed sends -- so ok is true with n == 0. A present-but-unreadable value
// is a 400 naming what it should be: coercing it to the head would serve
// somebody the newest bytes when they asked for an old one, under a 200.
func requestedVersion(r *http.Request) (int, bool, string) {
	raw := strings.TrimSpace(r.URL.Query().Get(libraryVersionQueryKey))
	if raw == "" {
		return 0, true, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false, fmt.Sprintf(
			"%s must be a version number of 1 or more; %q is not one. Omit it for the current version.",
			libraryVersionQueryKey, raw)
	}
	return n, true, ""
}

// serveFileBytes streams a file artifact's stored bytes. No redirect: the
// bytes come through the bff, after the backing row has been admitted for this
// caller in its own right.
func (h *ArtifactHandler) serveFileBytes(w http.ResponseWriter, r *http.Request, artifact *LibraryArtifactRow) {
	ctx := r.Context()

	wantVersion, ok, msg := requestedVersion(r)
	if !ok {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

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

	// An OLDER version: resolved through the owner-gated version read, so a
	// version of a file the caller may not see does not resolve -- and the
	// answer is the same 404 every other refusal on this route gives. The
	// head is served unchanged when the number IS the head's, so a client
	// that names the current version explicitly gets the same bytes, the
	// same headers and the same Range support as one that names none.
	if wantVersion > 0 && wantVersion != headVersionNumber(row.VersionNumber) {
		fileId := strings.TrimPrefix(artifact.SourceConceptRef, libraryFileConcept+":")
		version, err := h.store.FileVersion(ctx, fileId, wantVersion)
		if err != nil {
			h.logger.Error("library file version lookup failed", "error", err,
				"artifactId", artifact.ID, "fileId", fileId, "version", wantVersion)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if version == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// A version row IS a file row for the purposes of serving bytes: a
		// name, a type, a size and a path. Building one here rather than
		// giving the streamer a second shape keeps Range, Content-Length and
		// the buffered fallback on exactly one code path -- the alternative
		// is two download implementations that drift.
		//
		// Status is the one field with no counterpart: a superseded version
		// has no lifecycle, because nothing will ever analyse it again. It is
		// set to `ready` for the ONE thing the download path does with it --
		// a log field on an unavailable blob -- and is not a fact read off
		// the row. Nothing here gates on it.
		row = &LibraryFileRow{
			ID:       version.ID,
			Name:     version.Name,
			MimeType: version.MimeType,
			Size:     version.Size,
			BlobUrl:  version.BlobUrl,
			Status:   libraryStatusReady,
		}
	}
	// The constant-memory path (memql#4782, design C5): stream, with
	// Content-Length from the ROW, honouring a single-range Range. The
	// buffered fallback below survives for nodes whose downloader cannot
	// stream -- correct, just not constant-memory and Range-blind.
	if h.streamer != nil {
		h.streamFileBytes(w, r, row, sanitizeLibraryFileName(firstNonBlank(row.Name, artifact.Title)))
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
	if v := strings.TrimSpace(p.FolderId); v != "" {
		args["folderId"] = v
	}
	if v := strings.TrimSpace(p.UploadedFromWorkerId); v != "" {
		args["uploadedFromWorkerId"] = v
	}
	if v := strings.TrimSpace(p.UploadedFromWorkerName); v != "" {
		args["uploadedFromWorkerName"] = v
	}
	if v := strings.TrimSpace(p.UploadedFromPath); v != "" {
		args["uploadedFromPath"] = v
	}
	_, err := s.exec(ctx, "createLibraryFile", args)
	return err
}

// libraryWorkerConcept prefixes a canonical worker-registration node id, for
// tolerant matching in OwnedWorker: the OS sends the bare id (the client
// contract), while a materialized row's id is canonical.
const libraryWorkerConcept = "v1:worker:registration"

// StorageFootprint sums the caller's stored file bytes, their SUPERSEDED
// versions' bytes and their open sessions' declared bytes, through the three
// unbounded quota reads (memql#4782, epic memql#4806 design D9). All run
// under the caller's actor; all three are @unbounded in the DSL precisely so
// this sum cannot silently undercount past a page boundary and fail the
// quota open.
//
// The version half is not optional politeness: superseding destroys nothing,
// so those bytes are as real as a head file's, and a quota that ignored them
// would refuse a person using numbers they cannot see anywhere.
func (s *EngineLibraryStore) StorageFootprint(ctx context.Context) (int64, int64, int64, error) {
	sum := func(fn string) (int64, error) {
		res, err := s.exec(ctx, fn, nil)
		if err != nil {
			return 0, err
		}
		var total int64
		for _, r := range memql.MaterializeRows(res) {
			total += int64(rowInt(r, "size"))
		}
		return total, nil
	}
	fileBytes, err := sum("libraryFileSizesForOwner")
	if err != nil {
		return 0, 0, 0, err
	}
	versionBytes, err := sum("libraryFileVersionSizesForOwner")
	if err != nil {
		return 0, 0, 0, err
	}
	sessionBytes, err := sum("openUploadSessionsForOwner")
	if err != nil {
		return 0, 0, 0, err
	}
	return fileBytes, versionBytes, sessionBytes, nil
}

// OwnedWorker resolves a registration in the caller's own fleet through
// myWorkersWithStatus -- the SAME read the fleet router runs, chosen over a
// by-id query because it takes no argument at all: there is no id to widen
// the row set with, so the ownership check IS the read. The id match
// tolerates bare and canonical spellings on either side.
func (s *EngineLibraryStore) OwnedWorker(ctx context.Context, workerId string) (*LibraryWorkerRef, error) {
	workerId = strings.TrimSpace(workerId)
	if workerId == "" {
		return nil, fmt.Errorf("workerId is required")
	}
	res, err := s.exec(ctx, "myWorkersWithStatus", nil)
	if err != nil {
		return nil, err
	}
	want := strings.TrimPrefix(workerId, libraryWorkerConcept+":")
	for _, r := range memql.MaterializeRows(res) {
		got := strings.TrimPrefix(rowString(r, "id"), libraryWorkerConcept+":")
		if got == "" || got != want {
			continue
		}
		return &LibraryWorkerRef{
			ID: got,
			// The fleet's own label: the owner's displayName when set, else
			// the hostname the cockpit reported -- the same precedence the
			// fleet page renders.
			Name: firstNonBlank(rowString(r, "displayName"), rowString(r, "name")),
		}, nil
	}
	return nil, nil
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
	return libraryFileRowFrom(rows[0]), nil
}

// FileByUploadedFrom resolves the LIVE file a machine pushed from a path --
// the (machine, path) key the watched-folder backup versions on (epic
// memql#4783, design E).
//
// Under the caller's actor like every read here, so somebody else's file at
// the same path on the same machine id comes back as the nil a missing one
// does. Blank on either half answers nil WITHOUT running the read: the query
// guards both arguments, so a half-supplied key would return the caller's
// whole live file set and the first row of it is not the answer to anything.
func (s *EngineLibraryStore) FileByUploadedFrom(ctx context.Context, workerId, path string) (*LibraryFileRow, error) {
	workerId = strings.TrimSpace(workerId)
	path = strings.TrimSpace(path)
	if workerId == "" || path == "" {
		return nil, nil
	}
	res, err := s.exec(ctx, "libraryFileByUploadedFrom", map[string]any{
		"workerId": workerId,
		"path":     path,
	})
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	// A key that matched more than one live file is a state this design says
	// cannot happen -- a re-push versions the row it found -- so the NEWEST is
	// taken rather than an arbitrary one, and the read is sorted by the
	// engine's own default. Taking the first quietly would pick by wire order.
	return libraryFileRowFrom(rows[0]), nil
}

// libraryFileRowFrom materialises one v1:library:file row. Shared by every
// file read so a field added to the shape reaches all of them at once -- the
// version-target resolve and the (machine, path) resolve differ in how they
// FIND the row and not at all in what it is.
func libraryFileRowFrom(r map[string]any) *LibraryFileRow {
	return &LibraryFileRow{
		ID:       rowString(r, "id"),
		Name:     rowString(r, "name"),
		MimeType: rowString(r, "mimeType"),
		Size:     rowInt(r, "size"),
		BlobUrl:  rowString(r, "blobUrl"),
		Status:   rowString(r, "status"),
		Sha256:   rowString(r, "sha256"),
		Format:   rowString(r, "format"),
		Summary:  rowString(r, "summary"),
		// ABSENT IS 1 (epic memql#4806): every file uploaded before the
		// field existed has no member at all, and a supersede that read
		// that as 0 would freeze the outgoing head as "version 0" and
		// move the head to 1 -- renumbering a file that never moved.
		VersionNumber:          headVersionNumber(rowInt(r, "versionNumber")),
		VersionUploadedAt:      rowString(r, "versionUploadedAt"),
		UploadedFromWorkerId:   rowString(r, "uploadedFromWorkerId"),
		UploadedFromWorkerName: rowString(r, "uploadedFromWorkerName"),
		UploadedFromPath:       rowString(r, "uploadedFromPath"),
		LinkState:              rowString(r, "linkState"),
		LinkCheckedAt:          rowString(r, "linkCheckedAt"),
		// ABSENT IS NOT ARCHIVED: every file stored before memql#4340 has no
		// member at all, which is why the query spells the filter `!= true`.
		Archived: rowBool(r, "archived"),
	}
}

// SetFileLinkState runs setLibraryFileLinkState. The moment is stamped by the
// mutation rather than passed, so a caller cannot claim to have checked at a
// time it did not.
func (s *EngineLibraryStore) SetFileLinkState(ctx context.Context, fileId, state string) error {
	fileId = strings.TrimSpace(fileId)
	state = strings.TrimSpace(state)
	if fileId == "" || state == "" {
		return nil
	}
	_, err := s.exec(ctx, "setLibraryFileLinkState", map[string]any{
		"fileId":    fileId,
		"linkState": state,
	})
	return err
}

// headVersionNumber reads a file row's versionNumber, treating absent (and
// any nonsense below 1) as version 1.
//
// The rule has ONE implementation on each side of the wire and this is the
// server's; the Files app carries the other, and both cite the reason: a row
// promoted before versions existed carries no member, and every other reading
// of that absence renumbers files nobody touched.
func headVersionNumber(raw int) int {
	if raw < 1 {
		return 1
	}
	return raw
}

// FileVersion reads one superseded version through libraryFileVersionByNumber.
// Under the caller's actor, like every other read here: a version of somebody
// else's file comes back as the nil a missing one does.
func (s *EngineLibraryStore) FileVersion(ctx context.Context, fileId string, versionNumber int) (*LibraryFileVersionRow, error) {
	fileId = strings.TrimSpace(fileId)
	if fileId == "" || versionNumber < 1 {
		return nil, nil
	}
	res, err := s.exec(ctx, "libraryFileVersionByNumber", map[string]any{
		"fileId":        fileId,
		"versionNumber": versionNumber,
	})
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &LibraryFileVersionRow{
		ID:                     rowString(r, "id"),
		FileId:                 rowString(r, "fileId"),
		VersionNumber:          rowInt(r, "versionNumber"),
		Name:                   rowString(r, "name"),
		MimeType:               rowString(r, "mimeType"),
		Size:                   rowInt(r, "size"),
		Sha256:                 rowString(r, "sha256"),
		BlobUrl:                rowString(r, "blobUrl"),
		Format:                 rowString(r, "format"),
		Summary:                rowString(r, "summary"),
		UploadedFromWorkerId:   rowString(r, "uploadedFromWorkerId"),
		UploadedFromWorkerName: rowString(r, "uploadedFromWorkerName"),
		UploadedFromPath:       rowString(r, "uploadedFromPath"),
		UploadedAt:             rowString(r, "uploadedAt"),
		SupersededAt:           rowString(r, "supersededAt"),
	}, nil
}

// SupersedeFile runs the two @serverOnly version writes through the
// fileversion store, which owns the internal-origin stamp and the order.
func (s *EngineLibraryStore) SupersedeFile(ctx context.Context, snap LibraryVersionSnapshot, head LibraryHeadMove) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	return fileversion.NewStore(s.engine).Supersede(ctx,
		fileversion.Snapshot(snap), fileversion.Head(head))
}

// RestampFileArtifact re-versions the artifact index row from its backing
// file through the libraryRestampFileArtifact builtin -- the same seam
// AddArtifactLabel uses, for the same reason: the write lives beside the
// package's other artifact-index writers so the carry-forward has one author.
func (s *EngineLibraryStore) RestampFileArtifact(ctx context.Context, fileId string) error {
	fileId = strings.TrimSpace(fileId)
	if fileId == "" {
		return fmt.Errorf("fileId is required")
	}
	q, err := dslBuiltinCall("libraryRestampFileArtifact", map[string]any{"fileId": fileId})
	if err != nil {
		return err
	}
	_, err = s.engine.Execute(ctx, q)
	return err
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
//
// SATURATES out of range (memql#4779). The values are byte SIZES and version
// ORDINALS, and both readings need the order: the quota total sums sizes, and
// a negative one would credit a user back storage they are using.
// rowBool reads a boolean field off a materialized row.
//
// ABSENT READS FALSE, and for `archived` that is exactly right: every file
// stored before memql#4340 has no member at all and genuinely is not archived,
// which is the same asymmetry the query spells as `archived != true`. It is
// NOT right for a field whose default is true, and there is no such field
// here -- a reader that needed to tell absent from false would need a second
// return value this signature does not have.
func rowBool(row map[string]any, key string) bool {
	b, _ := row[key].(bool)
	return b
}

func rowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case float64:
		return num.ClampFloat64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return num.ClampInt64(n)
		}
	}
	return 0
}
