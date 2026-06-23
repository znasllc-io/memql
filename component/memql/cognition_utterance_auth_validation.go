package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// validateCognitionUtteranceWriteAuthorization prevents participant spoofing for
// cognition utterance writes. System actors may write AI/system utterances.
func (e *MemQLEngine) validateCognitionUtteranceWriteAuthorization(ctx context.Context, payload map[string]any, actor string) error {
	if e == nil || payload == nil {
		return nil
	}

	participantId := strings.TrimSpace(stringFromAny(payload["participantId"]))
	partitionId := strings.TrimSpace(stringFromAny(payload["partitionId"]))
	if participantId == "" || partitionId == "" {
		return fmt.Errorf("cognition utterance requires participantId and partitionId")
	}

	participantPayload, err := e.getLatestParticipantPayload(ctx, participantId)
	if err != nil {
		return err
	}

	// Compare canonicalized space ids on both sides. The participant
	// row's partitionId gets auto-canonicalized at insert time (see
	// canonicalizeRelationshipFields) so it always reads as a full
	// `<partition>:v1:cognition:space:<slug>`. The incoming
	// `partitionId` from the utterance payload may be either canonical
	// or a bare slug -- the polyphon bridge agent in particular
	// passes the bare slug. Strict string equality used to reject
	// the bridge with "participant does not belong to space" even
	// though they referenced the same row.
	participantPartitionId := strings.TrimSpace(stringFromAny(participantPayload["partitionId"]))
	if participantPartitionId == "" {
		return fmt.Errorf("participant %q does not belong to space %q", participantId, partitionId)
	}
	canonicalParticipantPartitionId, perr := e.canonicalizeIdValue(ctx, participantPartitionId, "v1:cognition:space")
	if perr != nil {
		return fmt.Errorf("participant %q does not belong to space %q: %w", participantId, partitionId, perr)
	}
	canonicalRequestPartitionId, rerr := e.canonicalizeIdValue(ctx, partitionId, "v1:cognition:space")
	if rerr != nil {
		return fmt.Errorf("participant %q does not belong to space %q: %w", participantId, partitionId, rerr)
	}
	if canonicalParticipantPartitionId != canonicalRequestPartitionId {
		return fmt.Errorf("participant %q does not belong to space %q", participantId, partitionId)
	}

	// Use the participant node's participantType as the source of truth
	// The utterance payload may or may not include participantType, but we always
	// trust what's in the participant node record
	participantType := strings.TrimSpace(stringFromAny(participantPayload["participantType"]))

	identity, _ := auth.UserIdentityFromContext(ctx)
	if participantType == "si" || participantType == "system" {
		// Allow transcript-only realtime voice utterances from any authenticated user
		// These are just transcriptions of what the AI already said via voice
		if isTranscriptOnlyUtterance(payload) {
			return nil
		}
		// Otherwise, require system actor for AI/system utterances
		if !isSystemActor(identity, actor) {
			return fmt.Errorf("only system actors may write %s utterances", participantType)
		}
		// Specialists never speak to humans directly. Their replies
		// flow through the assistant via the askSpecialist tool.
		if participantType == "si" {
			agentId := strings.TrimSpace(stringFromAny(participantPayload["agentId"]))
			if agentId != "" {
				if role, err := e.getAgentRole(ctx, agentId); err == nil && role == "specialist" {
					return fmt.Errorf("specialist agents may not publish utterances directly; use the askSpecialist tool from the assistant")
				}
			}
		}
		return nil
	}

	// Human utterances must be attributed to the authenticated user unless
	// an elevated backend role performs the write.
	if participantType == "human" && !hasElevatedWriteRole(identity.Role) {
		participantUserId := strings.TrimSpace(stringFromAny(participantPayload["userId"]))
		if participantUserId == "" {
			return fmt.Errorf("participant %q missing userId for authorization", participantId)
		}
		if !matchesAuthenticatedIdentity(participantUserId, actor, identity) {
			return fmt.Errorf("actor is not authorized to write as participant %q", participantId)
		}
	}

	return nil
}

func (e *MemQLEngine) getLatestParticipantPayload(ctx context.Context, participantId string) (map[string]any, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("id = ?", participantId).
		Where("concept = ?", memorynodes.ConceptCognitionParticipant).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("participant %q not found", participantId)
		}
		return nil, fmt.Errorf("load participant %q: %w", participantId, err)
	}

	payload := make(map[string]any)
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode participant %q payload: %w", participantId, err)
	}
	return payload, nil
}

func isSystemActor(identity auth.UserIdentity, actor string) bool {
	if strings.EqualFold(strings.TrimSpace(identity.Role), "system") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(actor), "system:")
}

func hasElevatedWriteRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner", "developer", "admin", "system":
		return true
	default:
		return false
	}
}

func matchesAuthenticatedIdentity(participantUserId, actor string, identity auth.UserIdentity) bool {
	participantUserId = strings.TrimSpace(participantUserId)
	if participantUserId == "" {
		return false
	}

	candidates := []string{
		strings.TrimSpace(actor),
		strings.TrimSpace(identity.Subject),
		strings.TrimSpace(identity.Email),
	}
	for _, candidate := range candidates {
		if candidate != "" && candidate == participantUserId {
			return true
		}
	}
	return false
}

// getAgentRole returns the role ("specialist" or "assistant") for
// the agent row with the given id, looking up the latest version.
// Returns "" when the agent is not found or the role field is absent.
func (e *MemQLEngine) getAgentRole(ctx context.Context, agentId string) (string, error) {
	db := e.database()
	if db == nil {
		return "", fmt.Errorf("memory engine database not configured")
	}
	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("id = ?", agentId).
		Where("concept = ?", "v1:agents:agent").
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("load agent %q: %w", agentId, err)
	}
	payload := make(map[string]any)
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode agent %q payload: %w", agentId, err)
	}
	return strings.TrimSpace(stringFromAny(payload["role"])), nil
}

func isTranscriptOnlyUtterance(payload map[string]any) bool {
	// Check if source.transcriptOnly is true
	source, ok := payload["source"].(map[string]any)
	if !ok {
		return false
	}

	transcriptOnly := strings.TrimSpace(stringFromAny(source["transcriptOnly"]))
	return strings.EqualFold(transcriptOnly, "true")
}
