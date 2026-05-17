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
	spaceId := strings.TrimSpace(stringFromAny(payload["spaceId"]))
	if participantId == "" || spaceId == "" {
		return fmt.Errorf("cognition utterance requires participantId and spaceId")
	}

	participantPayload, err := e.getLatestParticipantPayload(ctx, participantId)
	if err != nil {
		return err
	}

	// Compare canonicalized space ids on both sides. The participant
	// row's spaceId gets auto-canonicalized at insert time (see
	// canonicalizeRelationshipFields) so it always reads as a full
	// `<partition>:v1:cognition:space:<slug>`. The incoming
	// `spaceId` from the utterance payload may be either canonical
	// or a bare slug -- the polyphon bridge agent in particular
	// passes the bare slug. Strict string equality used to reject
	// the bridge with "participant does not belong to space" even
	// though they referenced the same row.
	participantSpaceId := strings.TrimSpace(stringFromAny(participantPayload["spaceId"]))
	if participantSpaceId == "" {
		return fmt.Errorf("participant %q does not belong to space %q", participantId, spaceId)
	}
	canonicalParticipantSpaceId, perr := e.canonicalizeIdValue(ctx, participantSpaceId, "v1:cognition:space")
	if perr != nil {
		return fmt.Errorf("participant %q does not belong to space %q: %w", participantId, spaceId, perr)
	}
	canonicalRequestSpaceId, rerr := e.canonicalizeIdValue(ctx, spaceId, "v1:cognition:space")
	if rerr != nil {
		return fmt.Errorf("participant %q does not belong to space %q: %w", participantId, spaceId, rerr)
	}
	if canonicalParticipantSpaceId != canonicalRequestSpaceId {
		return fmt.Errorf("participant %q does not belong to space %q", participantId, spaceId)
	}

	// Use the participant node's participantType as the source of truth
	// The utterance payload may or may not include participantType, but we always
	// trust what's in the participant node record
	participantType := strings.TrimSpace(stringFromAny(participantPayload["participantType"]))

	identity, _ := auth.UserIdentityFromContext(ctx)
	if participantType == "si" || participantType == "system" {
		// Allow transcript-only realtime voice utterances from any authenticated user
		// These are just transcriptions of what the SI already said via voice
		if isTranscriptOnlyUtterance(payload) {
			return nil
		}
		// Otherwise, require system actor for AI/system utterances
		if !isSystemActor(identity, actor) {
			return fmt.Errorf("only system actors may write %s utterances", participantType)
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

func isTranscriptOnlyUtterance(payload map[string]any) bool {
	// Check if source.transcriptOnly is true
	source, ok := payload["source"].(map[string]any)
	if !ok {
		return false
	}

	transcriptOnly := strings.TrimSpace(stringFromAny(source["transcriptOnly"]))
	return strings.EqualFold(transcriptOnly, "true")
}
