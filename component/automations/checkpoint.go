package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// CheckpointConcept is the MemQL concept name for execution checkpoints.
// Uses the centralized constant from the concept_ids registry.
var CheckpointConcept = memoryNodes.ConceptMemQLCheckpoint

// DefaultCheckpointTTL is how long checkpoints remain valid (24 hours).
const DefaultCheckpointTTL = 24 * time.Hour

// Checkpoint errors.
var (
	ErrCheckpointNotFound = errors.New("checkpoint not found")
	ErrCheckpointExpired  = errors.New("checkpoint expired")
	ErrAutomationChanged  = errors.New("automation definition changed since checkpoint was saved")
	ErrCheckpointInvalid  = errors.New("checkpoint is invalid")
	ErrNonRetryableStep   = errors.New("step is not safely retryable (mutation or webhook)")
)

// SaveCheckpoint persists an execution checkpoint to MemQL.
// The checkpoint is stored with an explicit ID for easy retrieval.
func SaveCheckpoint(ctx context.Context, engine *memql.MemQLEngine, checkpoint *ExecutionCheckpoint) error {
	if engine == nil {
		return fmt.Errorf("engine is nil")
	}
	if checkpoint == nil {
		return fmt.Errorf("checkpoint is nil")
	}
	if checkpoint.ExecutionId == "" {
		return fmt.Errorf("executionId is required")
	}

	// Set timestamps if not already set
	now := time.Now().UTC()
	if checkpoint.SavedAt.IsZero() {
		checkpoint.SavedAt = now
	}
	if checkpoint.ExpiresAt.IsZero() {
		checkpoint.ExpiresAt = checkpoint.SavedAt.Add(DefaultCheckpointTTL)
	}

	// Use executionId as the bare shortId. The engine prepends
	// {partition}:{concept}: so the stored id is
	// "_system:v1:memql:checkpoint:<executionId>". Don't prepend
	// "checkpoint:" here -- that duplicates the concept name and
	// produces non-canonical ids per docs/public/concepts/identifiers.md.
	checkpointId := checkpoint.ExecutionId

	// Serialize the checkpoint to JSON
	payloadJSON, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint: %w", err)
	}

	// Insert the checkpoint
	query := fmt.Sprintf(`insert(%q, id=%s, payload=%s)`,
		CheckpointConcept,
		jsonString(checkpointId),
		string(payloadJSON),
	)

	// #2800: checkpoint bookkeeping is engine-internal, never client text.
	_, err = engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return fmt.Errorf("failed to save checkpoint: %w", err)
	}

	return nil
}

// LoadCheckpoint retrieves a checkpoint by execution ID.
// Returns ErrCheckpointNotFound if no checkpoint exists for the given execution.
func LoadCheckpoint(ctx context.Context, engine *memql.MemQLEngine, executionId string) (*ExecutionCheckpoint, error) {
	if engine == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if executionId == "" {
		return nil, fmt.Errorf("executionId is required")
	}

	// Query by concept and payload.executionId
	query := fmt.Sprintf(`concept==%s;payload.executionId==%s`,
		CheckpointConcept,
		jsonString(executionId),
	)

	// #2800: symmetric with SaveCheckpoint above -- checkpoint bookkeeping is
	// engine-internal, never client text. No @serverOnly construct is on this
	// path today; the pair is stamped alike so a future one does not depend on
	// which half of the read/write pair it landed in.
	result, err := engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("failed to query checkpoint: %w", err)
	}

	// Extract payload from result
	payload, err := extractCheckpointPayload(result)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	// Unmarshal into checkpoint struct
	var checkpoint ExecutionCheckpoint
	if err := mapStructFromPayload(payload, &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to unmarshal checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// ValidateCheckpoint checks if a checkpoint is valid for resume.
// It verifies:
//   - The checkpoint has not expired
//   - The automation definition has not changed (fingerprint matches)
//   - Required fields are present
//
// Returns nil if valid, or an appropriate error otherwise.
func ValidateCheckpoint(checkpoint *ExecutionCheckpoint, automation *Automation, idEngine *id.Engine) error {
	if checkpoint == nil {
		return ErrCheckpointInvalid
	}

	// Check required fields
	if checkpoint.ExecutionId == "" {
		return fmt.Errorf("%w: missing executionId", ErrCheckpointInvalid)
	}
	if checkpoint.AutomationName == "" {
		return fmt.Errorf("%w: missing automationName", ErrCheckpointInvalid)
	}
	if checkpoint.FailedAt == nil {
		return fmt.Errorf("%w: missing failedAt", ErrCheckpointInvalid)
	}

	// Check expiration
	if !checkpoint.ExpiresAt.IsZero() && time.Now().UTC().After(checkpoint.ExpiresAt) {
		return ErrCheckpointExpired
	}

	// Validate automation fingerprint if we have both the checkpoint fingerprint and current automation
	if checkpoint.AutomationFingerprint != "" && automation != nil && idEngine != nil {
		currentFingerprint := automation.DefinitionFingerprint(idEngine)
		if currentFingerprint != "" && currentFingerprint != checkpoint.AutomationFingerprint {
			return ErrAutomationChanged
		}
	}

	return nil
}

// IsStepRetryable checks if a step type can be safely retried.
// Mutations and webhooks have side effects and require explicit confirmation.
func IsStepRetryable(stepType StepType) bool {
	switch stepType {
	case StepTypeMutation, StepTypeWebhook:
		return false
	default:
		return true
	}
}

// ToMinimalStepResults converts a map of full StepResults to MinimalStepResults.
// This reduces checkpoint size by omitting large payloads while preserving
// the data needed for evaluator rehydration.
func ToMinimalStepResults(steps map[string]*StepResult) map[string]*MinimalStepResult {
	if steps == nil {
		return nil
	}

	minimal := make(map[string]*MinimalStepResult, len(steps))
	for stepId, result := range steps {
		if result == nil {
			continue
		}

		minResult := &MinimalStepResult{
			StepId:    result.StepId,
			Status:    string(result.Status),
			Error:     result.Error,
			ContentId: result.ContentId,
		}

		// Include result if it's not too large
		// For queries with many nodes, we omit the result to save space
		if result.Result != nil {
			if shouldIncludeResult(result) {
				minResult.Result = result.Result
			}
		}

		// Extract key metadata for evaluator
		if result.Metadata != nil {
			minResult.Metadata = make(map[string]any)
			// Copy essential metadata fields
			for _, key := range []string{"itemCount", "query", "resultQuery", "topic", "url", "statusCode"} {
				if v, ok := result.Metadata[key]; ok {
					minResult.Metadata[key] = v
				}
			}
		}

		minimal[stepId] = minResult
	}

	return minimal
}

// shouldIncludeResult determines if a step result should be stored in the checkpoint.
// Large results (many nodes) are omitted to keep checkpoint size reasonable.
func shouldIncludeResult(result *StepResult) bool {
	if result == nil || result.Result == nil {
		return false
	}

	// Check if result is a MemQL result with many nodes
	if resultMap, ok := result.Result.(map[string]any); ok {
		if bundle, ok := resultMap["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				// Omit results with more than 100 nodes
				if len(nodes) > 100 {
					return false
				}
			}
		}
	}

	// Include by default for non-query results
	return true
}

// extractCheckpointPayload extracts the payload from a MemQL query result.
func extractCheckpointPayload(result any) (map[string]any, error) {
	if result == nil {
		return nil, fmt.Errorf("result is nil")
	}

	// Handle *memql.ExecuteResult directly (from engine.Execute)
	if er, ok := result.(*memql.ExecuteResult); ok {
		if er.Bundle == nil || len(er.Bundle.Nodes) == 0 {
			return nil, fmt.Errorf("no nodes found")
		}
		node := er.Bundle.Nodes[0]
		if node == nil || node.Payload == nil {
			return nil, fmt.Errorf("no payload in node")
		}
		return node.Payload.AsMap(), nil
	}

	// Fallback: handle map[string]any (e.g., from WebSocket responses)
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	// Navigate to result.bundle.nodes[0].payload
	bundle, ok := resultMap["bundle"].(map[string]any)
	if !ok {
		// Try result.Bundle for Go struct
		bundle, ok = resultMap["Bundle"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("no bundle in result")
		}
	}

	nodes, ok := bundle["nodes"].([]any)
	if !ok {
		// Try bundle.Nodes for Go struct
		nodes, ok = bundle["Nodes"].([]any)
		if !ok {
			return nil, fmt.Errorf("no nodes in bundle")
		}
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes found")
	}

	node, ok := nodes[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected node type: %T", nodes[0])
	}

	payload, ok := node["payload"].(map[string]any)
	if !ok {
		// Try node.Payload for Go struct
		payload, ok = node["Payload"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("no payload in node")
		}
	}

	return payload, nil
}

// mapStructFromPayload unmarshals a map into a struct via JSON.
func mapStructFromPayload(m map[string]any, v any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// jsonString returns a JSON-encoded string value.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
