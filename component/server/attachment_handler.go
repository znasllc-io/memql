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

	"github.com/google/uuid"
	"github.com/visionarys-io/memql/component/auth"
)

const (
	maxAttachmentBytes     = 25 * 1024 * 1024 // 25 MB
	maxMultipartMemory     = 32 * 1024 * 1024 // 32 MB
	attachmentFormFileKey  = "file"

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
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (gcsURL string, err error)
}

// TextExtractor extracts plain text from file bytes.
type TextExtractor interface {
	// Extract returns plain text for the given MIME type and raw bytes.
	Extract(ctx context.Context, mimeType string, data []byte) (string, error)
}

// AttachmentStore creates and stores attachment memory nodes.
type AttachmentStore interface {
	// CreateAttachment creates a new v1:copresent:attachment node and returns the node JSON.
	CreateAttachment(ctx context.Context, params AttachmentCreateParams) (json.RawMessage, error)
}

// AISummarizer generates a short summary of text content.
type AISummarizer interface {
	// Summarize produces a 2-3 sentence summary of the provided text.
	Summarize(ctx context.Context, text string) (string, error)
}

// AttachmentCreateParams describes the data needed to persist an attachment node.
type AttachmentCreateParams struct {
	SpaceId       string
	FileName      string
	MimeType      string
	FileSize      int
	GCSUrl        string
	Transcription string
	Summary       string
	Status        string
	UploadedBy    string
}

// AttachmentResponse is the JSON body returned on successful upload.
type AttachmentResponse struct {
	ID            string `json:"id"`
	SpaceId       string `json:"spaceId"`
	FileName      string `json:"fileName"`
	MimeType      string `json:"mimeType"`
	FileSize      int    `json:"fileSize"`
	GCSUrl        string `json:"gcsURL"`
	Transcription string `json:"transcription,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Status        string `json:"status"`
	UploadedBy    string `json:"uploadedBy"`
	CreatedAt     string `json:"createdAt"`
}

// AttachmentHandler handles POST /spaces/{spaceId}/attachments.
type AttachmentHandler struct {
	logger        *slog.Logger
	bucket        string
	uploader      FileUploader
	extractor     TextExtractor
	store         AttachmentStore
	summarizer    AISummarizer // optional
	planStore     PlanStore    // optional; when set, every successful upload spawns an analyzeFile Plan + plan.completed canvas card
}

// AttachmentHandlerOptions configures an AttachmentHandler.
type AttachmentHandlerOptions struct {
	Logger     *slog.Logger
	Bucket     string
	Uploader   FileUploader
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
		extractor:  opts.Extractor,
		store:      opts.Store,
		summarizer: opts.Summarizer,
		planStore:  opts.PlanStore,
	}
}

// ServeHTTP dispatches POST /spaces/{spaceId}/attachments.
// Route must be registered on a ServeMux with a prefix that captures {spaceId}.
func (h *AttachmentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}

	// Extract spaceId from path: /spaces/{spaceId}/attachments
	spaceId := extractSpaceIdFromAttachmentPath(r.URL.Path)
	if spaceId == "" {
		http.Error(w, "space ID is required", http.StatusBadRequest)
		return
	}

	// Extract actor from auth context.
	actor := strings.TrimSpace(auth.ActorFromContext(r.Context()))
	if actor == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
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

	// Upload to GCS.
	objectName := fmt.Sprintf("spaces/%s/attachments/%s/%s", spaceId, uuid.New().String(), fileName)
	gcsURL := ""
	if h.uploader != nil && h.bucket != "" {
		gcsURL, err = h.uploader.Upload(ctx, h.bucket, objectName, data, mimeType)
		if err != nil {
			h.logger.Error("upload attachment to gcs", "error", err, "spaceId", spaceId)
			http.Error(w, "failed to upload file", http.StatusInternalServerError)
			return
		}
	} else {
		// No uploader configured (dev/test): use a placeholder URI.
		gcsURL = fmt.Sprintf("gs://local/%s", objectName)
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
		SpaceId:       spaceId,
		FileName:      fileName,
		MimeType:      mimeType,
		FileSize:      len(data),
		GCSUrl:        gcsURL,
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
		h.logger.Error("create attachment node", "error", err, "spaceId", spaceId)
		http.Error(w, fmt.Sprintf("failed to create attachment: %v", err), http.StatusInternalServerError)
		return
	}

	// Step 2: stamp the queued Plan + Task + plan.created card.
	if h.planStore != nil {
		attachmentId := extractAttachmentId(nodeJSON)
		if attachmentId == "" {
			h.logger.Warn("could not extract attachment id from create response; skipping Plan creation",
				"spaceId", spaceId, "fileName", fileName)
		} else {
			analyzeParams := CreatePlanForAttachmentParams{
				AttachmentId: attachmentId,
				SpaceId:      spaceId,
				FileName:     fileName,
				RequestedBy:  actor,
				MimeType:     mimeType,
				FileSize:     len(data),
			}
			planId, queueErr := h.planStore.CreateQueuedAnalyzePlan(ctx, analyzeParams)
			if queueErr != nil {
				h.logger.Warn("queue analyzeFile Plan", "error", queueErr,
					"spaceId", spaceId, "attachmentId", attachmentId)
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
			SpaceId:       spaceId,
			FileName:      fileName,
			MimeType:      mimeType,
			FileSize:      len(data),
			GCSUrl:        gcsURL,
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

// extractSpaceIdFromAttachmentPath parses /spaces/{spaceId}/attachments.
// Returns empty string if the path doesn't match.
func extractSpaceIdFromAttachmentPath(path string) string {
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
