package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Presence states are intentionally UI-friendly and stable.
//
// State machine:
//
//	idle → thinking → typing → idle
//	                → using_tool → typing (brief, non-claw tools)
//	                → working → typing (long-running, claw tools)
//	                → responding → idle (TTS/final delivery)
//	                → error
//	idle → researching → idle (reactive: heartbeat-driven background task)
//	idle → investigating → idle (reactive: topic-listener triggered)
//	waiting (another agent is responding to the current human turn)
//	needs_human (destructive claw op requires approval)
//
// Reactive presence states (researching, investigating) coexist with
// the conversational states. An agent can be `idle` for the chat lane
// while running a background task that the UI surfaces as
// `researching` -- same agent, two concurrent activities, distinct
// UI affordances. The cognition pipeline doesn't force agents out of
// reactive states when picking a turn winner.
const (
	presenceStateIdle       = "idle"
	presenceStateThinking   = "thinking"
	presenceStateTyping     = "typing"
	presenceStateUsingTool  = "using_tool"
	presenceStateWorking    = "working"
	presenceStateResponding = "responding"
	presenceStateWaiting    = "waiting"
	presenceStateNeedsHuman = "needs_human"
	presenceStateError      = "error"
	// Reactive states (phase 3): set by the heartbeat-driven topic
	// listener and standing-task scheduler when an agent is doing
	// background work outside the primary turn-taking loop.
	presenceStateResearching   = "researching"
	presenceStateInvestigating = "investigating"
)

// clawToolNames lists the NemoClaw tool names that trigger the "working" state
// (long-running coding/automation tasks). All other tools use "using_tool".
var clawToolNames = map[string]bool{
	"clawExecuteTask": true,
	"clawReadFile":    true,
	"clawListFiles":   true,
	"clawSearchCode":  true,
}

func presenceRecordId(participantId string) string {
	pid := strings.TrimSpace(participantId)
	if pid == "" {
		return ""
	}
	// Deterministic ID: one presence record per participant
	// (append-only versions). The canonical id format already carries
	// `:participantPresence:` in the middle position; the shortId
	// stays the bare participant id.
	return pid
}

func (c *CognitionIntegration) upsertParticipantPresence(ctx context.Context, spaceId, participantId, state, label, reason, lastUtteranceId, lastError string, metadata map[string]any) error {
	if c == nil || c.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	participantId = strings.TrimSpace(participantId)
	if spaceId == "" || participantId == "" {
		return fmt.Errorf("spaceId and participantId are required")
	}

	id := presenceRecordId(participantId)
	if id == "" {
		return fmt.Errorf("invalid presence record id")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"spaceId":       spaceId,
		"participantId": participantId,
		"state":         strings.TrimSpace(state),
		"label":         strings.TrimSpace(label),
		"reason":        strings.TrimSpace(reason),
		"sinceAt":       now,
		"lastUpdatedAt": now,
	}
	if strings.TrimSpace(lastUtteranceId) != "" {
		payload["lastUtteranceId"] = strings.TrimSpace(lastUtteranceId)
	}
	if strings.TrimSpace(lastError) != "" {
		payload["lastError"] = strings.TrimSpace(lastError)
	}
	// Merge metadata fields (activeTaskCount, activeTaskLabel, intent, etc.)
	// directly into the payload so the frontend can read them.
	for k, v := range metadata {
		payload[k] = v
	}

	query := fmt.Sprintf(`insert("%s", id=%s, payload=%s)`,
		memoryNodes.ConceptCognitionParticipantPresence,
		escapeJSONString(id),
		mustJSON(payload),
	)
	_, err := c.engine.Execute(ctx, query)
	return err
}
