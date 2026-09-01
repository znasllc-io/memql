package server

// artifact_upload_sessions.go -- chunked resumable uploads (memql#4782,
// design C1-C4) and the streaming/Range half of the content route (C5).
//
// Files at or under the one-shot threshold keep POST /artifacts; above it,
// the client opens a SESSION and streams 16 MiB chunks to Azure staged
// blocks:
//
//	POST /artifacts/uploads                   open a session
//	GET  /artifacts/uploads/{id}              staged-chunk inventory (resume)
//	PUT  /artifacts/uploads/{id}/chunks/{n}   one raw chunk -> one staged block
//	POST /artifacts/uploads/{id}/complete     commit -> rows -> {artifactId, fileId}
//
// REPLICA-AGNOSTIC BY CONSTRUCTION. The session row lives in the graph and
// the staged blocks live with the blob, so any bff serves any chunk and
// completes any session; no handler holds more than the one chunk it is
// currently streaming. TestOneSessionCompletesThroughTwoIndependentHandlerInstances
// is the gate on that claim.
//
// NO SWEEPER. An abandoned session's staged blocks garbage-collect on
// Azure's ~7-day uncommitted-block clock; the row itself is inert. Abort is
// the client ceasing to send.
//
// EVERY ROUTE IS BEARER-AUTHENTICATED AND OWNER-GATED THE SAME WAY: the
// session resolves through uploadSessionById under the caller's own actor,
// so "not yours" and "not there" are one 404. The front door already routes
// all of it -- the paths live under the /artifacts prefix ArtifactPaths()
// declares -- and PUT joins POST/GET in app/transport_artifacts.go's
// registration.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server/fileversion"
	"github.com/znasllc-io/memql/component/server/uploadsession"
	"github.com/znasllc-io/memql/core/id"
)

const (
	// LibraryChunkSizeBytes is the chunk size -- a CONSTANT of the design
	// (D8), not a knob: 16 MiB balances per-chunk HTTP overhead against
	// retry cost, and resume does not need tuning to work. Recorded on
	// every session row so a session opened under one release completes
	// correctly under another even if this number ever changes.
	LibraryChunkSizeBytes int64 = 16 << 20

	// DefaultLibraryUserQuotaBytes caps ONE USER's total Library storage
	// when MEMQL_LIBRARY_USER_QUOTA_BYTES is unset: 100 GiB. A per-file cap
	// alone cannot stop one person filling the account (design D9).
	DefaultLibraryUserQuotaBytes int64 = 100 << 30

	// libraryUploadInitMaxBody bounds the session-init JSON body. Init
	// carries names and ids, never bytes; 1 MB is generous.
	libraryUploadInitMaxBody = 1 << 20

	// libraryChunkBodySlack is the allowance MaxBytesReader gets on top of
	// the chunk size, so "one byte over" is detected as an oversize chunk
	// (413 with the number named) rather than as a truncated read.
	libraryChunkBodySlack = 1
)

// LibraryUserQuotaBytesEnv is the operator knob for the per-user storage
// quota. Named as a constant and read through it for LibraryMaxUploadBytesEnv's
// reason: a plain grep must find the read.
const LibraryUserQuotaBytesEnv = "MEMQL_LIBRARY_USER_QUOTA_BYTES"

// LibraryUserQuotaBytes resolves the per-user quota from the environment,
// falling back to the default. Same posture as the upload cap: a set-but-
// unparseable or non-positive value falls back to the DEFAULT, never to
// "no limit".
func LibraryUserQuotaBytes() int64 {
	raw, ok := os.LookupEnv(LibraryUserQuotaBytesEnv)
	if !ok {
		return DefaultLibraryUserQuotaBytes
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return DefaultLibraryUserQuotaBytes
	}
	return n
}

// UploadSessionStore is the graph seam for session rows -- implemented by
// component/server/uploadsession, whose package comment explains why the
// two writes stamp internal origin and the read deliberately does not.
type UploadSessionStore interface {
	Create(ctx context.Context, p uploadsession.CreateParams) error
	ByID(ctx context.Context, uploadId string) (*uploadsession.Row, error)
	Complete(ctx context.Context, uploadId string) error
}

// BlockStore is the staged-block seam: stage one block, inventory the
// uncommitted set, commit the list. *azureblob.AzureBlobUploader implements
// it; the fakes in the tests model its one load-bearing property -- state
// lives with the blob, not with any handler instance.
type BlockStore interface {
	StageBlock(ctx context.Context, container, objectName, blockID string, chunk []byte) error
	CommitBlockList(ctx context.Context, container, objectName string, blockIDs []string, contentType string) error
	UncommittedBlocks(ctx context.Context, container, objectName string) (map[string]int64, error)
}

// StreamDownloader is the streaming seam for the content route (design
// D13/C5): a full-body stream, and a single byte range. When the wired
// downloader does not implement it, the route falls back to the buffered
// path -- correct, just not constant-memory.
type StreamDownloader interface {
	DownloadStreamURL(ctx context.Context, blobURL string) (io.ReadCloser, error)
	DownloadRangeURL(ctx context.Context, blobURL string, offset, count int64) (io.ReadCloser, error)
}

// ---------------------------------------------------------------------------
// block ids
// ---------------------------------------------------------------------------

// uploadBlockID encodes chunk index n as Azure block id: base64 over a
// FIXED-WIDTH decimal. Fixed width is Azure's requirement (ids in one block
// list must share a length) and what makes the encoding order-stable.
func uploadBlockID(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%08d", n)))
}

// uploadBlockN decodes a block id back to its chunk index. Foreign ids --
// anything this handler family did not mint -- report ok=false and are
// treated by complete as a size mismatch, never silently committed.
func uploadBlockN(blockID string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(blockID)
	if err != nil || len(raw) != 8 {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// chunkCountFor is ceil(size / chunkSize) -- the N the chunk route bounds
// against and the commit expects exactly.
func chunkCountFor(size, chunkSize int64) int {
	if size <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((size + chunkSize - 1) / chunkSize)
}

// ---------------------------------------------------------------------------
// paths
// ---------------------------------------------------------------------------

// uploadSegments returns the path segments after ".../artifacts/uploads",
// tolerating a base prefix, or ok=false when the path is not under it.
func uploadSegments(path string) ([]string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "artifacts" && i+1 < len(parts) && parts[i+1] == "uploads" {
			return parts[i+2:], true
		}
	}
	return nil, false
}

func isUploadInitPath(path string) bool {
	rest, ok := uploadSegments(path)
	return ok && len(rest) == 0
}

func parseUploadSessionPath(path string) (string, bool) {
	rest, ok := uploadSegments(path)
	if !ok || len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		return "", false
	}
	return rest[0], true
}

func parseUploadChunkPath(path string) (uploadId string, n int, ok bool) {
	rest, restOK := uploadSegments(path)
	if !restOK || len(rest) != 3 || rest[1] != "chunks" || strings.TrimSpace(rest[0]) == "" {
		return "", 0, false
	}
	parsed, err := strconv.Atoi(rest[2])
	if err != nil {
		return "", 0, false
	}
	return rest[0], parsed, true
}

func parseUploadCompletePath(path string) (string, bool) {
	rest, ok := uploadSegments(path)
	if !ok || len(rest) != 2 || rest[1] != "complete" || strings.TrimSpace(rest[0]) == "" {
		return "", false
	}
	return rest[0], true
}

// ---------------------------------------------------------------------------
// shared checks
// ---------------------------------------------------------------------------

// requireUser resolves the authenticated user id, writing the 401 itself
// when there is none. Shared by every session route: the actor must resolve
// to a USER, for the one-shot route's reason -- ownership stamps from it.
func (h *ArtifactHandler) requireUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	access, _ := auth.AccessFromContext(r.Context())
	userId := ""
	if access != nil {
		userId = strings.TrimSpace(access.UserId)
	}
	if strings.TrimSpace(auth.ActorFromContext(r.Context())) == "" || userId == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return userId, true
}

// verifyProvenance applies design D5's rule for a machine-provenance claim:
// no claim verifies trivially; a name or path without the id is half a
// claim and refused; a claimed id must resolve in the CALLER's own fleet,
// and the fleet's own label for the machine wins over the claimed name.
// Returns the resolved name and (0, "") on success, or an HTTP status and
// the sentence to send.
func (h *ArtifactHandler) verifyProvenance(ctx context.Context, workerId, workerName, fromPath string) (string, int, string) {
	if (fromPath != "" || workerName != "") && workerId == "" {
		return "", http.StatusBadRequest, fmt.Sprintf(
			"%s and %s need %s: machine provenance is anchored on the registration id",
			libraryFormPathKey, libraryFormWorkerNameKey, libraryFormWorkerIdKey)
	}
	if workerId == "" {
		return workerName, 0, ""
	}
	ref, err := h.store.OwnedWorker(ctx, workerId)
	if err != nil {
		h.logger.Error("verify upload provenance", "error", err, "workerId", workerId)
		return "", http.StatusInternalServerError, "provenance verification failed"
	}
	if ref == nil {
		return "", http.StatusForbidden, fmt.Sprintf(
			"the worker registration %q is not one of your machines, so the upload's provenance claim was refused",
			workerId)
	}
	if resolved := strings.TrimSpace(ref.Name); resolved != "" {
		return resolved, 0, ""
	}
	return workerName, 0, ""
}

// checkQuota enforces MEMQL_LIBRARY_USER_QUOTA_BYTES (design C4, extended by
// epic memql#4806 design D9): the sum of the owner's stored file sizes
// (archived included -- retention is real), the bytes of every SUPERSEDED
// version they hold, and their open sessions' declared sizes, plus addBytes,
// must stay at or under the quota. The refusal NAMES BOTH NUMBERS, because a
// caller who cannot see them can only bisect for them -- and it names all
// three things that count, because a person looking at their Library sees
// one row per file and would otherwise have no way to reconcile the total.
// Returns (0, "") when admitted.
func (h *ArtifactHandler) checkQuota(ctx context.Context, addBytes int64) (int, string) {
	if h.userQuotaBytes <= 0 {
		return 0, ""
	}
	fileBytes, versionBytes, sessionBytes, err := h.store.StorageFootprint(ctx)
	if err != nil {
		h.logger.Error("resolve library storage footprint", "error", err)
		return http.StatusInternalServerError, "storage quota check failed"
	}
	total := fileBytes + versionBytes + sessionBytes + addBytes
	if total <= h.userQuotaBytes {
		return 0, ""
	}
	return http.StatusInsufficientStorage, fmt.Sprintf(
		"storage quota exceeded: this upload would take your Library to %d bytes, over the quota of %d bytes (%s). "+
			"Stored files -- archived ones included -- every earlier version of them, and the declared sizes of "+
			"your open upload sessions all count.",
		total, h.userQuotaBytes, LibraryUserQuotaBytesEnv)
}

// sessionsConfigured answers 501 when the chunked path is not wired --
// distinct from 404 (which would read as "no such session") and honest
// about whose condition it is.
func (h *ArtifactHandler) sessionsConfigured(w http.ResponseWriter) bool {
	if h.sessions == nil || h.blocks == nil || h.bucket == "" {
		http.Error(w, "chunked uploads are not available on this node (upload sessions or block storage not configured)",
			http.StatusNotImplemented)
		return false
	}
	return true
}

// ownedOpenSession resolves the session under the caller's actor and writes
// the refusal itself when it is absent (404 -- not yours and not there are
// one answer) or not open (409). requireOpen=false skips the status check.
func (h *ArtifactHandler) ownedOpenSession(w http.ResponseWriter, r *http.Request, uploadId string, requireOpen bool) *uploadsession.Row {
	row, err := h.sessions.ByID(r.Context(), uploadId)
	if err != nil {
		h.logger.Error("resolve upload session", "error", err, "uploadId", uploadId)
		http.Error(w, "session lookup failed", http.StatusInternalServerError)
		return nil
	}
	if row == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	if requireOpen && row.Status != "open" {
		http.Error(w, fmt.Sprintf("upload session is %s, not open", row.Status), http.StatusConflict)
		return nil
	}
	return row
}

// ---------------------------------------------------------------------------
// POST /artifacts/uploads
// ---------------------------------------------------------------------------

type uploadInitRequest struct {
	Name                   string   `json:"name"`
	Size                   int64    `json:"size"`
	MimeType               string   `json:"mimeType"`
	FolderId               string   `json:"folderId"`
	Labels                 []string `json:"labels"`
	UploadedFromWorkerId   string   `json:"uploadedFromWorkerId"`
	UploadedFromWorkerName string   `json:"uploadedFromWorkerName"`
	UploadedFromPath       string   `json:"uploadedFromPath"`
	// TargetArtifactId makes this session a new VERSION of an existing
	// artifact rather than a fresh upload (epic memql#4806, design D7). The
	// three target gates run HERE, at init -- fail-fast, before anybody
	// streams gigabytes at a target they may not write to.
	TargetArtifactId string `json:"targetArtifactId"`
}

type uploadInitResponse struct {
	UploadId  string `json:"uploadId"`
	ChunkSize int64  `json:"chunkSize"`
}

func (h *ArtifactHandler) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	userId, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if !h.sessionsConfigured(w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, libraryUploadInitMaxBody)
	var req uploadInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid session request body", http.StatusBadRequest)
		return
	}

	name := sanitizeLibraryFileName(req.Name)
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Size <= 0 {
		http.Error(w, "size must be a positive byte count -- it is what the commit is verified against", http.StatusBadRequest)
		return
	}
	if req.Size > h.maxUploadBytes {
		h.tooLarge(w)
		return
	}

	ctx := r.Context()

	// The same gates the one-shot route runs, in the same order and for the
	// same reason -- everything that can refuse this upload refuses it before
	// a byte moves: the version target, then provenance, then quota.
	targetArtifact, head, status, msg := h.resolveVersionTarget(ctx, strings.TrimSpace(req.TargetArtifactId))
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	workerName, status, msg := h.verifyProvenance(ctx,
		strings.TrimSpace(req.UploadedFromWorkerId),
		strings.TrimSpace(req.UploadedFromWorkerName),
		strings.TrimSpace(req.UploadedFromPath))
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	// --- the KEYED version target (epic memql#4783, design E) ---
	//
	// This route needs it MORE than the one-shot route does, not less: the
	// watched folder in the epic's own scenario holds client video, every file
	// in it is past the one-shot threshold, and so every re-push a watcher
	// makes arrives here. A keyed resolve on the one-shot path alone would
	// version small files and duplicate large ones -- working in the demo and
	// broken for the feature it was built for.
	//
	// Same precedence as there: only when the caller named nothing, and after
	// verifyProvenance has checked the machine half against their own fleet.
	if targetArtifact == nil {
		keyedArtifact, keyedHead, keyedStatus, keyedMsg := h.resolveKeyedVersionTarget(ctx,
			strings.TrimSpace(req.UploadedFromWorkerId), strings.TrimSpace(req.UploadedFromPath))
		if keyedStatus != 0 {
			http.Error(w, keyedMsg, keyedStatus)
			return
		}
		if keyedHead != nil {
			targetArtifact, head = keyedArtifact, keyedHead
		}
	}
	if status, msg := h.checkQuota(ctx, req.Size); status != 0 {
		http.Error(w, msg, status)
		return
	}

	fileId := id.NewShortId()
	uploadId := id.NewShortId()
	// The storage path the file concept documents, verbatim -- composed
	// here, server-side, from the VERIFIED actor and the engine-minted
	// fileId. This composition is the whole reason createUploadSession is
	// @serverOnly: a caller-authored path could escape this prefix.
	blobPath := fmt.Sprintf("library/%s/%s/%s", userId, fileId, name)
	targetArtifactId := ""
	if head != nil {
		// A version keeps the file's identity, so the session carries the
		// EXISTING file id and stages into a path no version has used. Both
		// are composed here for the same reason a fresh upload's is: the
		// client never authors a storage path.
		fileId = head.ID
		blobPath = libraryVersionObjectName(userId, fileId, name)
		targetArtifactId = targetArtifact.ID
	}

	if err := h.sessions.Create(ctx, uploadsession.CreateParams{
		UploadId: uploadId, Name: name, Size: req.Size,
		MimeType: strings.TrimSpace(req.MimeType), FolderId: strings.TrimSpace(req.FolderId),
		Labels:                 req.Labels,
		UploadedFromWorkerId:   strings.TrimSpace(req.UploadedFromWorkerId),
		UploadedFromWorkerName: workerName,
		UploadedFromPath:       strings.TrimSpace(req.UploadedFromPath),
		BlobPath:               blobPath, FileId: fileId, ChunkSize: h.chunkSizeBytes,
		TargetArtifactId: targetArtifactId,
	}); err != nil {
		h.logger.Error("create upload session", "error", err)
		http.Error(w, fmt.Sprintf("failed to open upload session: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(uploadInitResponse{UploadId: uploadId, ChunkSize: h.chunkSizeBytes})
}

// ---------------------------------------------------------------------------
// PUT /artifacts/uploads/{id}/chunks/{n}
// ---------------------------------------------------------------------------

func (h *ArtifactHandler) handleUploadChunk(w http.ResponseWriter, r *http.Request, uploadId string, n int) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	if !h.sessionsConfigured(w) {
		return
	}
	session := h.ownedOpenSession(w, r, uploadId, true)
	if session == nil {
		return
	}

	total := chunkCountFor(session.Size, session.ChunkSize)
	if n < 1 || n > total {
		http.Error(w, fmt.Sprintf("chunk index %d is out of range: this session has chunks 1..%d "+
			"(size %d at chunk size %d)", n, total, session.Size, session.ChunkSize), http.StatusBadRequest)
		return
	}

	// One chunk is the most this handler ever holds -- the body is bounded
	// BEFORE it is read, and staging streams it straight out again.
	r.Body = http.MaxBytesReader(w, r.Body, session.ChunkSize+libraryChunkBodySlack)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		if isRequestTooLarge(err) {
			http.Error(w, fmt.Sprintf("chunk body exceeds the chunk size of %d bytes", session.ChunkSize),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read chunk body", http.StatusInternalServerError)
		return
	}
	if int64(len(data)) > session.ChunkSize {
		http.Error(w, fmt.Sprintf("chunk body exceeds the chunk size of %d bytes", session.ChunkSize),
			http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		http.Error(w, "chunk body is empty", http.StatusBadRequest)
		return
	}

	if err := h.blocks.StageBlock(r.Context(), h.bucket, session.BlobPath, uploadBlockID(n), data); err != nil {
		h.logger.Error("stage upload chunk", "error", err, "uploadId", uploadId, "n", n)
		http.Error(w, "block storage refused the chunk", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// GET /artifacts/uploads/{id}
// ---------------------------------------------------------------------------

type uploadStagedChunk struct {
	N    int   `json:"n"`
	Size int64 `json:"size"`
}

type uploadInventoryResponse struct {
	UploadId  string              `json:"uploadId"`
	Status    string              `json:"status"`
	Size      int64               `json:"size"`
	ChunkSize int64               `json:"chunkSize"`
	Staged    []uploadStagedChunk `json:"staged"`
}

func (h *ArtifactHandler) handleUploadInventory(w http.ResponseWriter, r *http.Request, uploadId string) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	if !h.sessionsConfigured(w) {
		return
	}
	// Status is reported, not required: resume needs to see "completed" to
	// know the file landed (the kill-after-complete case).
	session := h.ownedOpenSession(w, r, uploadId, false)
	if session == nil {
		return
	}

	staged := []uploadStagedChunk{}
	if session.Status == "open" {
		blocks, err := h.blocks.UncommittedBlocks(r.Context(), h.bucket, session.BlobPath)
		if err != nil {
			h.logger.Error("inventory staged blocks", "error", err, "uploadId", uploadId)
			http.Error(w, "block storage inventory failed", http.StatusBadGateway)
			return
		}
		for blockID, size := range blocks {
			if n, ok := uploadBlockN(blockID); ok {
				staged = append(staged, uploadStagedChunk{N: n, Size: size})
			}
		}
		sort.Slice(staged, func(i, j int) bool { return staged[i].N < staged[j].N })
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(uploadInventoryResponse{
		UploadId: session.ID, Status: session.Status,
		Size: session.Size, ChunkSize: session.ChunkSize, Staged: staged,
	})
}

// ---------------------------------------------------------------------------
// POST /artifacts/uploads/{id}/complete
// ---------------------------------------------------------------------------

func (h *ArtifactHandler) handleUploadComplete(w http.ResponseWriter, r *http.Request, uploadId string) {
	userId, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if !h.sessionsConfigured(w) {
		return
	}
	session := h.ownedOpenSession(w, r, uploadId, false)
	if session == nil {
		return
	}
	ctx := r.Context()

	// A COMPLETED session answers its ids instead of re-running the commit:
	// the client that died between commit and response re-completes and
	// gets the same answer. The artifact id derives deterministically from
	// the source ref, which is what makes this resolvable at all -- and for
	// a version session the target was recorded at init, so the answer needs
	// no derivation. The version NUMBER is read back off the head rather
	// than remembered, because by now it is a fact about a row.
	if session.Status != "open" {
		artifactId := session.TargetArtifactId
		if artifactId == "" {
			resolved, err := h.store.ArtifactForFile(ctx, session.FileId)
			if err != nil {
				h.logger.Warn("resolve artifact for completed session", "error", err, "uploadId", uploadId)
			}
			artifactId = resolved
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ArtifactUploadResponse{
			ArtifactId:    artifactId,
			FileId:        session.FileId,
			VersionNumber: h.completedSessionVersion(ctx, session),
		})
		return
	}

	// The declared size is a CLAIM (design D8/D9), and this is where it is
	// checked against both caps -- a session minted under an older, larger
	// cap must not commit past the current one.
	if session.Size > h.maxUploadBytes {
		h.tooLarge(w)
		return
	}
	// The session's own declared size is already in the footprint's open-
	// session half, and after commit it moves to the file half -- net zero
	// -- so the check is the footprint against the quota, with nothing
	// added.
	if status, msg := h.checkQuota(ctx, 0); status != 0 {
		http.Error(w, msg, status)
		return
	}

	// --- verify the staged set: exactly chunks 1..N, summing to size ---
	blocks, err := h.blocks.UncommittedBlocks(ctx, h.bucket, session.BlobPath)
	if err != nil {
		h.logger.Error("inventory staged blocks for complete", "error", err, "uploadId", uploadId)
		http.Error(w, "block storage inventory failed", http.StatusBadGateway)
		return
	}
	total := chunkCountFor(session.Size, session.ChunkSize)
	var stagedBytes int64
	missing := 0
	byN := map[int]int64{}
	for blockID, size := range blocks {
		if n, ok := uploadBlockN(blockID); ok {
			byN[n] = size
		}
	}
	for n := 1; n <= total; n++ {
		size, ok := byN[n]
		if !ok {
			missing++
			continue
		}
		stagedBytes += size
	}
	if missing > 0 || stagedBytes != session.Size || len(byN) != total {
		// The session STAYS OPEN: the client reads the inventory, uploads
		// what is missing, and completes again. The sentence names the
		// numbers a client can act on.
		http.Error(w, fmt.Sprintf(
			"upload is not complete: %d bytes are staged of the declared %d, with %d of %d chunks missing -- "+
				"read GET /artifacts/uploads/%s for the staged inventory, upload what is absent, and complete again",
			stagedBytes, session.Size, missing, total, session.ID), http.StatusConflict)
		return
	}

	// --- commit, in ascending chunk order ---
	blockIDs := make([]string, 0, total)
	for n := 1; n <= total; n++ {
		blockIDs = append(blockIDs, uploadBlockID(n))
	}
	if err := h.blocks.CommitBlockList(ctx, h.bucket, session.BlobPath, blockIDs, session.MimeType); err != nil {
		h.logger.Error("commit upload block list", "error", err, "uploadId", uploadId)
		http.Error(w, "block storage refused the commit", http.StatusBadGateway)
		return
	}

	// --- the row. sha256 is ABSENT: the analysis pass stamps it (D10). ---
	params := LibraryFileCreateParams{
		FileId:                 session.FileId,
		Name:                   session.Name,
		MimeType:               session.MimeType,
		Size:                   int(session.Size),
		BlobUrl:                session.BlobPath,
		Source:                 "uploaded",
		Format:                 LibraryFormatForMIME(session.MimeType),
		FolderId:               session.FolderId,
		UploadedFromWorkerId:   session.UploadedFromWorkerId,
		UploadedFromWorkerName: session.UploadedFromWorkerName,
		UploadedFromPath:       session.UploadedFromPath,
	}

	// --- a VERSION session supersedes instead of creating (epic memql#4806) ---
	//
	// The target was gated at init, but the head is re-read HERE, under the
	// caller's actor, and for two reasons: its version number and its byte
	// facts are what the snapshot freezes, and a session can sit open for
	// days -- the head it started against may not be the head any more, and
	// freezing init-time facts would write a version row describing bytes
	// that were already superseded by somebody else.
	// The link, stamped on both branches below and from the same evidence the
	// one-shot route uses (epic memql#4783): a session that named a
	// (machine, path) delivered bytes that equalled the origin at the moment
	// they finished arriving. Best effort -- the upload is done, and a file
	// that is stored and unlabelled beats a 500 that says it is not.
	stampLink := func() {
		if session.UploadedFromWorkerId == "" || session.UploadedFromPath == "" {
			return
		}
		if err := h.store.SetFileLinkState(ctx, session.FileId, libraryLinkStateSynced); err != nil {
			h.logger.Warn("stamp library file link state", "error", err, "fileId", session.FileId)
		}
	}

	if session.TargetArtifactId != "" {
		artifactId, version, ok := h.completeVersionSession(w, r, session)
		if !ok {
			return
		}
		stampLink()
		if err := h.sessions.Complete(ctx, session.ID); err != nil {
			h.logger.Error("mark upload session completed", "error", err, "uploadId", uploadId)
		}
		h.restampAfterSupersede(ctx, session.FileId, artifactId)
		for _, label := range session.Labels {
			if strings.TrimSpace(label) == "" {
				continue
			}
			if err := h.store.AddArtifactLabel(ctx, artifactId, strings.TrimSpace(label)); err != nil {
				h.logger.Warn("apply session label", "error", err, "artifactId", artifactId, "label", label)
			}
		}
		h.startAnalysis(userId, artifactId, params, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ArtifactUploadResponse{
			ArtifactId: artifactId, FileId: session.FileId, VersionNumber: version,
		})
		return
	}

	if err := h.store.CreateFile(ctx, params); err != nil {
		h.logger.Error("create library file row for session", "error", err, "uploadId", uploadId)
		http.Error(w, fmt.Sprintf("failed to create library file: %v", err), http.StatusInternalServerError)
		return
	}
	stampLink()

	artifactId := h.waitForPromotion(ctx, session.FileId)
	if artifactId == "" {
		h.logger.Warn("library artifact promotion not visible yet for session; returning the file id alone",
			"fileId", session.FileId, "waited", h.promotionWait, "labelsDropped", len(session.Labels))
	} else {
		for _, label := range session.Labels {
			if strings.TrimSpace(label) == "" {
				continue
			}
			if err := h.store.AddArtifactLabel(ctx, artifactId, strings.TrimSpace(label)); err != nil {
				h.logger.Warn("apply session label", "error", err, "artifactId", artifactId, "label", label)
			}
		}
	}

	if err := h.sessions.Complete(ctx, session.ID); err != nil {
		// The file row exists and the bytes are committed; a failed status
		// flip costs idempotency, not data. Logged loudly, not fatal.
		h.logger.Error("mark upload session completed", "error", err, "uploadId", uploadId)
	}

	// Data is nil ON PURPOSE: no handler on this path ever held the file.
	// The analysis pass streams the committed blob -- for the hash always,
	// and for extraction when the type is readable.
	h.startAnalysis(userId, artifactId, params, nil)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ArtifactUploadResponse{
		ArtifactId: artifactId, FileId: session.FileId, VersionNumber: 1,
	})
}

// completeVersionSession re-gates the target and runs the supersede for a
// chunked new version. It writes its own refusal and answers ok=false; on
// success it returns the artifact id and the version number that landed.
//
// RE-GATING AT COMPLETE IS NOT PARANOIA. The bytes are already committed, so
// this cannot refuse them back out of storage -- but the alternative is
// writing a head update against an artifact the caller has since lost access
// to, or against a file that has been archived and re-owned. The read costs
// one query on a path that just moved gigabytes.
func (h *ArtifactHandler) completeVersionSession(w http.ResponseWriter, r *http.Request, session *uploadsession.Row) (string, int, bool) {
	ctx := r.Context()
	targetArtifact, head, status, msg := h.resolveVersionTarget(ctx, session.TargetArtifactId)
	if status != 0 {
		http.Error(w, msg, status)
		return "", 0, false
	}
	if head.ID != session.FileId {
		// The artifact's backing file changed identity under the session.
		// Nothing in this epic can do that, which is exactly why it is worth
		// refusing loudly rather than superseding whatever is there now.
		h.logger.Error("upload session target no longer backs the file it was opened against",
			"uploadId", session.ID, "sessionFileId", session.FileId, "headFileId", head.ID)
		http.Error(w, "this upload session's target has changed since it was opened, so nothing was written",
			http.StatusConflict)
		return "", 0, false
	}
	version := head.VersionNumber + 1
	if err := h.store.SupersedeFile(ctx,
		LibraryVersionSnapshot{
			VersionId:              fileversion.DerivedVersionId(head.ID, head.VersionNumber),
			FileId:                 head.ID,
			VersionNumber:          head.VersionNumber,
			Name:                   head.Name,
			MimeType:               head.MimeType,
			Size:                   int64(head.Size),
			Sha256:                 head.Sha256,
			BlobUrl:                head.BlobUrl,
			Format:                 head.Format,
			Summary:                head.Summary,
			UploadedFromWorkerId:   head.UploadedFromWorkerId,
			UploadedFromWorkerName: head.UploadedFromWorkerName,
			UploadedFromPath:       head.UploadedFromPath,
			UploadedAt:             head.VersionUploadedAt,
		},
		LibraryHeadMove{
			FileId:        head.ID,
			VersionNumber: version,
			Name:          session.Name,
			MimeType:      session.MimeType,
			Size:          session.Size,
			// Sha256 is blank ON PURPOSE and written as such: no handler on
			// this path ever held the file, and the analysis pass stamps
			// what it measures. Inheriting the previous version's hash would
			// be a false integrity claim rather than a missing one (D5).
			BlobUrl:                session.BlobPath,
			Format:                 LibraryFormatForMIME(session.MimeType),
			UploadedFromWorkerId:   session.UploadedFromWorkerId,
			UploadedFromWorkerName: session.UploadedFromWorkerName,
			UploadedFromPath:       session.UploadedFromPath,
		}); err != nil {
		h.logger.Error("supersede library file for session", "error", err,
			"uploadId", session.ID, "fileId", head.ID, "version", version)
		http.Error(w, fmt.Sprintf("failed to record the new version: %v", err), http.StatusInternalServerError)
		return "", 0, false
	}
	return targetArtifact.ID, version, true
}

// completedSessionVersion answers the version a finished session landed as,
// read off the head now rather than remembered.
//
// Best-effort by design: this only ever runs on the idempotent re-complete
// path, where the ids are the answer the caller came for and the version is
// a courtesy. Zero when the head cannot be read, which a client reads as
// "not stated" rather than as version zero.
func (h *ArtifactHandler) completedSessionVersion(ctx context.Context, session *uploadsession.Row) int {
	row, err := h.store.File(ctx, LibraryFileConceptRef(session.FileId))
	if err != nil || row == nil {
		return 0
	}
	return headVersionNumber(row.VersionNumber)
}

// ---------------------------------------------------------------------------
// streaming + Range (the content route's C5 half)
// ---------------------------------------------------------------------------

// byteRange is one satisfiable request range, inclusive on both ends.
type byteRange struct {
	start, end int64
}

// parseRangeHeader interprets a Range header against a known size, per RFC
// 9110's single-range subset (design C5). Returns:
//
//	(nil, false)  -- no header, or one this server IGNORES (malformed, a
//	                 unit that is not bytes, or multiple ranges): serve 200.
//	(nil, true)   -- a well-formed bytes range that is UNSATISFIABLE: 416.
//	(&r,  true)   -- a satisfiable range, clamped to the size: 206.
func parseRangeHeader(header string, size int64) (*byteRange, bool) {
	header = strings.TrimSpace(header)
	if header == "" || !strings.HasPrefix(header, "bytes=") {
		return nil, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		// Multi-range: within our single-range subset the honest answer is
		// the full body, which RFC 9110 permits (Range is advisory).
		return nil, false
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return nil, false
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	switch {
	case first == "" && last == "":
		return nil, false
	case first == "":
		// Suffix form: the final k bytes.
		k, err := strconv.ParseInt(last, 10, 64)
		if err != nil || k <= 0 {
			return nil, false
		}
		if size == 0 {
			return nil, true
		}
		if k > size {
			k = size
		}
		return &byteRange{start: size - k, end: size - 1}, true
	default:
		start, err := strconv.ParseInt(first, 10, 64)
		if err != nil || start < 0 {
			return nil, false
		}
		end := size - 1
		if last != "" {
			parsed, err := strconv.ParseInt(last, 10, 64)
			if err != nil || parsed < start {
				return nil, false
			}
			if parsed < end {
				end = parsed
			}
		}
		if start >= size {
			return nil, true // well-formed, unsatisfiable
		}
		return &byteRange{start: start, end: end}, true
	}
}

// streamFileBytes is serveFileBytes' constant-memory half: full-body 200s
// and single-range 206s through the StreamDownloader, with Content-Length
// taken from the ROW -- the stream is copied, never buffered or measured.
func (h *ArtifactHandler) streamFileBytes(w http.ResponseWriter, r *http.Request, row *LibraryFileRow, displayName string) {
	size := int64(row.Size)
	mimeType := strings.TrimSpace(row.MimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	rng, wellFormed := parseRangeHeader(r.Header.Get("Range"), size)
	if wellFormed && rng == nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	var (
		rc  io.ReadCloser
		err error
	)
	if rng != nil {
		rc, err = h.streamer.DownloadRangeURL(r.Context(), row.BlobUrl, rng.start, rng.end-rng.start+1)
	} else {
		rc, err = h.streamer.DownloadStreamURL(r.Context(), row.BlobUrl)
	}
	if err != nil {
		h.logger.Warn("library file stream unavailable", "error", err, "fileId", row.ID, "status", row.Status)
		http.Error(w, "file not available", http.StatusNotFound)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", displayName))
	if rng != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(rng.end-rng.start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}
	if _, err := io.Copy(w, rc); err != nil {
		// The headers are gone; all that is left is the log line.
		h.logger.Warn("library file stream interrupted", "error", err, "fileId", row.ID)
	}
}
