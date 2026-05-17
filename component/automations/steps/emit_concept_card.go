package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// EmitConceptCardExecutor emits concept card utterances to conversations.
// Concept cards are system-generated messages that surface automation output.
type EmitConceptCardExecutor struct{}

// Execute runs an emitConceptCard step.
func (e *EmitConceptCardExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.EmitConceptCard == nil {
		result.Status = "failed"
		result.Error = "emitConceptCard configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("emitConceptCard configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	cfg := step.EmitConceptCard

	// Evaluate cardType
	cardType := cfg.CardType
	if strings.HasPrefix(cardType, "$") {
		evaluated, err := stepCtx.Evaluator.EvaluateString(cardType)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate cardType: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate cardType: %w", err)
		}
		cardType = evaluated
	}

	// Evaluate spaceId
	spaceId := cfg.SpaceId
	if strings.HasPrefix(spaceId, "$") {
		evaluated, err := stepCtx.Evaluator.EvaluateString(spaceId)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate spaceId: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate spaceId: %w", err)
		}
		spaceId = evaluated
	}

	// Evaluate conceptRef
	conceptRef := cfg.ConceptRef
	if strings.HasPrefix(conceptRef, "$") {
		evaluated, err := stepCtx.Evaluator.EvaluateString(conceptRef)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate conceptRef: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate conceptRef: %w", err)
		}
		conceptRef = evaluated
	}

	// Evaluate data fields
	evaluatedData, err := stepCtx.Evaluator.EvaluateMap(cfg.Data)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate data: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate data: %w", err)
	}

	// Build the utterance payload for the concept card
	// Uses utteranceType "system" with action payload to hold card data
	utterancePayload := map[string]any{
		"spaceId":         spaceId,
		"participantId":   "system:concept-card",
		"participantType": "system",
		"utteranceType":   "system",
		"action": map[string]any{
			"type": "concept_card." + cardType,
			"payload": map[string]any{
				"cardType":   cardType,
				"conceptRef": conceptRef,
				"data":       evaluatedData,
			},
		},
	}

	// Generate utterance ID
	utteranceId := fmt.Sprintf("concept-card-%s-%d", cardType, time.Now().UnixNano())

	// Build the insert query
	payloadJSON, err := json.Marshal(utterancePayload)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to marshal payload: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to marshal payload: %w", err)
	}

	insertQuery := fmt.Sprintf(`insert(%q, id=%q, payload=%s)`, memoryNodes.ConceptCognitionUtterance, utteranceId, string(payloadJSON))

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("emitting concept card",
			"step", step.ID,
			"cardType", cardType,
			"spaceId", spaceId,
			"conceptRef", conceptRef,
		)
	}

	// Execute the insert
	execResult, err := stepCtx.Engine.Execute(ctx, insertQuery)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("concept card insertion failed: %w", err)
	}

	result.Status = "success"
	result.Result = map[string]any{
		"utteranceId":   utteranceId,
		"cardType":      cardType,
		"spaceId":       spaceId,
		"conceptRef":    conceptRef,
		"utteranceType": "system",
		"action": map[string]any{
			"type": "concept_card." + cardType,
			"payload": map[string]any{
				"cardType":   cardType,
				"conceptRef": conceptRef,
				"data":       evaluatedData,
			},
		},
	}
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"utteranceId": utteranceId,
		"cardType":    cardType,
		"conceptRef":  conceptRef,
	}

	// Record step execution
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:       runId,
		StepId:      step.ID,
		StepType:    "emitConceptCard",
		Status:      result.Status,
		ResultQuery: fmt.Sprintf(`concept==v1:cognition:utterance;id==%q`, utteranceId),
		ItemCount:   1,
		Duration:    float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("concept card emitted",
			"step", step.ID,
			"utteranceId", utteranceId,
			"cardType", cardType,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	// Suppress unused variable warning
	_ = execResult

	return result, nil
}
