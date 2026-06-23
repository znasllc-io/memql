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
	estimateBlock := fmt.Sprintf(
		`{"p50Ms": %d, "p90Ms": %d, "confidence": "heuristic"}`,
		estP50, estP90,
	)

	// 1. Create the Plan in 'queued'.
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`createPlan({"planId": %s, "partitionId": %s, "kind": "analyzeFile", "goal": %s, "requestedBy": %s, "triggerSource": "user.explicit", "input": {"attachmentId": %s, "fileName": %s}})`,
		jsonString(planId),
		jsonString(p.PartitionId),
		jsonString(goal),
		jsonString(p.RequestedBy),
		jsonString(p.AttachmentId),
		jsonString(p.FileName),
	)); err != nil {
		return "", fmt.Errorf("execute createPlan: %w", err)
	}

	// 1.5. Stamp the heuristic estimate on the Plan immediately
	// after creation so the canvas card has it on first render.
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`updatePlanStatus({"planId": %s, "status": "queued", "estimate": %s, "estimatedAt": %s})`,
		jsonString(planId), estimateBlock, jsonString(time.Now().UTC().Format(time.RFC3339)),
	)); err != nil {
		// Non-fatal: estimate is nice-to-have on the card.
		_ = err
	}

	// 1a. Emit the plan.created canvas card with the heuristic
	// estimate baked in so the user sees the Plan exists from the
	// moment it lands.
	createdStateId := planId + ":created"
	createdCardData := fmt.Sprintf(
		`{"variant": "plan.created", "planId": %s, "goal": %s, "estimate": %s}`,
		jsonString(planId),
		jsonString(goal),
		estimateBlock,
	)
	createdActor := fmt.Sprintf(`{"kind": "user", "userId": %s}`, jsonString(p.RequestedBy))
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateCanvasState({"stateId": %s, "space": %s, "kind": "card", "data": %s, "visibility": "private", "forUserId": %s, "actor": %s, "importance": "ambient"})`,
		jsonString(createdStateId),
		jsonString(p.PartitionId),
		createdCardData,
		jsonString(p.RequestedBy),
		createdActor,
	)); err != nil {
		// Non-fatal -- Plan exists, card just won't render.
		_ = err
	}

	// 2. Create the single Task in 'queued'.
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`createTask({"taskId": %s, "planId": %s, "kind": "fileProcessor", "seq": 0, "input": {"attachmentId": %s}})`,
		jsonString(taskId),
		jsonString(planId),
		jsonString(p.AttachmentId),
	)); err != nil {
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

	// 3. Transition Plan + Task to 'running'.
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`updatePlanStatus({"planId": %s, "status": "running", "startedAt": %s})`,
		jsonString(planId), jsonString(now),
	)); err != nil {
		return fmt.Errorf("execute updatePlanStatus(running): %w", err)
	}
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`updateTaskStatus({"taskId": %s, "status": "running", "startedAt": %s})`,
		jsonString(taskId), jsonString(now),
	)); err != nil {
		return fmt.Errorf("execute updateTaskStatus(running): %w", err)
	}

	// 4. Transition Task to 'succeeded' with the analysis output.
	taskOutput := fmt.Sprintf(
		`{"extractedText": %s, "mimeType": %s, "sizeBytes": %d}`,
		jsonString(p.Transcription),
		jsonString(p.MimeType),
		p.FileSize,
	)
	taskCompletedAt := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`updateTaskStatus({"taskId": %s, "status": "succeeded", "output": %s, "completedAt": %s})`,
		jsonString(taskId), taskOutput, jsonString(taskCompletedAt),
	)); err != nil {
		return fmt.Errorf("execute updateTaskStatus(succeeded): %w", err)
	}

	// 5. Create the v1:knowledge:document container row.
	documentId := planId + ":document"
	docFormat := pickDocumentFormat(p.MimeType)
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateDocument({"documentId": %s, "attachmentId": %s, "planId": %s, "partitionId": %s, "fileName": %s, "mimeType": %s, "format": %s, "summary": %s, "uploadedBy": %s})`,
		jsonString(documentId),
		jsonString(p.AttachmentId),
		jsonString(planId),
		jsonString(p.PartitionId),
		jsonString(p.FileName),
		jsonString(p.MimeType),
		jsonString(docFormat),
		jsonString(p.Summary),
		jsonString(p.RequestedBy),
	)); err != nil {
		_ = err
	}

	// 6. Transition Plan to 'succeeded' with the rolled-up output.
	sample := p.Transcription
	if len(sample) > 500 {
		sample = sample[:500] + "…"
	}
	planCompletedAt := time.Now().UTC().Format(time.RFC3339)
	planOutput := fmt.Sprintf(
		`{"summary": %s, "extractedTextSample": %s, "fullTextLength": %d, "fileName": %s, "attachmentId": %s, "documentId": %s}`,
		jsonString(p.Summary),
		jsonString(sample),
		len(p.Transcription),
		jsonString(p.FileName),
		jsonString(p.AttachmentId),
		jsonString(documentId),
	)
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`updatePlanStatus({"planId": %s, "status": "succeeded", "output": %s, "completedAt": %s})`,
		jsonString(planId), planOutput, jsonString(planCompletedAt),
	)); err != nil {
		return fmt.Errorf("execute updatePlanStatus(succeeded): %w", err)
	}

	// 7. Publish the plan.completed canvas card. documentId carried
	// so the card's Validate / Reject / Attach / Refine actions
	// target the right Document.
	stateId := planId + ":completed"
	cardData := fmt.Sprintf(
		`{"variant": "plan.completed", "planId": %s, "fileName": %s, "summary": %s, "status": "succeeded", "documentId": %s}`,
		jsonString(planId),
		jsonString(p.FileName),
		jsonString(p.Summary),
		jsonString(documentId),
	)
	actor := fmt.Sprintf(`{"kind": "user", "userId": %s}`, jsonString(p.RequestedBy))
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateCanvasState({"stateId": %s, "space": %s, "kind": "card", "data": %s, "visibility": "private", "forUserId": %s, "actor": %s, "importance": "ambient"})`,
		jsonString(stateId),
		jsonString(p.PartitionId),
		cardData,
		jsonString(p.RequestedBy),
		actor,
	)); err != nil {
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
