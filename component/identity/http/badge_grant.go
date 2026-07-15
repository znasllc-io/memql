package http

// POST /auth/badge/grant -- the shared-terminal operator grant
// exchange (memql#2513).
//
// An already-authenticated terminal (kiosk / station holding its own
// user-class bearer) presents a registered badge/device id and
// receives a short-lived class="badge" JWT for the badge's owner. The
// terminal rotates the grant onto its live stream (RotateAuthMsg /
// sdk rotateAuth), so writes during the operator's window carry the
// operator's actor.userId; tap-out or expiry rotates back to the
// terminal's own bearer.
//
// Lives on the identity HTTP surface (not the bff gRPC plane) for the
// same reason worker pairing does: the exchange is fundamentally an
// auth operation and JWT minting requires this node's signing keys.
// Follows the pair.go conventions: HTTPS-required, bearer-verified via
// the local issuer, rate-limited, audited with SourceIP.
//
// Scoping is claim-borne, not endpoint-borne: the grant token carries
// role_ceiling = the terminal's own role, clamped at AccessContext
// resolution, and the stream's per-envelope expiry gate stops an
// expired grant mid-stream. See component/identity/jwt.go ClassBadge.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/badge"
)

// badgeGrantMaxTTL caps the per-request ttlSeconds override. A badge
// grant is a walk-away containment boundary; anything longer than an
// hour defeats it.
const badgeGrantMaxTTL = time.Hour

// envBadgeGrantTTLSeconds overrides the default grant TTL
// (identity.DefaultBadgeGrantTTLSeconds) cluster-wide.
const envBadgeGrantTTLSeconds = "MEMQL_IDENTITY_BADGE_GRANT_TTL_SECONDS"

// envBadgeGrantPerHour tunes the per-IP grant-attempt rate limit.
// Generous default: floor work re-taps every few minutes; brute-force
// enumeration of badge ids is what the limiter exists to blunt.
const envBadgeGrantPerHour = "MEMQL_IDENTITY_BADGE_GRANT_PER_HOUR"

const defaultBadgeGrantPerHour = 240

// grantLimiter returns this Server's per-IP grant-attempt limiter,
// built once on first use. Server-scoped (the fields live on Server)
// so each Server + each test gets its own bucket map and captures its
// own logger. Same in-memory scope caveat as the magic-link limiter
// (abuse/ratelimit.go): multi-replica deployments get per-replica
// budgets.
func grantLimiter(s *Server) *abuse.IPRateLimiter {
	s.badgeGrantLimiterOnce.Do(func() {
		perHour := defaultBadgeGrantPerHour
		if v := strings.TrimSpace(os.Getenv(envBadgeGrantPerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		s.badgeGrantLimiter = abuse.NewIPRateLimiter(perHour, s.Logger)
	})
	return s.badgeGrantLimiter
}

// BadgeGrantRequest is the JSON body for POST /auth/badge/grant.
type BadgeGrantRequest struct {
	// BadgeId is the plaintext badge/device identifier from the tap /
	// scan. Hashed immediately; never persisted or logged.
	BadgeId string `json:"badgeId"`
	// TTLSeconds optionally shortens (or modestly extends, capped at
	// one hour) the grant lifetime for this exchange.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
}

// BadgeGrantResponse carries the minted grant. The client decodes
// sub/exp from the token itself for its "signed in as X until T" UI.
type BadgeGrantResponse struct {
	Success        bool   `json:"success"`
	AccessToken    string `json:"accessToken,omitempty"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	OperatorUserId string `json:"operatorUserId,omitempty"`
	Error          string `json:"error,omitempty"`
	ErrorCode      string `json:"errorCode,omitempty"`
}

func (s *Server) writeBadgeGrantError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, BadgeGrantResponse{Success: false, Error: msg, ErrorCode: code})
}

// handleBadgeGrant exchanges (terminal bearer + badge id) for a
// short-lived operator grant token.
func (s *Server) handleBadgeGrant(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		s.writeBadgeGrantError(w, http.StatusInternalServerError, "server", "identity engine not wired")
		return
	}
	ctx := r.Context()
	sourceIP := clientIP(r)

	if allowed, retryAfter := grantLimiter(s).Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.writeBadgeGrantError(w, http.StatusTooManyRequests, "rate_limited", "too many grant attempts from this address")
		return
	}

	bearer := strings.TrimSpace(extractAuthScheme(r, "Bearer"))
	caller, ok := s.requireBearer(w, r)
	if !ok {
		return
	}
	terminalUserId := strings.TrimSpace(caller.Subject)
	if terminalUserId == "" {
		s.writeBadgeGrantError(w, http.StatusUnauthorized, "unauthenticated", "subject claim missing")
		return
	}
	// Only user-class terminal sessions may exchange. A badge grant
	// cannot mint from another badge grant (no chaining), and the
	// machine classes are surface-pinned elsewhere for exactly this
	// kind of reason.
	if class := strings.TrimSpace(caller.Class); class != "" && class != identity.ClassUser {
		s.auditBadgeGrant(ctx, terminalUserId, "", "", sourceIP, identity.AuditOutcomeBlocked, "terminal_class_"+class)
		s.writeBadgeGrantError(w, http.StatusForbidden, "terminal_class_rejected",
			"badge grants require a user-class terminal session; got class="+class)
		return
	}
	// A grant's role_ceiling is the terminal's role; an empty role
	// (external users carry one, see jwt.go) cannot produce a usable
	// ceiling, so refuse with a typed error rather than a 500 from the
	// issuer's required-field check.
	terminalRole := strings.TrimSpace(caller.Role)
	if terminalRole == "" {
		s.auditBadgeGrant(ctx, terminalUserId, "", "", sourceIP, identity.AuditOutcomeBlocked, "terminal_role_missing")
		s.writeBadgeGrantError(w, http.StatusForbidden, "terminal_role_missing",
			"the terminal session carries no role; a badge grant needs a role to set its ceiling")
		return
	}
	// Honor terminal-session revocation: the identity node runs no
	// session-revocation interceptor, so check the authSession row here
	// (memql#2513). Fail-open on a lookup error -- a transient engine
	// blip must not wedge the floor -- but block a definitively revoked
	// session.
	store := &badge.Store{Engine: s.Store.Engine, Logger: s.Logger}
	if revoked, err := store.TerminalBearerRevoked(ctx, bearer); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("badge grant: session-revocation check failed; proceeding", "error", err)
		}
	} else if revoked {
		s.auditBadgeGrant(ctx, terminalUserId, "", "", sourceIP, identity.AuditOutcomeBlocked, "terminal_session_revoked")
		s.writeBadgeGrantError(w, http.StatusUnauthorized, "terminal_session_revoked",
			"the terminal session has been revoked")
		return
	}

	var body BadgeGrantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body); err != nil {
		s.writeBadgeGrantError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	badgeId := strings.TrimSpace(body.BadgeId)
	if badgeId == "" {
		s.writeBadgeGrantError(w, http.StatusBadRequest, "bad_request", "badgeId required")
		return
	}

	row, err := store.LookupByKeyHash(ctx, badge.HashBadgeId(badgeId))
	if err != nil {
		s.writeBadgeGrantError(w, http.StatusInternalServerError, "lookup_failed", err.Error())
		return
	}
	if row == nil {
		s.auditBadgeGrant(ctx, terminalUserId, "", "", sourceIP, identity.AuditOutcomeFailure, "unknown_badge")
		s.writeBadgeGrantError(w, http.StatusForbidden, "unknown_badge", "no registered badge matches")
		return
	}
	if !row.Active {
		s.auditBadgeGrant(ctx, terminalUserId, row.ID, "", sourceIP, identity.AuditOutcomeBlocked, "badge_revoked")
		s.writeBadgeGrantError(w, http.StatusForbidden, "badge_revoked", "badge has been revoked")
		return
	}
	operatorUserId := strings.TrimSpace(row.UserId)
	if operatorUserId == "" {
		s.writeBadgeGrantError(w, http.StatusInternalServerError, "badge_unowned", "badge row carries no owner")
		return
	}

	ttl := badgeGrantTTL(body.TTLSeconds)
	now := time.Now().UTC()
	token, expiresAt, err := s.Issuer.IssueBadgeGrantToken(identity.BadgeGrantIssueInput{
		UserId: operatorUserId,
		// The ceiling is the TERMINAL's role: the operator's own row
		// role is min'd in at AccessContext resolution, so the
		// effective role is min(terminal@grant, operator@resolve).
		RoleCeiling:     terminalRole,
		BadgeIdentityId: row.ID,
		TTLOverride:     ttl,
	}, now)
	if err != nil {
		s.writeBadgeGrantError(w, http.StatusInternalServerError, "mint_failed", err.Error())
		return
	}

	// Best-effort bookkeeping; the grant stands even when this write
	// fails. The badge row is a machine credential, so the touch is
	// gated by the memql#2513 credential-actor guard: only a system
	// actor may write it. This handler runs under the terminal operator's
	// bearer (a non-system actor the guard rejects), so stamp the system
	// credential actor for the touch. The terminal bearer + badge id
	// validated above are the authorization. memql#2549.
	if err := store.TouchLastUsed(identity.ContextWithSystemCredentialActor(ctx), row, now); err != nil && s.Logger != nil {
		s.Logger.Warn("badge grant: lastUsedAt touch failed", "badge", row.ID, "error", err)
	}

	s.auditBadgeGrantAt(ctx, terminalUserId, row.ID, operatorUserId, sourceIP, expiresAt, identity.AuditOutcomeSuccess, "")

	writeJSON(w, http.StatusOK, BadgeGrantResponse{
		Success:        true,
		AccessToken:    token,
		ExpiresAt:      expiresAt.Format(time.RFC3339Nano),
		OperatorUserId: operatorUserId,
	})
}

// badgeGrantTTL resolves the effective grant lifetime: request
// override > env default > built-in default, capped at one hour.
func badgeGrantTTL(requestSeconds int64) time.Duration {
	ttl := time.Duration(identity.DefaultBadgeGrantTTLSeconds) * time.Second
	if v := strings.TrimSpace(os.Getenv(envBadgeGrantTTLSeconds)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	if requestSeconds > 0 {
		ttl = time.Duration(requestSeconds) * time.Second
	}
	if ttl > badgeGrantMaxTTL {
		ttl = badgeGrantMaxTTL
	}
	return ttl
}

// auditBadgeGrant emits the grant-exchange audit event. failureReason
// is empty on success.
// auditBadgeGrant records a denial / non-success grant event (no
// operator resolved, no expiry).
func (s *Server) auditBadgeGrant(ctx context.Context, terminalUserId, badgeIdentityId, operatorUserId, sourceIP string, outcome identity.AuditOutcome, failureReason string) {
	s.auditBadgeGrantAt(ctx, terminalUserId, badgeIdentityId, operatorUserId, sourceIP, time.Time{}, outcome, failureReason)
}

// auditBadgeGrantAt records a grant event, stamping the operator id and
// grant expiry -- the two facts an attribution audit needs (who the
// terminal acted as, and until when).
func (s *Server) auditBadgeGrantAt(ctx context.Context, terminalUserId, badgeIdentityId, operatorUserId, sourceIP string, expiresAt time.Time, outcome identity.AuditOutcome, failureReason string) {
	if s.Audit == nil {
		return
	}
	action := "badge_grant_issued"
	if outcome != identity.AuditOutcomeSuccess {
		action = "badge_grant_denied"
	}
	detail := map[string]any{"terminalUserId": terminalUserId}
	if operatorUserId != "" {
		detail["operatorUserId"] = operatorUserId
	}
	if !expiresAt.IsZero() {
		detail["grantExpiresAt"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	s.Audit.Log(ctx, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		ActorUserId:   terminalUserId,
		TargetType:    "badgeIdentity",
		TargetId:      badgeIdentityId,
		SourceIP:      sourceIP,
		Outcome:       outcome,
		FailureReason: failureReason,
		Detail:        detail,
	})
}
