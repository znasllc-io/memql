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
// active AI participants. Under the one-assistant space model
// (copresent #124) a space carries EXACTLY ONE assistant -- the owner's
// active one, auto-joined by autoJoinAI. Specialists and system agents
// never participate in spaces, so the per-user AI cap is 1. This is the
// Go-side enforcement of the declarative space.maxAgents=1 default.
const participantPerUserAgentCap = 1

// defaultMaxHumansPerSpace is the fallback human cap applied when a space
// row carries no explicit (or a non-positive) maxHumans. Mirrors the
// v1:cognition:space.maxHumans default.
const defaultMaxHumansPerSpace = 5

// validateAndStampParticipantPayload runs the engine pre-insert guard for
// v1:cognition:participant rows that target AI participants. Phase 1.4 of
// the chat-architecture plan: per-user team rosters with forUserId
// scoping, the per-user-per-space 3-cap, and GA-pin protection.
//
// Behavior summary (AI participants only -- human participants pass
// through untouched):
//
//  1. forUserId is server-stamped from the authenticated caller for
//     non-elevated callers when the field is missing. A non-elevated
//     caller who supplies an explicit forUserId that does not match
//     their authenticated subject is rejected outright -- silently
//     overriding would mask a buggy / malicious client.
//
//  2. Elevated actors (system / owner / developer / admin / writer)
//     may supply any forUserId. The autoJoinAI automation relies on
//     this to land the owner GA with forUserId = space-creator.
//
//  3. Per-user-per-space 3-cap. Counts current-active AI participants
//     in the same space that share the resolved forUserId, excluding
//     the participant id being inserted. Reject when post-insert
//     count would exceed 3 (i.e., new row is status='active' AND
//     current count is already at the cap with the new id not
//     already counted).
//
//  4. GA-pin protection. If the latest version of the participant id
//     being superseded carries isGroupGA=true and the new row's
//     status is 'left', reject for non-elevated callers. The owner
//     GA cannot be removed via the Roster tab; only system actors
//     (e.g., space-deletion cleanup) may demote it.
//
// Called from executor.executeInsert; not part of the public API.
func (e *MemQLEngine) validateAndStampParticipantPayload(ctx context.Context, payload map[string]any, mutationId, actor string) error {
	if payload == nil {
		return fmt.Errorf("v1:cognition:participant: payload is required")
	}

	participantType := strings.TrimSpace(stringFromAny(payload["participantType"]))
	if participantType != "si" {
		// Human participants are capped per-space by the space's
		// maxHumans (default 5). Other non-AI types (if any) pass
		// through untouched.
		if participantType == "human" {
			return e.validateHumanParticipantCap(ctx, payload, mutationId)
		}
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
			partitionId := strings.TrimSpace(stringFromAny(payload["partitionId"]))
			if partitionId == "" {
				return fmt.Errorf("v1:cognition:participant: partitionId required")
			}

			activeCount, err := e.countActiveAIParticipantsForUser(
				ctx, partitionId, resolvedForUser, strings.TrimSpace(mutationId))
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

// countActiveAIParticipantsForUser counts distinct AI participant ids in
// the given space whose latest version is status='active' and whose
// forUserId matches the supplied user. Excludes the supplied excludeId
// (the participant id about to be inserted) so a re-version of the same
// agent is not double-counted.
//
// Implementation walks the time-series rows for the space and dedups by
// id in Go. Spaces typically carry fewer than 50 participants over their
// lifetime, so a full scan + Go-side fold is acceptable -- and it avoids
// dialect-specific DISTINCT ON wiring through bun.
func (e *MemQLEngine) countActiveAIParticipantsForUser(ctx context.Context, partitionId, forUserId, excludeId string) (int, error) {
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
		Where("payload->>'partitionId' = ?", partitionId).
		Where("payload->>'participantType' = ?", "si").
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("scan participants for space %q: %w", partitionId, err)
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

// validateHumanParticipantCap enforces the space's maxHumans cap at
// human-join time. Under the one-assistant model (copresent #124) a
// space holds 1+ humans (max 5 by default) plus exactly one assistant;
// this is the human side of that invariant. Only active inserts grow
// the count (a 'left'/'idle' insert never does). Idempotent re-joins
// land on the same content-addressed participant id, which is excluded
// from the count, so re-joining an existing member never trips the cap.
func (e *MemQLEngine) validateHumanParticipantCap(ctx context.Context, payload map[string]any, mutationId string) error {
	newStatus := strings.TrimSpace(stringFromAny(payload["status"]))
	if !humanInsertIsActive(newStatus) {
		return nil
	}

	partitionId := strings.TrimSpace(stringFromAny(payload["partitionId"]))
	if partitionId == "" {
		return fmt.Errorf("v1:cognition:participant: partitionId required")
	}

	maxHumans, err := e.spaceMaxHumans(ctx, partitionId)
	if err != nil {
		return fmt.Errorf("v1:cognition:participant: human cap check failed: %w", err)
	}

	activeCount, err := e.countActiveHumanParticipants(ctx, partitionId, strings.TrimSpace(mutationId))
	if err != nil {
		return fmt.Errorf("v1:cognition:participant: human cap check failed: %w", err)
	}

	if humanCapExceeded(activeCount, maxHumans) {
		return fmt.Errorf(
			"v1:cognition:participant: space is full (max %d humans)", maxHumans)
	}
	return nil
}

// humanInsertIsActive reports whether an insert with the given status
// grows the active human count. An empty status defaults to active
// (mirrors joinSpaceAsHuman's coalesce(status, "active")).
func humanInsertIsActive(status string) bool {
	return status == "" || strings.EqualFold(status, "active")
}

// humanCapExceeded reports whether adding one more active human to a
// space that already has activeCount active humans would exceed the
// cap. Pure arithmetic, split out so the cap rule is unit-testable
// without a database.
func humanCapExceeded(activeCount, maxHumans int) bool {
	return activeCount+1 > maxHumans
}

// spaceMaxHumans reads the latest version of the space row and returns
// its maxHumans, falling back to defaultMaxHumansPerSpace when the row
// is missing the field or carries a non-positive value.
func (e *MemQLEngine) spaceMaxHumans(ctx context.Context, partitionId string) (int, error) {
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
		Where("concept = ?", memorynodes.ConceptCognitionSpace).
		Where("id = ?", partitionId).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return defaultMaxHumansPerSpace, nil
		}
		return 0, fmt.Errorf("scan space %q: %w", partitionId, err)
	}
	if len(nodes) == 0 {
		return defaultMaxHumansPerSpace, nil
	}

	var payload map[string]any
	if jerr := json.Unmarshal(nodes[0].Payload, &payload); jerr != nil {
		return defaultMaxHumansPerSpace, nil
	}
	if n := intFromAny(payload["maxHumans"]); n > 0 {
		return n, nil
	}
	return defaultMaxHumansPerSpace, nil
}

// countActiveHumanParticipants counts distinct human participant ids in
// the given space whose latest version is status='active'. Excludes
// excludeId (the participant id about to be inserted) so an idempotent
// re-join of the same member is not counted twice. Mirrors the scan +
// Go-side dedup approach of countActiveAIParticipantsForUser.
func (e *MemQLEngine) countActiveHumanParticipants(ctx context.Context, partitionId, excludeId string) (int, error) {
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
		Where("payload->>'partitionId' = ?", partitionId).
		Where("payload->>'participantType' = ?", "human").
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("scan human participants for space %q: %w", partitionId, err)
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
		count++
	}

	return count, nil
}
