package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// PlanStore creates Plan + Task records that wrap an attachment's
// analysis lifecycle, then publishes a plan.completed canvas card.
//
// Two-phase API mirrors the v1 async-planner architecture from the
// brainstorm:
//
//   - CreateQueuedAnalyzePlan -- synchronous; stamps Plan + Task in
//     'queued' state and emits the plan.created canvas card so the
//     user sees the work exists immediately. Cheap; runs in the HTTP
//     request path.
//
//   - CompleteAnalyzePlan -- does the actual lifecycle transitions
//     (queued -> running -> succeeded), creates the v1:knowledge:document
//     row, and emits the plan.completed card. Designed to be called
//     from a detached goroutine OR (in subsequent rounds) from a
//     planner-owned automation triggered by the Plan creation event.
//
//   - CreateAndCompleteAnalyzePlan -- back-compat convenience that
//     calls both inline. Used when the caller is already in a
//     synchronous extract+summarize path.
type PlanStore interface {
	CreateQueuedAnalyzePlan(ctx context.Context, params CreatePlanForAttachmentParams) (string, error)
	CompleteAnalyzePlan(ctx context.Context, planId string, params CreatePlanForAttachmentParams) error
	CreateAndCompleteAnalyzePlan(ctx context.Context, params CreatePlanForAttachmentParams) error
}

// CreatePlanForAttachmentParams carries everything the planner /
// canvas card need to record the analysis lifecycle.
type CreatePlanForAttachmentParams struct {
	AttachmentId  string
	PartitionId   string
	FileName      string
	RequestedBy   string
	Transcription string // extracted text (populated by analyzer / handler)
	Summary       string // LLM summary (populated by analyzer / handler)
	MimeType      string
	FileSize      int
}

// EnginePlanStore implements PlanStore via the MemQL DSL mutations
// declared in mutations/v1/copresent/{create,update}{Plan,Task} and
// createCanvasState. Concept-agnostic; the DSL owns the row shapes.
type EnginePlanStore struct {
	engine MemQLExecutor
}

// NewEnginePlanStore creates a PlanStore backed by a MemQL engine.
func NewEnginePlanStore(engine MemQLExecutor) *EnginePlanStore {
	return &EnginePlanStore{engine: engine}
}

// planAndTaskIds returns the deterministic Plan + Task ids for an
// attachment-driven analysis Plan. Anchored on the attachmentId for
// idempotent re-upload semantics.
func planAndTaskIds(attachmentId string) (planId string, taskId string) {
	planId = strings.TrimSpace(attachmentId) + ":plan"
	taskId = planId + ":task:0"
	return planId, taskId
}

// CreateQueuedAnalyzePlan stamps the Plan + Task in 'queued' state
// and emits the plan.created canvas card. Synchronous; called in the
// HTTP request path so the user sees the Plan exist before the async
// analysis kicks off.
func (s *EnginePlanStore) CreateQueuedAnalyzePlan(ctx context.Context, p CreatePlanForAttachmentParams) (string, error) {
	if s == nil || s.engine == nil {
		return "", fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(p.AttachmentId) == "" {
		return "", fmt.Errorf("attachmentId required")
	}
	if strings.TrimSpace(p.PartitionId) == "" {
		return "", fmt.Errorf("partitionId required")
	}
	if strings.TrimSpace(p.RequestedBy) == "" {
		return "", fmt.Errorf("requestedBy required")
	}

	planId, taskId := planAndTaskIds(p.AttachmentId)

	displayName := strings.TrimSpace(p.FileName)
	if displayName == "" {
		displayName = "this file"
	}
	goal := fmt.Sprintf("Analyze %s", displayName)

	// Compute a quick heuristic estimate from file size + mime type.
	// (LLM-based estimate via the planEstimate prompt template is
	// invoked from the planner integration once that lands; for now
	// we use a deterministic heuristic so the estimate strip on the
	// plan.created card has a number to show.)
	estP50, estP90 := heuristicEstimateAnalyzeFile(p.MimeType, p.FileSize)
	// Build every DSL call via dslCall, which marshals the whole
	// argument object with encoding/json. This keeps the quote
	// characters out of the Go format string entirely -- a value
	// containing a double quote can't break out of its enclosing
	// literal (go/unsafe-quoting), and CodeQL recognizes json.Marshal's
	// output as safely quoted.
	estimate := map[string]any{"p50Ms": estP50, "p90Ms": estP90, "confidence": "heuristic"}

	// 1. Create the Plan in 'queued'.
	createPlanQ, err := dslCall("createPlan", map[string]any{
		"planId":        planId,
		"partitionId":   p.PartitionId,
		"kind":          "analyzeFile",
		"goal":          goal,
		"requestedBy":   p.RequestedBy,
		"triggerSource": "user.explicit",
		"input":         map[string]any{"attachmentId": p.AttachmentId, "fileName": p.FileName},
	})
	if err != nil {
		return "", fmt.Errorf("build createPlan: %w", err)
	}
	if _, err := s.engine.Execute(ctx, createPlanQ); err != nil {
		return "", fmt.Errorf("execute createPlan: %w", err)
	}

	// 1.5. Stamp the heuristic estimate on the Plan immediately
	// after creation so the canvas card has it on first render.
	if estQ, err := dslCall("updatePlanStatus", map[string]any{
		"planId":      planId,
		"status":      "queued",
		"estimate":    estimate,
		"estimatedAt": time.Now().UTC().Format(time.RFC3339),
	}); err == nil {
		// Non-fatal: estimate is nice-to-have on the card.
		_, _ = s.engine.Execute(ctx, estQ)
	}

	// 1a. Emit the plan.created canvas card with the heuristic
	// estimate baked in so the user sees the Plan exists from the
	// moment it lands.
	createdStateId := planId + ":created"
	if cardQ, err := dslCall("mutationCreateCanvasState", map[string]any{
		"stateId": createdStateId,
		"space":   p.PartitionId,
		"kind":    "card",
		"data": map[string]any{
			"variant":  "plan.created",
			"planId":   planId,
			"goal":     goal,
			"estimate": estimate,
		},
		"visibility": "private",
		"forUserId":  p.RequestedBy,
		"actor":      map[string]any{"kind": "user", "userId": p.RequestedBy},
		"importance": "ambient",
	}); err == nil {
		// Non-fatal -- Plan exists, card just won't render.
		_, _ = s.engine.Execute(ctx, cardQ)
	}

	// 2. Create the single Task in 'queued'.
	createTaskQ, err := dslCall("createTask", map[string]any{
		"taskId": taskId,
		"planId": planId,
		"kind":   "fileProcessor",
		"seq":    0,
		"input":  map[string]any{"attachmentId": p.AttachmentId},
	})
	if err != nil {
		return "", fmt.Errorf("build createTask: %w", err)
	}
	if _, err := s.engine.Execute(ctx, createTaskQ); err != nil {
		return "", fmt.Errorf("execute createTask: %w", err)
	}

	return planId, nil
}

// CompleteAnalyzePlan does the lifecycle transitions, creates the
// v1:knowledge:document container, and emits the plan.completed
// canvas card. Designed to be called from a detached goroutine; the
// caller passes a fresh background context so the HTTP request
// cancellation doesn't kill the in-flight work.
func (s *EnginePlanStore) CompleteAnalyzePlan(ctx context.Context, planId string, p CreatePlanForAttachmentParams) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	_, taskId := planAndTaskIds(p.AttachmentId)
	now := time.Now().UTC().Format(time.RFC3339)

	// 3. Transition Plan + Task to 'running'. (All DSL built via
	// dslCall -> whole-object json.Marshal; no quote chars in the Go
	// format string, so go/unsafe-quoting can't fire.)
	runningPlanQ, err := dslCall("updatePlanStatus", map[string]any{
		"planId": planId, "status": "running", "startedAt": now,
	})
	if err != nil {
		return fmt.Errorf("build updatePlanStatus(running): %w", err)
	}
	if _, err := s.engine.Execute(ctx, runningPlanQ); err != nil {
		return fmt.Errorf("execute updatePlanStatus(running): %w", err)
	}
	runningTaskQ, err := dslCall("updateTaskStatus", map[string]any{
		"taskId": taskId, "status": "running", "startedAt": now,
	})
	if err != nil {
		return fmt.Errorf("build updateTaskStatus(running): %w", err)
	}
	if _, err := s.engine.Execute(ctx, runningTaskQ); err != nil {
		return fmt.Errorf("execute updateTaskStatus(running): %w", err)
	}

	// 4. Transition Task to 'succeeded' with the analysis output.
	taskSucceededQ, err := dslCall("updateTaskStatus", map[string]any{
		"taskId": taskId,
		"status": "succeeded",
		"output": map[string]any{
			"extractedText": p.Transcription,
			"mimeType":      p.MimeType,
			"sizeBytes":     p.FileSize,
		},
		"completedAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("build updateTaskStatus(succeeded): %w", err)
	}
	if _, err := s.engine.Execute(ctx, taskSucceededQ); err != nil {
		return fmt.Errorf("execute updateTaskStatus(succeeded): %w", err)
	}

	// 5. Create the v1:knowledge:document container row.
	documentId := planId + ":document"
	docFormat := pickDocumentFormat(p.MimeType)
	if docQ, err := dslCall("mutationCreateDocument", map[string]any{
		"documentId":   documentId,
		"attachmentId": p.AttachmentId,
		"planId":       planId,
		"partitionId":  p.PartitionId,
		"fileName":     p.FileName,
		"mimeType":     p.MimeType,
		"format":       docFormat,
		"summary":      p.Summary,
		"uploadedBy":   p.RequestedBy,
	}); err == nil {
		_, _ = s.engine.Execute(ctx, docQ)
	}

	// 6. Transition Plan to 'succeeded' with the rolled-up output.
	sample := p.Transcription
	if len(sample) > 500 {
		sample = sample[:500] + "…"
	}
	planSucceededQ, err := dslCall("updatePlanStatus", map[string]any{
		"planId": planId,
		"status": "succeeded",
		"output": map[string]any{
			"summary":             p.Summary,
			"extractedTextSample": sample,
			"fullTextLength":      len(p.Transcription),
			"fileName":            p.FileName,
			"attachmentId":        p.AttachmentId,
			"documentId":          documentId,
		},
		"completedAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("build updatePlanStatus(succeeded): %w", err)
	}
	if _, err := s.engine.Execute(ctx, planSucceededQ); err != nil {
		return fmt.Errorf("execute updatePlanStatus(succeeded): %w", err)
	}

	// 7. Publish the plan.completed canvas card. documentId carried
	// so the card's Validate / Reject / Attach / Refine actions
	// target the right Document.
	stateId := planId + ":completed"
	completedCardQ, err := dslCall("mutationCreateCanvasState", map[string]any{
		"stateId": stateId,
		"space":   p.PartitionId,
		"kind":    "card",
		"data": map[string]any{
			"variant":    "plan.completed",
			"planId":     planId,
			"fileName":   p.FileName,
			"summary":    p.Summary,
			"status":     "succeeded",
			"documentId": documentId,
		},
		"visibility": "private",
		"forUserId":  p.RequestedBy,
		"actor":      map[string]any{"kind": "user", "userId": p.RequestedBy},
		"importance": "ambient",
	})
	if err != nil {
		return fmt.Errorf("build mutationCreateCanvasState(plan.completed): %w", err)
	}
	if _, err := s.engine.Execute(ctx, completedCardQ); err != nil {
		return fmt.Errorf("execute mutationCreateCanvasState(plan.completed): %w", err)
	}

	return nil
}

// CreateAndCompleteAnalyzePlan is the legacy synchronous wrapper:
// queue + complete in one inline call. Kept for callers that already
// produced the analysis synchronously and just want the Plan
// lifecycle stamped.
func (s *EnginePlanStore) CreateAndCompleteAnalyzePlan(ctx context.Context, p CreatePlanForAttachmentParams) error {
	planId, err := s.CreateQueuedAnalyzePlan(ctx, p)
	if err != nil {
		return err
	}
	return s.CompleteAnalyzePlan(ctx, planId, p)
}

// heuristicEstimateAnalyzeFile produces a deterministic p50/p90
// estimate (in milliseconds) for an analyzeFile Plan based on the
// file's mime type + size. Per Q5: this is the "heuristic" tier of
// the estimate; the planner integration's LLM-backed estimate
// (planEstimate prompt) replaces this once historical data exists
// and a richer estimate is needed.
//
// Numbers are tuned to roughly match the synchronous in-handler
// extract+summary timings observed in dev (PDF takes longer than
// plaintext; image OCR slow; spreadsheet O(rowCount)).
func heuristicEstimateAnalyzeFile(mime string, sizeBytes int) (p50Ms int64, p90Ms int64) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	mb := int64(sizeBytes) / (1024 * 1024)
	if mb < 1 {
		mb = 1
	}
	switch {
	case mime == "application/pdf":
		// ~10s per MB p50, 30s per MB p90, capped at 5min/15min.
		p50Ms = clamp64(mb*10_000, 5_000, 300_000)
		p90Ms = clamp64(mb*30_000, 15_000, 900_000)
	case strings.HasPrefix(mime, "image/"):
		// Vision call dominates; size barely matters.
		p50Ms, p90Ms = 10_000, 30_000
	case strings.Contains(mime, "spreadsheet") || strings.Contains(mime, "excel"):
		// Per-row extraction; assume ~100 bytes/row and ~10ms/row.
		approxRows := int64(sizeBytes) / 100
		p50Ms = clamp64(approxRows*10, 20_000, 600_000)
		p90Ms = clamp64(approxRows*30, 60_000, 1_800_000)
	case strings.HasPrefix(mime, "text/"):
		// Plain text: extract is instant, summary is a single LLM call.
		p50Ms, p90Ms = 5_000, 15_000
	default:
		p50Ms, p90Ms = 10_000, 30_000
	}
	return p50Ms, p90Ms
}

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// pickDocumentFormat maps a mime type to the v1:knowledge:document
// format enum value. Defaults to 'text' for anything unrecognised.
func pickDocumentFormat(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case mime == "application/pdf":
		return "pdf"
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case mime == "text/markdown":
		return "markdown"
	case strings.HasPrefix(mime, "text/"):
		return "text"
	case strings.Contains(mime, "spreadsheet") || strings.Contains(mime, "excel"):
		return "spreadsheet"
	default:
		return "text"
	}
}

// freshPlanId returns a uuid-suffixed plan id when a deterministic
// id isn't appropriate. Currently unused -- attachment-driven Plans
// key off the attachment id for idempotency.
func freshPlanId() string {
	return "plan:" + id.NewShortId()
}
