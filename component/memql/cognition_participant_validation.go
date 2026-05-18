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

// participantPerUserAgentCap is the per-(user, space) maximum number of
// active SI participants. Mirrors the existing per-space 3-agent UX rule
// the polyphon path used to enforce, reinterpreted per-user as part of
// the activity-model architectural correction (humans are 1-active-space,
// agents are unbounded across spaces but capped per-team-per-space).
const participantPerUserAgentCap = 3

// validateAndStampParticipantPayload runs the engine pre-insert guard for
// v1:cognition:participant rows that target SI participants. Phase 1.4 of
// the chat-architecture plan: per-user team rosters with forUserId
// scoping, the per-user-per-space 3-cap, and GA-pin protection.
//
// Behavior summary (SI participants only -- human participants pass
// through untouched):
//
//   1. forUserId is server-stamped from the authenticated caller for
//      non-elevated callers when the field is missing. A non-elevated
//      caller who supplies an explicit forUserId that does not match
//      their authenticated subject is rejected outright -- silently
//      overriding would mask a buggy / malicious client.
//
//   2. Elevated actors (system / owner / developer / admin / writer)
//      may supply any forUserId. The autoJoinSI automation relies on
//      this to land the owner GA with forUserId = space-creator.
//
//   3. Per-user-per-space 3-cap. Counts current-active SI participants
//      in the same space that share the resolved forUserId, excluding
//      the participant id being inserted. Reject when post-insert
//      count would exceed 3 (i.e., new row is status='active' AND
//      current count is already at the cap with the new id not
//      already counted).
//
//   4. GA-pin protection. If the latest version of the participant id
//      being superseded carries isGroupGA=true and the new row's
//      status is 'left', reject for non-elevated callers. The owner
//      GA cannot be removed via the Roster tab; only system actors
//      (e.g., space-deletion cleanup) may demote it.
//
// Called from executor.executeInsert; not part of the public API.
func (e *MemQLEngine) validateAndStampParticipantPayload(ctx context.Context, payload map[string]any, mutationId, actor string) error {
	if payload == nil {
		return fmt.Errorf("v1:cognition:participant: payload is required")
	}

	participantType := strings.TrimSpace(stringFromAny(payload["participantType"]))
	if participantType != "si" {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	authenticatedSubject := strings.TrimSpace(identity.Subject)
	elevated := hasElevatedWriteRole(identity.Role) || isSystemActor(identity, actor)

	rawForUser := strings.TrimSpace(stringFromAny(payload["forUserId"]))
	switch {
	case elevated && rawForUser != "":
		payload["forUserId"] = rawForUser
	case elevated && rawForUser == "":
		// Elevated caller chose not to supply -- pass through. Downstream
		// roster queries will reveal the gap if any. We do NOT default-
		// stamp a system actor's identity onto the participant; that would
		// silently change ownership for any system-side write.
	case !elevated && rawForUser == "" && authenticatedSubject != "":
		payload["forUserId"] = authenticatedSubject
		rawForUser = authenticatedSubject
	case !elevated && rawForUser != "" && authenticatedSubject == "":
		return fmt.Errorf("v1:cognition:participant: forUserId provided but no authenticated user in context")
	case !elevated && rawForUser != authenticatedSubject:
		return fmt.Errorf(
			"v1:cognition:participant: forUserId %q does not match authenticated user %q",
			rawForUser, authenticatedSubject)
	}

	resolvedForUser := strings.TrimSpace(stringFromAny(payload["forUserId"]))

	// GA-pin protection: a non-elevated caller may not flip an
	// isGroupGA=true row to status='left'. Look up the existing latest
	// version of the same id and reject if the conditions match.
	if !elevated {
		newStatus := strings.TrimSpace(stringFromAny(payload["status"]))
		if strings.EqualFold(newStatus, "left") && strings.TrimSpace(mutationId) != "" {
			prior, err := e.getLatestParticipantPayload(ctx, strings.TrimSpace(mutationId))
			if err == nil && prior != nil {
				priorIsGroupGA, _ := prior["isGroupGA"].(bool)
				if priorIsGroupGA {
					return fmt.Errorf(
						"v1:cognition:participant: owner Assistant cannot be removed via the Roster tab")
				}
			}
		}
	}

	// Per-user-per-space cap. Skip when forUserId is empty (elevated
	// system writes that don't bind to a user are deliberate); skip when
	// status is not active (a 'left' or 'idle' insert never grows the
	// active count).
	if resolvedForUser != "" {
		newStatus := strings.TrimSpace(stringFromAny(payload["status"]))
		if newStatus == "" || strings.EqualFold(newStatus, "active") {
			spaceId := strings.TrimSpace(stringFromAny(payload["spaceId"]))
			if spaceId == "" {
				return fmt.Errorf("v1:cognition:participant: spaceId required")
			}

			activeCount, err := e.countActiveSIParticipantsForUser(
				ctx, spaceId, resolvedForUser, strings.TrimSpace(mutationId))
			if err != nil {
				return fmt.Errorf("v1:cognition:participant: cap check failed: %w", err)
			}

			// The new row is one more active. Compare against the cap.
			postInsertCount := activeCount + 1
			if postInsertCount > participantPerUserAgentCap {
				return fmt.Errorf(
					"v1:cognition:participant: per-user agent cap (%d) reached in this space",
					participantPerUserAgentCap)
			}
		}
	}

	return nil
}

// countActiveSIParticipantsForUser counts distinct SI participant ids in
// the given space whose latest version is status='active' and whose
// forUserId matches the supplied user. Excludes the supplied excludeId
// (the participant id about to be inserted) so a re-version of the same
// agent is not double-counted.
//
// Implementation walks the time-series rows for the space and dedups by
// id in Go. Spaces typically carry fewer than 50 participants over their
// lifetime, so a full scan + Go-side fold is acceptable -- and it avoids
// dialect-specific DISTINCT ON wiring through bun.
func (e *MemQLEngine) countActiveSIParticipantsForUser(ctx context.Context, spaceId, forUserId, excludeId string) (int, error) {
	if e == nil {
		return 0, fmt.Errorf("engine is nil")
	}
	db := e.database()
	if db == nil {
		return 0, fmt.Errorf("memory engine database not configured")
	}

	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptCognitionParticipant).
		Where("payload->>'spaceId' = ?", spaceId).
		Where("payload->>'participantType' = ?", "si").
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("scan participants for space %q: %w", spaceId, err)
	}

	seen := make(map[string]struct{})
	count := 0
	for _, node := range nodes {
		if node.ID == excludeId {
			continue
		}
		if _, dup := seen[node.ID]; dup {
			continue
		}
		seen[node.ID] = struct{}{}

		var payload map[string]any
		if jerr := json.Unmarshal(node.Payload, &payload); jerr != nil {
			continue
		}
		status := strings.TrimSpace(stringFromAny(payload["status"]))
		if !strings.EqualFold(status, "active") {
			continue
		}
		rowForUser := strings.TrimSpace(stringFromAny(payload["forUserId"]))
		if rowForUser != forUserId {
			continue
		}
		count++
	}

	return count, nil
}
