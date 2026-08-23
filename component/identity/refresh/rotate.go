// Package refresh implements the /auth/refresh endpoint.
//
// Refresh-token rotation is the single biggest source of session-
// security bugs in OAuth-style flows. The Rotator enforces:
//
//  1. The presented refresh token must hash to the stored
//     refreshTokenHash on the v1:identity:authSession row, or to the
//     immediately-previous hash inside a 30-second grace window.
//     Anything else that MATCHES A HASH SOME ROTATION RETIRED is a
//     replay: the session is revoked, a refresh_token_reuse_detected
//     signal lands on the audit log, the user is notified, and the
//     caller gets 401 with ErrTokenMismatch (memql#4329). This was
//     described here as intent for as long as the package existed and
//     is built now; the evidence it keys on is the retiredTokenHash
//     recorded by each rotation's v1:identity:authActivity row.
//  2. The session row must not be revoked.
//  3. The session must be inside the idle window
//     (lastRefreshedAt + MEMQL_IDENTITY_SESSION_IDLE_DAYS) and the absolute
//     max-age (firstAuthenticatedAt + MEMQL_IDENTITY_SESSION_MAX_DAYS).
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
	// ErrTokenMismatch is REUSE: the presented token matches a hash some
	// rotation retired, so it can only have been replayed. Distinct from
	// ErrSessionNotFound, which is a token nothing ever issued (a stale
	// cookie). Both map to 401 -- the caller learns nothing either way --
	// but only this one revokes the session and raises a signal. Declared
	// and never returned until memql#4329.
	ErrTokenMismatch = errors.New("refresh: refresh token was already retired and has been replayed (session revoked)")
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
// fallback closes. Past the window the same presentation is judged by
// reuse detection (memql#4329) rather than merely refused: inside the
// window it is a grace_window_accept, outside it is a replay. That is
// the whole of the boundary, and it is why the window is measured from
// previousRotatedAt rather than from anything the client controls.
const previousRefreshGraceWindow = 30 * time.Second

// Rotator handles refresh-token rotation. Constructed once at boot.
type Rotator struct {
	Cfg    identity.Config
	Store  *identity.Store
	Issuer *identity.JWTIssuer
	Audit  identity.AuditLogger
	Logger *slog.Logger

	// SecurityNotice, when non-nil, is called when a refresh-token replay
	// is detected, so the affected user is TOLD their session was signed
	// out and why (memql#4329, design D5).
	//
	// A hook rather than a direct dependency because the notice sender is
	// sub-project A's (memql#4305) and the two land independently; nil
	// falls back to a WARN log line, which is what the design specifies
	// until it exists. The detection, the revoke and the audit signal do
	// not depend on it -- a missing notice must never mean a missing
	// revoke.
	SecurityNotice func(ctx context.Context, in SecurityNoticeInput)

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

// SecurityNoticeInput is what a detected replay hands the notice sender.
// Deliberately minimal: who to tell, which session was signed out, and the
// presenting device's fingerprint, which is the only thing the recipient can
// actually recognise or fail to recognise.
type SecurityNoticeInput struct {
	UserId    string
	SessionId string
	SourceIP  string
	UserAgent string
	RetiredAt time.Time
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
		inGrace := prev != nil &&
			!prev.PreviousRotatedAt.IsZero() &&
			now.Sub(prev.PreviousRotatedAt) <= previousRefreshGraceWindow

		if !inGrace {
			// The presented hash matches neither the current hash nor an
			// IN-GRACE previous one. Before refusing it as a stale cookie,
			// ask whether any rotation ever RETIRED it -- because if one
			// did, this token has been replayed and the session is
			// compromised (memql#4329).
			if reuseErr, isReuse := r.detectReuse(ctx, in, tokenHash, prev); isReuse {
				return nil, reuseErr
			}
			reason := "session_not_found"
			sessionId := ""
			if prev != nil {
				// The grace window is what expired, not the session. Keep
				// the more specific reason: it distinguishes "your client
				// retried too late" from "this token was never ours".
				reason = "previous_refresh_grace_expired"
				sessionId = prev.ID
			}
			r.activityFailure(ctx, in, reason, prev, sessionId)
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
		// ... and it gets a row of its own on the activity stream. It was
		// slog-only, which meant the one signal distinguishing a legitimate
		// mid-rotation retry from a replay existed nowhere an operator could
		// read it (memql#4328).
		r.activity(ctx, identity.AuditEvent{
			Stream:      identity.StreamActivity,
			Category:    identity.AuditCategoryAuth,
			Action:      "grace_window_accept",
			TargetId:    prev.ID,
			ActorUserId: prev.UserId,
			ClientLabel: prev.ClientLabel,
			SourceIP:    in.SourceIP,
			UserAgent:   in.UserAgent,
			Outcome:     identity.AuditOutcomeSuccess,
			Detail: map[string]any{
				"rotatedAgoSeconds": int(now.Sub(prev.PreviousRotatedAt).Seconds()),
			},
		}, r.resolveUser(ctx, prev.UserId))
		row = prev
	}

	if !row.RevokedAt.IsZero() {
		r.activityFailure(ctx, in, "session_revoked", row, row.ID)
		return nil, ErrSessionRevoked
	}

	if !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt) {
		r.activityFailure(ctx, in, "session_expired_absolute", row, row.ID)
		return nil, ErrSessionExpired
	}

	// Idle timeout: lastRefreshedAt + IdleTimeout < now.
	if r.Cfg.SessionIdleTimeout > 0 && !row.LastRefreshedAt.IsZero() {
		if now.After(row.LastRefreshedAt.Add(r.Cfg.SessionIdleTimeout)) {
			r.activityFailure(ctx, in, "session_idle_timeout", row, row.ID)
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
			r.activityFailure(ctx, in, "session_max_age_exceeded", row, row.ID)
			return nil, ErrSessionExpired
		}
	}

	// Theft detection happened at the lookup-miss path above
	// (detectReuse). We looked up by refreshTokenHash, so a row coming
	// back means the hashes match by definition; everything that did NOT
	// match has already been judged against the retired hashes the
	// activity log records, and a replay never reaches here.
	//
	// The "separate audit table tracking the previous N hashes" this
	// comment used to name as future work is v1:identity:authActivity
	// (memql#4328), and it is not a second table -- it is the mechanics
	// log the rotation rows already move to.

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
	user := r.resolveUser(ctx, row.UserId)
	if user != nil {
		tokenInput.Email = user.PrimaryEmail
		tokenInput.Name = user.DisplayName
		tokenInput.GivenName = user.FirstName
		tokenInput.FamilyName = user.LastName
		tokenInput.Role = user.Role
		tokenInput.Internal = user.Internal
		tokenInput.RevocationEpoch = user.RevocationEpoch
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

	// The rotation row. On the ACTIVITY stream (memql#4328), naming its
	// actor (memql#4327 -- the Trail's actor column was blank on every one
	// of these), and recording the hash this rotation RETIRED, which is the
	// evidence detectReuse runs on for the next presentation of that token.
	//
	// retiredHash is row.RefreshTokenHash, the value that was current a
	// moment ago. On the grace-window path it is the hash the client never
	// received, which is the same value RotateAuthSession just stored as
	// previousRefreshTokenHash -- the two stay in step deliberately.
	r.activity(ctx, identity.AuditEvent{
		Stream:      identity.StreamActivity,
		Category:    identity.AuditCategoryAuth,
		Action:      "session_refreshed",
		TargetId:    row.ID,
		ActorUserId: row.UserId,
		ClientLabel: row.ClientLabel,
		SourceIP:    in.SourceIP,
		UserAgent:   in.UserAgent,
		Outcome:     identity.AuditOutcomeSuccess,
		RetiredHash: row.RefreshTokenHash,
	}, user)

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

// audit / activity / activityFailure mirror the magiclink helpers, split by
// destination log (memql#4328).

func (r *Rotator) audit(ctx context.Context, ev identity.AuditEvent) {
	if r == nil || r.Audit == nil {
		return
	}
	r.Audit.Log(ctx, ev)
}

// activity stamps the constant fields every rotation row carries and routes
// the event to v1:identity:authActivity. The actor's email and role come from
// the resolved user row, which is why every caller passes one -- a row that
// cannot name who refreshed is the blank actor column memql#4327 was filed for.
func (r *Rotator) activity(ctx context.Context, ev identity.AuditEvent, user *identity.UserRow) {
	// Re-stamped rather than assumed. Every caller already sets it -- stating
	// the destination AT the writer is what lets a reader, and
	// test/dslconformance/identity_activity_enum_contract_test.go's AST walk,
	// see which log a row lands on without following it in here. This line is
	// the backstop for a caller that forgets, and the cost of the wrong
	// default is a mechanic on the operator's Trail.
	ev.Stream = identity.StreamActivity
	ev.Category = identity.AuditCategoryAuth
	ev.TargetType = "session"
	// Category and TargetType are re-stamped here for the same reason Stream
	// is: every caller states them, and stating them AT the writer is what
	// test/dslconformance/identity_audit_enum_contract_test.go's AST walk can
	// see. It follows a literal into a call it is a direct argument of, but not
	// into one reached through a local -- which is exactly the shape
	// activityFailure builds.
	if user != nil {
		if ev.ActorUserId == "" {
			ev.ActorUserId = user.ID
		}
		ev.ActorEmail = user.PrimaryEmail
		ev.ActorRole = user.Role
	}
	r.audit(ctx, ev)
}

// activityFailure writes the blocked-rotation row. `row` may be nil -- that is
// the session_not_found case, where nothing resolved and the row is therefore
// attributable to nobody; it lands with an empty owner and is a cluster
// owner's to read, which is the honest record of an unattributable attempt.
func (r *Rotator) activityFailure(ctx context.Context, in RotateInput, reason string, row *identity.AuthSessionRow, sessionId string) {
	ev := identity.AuditEvent{
		Stream:        identity.StreamActivity,
		Category:      identity.AuditCategoryAuth,
		Action:        "session_refresh_blocked",
		TargetId:      sessionId,
		SourceIP:      in.SourceIP,
		UserAgent:     in.UserAgent,
		Outcome:       identity.AuditOutcomeBlocked,
		FailureReason: reason,
	}
	var user *identity.UserRow
	if row != nil {
		ev.ActorUserId = row.UserId
		ev.ClientLabel = row.ClientLabel
		user = r.resolveUser(ctx, row.UserId)
	}
	r.activity(ctx, ev, user)
}

// resolveUser reads the directory row behind a session. Failure is logged and
// returns nil: a lookup hiccup must not turn a valid refresh into a 401, and
// the activity row degrades to a blank actor rather than being lost.
func (r *Rotator) resolveUser(ctx context.Context, userId string) *identity.UserRow {
	if r == nil || r.Store == nil || strings.TrimSpace(userId) == "" {
		return nil
	}
	user, err := r.Store.LookupUserById(ctx, userId)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("refresh: user lookup failed",
				slog.String("user_id", userId),
				slog.String("error", err.Error()))
		}
		return nil
	}
	return user
}

// detectReuse is memql#4329.
//
// It is reached only when the presented hash matched neither the current hash
// nor an in-grace previous one -- so the token is at least one rotation stale
// and the client should not still be holding it. The question this answers is
// whether the token is one WE ever issued: a hash that some rotation retired
// can only have come from a copy of a credential that has since moved on, and
// the legitimate holder's next rotation would be indistinguishable from the
// attacker's. So the session goes.
//
// A miss is the ordinary stale cookie and is left entirely alone -- handled
// reports false and the caller writes its usual blocked row. That asymmetry is
// the point: revoking on an ambiguous signal is a self-inflicted denial of
// service, and the evidence here is not ambiguous.
//
// The lookup reaches back exactly as far as the activity retention window
// (memql#4330). Past it the row is gone and a replay degrades to
// session_not_found -- a documented limit, and a safe one: the default 30 days
// exceeds both the idle timeout and the refresh-token TTL, so a token older
// than the window is already dead on its own account.
func (r *Rotator) detectReuse(ctx context.Context, in RotateInput, tokenHash string, prev *identity.AuthSessionRow) (error, bool) {
	if r == nil || r.Store == nil {
		return nil, false
	}
	act, err := r.Store.AuthActivityByRetiredHash(ctx, tokenHash)
	if err != nil {
		// FAIL OPEN, deliberately. A lookup error is not evidence of a
		// replay, and revoking a session on one would let a database hiccup
		// sign every user out. The caller falls through to its ordinary
		// refusal, which is what happened before this existed.
		if r.Logger != nil {
			r.Logger.Warn("refresh: retired-hash lookup failed; treating as a stale token",
				slog.String("error", err.Error()))
		}
		return nil, false
	}
	if act == nil {
		return nil, false
	}

	sessionId := strings.TrimSpace(act.SessionId)
	if sessionId == "" && prev != nil {
		sessionId = prev.ID
	}
	userId := strings.TrimSpace(act.ActorUserId)
	if userId == "" && prev != nil {
		userId = prev.UserId
	}

	if sessionId != "" {
		if revErr := r.Store.RevokeAuthSession(ctx, sessionId, "reuse_detected"); revErr != nil && r.Logger != nil {
			// Logged, not returned. The signal and the notice below are
			// what the user and the operator actually act on, and losing
			// them because the revoke write failed would leave a detected
			// compromise entirely unrecorded.
			r.Logger.Error("refresh: could not revoke a session on detected token reuse",
				slog.String("session_id", sessionId),
				slog.String("error", revErr.Error()))
		}
	}

	user := r.resolveUser(ctx, userId)
	detail := map[string]any{"retiredAt": act.OccurredAt.UTC().Format(time.RFC3339Nano)}
	ev := identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        "refresh_token_reuse_detected",
		TargetType:    "session",
		TargetId:      sessionId,
		ActorUserId:   userId,
		SourceIP:      in.SourceIP,
		UserAgent:     in.UserAgent,
		Outcome:       identity.AuditOutcomeBlocked,
		FailureReason: "refresh_token_reuse",
		Detail:        detail,
	}
	if user != nil {
		ev.ActorEmail = user.PrimaryEmail
		ev.ActorRole = user.Role
		ev.TargetEmail = user.PrimaryEmail
	}
	// The AUDIT stream, not the activity stream: this is a security decision,
	// and it is precisely the kind of row the operator's Trail exists for.
	r.audit(ctx, ev)

	notice := SecurityNoticeInput{
		UserId:    userId,
		SessionId: sessionId,
		SourceIP:  in.SourceIP,
		UserAgent: in.UserAgent,
		RetiredAt: act.OccurredAt,
	}
	if r.SecurityNotice != nil {
		r.SecurityNotice(ctx, notice)
	} else if r.Logger != nil {
		r.Logger.Warn("refresh: token reuse detected; no security-notice sender is wired, so the user was NOT told",
			slog.String("session_id", sessionId),
			slog.String("user_id", userId),
			slog.String("source_ip", in.SourceIP))
	}

	return ErrTokenMismatch, true
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
