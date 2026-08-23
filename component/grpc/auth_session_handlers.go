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
	langparser "github.com/znasllc-io/memql/component/language/parser"
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
	query := fmt.Sprintf(`query authSessionsForSubject(subject: %s)`, langparser.QuoteString(subject))
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
// revokeAuthSession. Exported so the LogoutHandler adapter (in app/) can
// revoke a row directly without dispatching through the gRPC stream.
//
// It used to pass the discriminator fields too -- subject / tokenHash /
// source / expiresAt / userId -- "the fields the latest-wins projection
// requires". memql#1628 made the mutation read-merge the persisted row, and
// its doc comment has said so since: those fields "inherit from the persisted
// row instead of being re-supplied". The mutation declares exactly two
// arguments, so the other five were being dropped silently (memql#4258).
func RevokeAuthSessionRow(ctx context.Context, engine *memqlengine.MemQLEngine, sess *authSessionSummary, reason string) error {
	if sess == nil || strings.TrimSpace(sess.ID) == "" {
		return fmt.Errorf("session summary required")
	}
	args := map[string]any{
		"sessionId":     sess.ID,
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

// handleRevokeSession revokes ONE named session belonging to the caller
// (memql#4319).
//
// # Why this is not a thin pass-through to the mutation
//
// `revokeAuthSession` declares (sessionId, revokedReason) and nothing else.
// A mutation cannot carry an owner predicate -- `filter` is a read construct
// -- so the DSL has no way to say "only if this row is yours", and the
// mutation is not @serverOnly. A handler that forwarded the caller's
// session_id straight through would therefore hand every authenticated
// caller the ability to revoke ANY session in the cluster by id: a
// denial-of-service primitive against a named colleague, reachable from a
// browser.
//
// So the ownership check is here, and it is a MEMBERSHIP test rather than a
// comparison: the caller's own live rows are listed by verified subject, and
// an id that is not among them is refused. That is the same shape identity's
// own /me/devices revoke uses (component/identity/web/me_sessions.go), which
// is the point -- one rule, two renderers.
//
// # not_found covers both misses on purpose
//
// "that id is not yours" and "that id does not exist" return the same
// error_code. Separating them would answer, for any id an attacker cares to
// try, whether it names a real session -- and session ids are exactly the
// thing a sessions list puts in front of somebody.
func (s *streamSession) handleRevokeSession(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RevokeSessionMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.RevokeSessionResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RevokeSessionResult{
				RevokeSessionResult: result,
			},
		})
	}

	if s.service.engine == nil {
		return send(&memqlv1.RevokeSessionResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "engine not configured",
		})
	}

	target := strings.TrimSpace(msg.GetSessionId())
	if target == "" {
		return send(&memqlv1.RevokeSessionResult{
			ErrorCode:    "invalid",
			ErrorMessage: "session_id is required",
		})
	}

	// The JWT subject, for the reason handleRevokeAllSessions gives: it is
	// populated by the auth middleware on every request, while
	// AccessContext.UserId may still be the claims fallback on the first
	// request after signup.
	subject := ""
	if claims, ok := auth.ClaimsFromContext(s.stream.Context()); ok {
		if v, ok := claims["sub"].(string); ok {
			subject = strings.TrimSpace(v)
		}
	}
	if subject == "" {
		return send(&memqlv1.RevokeSessionResult{
			ErrorCode:    "unauthenticated",
			ErrorMessage: "caller has no resolved subject",
		})
	}

	ctx := contextWithSystemActor(s.stream.Context())
	sessions, err := listAuthSessionsForSubject(ctx, s.service.engine, subject)
	if err != nil {
		return s.sendAuthSessionError(requestId, correlate, codes.Internal, "revoke session: list", err)
	}

	owned := pickOwnedLiveSession(sessions, target, time.Now().UTC())
	if owned == nil {
		return send(&memqlv1.RevokeSessionResult{
			ErrorCode:    "not_found",
			ErrorMessage: "no live session of yours has that id",
		})
	}

	// Whether this is the row backing THIS connection, decided before the
	// write: afterwards the bearer no longer resolves and the answer would
	// be unavailable exactly when the client needs it.
	wasCurrent := false
	if plain := bearerTokenFromIncomingContext(s.stream.Context()); plain != "" {
		wasCurrent = owned.TokenHash != "" && owned.TokenHash == HashBearerToken(plain)
	}

	if err := RevokeAuthSessionRow(ctx, s.service.engine, owned, "user_action"); err != nil {
		return s.sendAuthSessionError(requestId, correlate, codes.Internal, "revoke session: persist", err)
	}
	return send(&memqlv1.RevokeSessionResult{
		Success:    true,
		SessionId:  owned.ID,
		WasCurrent: wasCurrent,
	})
}

// pickOwnedLiveSession is the ownership check, as a decision rather than a
// side effect: it returns the caller's own LIVE session with the given id, or
// nil.
//
// A separate function because it is the security-relevant half of
// handleRevokeSession and the only half worth testing directly -- the rest is
// transport. `sessions` is the caller's own set, resolved from the verified
// subject; nothing outside this file may pass it anything else.
//
// nil for three distinct situations, deliberately collapsed:
//
//   - the id is not the caller's,
//   - it names a row that is already revoked,
//   - it names one that has already expired.
//
// The first is the security case. The other two are not failures a client
// needs distinguished: there is no live session to end, so a success would
// tell it a write happened that did not, and separate codes would give an
// attacker a way to ask whether an arbitrary id names a real row.
func pickOwnedLiveSession(sessions []*authSessionSummary, id string, now time.Time) *authSessionSummary {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	for _, sess := range sessions {
		if sess == nil || sess.ID != id {
			continue
		}
		if !sess.RevokedAt.IsZero() {
			return nil
		}
		if !sess.ExpiresAt.IsZero() && now.After(sess.ExpiresAt) {
			return nil
		}
		return sess
	}
	return nil
}
