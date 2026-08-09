package http

// The shared "a grant succeeded -- give this user a session" tail of
// POST /oauth/token.
//
// Extracted when the RFC 8628 device grant landed (memql#3410). It is
// shared rather than copied because a device sign-in is not a
// different KIND of session: it produces the same access + refresh
// pair, the same v1:identity:authSession row, the same admin-tunable
// TTLs and the same refresh cookie as an authorization_code redemption
// does. Two copies of this would drift on the first TTL change, and
// the drift would be invisible until somebody noticed one grant's
// sessions outliving the other's.
//
// What is NOT here is anything grant-specific: validating a
// credential, checking PKCE, and spending the single-use row all
// happen in the caller, BEFORE this runs. By the time control reaches
// here the only question left is which user.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
)

// sessionMintInput names the facts a grant carries into the session.
type sessionMintInput struct {
	// UserId is the authenticated subject. Required.
	UserId string
	// IdentityId is the credential row the sign-in came through, used
	// for per-credential lastUsedAt bookkeeping. Empty for grants with
	// no credential row behind them -- the device grant's authorization
	// is a human clicking Approve, not an identity row.
	IdentityId string
	// ClientId is the OAuth client the session belongs to; recorded on
	// the audit event.
	ClientId string
	// Source labels the session row's origin ("bff_exchange",
	// "device_code"). It is what makes a device-originated session
	// distinguishable in /me/devices and in an incident review.
	Source string
	// Now is the issuance instant, passed in so a caller that has
	// already stamped a row with it does not record two different
	// times for one event.
	Now time.Time
}

// issueSessionForUser mints the access + refresh pair, persists the
// session row, sets the refresh cookie, and returns the RFC 6749 §5.1
// body. The caller writes the response.
func (s *Server) issueSessionForUser(w http.ResponseWriter, r *http.Request, in sessionMintInput) (tokenResponse, error) {
	var out tokenResponse
	if in.UserId == "" {
		return out, fmt.Errorf("issueSessionForUser: userId required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	source := in.Source
	if source == "" {
		source = "bff_exchange"
	}

	refreshPlain, refreshHash, err := refresh.NewRefreshToken()
	if err != nil {
		return out, fmt.Errorf("token mint: %w", err)
	}
	sessionId, err := identity.NewRandomId("")
	if err != nil {
		return out, fmt.Errorf("session id mint: %w", err)
	}

	// Pull the runtime-tunable token settings (admin-edited TTLs +
	// cookie SameSite) once. effectiveTokenSettings merges live
	// overrides with s.Cfg fallbacks so a freshly bootstrapped cluster
	// with no admin row still produces sane values.
	live := s.effectiveTokenSettings(r.Context())

	// Resolve the user row so the freshly minted JWT carries the
	// directory fields the SPA needs (email, name, given_name,
	// family_name, role). Without this the access token's `email` and
	// name claims would be empty strings -- the SPA would render the
	// profile as "N/A" until the next /auth/refresh.
	tokenInput := identity.IssueInput{
		UserId:      in.UserId,
		SessionId:   sessionId,
		TTLOverride: live.AccessTokenTTL,
	}
	if user, err := s.Store.LookupUserById(r.Context(), in.UserId); err == nil && user != nil {
		tokenInput.Email = user.PrimaryEmail
		tokenInput.Name = user.DisplayName
		tokenInput.GivenName = user.FirstName
		tokenInput.FamilyName = user.LastName
		tokenInput.Role = user.Role
		tokenInput.Internal = user.Internal
		tokenInput.RevocationEpoch = user.RevocationEpoch
	} else if err != nil && s.Logger != nil {
		s.Logger.Warn("token_user_lookup_failed", "user_id", in.UserId, "error", err.Error())
	}
	access, accessExp, err := s.Issuer.IssueAccessToken(tokenInput, now)
	if err != nil {
		return out, fmt.Errorf("issue access token: %w", err)
	}

	// The session row tracks the access-token hash for the auth
	// middleware's revocation check; the refresh-token hash rolls
	// forward via rotateAuthSession on every /auth/refresh.
	expiresAt := now.Add(live.RefreshTokenTTL).Format(time.RFC3339Nano)
	if err := s.Store.CreateAuthSession(
		r.Context(),
		sessionId,
		in.UserId, // subject for now is the userId
		hashCode(access),
		source,
		in.UserId,
		in.IdentityId,
		r.Header.Get("User-Agent"),
		expiresAt,
	); err != nil {
		return out, fmt.Errorf("session persist: %w", err)
	}

	// Stamp the freshly minted refresh-token hash so the first refresh
	// call has something to compare against. previousRefreshTokenHash
	// is empty on the initial mint -- there's no prior hash to keep in
	// the grace window yet. The first /auth/refresh will populate it.
	if err := s.Store.RotateAuthSession(r.Context(), sessionId, refreshHash, "", expiresAt); err != nil {
		// Non-fatal: the session is already there; the user can still
		// use the access token. The first /auth/refresh will mint a new
		// pair and the session row catches up at that point.
		if s.Logger != nil {
			s.Logger.Warn("token_session_initial_rotate_failed", "error", err.Error())
		}
	}

	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "session_created",
		TargetType:  "session",
		TargetId:    sessionId,
		ActorUserId: in.UserId,
		Outcome:     identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"clientId": in.ClientId,
			"source":   source,
		},
	})

	expiresIn := int(live.AccessTokenTTL / time.Second)
	if expiresIn <= 0 {
		expiresIn = int(time.Until(accessExp) / time.Second)
	}

	// Set the refresh-token cookie on the sign-in response. The SPA's
	// silent-rotate flow depends on it -- /auth/refresh reads the
	// cookie first before falling back to body / Authorization.
	// Without this, the SPA hit /auth/refresh on every access-token
	// expiry, got 401, classified the response as `unauthenticated`,
	// and bumped the user back to the sign-in page silently. The token
	// also rides on the JSON body so non-browser clients that don't
	// track cookies (CLI / SDK / device-flow callers) can still drive
	// the refresh flow off the body value.
	setRefreshCookie(w, refreshPlain, s.Cfg.BaseURL, live.RefreshCookieSameSite)

	return tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		RefreshToken: refreshPlain,
	}, nil
}
