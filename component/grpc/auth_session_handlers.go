// Package memql session-revocation handlers.
//
// Two gRPC handlers live here plus a small lookup helper:
//
//   - handleRevokeCurrentSession: per-device sign-out. Hashes the
//     caller's bearer token (read off the gRPC metadata), looks up
//     the matching v1:identity:authSession row, and stamps it
//     revoked. Idempotent: if no row is found we still succeed so
//     the frontend can clear local state without a retry loop.
//   - handleRevokeAllSessions: cross-device sign-out. Resolves the
//     caller's userId from the AccessContext, lists every session
//     row owned by them, and revokes every row that isn't already
//     revoked / past expiresAt.
//
// The bearer token never crosses an envelope -- only its SHA-256
// hash. Mirrors the v1:identity:invitation contract used by the
// guest-invite flow (see guest_handlers.go).

package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// authSessionSummary is the minimal projection the session handlers
// (and the revocation middleware) read back from a session row.
type authSessionSummary struct {
	ID             string
	UserId         string
	IdentityId     string
	Subject        string
	TokenHash      string
	Source         string
	ClientLabel    string
	ExpiresAt      time.Time
	LastActivityAt time.Time
	RevokedAt      time.Time
	RevokedReason  string
	CreatedAt      time.Time
}

// HashBearerToken returns the lowercase hex SHA-256 of a bearer
// token. Exported because the BFF token-exchange handler and the
// auth middleware both need it; keeping the algorithm in one place
// stops drift between issuance and lookup.
func HashBearerToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// bearerTokenFromIncomingContext pulls the raw bearer token out of
// the gRPC `authorization` metadata header. Returns "" when the
// stream is not bearer-authenticated (guest streams, no-auth dev,
// missing header).
func bearerTokenFromIncomingContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		values = md.Get("Authorization")
	}
	if len(values) == 0 {
		return ""
	}
	parts := strings.Fields(strings.TrimSpace(values[0]))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// LookupAuthSessionByTokenHash runs authSessionByTokenHash and
// returns the matching session (or nil when no row is found).
// Exported so the auth middleware can reuse the same projection.
func LookupAuthSessionByTokenHash(ctx context.Context, engine *memqlengine.MemQLEngine, tokenHash string) (*authSessionSummary, error) {
	if engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(tokenHash) == "" {
		return nil, fmt.Errorf("tokenHash required")
	}
	query := fmt.Sprintf(`query authSessionByTokenHash(tokenHash: "%s")`, tokenHash)
	result, err := engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query auth session: %w", err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, nil
	}
	return parseAuthSessionNode(result.Bundle.Nodes[0]), nil
}

// listAuthSessionsForSubject returns every session row owned by the
// JWT subject (revoked + active). The all-sessions revoke handler
// iterates over the result and only re-revokes rows still in scope.
// Subject is the canonical key because it's set unconditionally at
// token-issuance time, even before the user row has been written by
// the magic-link verifier on a first-time login.
func listAuthSessionsForSubject(ctx context.Context, engine *memqlengine.MemQLEngine, subject string) ([]*authSessionSummary, error) {
	if engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(subject) == "" {
		return nil, fmt.Errorf("subject required")
	}
	query := fmt.Sprintf(`query authSessionsForSubject(subject: %q)`, subject)
	result, err := engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query auth sessions for subject: %w", err)
	}
	if result == nil || result.Bundle == nil {
		return nil, nil
	}
	out := make([]*authSessionSummary, 0, len(result.Bundle.Nodes))
	for _, node := range result.Bundle.Nodes {
		if summary := parseAuthSessionNode(node); summary != nil {
			out = append(out, summary)
		}
	}
	return out, nil
}

// parseAuthSessionNode turns a graph-bundle node into the typed
// projection used by handlers + middleware. Mirrors the helper inside
// lookupInvitationByTokenHash but specific to the authSession shape.
func parseAuthSessionNode(node *memqlv1.MemoryNode) *authSessionSummary {
	if node == nil || node.Payload == nil {
		return nil
	}
	fields := node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	getStr := func(key string) string {
		v, ok := fields[key]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(v.GetStringValue())
	}
	parseTime := func(key string) time.Time {
		s := getStr(key)
		if s == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	id := getStr("id")
	if id == "" {
		id = node.Id
	}
	return &authSessionSummary{
		ID:             id,
		UserId:         getStr("userId"),
		IdentityId:     getStr("identityId"),
		Subject:        getStr("subject"),
		TokenHash:      getStr("tokenHash"),
		Source:         getStr("source"),
		ClientLabel:    getStr("clientLabel"),
		ExpiresAt:      parseTime("expiresAt"),
		LastActivityAt: parseTime("lastActivityAt"),
		RevokedAt:      parseTime("revokedAt"),
		RevokedReason:  getStr("revokedReason"),
		CreatedAt:      parseTime("createdAt"),
	}
}

// handleRevokeCurrentSession revokes only the session row tied to
// the caller's current bearer token. Idempotent: no-token / no-row
// flows succeed silently so the frontend's "clear local state then
// redirect" path doesn't need to retry.
func (s *streamSession) handleRevokeCurrentSession(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RevokeCurrentSessionMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.RevokeCurrentSessionResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RevokeCurrentSessionResult{
				RevokeCurrentSessionResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.RevokeCurrentSessionResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	plain := bearerTokenFromIncomingContext(s.stream.Context())
	if plain == "" {
		return send(&memqlv1.RevokeCurrentSessionResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "no bearer token on stream metadata",
		})
	}
	tokenHash := HashBearerToken(plain)

	ctx := contextWithSystemActor(s.stream.Context())
	session, err := LookupAuthSessionByTokenHash(ctx, s.service.engine, tokenHash)
	if err != nil {
		return s.sendAuthSessionError(requestId, correlate, codes.Internal, "revoke current: lookup", err)
	}
	if session == nil {
		// The frontend still wants to clear local state. Surface
		// "no_session" so it can log the no-op for diagnostics
		// without treating it as a retryable failure.
		return send(&memqlv1.RevokeCurrentSessionResult{
			Success:   true,
			ErrorCode: "no_session",
		})
	}
	if !session.RevokedAt.IsZero() {
		// Already revoked. Still report success.
		return send(&memqlv1.RevokeCurrentSessionResult{
			Success:   true,
			SessionId: session.ID,
		})
	}

	if err := RevokeAuthSessionRow(ctx, s.service.engine, session, "user_action"); err != nil {
		return s.sendAuthSessionError(requestId, correlate, codes.Internal, "revoke current: persist", err)
	}
	return send(&memqlv1.RevokeCurrentSessionResult{
		Success:   true,
		SessionId: session.ID,
	})
}

// handleRevokeAllSessions revokes every session row owned by the
// caller. Skips rows already revoked or past expiresAt -- those are
// no-ops and a wasted write. Returns the count of rows newly stamped
// revoked.
func (s *streamSession) handleRevokeAllSessions(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RevokeAllSessionsMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.RevokeAllSessionsResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RevokeAllSessionsResult{
				RevokeAllSessionsResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.RevokeAllSessionsResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	// Use the JWT subject directly: it's populated by the auth
	// middleware on every request, while AccessContext.UserId may
	// be a fallback (subject-as-userId) on the first request after
	// signup -- we want the canonical key here, not the fallback.
	subject := ""
	if claims, ok := auth.ClaimsFromContext(s.stream.Context()); ok {
		if v, ok := claims["sub"].(string); ok {
			subject = strings.TrimSpace(v)
		}
	}
	if subject == "" {
		return send(&memqlv1.RevokeAllSessionsResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller has no resolved subject",
		})
	}

	ctx := contextWithSystemActor(s.stream.Context())
	sessions, err := listAuthSessionsForSubject(ctx, s.service.engine, subject)
	if err != nil {
		return s.sendAuthSessionError(requestId, correlate, codes.Internal, "revoke all: list", err)
	}

	now := time.Now().UTC()
	revoked := int32(0)
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		if !sess.RevokedAt.IsZero() {
			continue
		}
		if !sess.ExpiresAt.IsZero() && now.After(sess.ExpiresAt) {
			continue
		}
		if err := RevokeAuthSessionRow(ctx, s.service.engine, sess, "all_sessions"); err != nil {
			// Log + keep going. A partial revoke is still better
			// than failing the whole batch on one bad row.
			if s.logger != nil {
				s.logger.Warn("revoke all: per-row revoke failed",
					"error", err, "session_id", sess.ID, "subject", subject)
			}
			continue
		}
		revoked++
	}

	return send(&memqlv1.RevokeAllSessionsResult{
		Success:      true,
		RevokedCount: revoked,
	})
}

// RevokeAuthSessionRow persists a single revocation by running
// revokeAuthSession with the discriminator fields the
// latest-wins projection requires. Exported so the LogoutHandler
// adapter (in app/) can revoke a row directly without dispatching
// through the gRPC stream.
func RevokeAuthSessionRow(ctx context.Context, engine *memqlengine.MemQLEngine, sess *authSessionSummary, reason string) error {
	if sess == nil || strings.TrimSpace(sess.ID) == "" {
		return fmt.Errorf("session summary required")
	}
	args := map[string]any{
		"sessionId":     sess.ID,
		"userId":        sess.UserId,
		"subject":       sess.Subject,
		"tokenHash":     sess.TokenHash,
		"source":        sess.Source,
		"expiresAt":     sess.ExpiresAt.Format(time.RFC3339),
		"revokedReason": reason,
	}
	argsJSON, _ := json.Marshal(args)
	query := fmt.Sprintf("revokeAuthSession(%s)", renderQueryArgs(argsJSON))
	if _, err := engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("revoke auth session: %w", err)
	}
	return nil
}

// CreateAuthSessionRow persists a fresh auth-session row at token
// issuance time. Called by the identity service's magic-link /
// refresh handlers when a session is born. Exported so the identity
// package can drive issuance without importing handler internals.
func CreateAuthSessionRow(ctx context.Context, engine *memqlengine.MemQLEngine, row AuthSessionInsert) error {
	if engine == nil {
		return fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(row.SessionId) == "" || strings.TrimSpace(row.UserId) == "" || strings.TrimSpace(row.TokenHash) == "" {
		return fmt.Errorf("sessionId, userId, tokenHash required")
	}
	args := map[string]any{
		"sessionId":   row.SessionId,
		"userId":      row.UserId,
		"identityId":  row.IdentityId,
		"subject":     row.Subject,
		"tokenHash":   row.TokenHash,
		"source":      row.Source,
		"clientLabel": row.ClientLabel,
		"expiresAt":   row.ExpiresAt.UTC().Format(time.RFC3339),
	}
	argsJSON, _ := json.Marshal(args)
	query := fmt.Sprintf("createAuthSession(%s)", renderQueryArgs(argsJSON))
	if _, err := engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	return nil
}

// AuthSessionInsert is the input shape for CreateAuthSessionRow.
// Field names match the underlying mutation arguments.
type AuthSessionInsert struct {
	SessionId   string
	UserId      string
	IdentityId  string
	Subject     string
	TokenHash   string
	Source      string // "bff_exchange" | "oidc_cookie"
	ClientLabel string
	ExpiresAt   time.Time
}

// sendAuthSessionError mirrors sendGuestError: emit a QueryErrorMsg
// with an error ID stamped into metadata and log with context.
func (s *streamSession) sendAuthSessionError(requestId, correlate string, code codes.Code, message string, err error) error {
	eid := generateErrorId()
	if s.logger != nil {
		s.logger.Error(message, "error", err, "errorId", eid, "requestId", requestId)
	}
	return s.sendQueryErrorWithMetadata(requestId, correlate, code, message, map[string]string{"errorId": eid})
}
