package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/automations"
	"github.com/visionarys-io/memql/component/events"
)

// EventExecutor publishes events to the event bus.
type EventExecutor struct{}

// Execute runs an event step.
func (e *EventExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Event == nil {
		result.Status = "failed"
		result.Error = "event configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("event configuration is required")
	}

	if stepCtx.EventBus == nil {
		result.Status = "failed"
		result.Error = "event bus not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("event bus not configured")
	}

	eventCfg := step.Event

	// Evaluate topic with $ expressions
	topic, err := stepCtx.Evaluator.EvaluateString(eventCfg.Topic)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate topic: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate topic: %w", err)
	}

	// Build payload
	payload := make(map[string]any)

	// Evaluate payload with $ expressions
	if eventCfg.Payload != nil {
		evaluatedPayload, err := stepCtx.Evaluator.EvaluateMap(eventCfg.Payload)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate payload: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate payload: %w", err)
		}
		for k, v := range evaluatedPayload {
			payload[k] = v
		}
	}

	// Include result from a previous step if requested
	if eventCfg.IncludeResult {
		resultFrom := eventCfg.ResultFrom
		if resultFrom == "" {
			// Include all step results
			payload["stepResults"] = stepCtx.Execution.Steps
		} else {
			if stepResult, ok := stepCtx.Execution.Steps[resultFrom]; ok {
				payload["stepResult"] = stepResult.Result
			}
		}
	}

	// Determine event kind
	kind := events.KindMessage
	if k := strings.TrimSpace(eventCfg.Kind); k != "" {
		kind = mapEventKind(k)
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Info("event: PUBLISHING EVENT",
			"step", step.ID,
			"topic", topic,
			"kind", kind.String(),
			"payloadKeys", getPayloadKeys(payload),
		)
	}

	// Publish event
	event := events.NewEvent(topic, kind, payload)
	stepCtx.EventBus.Publish(event)

	result.Status = "success"
	result.Result = map[string]any{
		"topic":   topic,
		"kind":    kind.String(),
		"payload": payload,
	}
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"topic": topic,
		"kind":  kind.String(),
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:    runId,
		StepId:   step.ID,
		StepType: "event",
		Status:   result.Status,
		Topic:    topic,
		Duration: float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("event step completed",
			"step", step.ID,
			"topic", topic,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// getPayloadKeys returns the keys of a payload map for logging.
func getPayloadKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	return keys
}

// mapEventKind converts a string kind to events.Kind.
func mapEventKind(kind string) events.Kind {
	switch strings.ToLower(kind) {
	case "telemetry":
		return events.KindTelemetry
	case "message":
		return events.KindMessage
	case "graph_update":
		return events.KindGraphUpdate
	case "ai_event":
		return events.KindAIEvent
	default:
		return events.KindMessage
	}
}
