package memql

// Badge registration lifecycle handlers (memql#2513). Mirrors the
// worker-token pair (worker_token_handlers.go): caller resolution off
// the stream claims, admin-gated owner override, hash-only
// persistence on a v1:identity:identity row, ownership-or-admin
// revoke, engine writes under the system actor.
//
// The GRANT exchange (badge id -> short-lived class="badge" operator
// token) is NOT here -- it lives on the identity HTTP surface
// (POST /auth/badge/grant), because minting requires the identity
// node's signing keys.

import (
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/badge"
)

// handleCreateBadge registers a badge for an operator. The plaintext
// badge id arrives once in the request (over the TLS stream), is
// hashed immediately, and never persists or echoes back.
func (s *streamSession) handleCreateBadge(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.CreateBadgeMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.CreateBadgeResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_CreateBadgeResult{
				CreateBadgeResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	caller, err := auth.UserIdentityFromContext(s.stream.Context())
	if err != nil {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller identity not resolved",
		})
	}
	callerUserId := strings.TrimSpace(caller.Subject)
	if callerUserId == "" {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller subject empty",
		})
	}

	ownerUserId := strings.TrimSpace(msg.GetOwnerUserId())
	if ownerUserId == "" {
		ownerUserId = callerUserId
	} else if ownerUserId != callerUserId && !isAdminRole(caller.Role) {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "permission_denied",
			ErrorMessage: "only admins may register badges for other users",
		})
	}

	label := strings.TrimSpace(msg.GetLabel())
	if label == "" {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "label is required",
		})
	}
	keyHash := badge.HashBadgeId(msg.GetBadgeId())
	if keyHash == "" {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "badge_id is required",
		})
	}

	store := &badge.Store{Engine: s.service.engine, Logger: s.service.logger}
	ctx := contextWithSystemActor(s.stream.Context())

	// One badge id maps to one operator: a duplicate registration
	// would make the grant exchange's single-row lookup ambiguous, so
	// refuse it rather than shadow the existing owner.
	if existing, err := store.LookupByKeyHash(ctx, keyHash); err != nil {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "lookup_failed",
			ErrorMessage: err.Error(),
		})
	} else if existing != nil {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "already_registered",
			ErrorMessage: "a badge with this identifier is already registered; revoke it first",
		})
	}

	identityId, err := badge.NewId()
	if err != nil {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "internal",
			ErrorMessage: "id mint failed: " + err.Error(),
		})
	}
	if err := store.Create(ctx, identityId, ownerUserId, label, keyHash, callerUserId); err != nil {
		return send(&memqlv1.CreateBadgeResult{
			ErrorCode:    "persist_failed",
			ErrorMessage: err.Error(),
		})
	}

	return send(&memqlv1.CreateBadgeResult{
		Success:     true,
		IdentityId:  badge.CanonicalId(identityId),
		OwnerUserId: ownerUserId,
	})
}

// handleRevokeBadge flips active=false on the badge identity row. The
// caller must own the row (or be an admin).
func (s *streamSession) handleRevokeBadge(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RevokeBadgeMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.RevokeBadgeResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RevokeBadgeResult{
				RevokeBadgeResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	caller, err := auth.UserIdentityFromContext(s.stream.Context())
	if err != nil {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller identity not resolved",
		})
	}
	callerUserId := strings.TrimSpace(caller.Subject)
	if callerUserId == "" {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller subject empty",
		})
	}

	identityId := strings.TrimSpace(msg.GetIdentityId())
	if identityId == "" {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "identity_id required",
		})
	}

	store := &badge.Store{Engine: s.service.engine, Logger: s.service.logger}
	// The WRITE runs as the system actor: a machine-credential
	// v1:identity:identity row (badge / worker_token / node_token / ...) is
	// only admitted from one (the memql#2513 credential-actor guard).
	ctx := contextWithSystemActor(s.stream.Context())

	// The READ does NOT. badgesForSelf is self-scoped on
	// `userId==actor.userId` (memql#3178), and the system actor's subject is
	// "polyphon-bridge-agent", not the caller -- reading under it would
	// return zero rows and wrongly deny every non-admin revoking their OWN
	// badge. The verifier interceptor stamps only CLAIMS onto the stream
	// context; ensureAccess converts them to the AccessContext that
	// resolveActorPath actually reads, and it has to be ATTACHED to the ctx
	// (returning it is not enough) -- same step handleQuery takes for
	// currentUser, and the same silent zero-row no-op if skipped (memql#216).
	readCtx := auth.ContextWithAccess(s.stream.Context(), s.ensureAccess(s.stream.Context()))

	rows, err := store.ListForSelf(readCtx)
	if err != nil {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "lookup_failed",
			ErrorMessage: err.Error(),
		})
	}
	canonical := badge.CanonicalId(identityId)
	owned := false
	for _, r := range rows {
		if r.ID == canonical {
			owned = true
			break
		}
	}
	if !owned && !isAdminRole(caller.Role) {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "permission_denied",
			ErrorMessage: "caller does not own this badge",
		})
	}

	if err := store.Revoke(ctx, identityId); err != nil {
		return send(&memqlv1.RevokeBadgeResult{
			ErrorCode:    "persist_failed",
			ErrorMessage: err.Error(),
		})
	}
	return send(&memqlv1.RevokeBadgeResult{Success: true})
}
