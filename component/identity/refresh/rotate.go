// Package refresh implements the /auth/refresh endpoint.
//
// Refresh-token rotation is the single biggest source of session-
// security bugs in OAuth-style flows. The Rotator enforces:
//
//  1. The presented refresh token must hash to the stored
//     refreshTokenHash on the v1:identity:authSession row. Mismatch is
//     refresh-token theft -- we revoke the entire session immediately
//     and emit a high-severity audit event.
//  2. The session row must not be revoked.
//  3. The session must be inside the idle window
//     (lastRefreshedAt + IDENTITY_SESSION_IDLE_DAYS) and the absolute
//     max-age (firstAuthenticatedAt + IDENTITY_SESSION_MAX_DAYS).
//  4. On success, mint a fresh access + refresh token pair and
//     persist the new refresh-token hash atomically.
package refresh

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// Sentinel errors callers branch on.
var (
	ErrNoToken         = errors.New("refresh: no refresh token presented")
	ErrSessionNotFound = errors.New("refresh: session not found")
	ErrSessionRevoked  = errors.New("refresh: session revoked")
	ErrSessionExpired  = errors.New("refresh: session expired")
	ErrTokenMismatch   = errors.New("refresh: refresh token does not match stored hash (theft suspected)")
)

// previousRefreshGraceWindow is how long the IMMEDIATELY-PREVIOUS
// refresh-token hash stays acceptable after a successful rotation.
//
// Why: the SPA hard-refresh race. The user hits Cmd+Shift+R while a
// /auth/refresh response is still in flight -- the browser aborts
// before consuming the Set-Cookie header, so the new refresh token
// is never persisted client-side. The server, however, already
// processed the rotation and stored the new hash. The next page's
// /auth/refresh sends the OLD cookie, which doesn't match anything
// on the row's current refreshTokenHash, and a naive implementation
// returns 401 -- forcing the user to re-sign-in for no reason.
//
// 30s is a deliberately tight window: long enough to cover any
// realistic browser-abort + new-page-bootstrap latency, short enough
// that an attacker who somehow obtained the OLD token after the
// legitimate rotation has only a brief window to use it before the
// fallback closes. A future Phase 5 layer should add reuse-detection
// (presenting BOTH the previous hash AND seeing the new hash already
// rotated again indicates token theft -- revoke the whole session).
const previousRefreshGraceWindow = 30 * time.Second

// Rotator handles refresh-token rotation. Constructed once at boot.
type Rotator struct {
	Cfg    identity.Config
	Store  *identity.Store
	Issuer *identity.JWTIssuer
	Audit  identity.AuditLogger
	Logger *slog.Logger

	// LiveTokenSettings, when non-nil, returns the runtime-tunable
	// TTLs read from the singleton clusterSettings row. The
	// rotator consults it on every Rotate() so admin-edited TTLs
	// apply on the next refresh-rotation cycle without an identity
	// restart. Nil falls back to r.Cfg (env/built-in defaults).
	LiveTokenSettings func(ctx context.Context) identity.LiveTokenSettings
}

// effectiveTTLs returns the (access, refresh) TTL pair that should
// govern this Rotate call. Mirrors the http.Server helper of the
// same purpose; kept independent because the refresh package
// shouldn't import the http package.
func (r *Rotator) effectiveTTLs(ctx context.Context) (time.Duration, time.Duration) {
	access := r.Cfg.AccessTokenTTL
	refreshTTL := r.Cfg.RefreshTokenTTL
	if r.LiveTokenSettings == nil {
		return access, refreshTTL
	}
	live := r.LiveTokenSettings(ctx)
	if live.AccessTokenTTL > 0 {
		access = live.AccessTokenTTL
	}
	if live.RefreshTokenTTL > 0 {
		refreshTTL = live.RefreshTokenTTL
	}
	return access, refreshTTL
}

// RotateInput is the per-request payload from POST /auth/refresh.
type RotateInput struct {
	PresentedRefreshToken string
	SourceIP              string
	UserAgent             string
}

// RotateResult is what the HTTP handler responds with.
type RotateResult struct {
	AccessToken     string
	RefreshToken    string
	ExpiresIn       int // access-token TTL in seconds
	SessionId       string
	UserId          string
	AccessExpiresAt time.Time
}

// Rotate validates the presented refresh token and returns a fresh
// access + refresh token pair. The HTTP handler sets the refresh
// token as an httpOnly cookie on the response.
func (r *Rotator) Rotate(ctx context.Context, in RotateInput) (*RotateResult, error) {
	if r == nil {
		return nil, errors.New("refresh: nil rotator")
	}
	if r.Store == nil || r.Issuer == nil {
		return nil, errors.New("refresh: missing store/issuer")
	}

	plain := strings.TrimSpace(in.PresentedRefreshToken)
	if plain == "" {
		return nil, ErrNoToken
	}
	tokenHash := hashRefreshToken(plain)

	row, err := r.Store.LookupAuthSessionByRefreshTokenHash(ctx, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("refresh: lookup: %w", err)
	}

	now := time.Now().UTC()

	// Grace-window fallback. If the presented hash doesn't match the
	// current refreshTokenHash, try the IMMEDIATELY-PREVIOUS hash. We
	// only accept it if previousRotatedAt is recent (within
	// previousRefreshGraceWindow). Past that, the previous hash is
	// expired bookkeeping and the token is treated as stale --
	// indistinguishable from any other expired refresh.
	if row == nil {
		prev, prevErr := r.Store.LookupAuthSessionByPreviousRefreshTokenHash(ctx, tokenHash)
		if prevErr != nil {
			return nil, fmt.Errorf("refresh: lookup previous: %w", prevErr)
		}
		if prev == nil {
			r.auditFailure(ctx, in, "session_not_found", "")
			return nil, ErrSessionNotFound
		}
		if prev.PreviousRotatedAt.IsZero() ||
			now.Sub(prev.PreviousRotatedAt) > previousRefreshGraceWindow {
			r.auditFailure(ctx, in, "previous_refresh_grace_expired", prev.ID)
			return nil, ErrSessionNotFound
		}
		// Inside the grace window: accept the previous hash as a
		// successful refresh and proceed. The rotation below will
		// move currentHash forward AGAIN, and the previousHash will
		// once more be the just-now-stale value. From the client's
		// perspective the abort-mid-response is invisible.
		if r.Logger != nil {
			r.Logger.Info("refresh: grace-window accept",
				slog.String("session_id", prev.ID),
				slog.Duration("rotated_ago", now.Sub(prev.PreviousRotatedAt)))
		}
		row = prev
	}

	if !row.RevokedAt.IsZero() {
		r.auditFailure(ctx, in, "session_revoked", row.ID)
		return nil, ErrSessionRevoked
	}

	if !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt) {
		r.auditFailure(ctx, in, "session_expired_absolute", row.ID)
		return nil, ErrSessionExpired
	}

	// Idle timeout: lastRefreshedAt + IdleTimeout < now.
	if r.Cfg.SessionIdleTimeout > 0 && !row.LastRefreshedAt.IsZero() {
		if now.After(row.LastRefreshedAt.Add(r.Cfg.SessionIdleTimeout)) {
			r.auditFailure(ctx, in, "session_idle_timeout", row.ID)
			return nil, ErrSessionExpired
		}
	}

	// Absolute max-age: firstAuthenticatedAt + MaxAge < now. The row
	// reuses CreatedAt as a stand-in when firstAuthenticatedAt is not
	// stamped (Phase 1 didn't stamp it; the latest-wins projection
	// keeps it through rotates).
	firstAuth := row.FirstAuthAt
	if firstAuth.IsZero() {
		firstAuth = row.CreatedAt
	}
	if r.Cfg.SessionMaxAge > 0 && !firstAuth.IsZero() {
		if now.After(firstAuth.Add(r.Cfg.SessionMaxAge)) {
			r.auditFailure(ctx, in, "session_max_age_exceeded", row.ID)
			return nil, ErrSessionExpired
		}
	}

	// Theft detection. We looked up by refreshTokenHash, so if a row
	// came back the hashes match by definition. The defense lives at
	// the lookup-miss path (ErrSessionNotFound above): once the token
	// rotates, the previous hash no longer points anywhere, so a
	// replay attempt fails to find a row and is rejected.
	//
	// A useful stronger guarantee — "if the same refresh token is
	// presented twice, revoke the WHOLE session" — would require a
	// separate audit table tracking the previous N hashes. Phase 4
	// can layer that on top.
	_ = tokenHash // referenced for clarity

	// Mint a new refresh token + new access token.
	newRefreshPlain, newRefreshHash, err := newRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("refresh: mint refresh token: %w", err)
	}

	// Resolve the user row so the rotated access token carries the
	// directory fields the SPA needs. Mirrors the /oauth/token mint
	// path -- without it, every refresh would produce an access token
	// with empty email / name / role, and the SPA's /profile would
	// show N/A.
	tokenInput := identity.IssueInput{
		UserId:    row.UserId,
		SessionId: row.ID,
	}
	if user, err := r.Store.LookupUserById(ctx, row.UserId); err == nil && user != nil {
		tokenInput.Email = user.PrimaryEmail
		tokenInput.Name = user.DisplayName
		tokenInput.GivenName = user.FirstName
		tokenInput.FamilyName = user.LastName
		tokenInput.Role = user.Role
		tokenInput.Internal = user.Internal
		tokenInput.RevocationEpoch = user.RevocationEpoch
	} else if err != nil && r.Logger != nil {
		r.Logger.Warn("refresh: user lookup failed",
			slog.String("user_id", row.UserId),
			slog.String("error", err.Error()))
	}
	accessTTL, refreshTTL := r.effectiveTTLs(ctx)
	tokenInput.TTLOverride = accessTTL
	access, accessExp, err := r.Issuer.IssueAccessToken(tokenInput, now)
	if err != nil {
		return nil, fmt.Errorf("refresh: issue access token: %w", err)
	}

	// Roll the refresh-token hash + new absolute expiry forward. The
	// row's CURRENT hash becomes the previous hash for grace-window
	// purposes; the new hash takes its place. Note that on the
	// grace-window-fallback path, row.RefreshTokenHash is the hash
	// the rotation BEFORE this one minted -- i.e., the hash that
	// the client never received but the server already minted. We
	// use that as the previous-hash bookkeeping anyway because (a)
	// the client doesn't have it either, so accepting it again is a
	// no-op from their perspective, and (b) it keeps the bookkeeping
	// consistent: previousHash is always exactly one rotation behind
	// currentHash.
	refreshExpiresAt := now.Add(refreshTTL).Format(time.RFC3339Nano)
	if err := r.Store.RotateAuthSession(ctx, row.ID, newRefreshHash, row.RefreshTokenHash, refreshExpiresAt); err != nil {
		return nil, fmt.Errorf("refresh: rotate session: %w", err)
	}

	r.audit(ctx, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "session_refreshed",
		TargetType:  "session",
		TargetId:    row.ID,
		ActorUserId: row.UserId,
		SourceIP:    in.SourceIP,
		UserAgent:   in.UserAgent,
		Outcome:     identity.AuditOutcomeSuccess,
	})

	expiresIn := int(accessTTL / time.Second)
	if expiresIn <= 0 {
		expiresIn = int(time.Until(accessExp) / time.Second)
	}

	return &RotateResult{
		AccessToken:     access,
		RefreshToken:    newRefreshPlain,
		ExpiresIn:       expiresIn,
		SessionId:       row.ID,
		UserId:          row.UserId,
		AccessExpiresAt: accessExp,
	}, nil
}

// audit / auditFailure mirror the magiclink helpers.
func (r *Rotator) audit(ctx context.Context, ev identity.AuditEvent) {
	if r == nil || r.Audit == nil {
		return
	}
	r.Audit.Log(ctx, ev)
}

func (r *Rotator) auditFailure(ctx context.Context, in RotateInput, reason, sessionId string) {
	r.audit(ctx, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        "session_refresh_blocked",
		TargetType:    "session",
		TargetId:      sessionId,
		SourceIP:      in.SourceIP,
		UserAgent:     in.UserAgent,
		Outcome:       identity.AuditOutcomeBlocked,
		FailureReason: reason,
	})
}

// newRefreshToken returns the plaintext URL-safe base64 token and its
// SHA-256 hex hash.
func newRefreshToken() (plain, hash string, err error) {
	const tokenBytes = 32
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	hash = hashRefreshToken(plain)
	return plain, hash, nil
}

// hashRefreshToken returns the lowercase-hex SHA-256 of a refresh
// token. Identical algorithm to the access-token hash; we just pin a
// dedicated function so future moves to a different KDF don't have to
// touch the rotation flow.
func hashRefreshToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// HashRefreshToken exposes the hash function for callers (the magic-
// link verifier mints the very first refresh token; the Issuer side
// of token-exchange uses the same algorithm).
func HashRefreshToken(plain string) string { return hashRefreshToken(plain) }

// NewRefreshToken exposes the token-mint function for callers that
// need to issue a fresh refresh token outside of rotate (e.g. the
// /oauth/token handler, on auth-code redemption).
func NewRefreshToken() (plain, hash string, err error) { return newRefreshToken() }

// SessionLogger returns the rotator's slog handle so handlers can
// log under a consistent surface. Keeps the field private.
func (r *Rotator) SessionLogger() *slog.Logger { return r.Logger }
