package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/visionarys-io/memql/component/auth"
)

// validateAndStampPrivateUtterancePayload is the engine pre-insert guard for
// v1:cognition:privateUtterance rows. It is the load-bearing wall for the
// chat-architecture two-thread model: every private utterance is scoped to
// exactly one human via forUserId, and that field must come from the
// authenticated caller -- not from a request payload -- so a malicious or
// buggy client cannot land a row in someone else's private thread.
//
// Behaviour:
//
//   1. forUserId is server-stamped from the authenticated caller's identity
//      (auth.UserIdentityFromContext(ctx).Subject) for non-elevated callers.
//      Any client-supplied forUserId is overridden silently. This is the
//      same pattern as identity's partition-stamping.
//
//   2. Elevated actors (system / owner / developer / admin) MAY supply an
//      explicit forUserId in the payload -- this is required for paths
//      like the discussion-mode dispatch loop (Phase 6) where the loop
//      authenticates as a system actor but writes utterances on behalf of
//      a specific user. Elevated callers who omit forUserId fall back to
//      the authenticated subject the same way non-elevated callers do.
//
//   3. The final forUserId must be non-empty. An empty forUserId would
//      defeat the subscription rewriter's per-user filter (the row would
//      be invisible to every user including its intended recipient).
//      Reject with an explicit error rather than letting the row land in
//      limbo.
//
// Called from executor.executeInsert; not part of the public API.
func validateAndStampPrivateUtterancePayload(ctx context.Context, payload map[string]any, actor string) error {
	if payload == nil {
		return fmt.Errorf("v1:cognition:privateUtterance: payload is required")
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	authenticatedUserId := strings.TrimSpace(identity.Subject)

	rawForUserId := strings.TrimSpace(stringFromAny(payload["forUserId"]))
	elevated := hasElevatedWriteRole(identity.Role) || isSystemActor(identity, actor)

	switch {
	case elevated && rawForUserId != "":
		// Elevated caller supplied an explicit target user. Trust it;
		// the caller has authority (e.g., discussion-mode dispatcher
		// writing on behalf of an active human). Audit logs are the
		// downstream check on misuse.
		payload["forUserId"] = rawForUserId
	case authenticatedUserId != "":
		// Non-elevated caller, OR elevated caller who omitted the field:
		// stamp the authenticated subject. Any client-supplied value
		// from a non-elevated caller is overwritten -- this is the
		// bleed-defense.
		payload["forUserId"] = authenticatedUserId
	default:
		return fmt.Errorf(
			"v1:cognition:privateUtterance: forUserId required and no authenticated user in context")
	}

	final := strings.TrimSpace(stringFromAny(payload["forUserId"]))
	if final == "" {
		return fmt.Errorf("v1:cognition:privateUtterance: forUserId resolved to empty value")
	}

	return nil
}
