package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
)

// IntegrationName returns the stable identifier for this integration.
func (c *CognitionIntegration) IntegrationName() string {
	return "cognition"
}

// Capabilities returns the list of capabilities this integration exposes
// as callable builtin functions from the MemQL DSL.
func (c *CognitionIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "scoreUtterance",
			Description: "Score utterance against agent candidates using Polyphon score engine 5-factor scoring algorithm",
			Handler:     c.handleScoreUtterance,
			ArgsSchema: map[string]string{
				"spaceId":     "string",
				"utteranceId": "string",
				"text":        "string",
				"speakerId":   "string",
				"speakerName": "string",
				"candidates":  "array",
			},
		},
		{
			Name:        "trackPresence",
			Description: "Update participant presence state (idle, thinking, typing, responding)",
			Handler:     c.handleTrackPresence,
			ArgsSchema: map[string]string{
				"participantId": "string",
				"spaceId":       "string",
				"state":         "string",
				"metadata":      "object",
			},
		},
	}
}

// handleScoreUtterance wraps the Polyphon score engine scoring algorithm.
// It converts the DSL args into the Polyphon types, runs the scoring, and
// returns the decision as a MemoryNode.
func (c *CognitionIntegration) handleScoreUtterance(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	spaceId, _ := args["spaceId"].(string)
	utteranceId, _ := args["utteranceId"].(string)
	text, _ := args["text"].(string)
	speakerId, _ := args["speakerId"].(string)
	speakerName, _ := args["speakerName"].(string)

	if spaceId == "" || utteranceId == "" || text == "" || speakerId == "" {
		return nil, fmt.Errorf("scoreUtterance requires spaceId, utteranceId, text, speakerId")
	}

	utterance := polyphon.Utterance{
		ID:            utteranceId,
		SpaceId:       spaceId,
		ParticipantId: speakerId,
		SpeakerName:   speakerName,
		Text:          text,
		Timestamp:     time.Now().UTC(),
	}

	// Parse candidates from args.
	var candidates []polyphon.AgentCandidate
	if candidatesRaw, ok := args["candidates"]; ok && candidatesRaw != nil {
		candidateBytes, err := json.Marshal(candidatesRaw)
		if err != nil {
			return nil, fmt.Errorf("marshal candidates: %w", err)
		}
		if err := json.Unmarshal(candidateBytes, &candidates); err != nil {
			return nil, fmt.Errorf("unmarshal candidates: %w", err)
		}
	}

	decision := c.scoreEngine.ProcessUtterance(ctx, utterance, candidates)

	// Convert decision to MemoryNode payload.
	decisionPayload := map[string]any{
		"spaceId":     decision.SpaceId,
		"utteranceId": decision.UtteranceId,
		"action":      decision.Action,
		"confidence":  decision.Confidence,
		"hasWinner":   decision.HasWinner(),
	}
	if decision.HasWinner() {
		decisionPayload["winnerId"] = decision.Winner.AgentId
		decisionPayload["winnerName"] = decision.Winner.AgentName
		decisionPayload["winnerScore"] = decision.Winner.TotalScore
		decisionPayload["winnerReason"] = decision.Winner.Reason
	}

	payloadBytes, err := json.Marshal(decisionPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal decision: %w", err)
	}

	node := memorynodes.MemoryNode{
		ID:        "cognition-decision:" + utteranceId,
		Concept:   "integration:cognition:decision",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// handleTrackPresence wraps upsertParticipantPresence for DSL access.
func (c *CognitionIntegration) handleTrackPresence(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	participantId, _ := args["participantId"].(string)
	spaceId, _ := args["spaceId"].(string)
	state, _ := args["state"].(string)

	if participantId == "" || spaceId == "" || state == "" {
		return nil, fmt.Errorf("trackPresence requires participantId, spaceId, state")
	}

	var metadata map[string]any
	if m, ok := args["metadata"].(map[string]any); ok {
		metadata = m
	}

	// Use the existing presence upsert logic.
	err := c.upsertParticipantPresence(ctx, spaceId, participantId, state, "", "", "", "", metadata)
	if err != nil {
		return nil, fmt.Errorf("trackPresence: %w", err)
	}

	// Return a confirmation node.
	payloadBytes, _ := json.Marshal(map[string]any{
		"participantId": participantId,
		"spaceId":       spaceId,
		"state":         state,
		"updatedAt":     time.Now().UTC().Format(time.RFC3339),
	})

	node := memorynodes.MemoryNode{
		ID:        "presence:" + presenceRecordId(participantId),
		Concept:   "integration:cognition:presence",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}
