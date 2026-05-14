package memql

import (
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/auth"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/identity/workertoken"
)

// handleCreateWorkerToken mints a fresh worker token. The plain
// token is shown ONCE in the reply and never persisted server-side
// -- only its SHA-256 hash lands on the v1:identity:identity row.
//
// Caller flow:
//
//   1. Caller authenticates with their own JWT or PAT (the
//      interceptor rejects worker tokens on this RPC).
//   2. Server resolves the caller's userId from claims; uses that
//      as both ownerUserId and registeredBy unless owner_user_id
//      is set explicitly (admin path).
//   3. Mints token via workertoken.Mint, persists the identity
//      row, returns plain_token in the reply.
//   4. Frontend pastes plain_token into the cockpit-worker config
//      on the target machine. The next connect attempt resolves
//      via queryWorkerTokenByKeyHash.
func (s *streamSession) handleCreateWorkerToken(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.CreateWorkerTokenMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.CreateWorkerTokenResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_CreateWorkerTokenResult{
				CreateWorkerTokenResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	caller, err := auth.UserIdentityFromContext(s.stream.Context())
	if err != nil {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller identity not resolved",
		})
	}
	callerUserId := strings.TrimSpace(caller.Subject)
	if callerUserId == "" {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller subject empty",
		})
	}

	ownerUserId := strings.TrimSpace(msg.GetOwnerUserId())
	if ownerUserId == "" {
		ownerUserId = callerUserId
	} else if ownerUserId != callerUserId && !isAdminRole(caller.Role) {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "permission_denied",
			ErrorMessage: "only admins may mint worker tokens for other users",
		})
	}

	name := strings.TrimSpace(msg.GetName())
	if name == "" {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "name is required",
		})
	}

	expiresAt := parseOptionalRFC3339(msg.GetExpiresAt())

	plain, hash, err := workertoken.Mint()
	if err != nil {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "internal",
			ErrorMessage: "token mint failed: " + err.Error(),
		})
	}

	identityId, err := workertoken.NewId()
	if err != nil {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "internal",
			ErrorMessage: "id mint failed: " + err.Error(),
		})
	}

	store := &workertoken.Store{Engine: s.service.engine, Logger: s.service.logger}
	ctx := contextWithSystemActor(s.stream.Context())
	if err := store.Create(ctx, identityId, ownerUserId, name, hash, callerUserId, expiresAt); err != nil {
		return send(&memqlv1.CreateWorkerTokenResult{
			ErrorCode:    "persist_failed",
			ErrorMessage: err.Error(),
		})
	}

	return send(&memqlv1.CreateWorkerTokenResult{
		Success:     true,
		PlainToken:  plain,
		IdentityId:  workertoken.CanonicalId(identityId),
		OwnerUserId: ownerUserId,
	})
}

// handleRevokeWorkerToken flips active=false on the worker_token
// identity row. The caller must own the row (or be an admin).
func (s *streamSession) handleRevokeWorkerToken(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RevokeWorkerTokenMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.RevokeWorkerTokenResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RevokeWorkerTokenResult{
				RevokeWorkerTokenResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	caller, err := auth.UserIdentityFromContext(s.stream.Context())
	if err != nil {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller identity not resolved",
		})
	}
	callerUserId := strings.TrimSpace(caller.Subject)
	if callerUserId == "" {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller subject empty",
		})
	}

	identityId := strings.TrimSpace(msg.GetIdentityId())
	if identityId == "" {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "bad_request",
			ErrorMessage: "identity_id required",
		})
	}

	store := &workertoken.Store{Engine: s.service.engine, Logger: s.service.logger}
	ctx := contextWithSystemActor(s.stream.Context())

	// Verify the caller owns the row (or is admin) before flipping
	// active. The lookup is by canonical id so the same engine row
	// the revoke targets is the one we authorize against.
	rows, err := store.ListForUser(ctx, callerUserId)
	if err != nil {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "lookup_failed",
			ErrorMessage: err.Error(),
		})
	}
	canonical := workertoken.CanonicalId(identityId)
	owned := false
	for _, r := range rows {
		if r.ID == canonical {
			owned = true
			break
		}
	}
	if !owned && !isAdminRole(caller.Role) {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "permission_denied",
			ErrorMessage: "caller does not own this worker token",
		})
	}

	if err := store.Revoke(ctx, identityId); err != nil {
		return send(&memqlv1.RevokeWorkerTokenResult{
			ErrorCode:    "persist_failed",
			ErrorMessage: err.Error(),
		})
	}
	return send(&memqlv1.RevokeWorkerTokenResult{Success: true})
}

// isAdminRole reports whether the cluster-wide role string carries
// "admin" or "owner" privileges. Mirrors auth.IsAdmin / auth.IsOwner
// without requiring a UserContext (those want a fully-resolved
// access context; we only have a token-derived role string here).
func isAdminRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin", "owner":
		return true
	}
	return false
}

func parseOptionalRFC3339(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}
