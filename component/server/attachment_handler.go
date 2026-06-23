package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

const (
	maxAttachmentBytes    = 25 * 1024 * 1024 // 25 MB
	maxMultipartMemory    = 32 * 1024 * 1024 // 32 MB
	attachmentFormFileKey = "file"

	attachmentStatusProcessing = "processing"
	attachmentStatusReady      = "ready"
	attachmentStatusError      = "error"
)

// allowedAttachmentMIMETypes is the set of MIME types accepted for upload.
// Expanded for v1 file-drop pipeline testing: spreadsheets (CSV / TSV /
// XLSX) and structured data (JSON / HTML) join PDF / DOCX / text /
// markdown / image. The analyzer routes per format via
// component/fileprocessor; new formats fall back to the text
// extractor's best-effort decode.
var allowedAttachmentMIMETypes = map[string]bool{
	// Documents
	"application/pdf": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	// Spreadsheets / structured data
	"text/csv":                  true,
	"application/csv":           true,
	"text/tab-separated-values": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true,
	"application/vnd.ms-excel": true,
	"application/json":         true,
	// Text formats
	"text/plain":    true,
	"text/markdown": true,
	"text/html":     true,
	// Images
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// FileUploader stores raw file bytes in a durable object store.
type FileUploader interface {
	// Upload persists data and returns a storage URI.
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (blobUrl string, err error)
}

// TextExtractor extracts plain text from file bytes.
type TextExtractor interface {
	// Extract returns plain text for the given MIME type and raw bytes.
	Extract(ctx context.Context, mimeType string, data []byte) (string, error)
}

// AttachmentStore creates and stores attachment memory nodes.
type AttachmentStore interface {
	// CreateAttachment creates a new v1:common:attachment node and returns the node JSON.
	CreateAttachment(ctx context.Context, params AttachmentCreateParams) (json.RawMessage, error)
	// CallerOwnsSpace reports whether the authenticated caller (resolved
	// server-side via the engine envelope) owns the given space. Used as
	// an explicit pre-upload ownership gate so cross-tenant uploads are
	// rejected before any blob bytes are written. The DSL mutation
	// re-enforces ownership; this is defense in depth.
	CallerOwnsSpace(ctx context.Context, partitionId string) (bool, error)
	// GetAttachment reads one v1:common:attachment row by id within a space
	// (queryAttachmentById). Returns nil (no error) when not found. Backs the
	// download endpoint (memql#804); callers gate on CallerOwnsSpace first.
	GetAttachment(ctx context.Context, attachmentId, partitionId string) (*AttachmentRow, error)
}

// AttachmentRow is the projected v1:common:attachment row the download path
// needs: the storage URL plus the metadata to serve the bytes back.
type AttachmentRow struct {
	ID       string
	FileName string
	MimeType string
	BlobUrl  string
	PartitionId  string
	Status   string
}

// FileDownloader reads stored file bytes back from object storage given the
// stored blob URL. Provider-agnostic: the handler passes the URL string, the
// backend (azureblob) parses + fetches. (memql#804)
type FileDownloader interface {
	DownloadURL(ctx context.Context, blobURL string) ([]byte, error)
}

// AISummarizer generates a short summary of text content.
type AISummarizer interface {
	// Summarize produces a 2-3 sentence summary of the provided text.
	Summarize(ctx context.Context, text string) (string, error)
}

// AttachmentCreateParams describes the data needed to persist an attachment node.
type AttachmentCreateParams struct {
	PartitionId       string
	FileName      string
	MimeType      string
	FileSize      int
	BlobUrl       string
	Transcription string
	Summary       string
	Status        string
	UploadedBy    string
}

// AttachmentResponse is the JSON body returned on successful upload.
type AttachmentResponse struct {
	ID            string `json:"id"`
	PartitionId       string `json:"partitionId"`
	FileName      string `json:"fileName"`
	MimeType      string `json:"mimeType"`
	FileSize      int    `json:"fileSize"`
	BlobUrl       string `json:"blobUrl"`
	Transcription string `json:"transcription,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Status        string `json:"status"`
	UploadedBy    string `json:"uploadedBy"`
	CreatedAt     string `json:"createdAt"`
}

// AttachmentHandler handles POST /spaces/{partitionId}/attachments.
type AttachmentHandler struct {
	logger     *slog.Logger
	bucket     string
	uploader   FileUploader
	downloader FileDownloader // optional; enables GET download of stored bytes (memql#804)
	extractor  TextExtractor
	store      AttachmentStore
	summarizer AISummarizer // optional
	planStore  PlanStore    // optional; when set, every successful upload spawns an analyzeFile Plan + plan.completed canvas card
}

// AttachmentHandlerOptions configures an AttachmentHandler.
type AttachmentHandlerOptions struct {
	Logger     *slog.Logger
	Bucket     string
	Uploader   FileUploader
	Downloader FileDownloader // optional; enables GET download (memql#804)
	Extractor  TextExtractor
	Store      AttachmentStore
	Summarizer AISummarizer // optional
	PlanStore  PlanStore    // optional; when set, every successful upload also spawns an analyzeFile Plan
}

// NewAttachmentHandler creates an AttachmentHandler.
func NewAttachmentHandler(opts AttachmentHandlerOptions) *AttachmentHandler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AttachmentHandler{
		logger:     logger,
		bucket:     opts.Bucket,
		uploader:   opts.Uploader,
		downloader: opts.Downloader,
		extractor:  opts.Extractor,
		store:      opts.Store,
		summarizer: opts.Summarizer,
		planStore:  opts.PlanStore,
	}
}

// ServeHTTP dispatches:
//   - POST /spaces/{partitionId}/attachments                 -> upload
//   - GET  /spaces/{partitionId}/attachments/{attachmentId}  -> download (memql#804)
//
// Route must be registered on a ServeMux with a prefix that captures {partitionId}.
func (h *AttachmentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.handleDownload(w, r)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodGet+", "+http.MethodPost)
		return
	}

	// Extract partitionId from path: /spaces/{partitionId}/attachments
	partitionId := extractPartitionIdFromAttachmentPath(r.URL.Path)
	if partitionId == "" {
		http.Error(w, "space ID is required", http.StatusBadRequest)
		return
	}

	// Extract actor from auth context.
	actor := strings.TrimSpace(auth.ActorFromContext(r.Context()))
	if actor == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Defense-in-depth: confirm the caller owns the target space
	// BEFORE doing any expensive work (multipart parse, file read,
	// blob upload). The DSL mutation re-enforces ownership too; this
	// short-circuits before bytes hit storage.
	if h.store != nil {
		owns, err := h.store.CallerOwnsSpace(r.Context(), partitionId)
		if err != nil {
			h.logger.Error("attachment ownership check failed", "error", err, "partitionId", partitionId, "actor", actor)
			http.Error(w, "ownership check failed", http.StatusInternalServerError)
			return
		}
		if !owns {
			// Generic 404 (not 403) so probing can't distinguish
			// "space exists but I don't own it" from "space doesn't exist".
			http.Error(w, "space not found", http.StatusNotFound)
			return
		}
	}

	// Parse multipart form.
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile(attachmentFormFileKey)
	if err != nil {
		http.Error(w, "file field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Validate file size.
	if header.Size > maxAttachmentBytes {
		http.Error(w, fmt.Sprintf("file too large: max %d bytes", maxAttachmentBytes), http.StatusRequestEntityTooLarge)
		return
	}

	// Detect MIME type.
	mimeType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	// Normalize MIME type (strip parameters).
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	mimeType = strings.ToLower(mimeType)

	if !allowedAttachmentMIMETypes[mimeType] {
		http.Error(w, fmt.Sprintf("unsupported file type: %s", mimeType), http.StatusUnsupportedMediaType)
		return
	}

	// Read file bytes.
	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentBytes+1))
	if err != nil {
		h.logger.Error("read attachment file", "error", err)
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	if int64(len(data)) > maxAttachmentBytes {
		http.Error(w, fmt.Sprintf("file too large: max %d bytes", maxAttachmentBytes), http.StatusRequestEntityTooLarge)
		return
	}

	ctx := r.Context()
	fileName := strings.TrimSpace(header.Filename)
	if fileName == "" {
		fileName = "upload"
	}

	// Upload to object storage (Azure Blob, memql#801).
	objectName := fmt.Sprintf("spaces/%s/attachments/%s/%s", partitionId, id.NewShortId(), fileName)
	blobUrl := ""
	if h.uploader != nil && h.bucket != "" {
		blobUrl, err = h.uploader.Upload(ctx, h.bucket, objectName, data, mimeType)
		if err != nil {
			h.logger.Error("upload attachment to blob storage", "error", err, "partitionId", partitionId)
			http.Error(w, "failed to upload file", http.StatusInternalServerError)
			return
		}
	} else {
		// No uploader configured (dev/test): use a placeholder URI.
		blobUrl = fmt.Sprintf("local://%s", objectName)
	}

	// Async analysis architecture (v1):
	//   1. Create the Attachment row in 'processing' status (no
	//      inline transcription yet).
	//   2. Create the Plan + Task in 'queued' state via
	//      CreateQueuedAnalyzePlan -- emits the plan.created
	//      canvas card so the user sees the work exists.
	//   3. Launch a goroutine with a detached context that runs
	//      extract + summarize + CompleteAnalyzePlan (the lifecycle
	//      transitions + Document creation + plan.completed card).
	//   4. Return the attachment node JSON immediately.
	//
	// The user's chat doesn't block; the canvas timeline picks up
	// plan.created right away, then plan.completed when the goroutine
	// finishes. From the user's perspective: instant ack, then the
	// result lands when ready -- exactly the brainstorm-locked
	// async behaviour without requiring a full planner integration
	// rewrite.

	// Step 1: Create attachment row in 'processing'. Empty
	// transcription / summary -- the goroutine will update later
	// (when v1 wires Attachment status updates from the planner;
	// for now the Attachment record exists immediately and the
	// canvas card carries the analysis result).
	params := AttachmentCreateParams{
		PartitionId:       partitionId,
		FileName:      fileName,
		MimeType:      mimeType,
		FileSize:      len(data),
		BlobUrl:       blobUrl,
		Transcription: "",
		Summary:       "",
		Status:        attachmentStatusProcessing,
		UploadedBy:    actor,
	}

	if h.store == nil {
		http.Error(w, "attachment store not configured", http.StatusInternalServerError)
		return
	}

	nodeJSON, err := h.store.CreateAttachment(ctx, params)
	if err != nil {
		h.logger.Error("create attachment node", "error", err, "partitionId", partitionId)
		http.Error(w, fmt.Sprintf("failed to create attachment: %v", err), http.StatusInternalServerError)
		return
	}

	// Step 2: stamp the queued Plan + Task + plan.created card.
	if h.planStore != nil {
		attachmentId := extractAttachmentId(nodeJSON)
		if attachmentId == "" {
			h.logger.Warn("could not extract attachment id from create response; skipping Plan creation",
				"partitionId", partitionId, "fileName", fileName)
		} else {
			analyzeParams := CreatePlanForAttachmentParams{
				AttachmentId: attachmentId,
				PartitionId:      partitionId,
				FileName:     fileName,
				RequestedBy:  actor,
				MimeType:     mimeType,
				FileSize:     len(data),
			}
			planId, queueErr := h.planStore.CreateQueuedAnalyzePlan(ctx, analyzeParams)
			if queueErr != nil {
				h.logger.Warn("queue analyzeFile Plan", "error", queueErr,
					"partitionId", partitionId, "attachmentId", attachmentId)
			} else {
				// Step 3: launch the async analysis goroutine. Detached
				// background context so HTTP request cancellation
				// doesn't kill the in-flight work. The bytes are
				// captured by closure -- safe because they're already
				// fully read into memory and capped at maxAttachmentBytes.
				bytesCopy := data
				h.runAnalysisAsync(planId, analyzeParams, bytesCopy)
			}
		}
	}

	// Step 4: return the attachment node JSON. The async goroutine
	// continues independently.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if len(nodeJSON) > 0 {
		_, _ = w.Write(nodeJSON)
	} else {
		// Fallback: write a basic response from params. transcription
		// + summary are populated asynchronously now (the goroutine
		// updates the Plan + Document, not the Attachment row); the
		// fallback returns empty values for them.
		resp := AttachmentResponse{
			ID:            objectName,
			PartitionId:       partitionId,
			FileName:      fileName,
			MimeType:      mimeType,
			FileSize:      len(data),
			BlobUrl:       blobUrl,
			Transcription: "",
			Summary:       "",
			Status:        attachmentStatusProcessing,
			UploadedBy:    actor,
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// runAnalysisAsync runs the file extraction + summarization in a
// detached goroutine and writes the lifecycle transitions back via
// the planStore. Replaces the previous synchronous extract+summary+
// stamp path that ran inside the HTTP request lifecycle.
//
// Detached context: uses context.Background() so HTTP request
// cancellation doesn't terminate the analysis. Errors are logged
// but don't bubble up; the Plan transitions to 'failed' if the
// pipeline produces no usable output.
func (h *AttachmentHandler) runAnalysisAsync(planId string, params CreatePlanForAttachmentParams, data []byte) {
	go func() {
		ctx := context.Background()

		// Extract text / describe image.
		transcription := ""
		if h.extractor != nil {
			extracted, err := h.extractor.Extract(ctx, params.MimeType, data)
			if err != nil {
				h.logger.Warn("[asyncAnalysis] extract attachment text", "error", err,
					"mimeType", params.MimeType, "planId", planId)
			} else {
				transcription = extracted
			}
		}

		// Generate AI summary if summarizer + transcription available.
		summary := ""
		if h.summarizer != nil && strings.TrimSpace(transcription) != "" {
			generated, err := h.summarizer.Summarize(ctx, transcription)
			if err != nil {
				h.logger.Warn("[asyncAnalysis] summarize attachment", "error", err, "planId", planId)
			} else {
				summary = generated
			}
		}

		params.Transcription = transcription
		params.Summary = summary

		if err := h.planStore.CompleteAnalyzePlan(ctx, planId, params); err != nil {
			h.logger.Warn("[asyncAnalysis] complete analyzeFile Plan", "error", err, "planId", planId)
		}
	}()
}

// extractAttachmentId parses the attachment-create response JSON to
// pull out the new attachment row's id. The response shape is the
// shaped output of mutationCreateAttachment, which carries the id
// as a top-level field. Returns empty string if missing or
// unparseable -- callers treat empty as "skip downstream Plan
// creation" rather than failing the upload.
func extractAttachmentId(nodeJSON json.RawMessage) string {
	if len(nodeJSON) == 0 {
		return ""
	}
	// The shaped response can be either an object or an array of
	// objects depending on which mutation surface was invoked.
	// Try the most common shape first.
	var obj map[string]any
	if err := json.Unmarshal(nodeJSON, &obj); err == nil {
		if id, ok := obj["id"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	var arr []map[string]any
	if err := json.Unmarshal(nodeJSON, &arr); err == nil && len(arr) > 0 {
		if id, ok := arr[0]["id"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// extractPartitionIdFromAttachmentPath parses /spaces/{partitionId}/attachments.
// Returns empty string if the path doesn't match.
func extractPartitionIdFromAttachmentPath(path string) string {
	const prefix = "/spaces/"
	const suffix = "/attachments"

	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	if !strings.HasSuffix(path, suffix) {
		return ""
	}

	middle := path[len(prefix) : len(path)-len(suffix)]
	middle = strings.TrimSpace(middle)
	if middle == "" || strings.Contains(middle, "/") {
		return ""
	}
	return middle
}

// parseAttachmentDownloadPath parses /spaces/{partitionId}/attachments/{attachmentId}
// (tolerating a leading base prefix like /api). partitionId is the segment before
// "attachments"; attachmentId the segment after. Both ids contain colons but no
// slashes, so they survive single-segment splitting. (memql#804)
func parseAttachmentDownloadPath(path string) (partitionId, attachmentId string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p != "attachments" {
			continue
		}
		if i >= 1 && i+1 < len(parts) {
			sid := strings.TrimSpace(parts[i-1])
			aid := strings.TrimSpace(parts[i+1])
			if sid != "" && aid != "" {
				return sid, aid, true
			}
		}
		return "", "", false
	}
	return "", "", false
}

// handleDownload serves GET /spaces/{partitionId}/attachments/{attachmentId}: it
// streams the stored file bytes back (memql#804). Authz mirrors the upload
// path -- the caller must own the space (404 otherwise, so id probing can't
// tell "not mine" from "absent"); the attachment's partitionId is re-checked
// against the path to block cross-space id probing.
func (h *AttachmentHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	partitionId, attachmentId, ok := parseAttachmentDownloadPath(r.URL.Path)
	if !ok {
		http.Error(w, "attachment id required", http.StatusBadRequest)
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

	owns, err := h.store.CallerOwnsSpace(ctx, partitionId)
	if err != nil {
		h.logger.Error("attachment download ownership check failed", "error", err, "partitionId", partitionId)
		http.Error(w, "ownership check failed", http.StatusInternalServerError)
		return
	}
	if !owns {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	row, err := h.store.GetAttachment(ctx, attachmentId, partitionId)
	if err != nil {
		h.logger.Error("attachment lookup failed", "error", err, "attachmentId", attachmentId, "partitionId", partitionId)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if row == nil || row.PartitionId != partitionId {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.downloader == nil {
		// No storage backend wired (e.g. local dev without a container): the
		// bytes live only on the producing machine / placeholder URL.
		http.Error(w, "file not available for download", http.StatusNotFound)
		return
	}

	data, err := h.downloader.DownloadURL(ctx, row.BlobUrl)
	if err != nil {
		h.logger.Warn("attachment bytes unavailable", "error", err, "attachmentId", attachmentId)
		http.Error(w, "file not available", http.StatusNotFound)
		return
	}

	mimeType := strings.TrimSpace(row.MimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	fileName := strings.TrimSpace(row.FileName)
	if fileName == "" {
		fileName = "download"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
