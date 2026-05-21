package http

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
)

// tokenRequest is the body of POST /oauth/token. We only support the
// authorization_code grant type in v1; the password / client_credentials
// grants are out of scope.
type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	ClientId     string `json:"client_id"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier,omitempty"`
}

// tokenResponse mirrors the RFC 6749 §5.1 successful-response shape.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	body, err := readTokenRequest(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	if body.GrantType != "authorization_code" {
		s.writeJSONError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	if body.Code == "" || body.ClientId == "" || body.RedirectURI == "" {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "code, client_id, and redirect_uri are required")
		return
	}

	// Validate registered client + redirect URI.
	if s.Cfg.FindClient(body.ClientId) == nil || !s.Cfg.AllowsRedirectURI(body.ClientId, body.RedirectURI) {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_client", "client_id or redirect_uri is not registered")
		return
	}

	codeHash := hashCode(body.Code)
	row, err := s.Store.LookupAuthCodeByCodeHash(r.Context(), codeHash)
	if err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("token_lookup_failed", slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "lookup failed; reference "+eid)
		return
	}
	if row == nil {
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "auth_code_redemption_blocked",
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "code_not_found",
		})
		s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "auth code is invalid")
		return
	}

	now := time.Now().UTC()
	switch {
	case !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt):
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "auth_code_redemption_blocked",
			TargetType:    "authCode",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "code_expired",
		})
		s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "auth code has expired")
		return

	case !row.ConsumedAt.IsZero():
		// Replay attempt — code already redeemed.
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "auth_code_replay",
			TargetType:    "authCode",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "code_replay",
		})
		s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "auth code has already been used")
		return

	case !constantTimeEq(row.ClientId, body.ClientId):
		s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "auth code does not match client_id")
		return

	case !constantTimeEq(row.RedirectURI, body.RedirectURI):
		s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "auth code does not match redirect_uri")
		return
	}

	// Consume the code BEFORE minting tokens so a concurrent replay
	// hits the consumedAt check.
	if err := s.Store.ConsumeAuthCode(r.Context(), row.ID, clientIP(r)); err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("token_consume_failed", slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "consume failed; reference "+eid)
		return
	}

	// Mint a fresh refresh + access pair, persist a new authSession row.
	refreshPlain, refreshHash, err := refresh.NewRefreshToken()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "token mint failed")
		return
	}
	sessionId, err := identity.NewRandomId("")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "session id mint failed")
		return
	}

	// Pull the runtime-tunable token settings (admin-edited TTLs +
	// cookie SameSite) once per request. effectiveTokenSettings
	// merges live overrides with s.Cfg fallbacks so a freshly
	// bootstrapped cluster with no admin row still produces sane
	// values.
	live := s.effectiveTokenSettings(r.Context())

	// Resolve the user row so the freshly minted JWT carries the
	// directory fields the SPA needs (email, name, given_name,
	// family_name, role). Without this the access token's `email` and
	// name claims would be empty strings -- the SPA would render the
	// profile as "N/A" until the next /auth/refresh.
	tokenInput := identity.IssueInput{
		UserId:      row.UserId,
		SessionId:   sessionId,
		TTLOverride: live.AccessTokenTTL,
	}
	if user, err := s.Store.LookupUserById(r.Context(), row.UserId); err == nil && user != nil {
		tokenInput.Email = user.PrimaryEmail
		tokenInput.Name = user.DisplayName
		tokenInput.GivenName = user.FirstName
		tokenInput.FamilyName = user.LastName
		tokenInput.Role = user.Role
		tokenInput.Internal = user.Internal
		tokenInput.RevocationEpoch = user.RevocationEpoch
	} else if err != nil && s.Logger != nil {
		s.Logger.Warn("token_user_lookup_failed", slog.String("user_id", row.UserId), slog.String("error", err.Error()))
	}
	access, accessExp, err := s.Issuer.IssueAccessToken(tokenInput, now)
	if err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("token_issue_failed", slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "issue failed; reference "+eid)
		return
	}

	// The session row tracks the access-token hash for the auth
	// middleware's revocation check; the refresh-token hash rolls
	// forward via mutationRotateAuthSession on every /auth/refresh.
	accessHash := hashCode(access)
	expiresAt := now.Add(live.RefreshTokenTTL).Format(time.RFC3339Nano)
	if err := s.Store.CreateAuthSession(
		r.Context(),
		sessionId,
		row.UserId, // subject for now is the userId
		accessHash,
		"bff_exchange",
		row.UserId,
		row.IdentityId,
		r.Header.Get("User-Agent"),
		expiresAt,
	); err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("token_session_persist_failed",
				slog.String("error_id", eid),
				slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "session persist failed; reference "+eid)
		return
	}

	// Stamp the freshly minted refresh-token hash so the first refresh
	// call has something to compare against. previousRefreshTokenHash
	// is empty on the initial mint -- there's no prior hash to keep
	// in the grace window yet. The first /auth/refresh will populate
	// it.
	if err := s.Store.RotateAuthSession(r.Context(), sessionId, refreshHash, "", expiresAt); err != nil {
		// Non-fatal: the session is already there; the user can still
		// use the access token. The first /auth/refresh will mint a
		// new pair and the session row catches up at that point.
		if s.Logger != nil {
			s.Logger.Warn("token_session_initial_rotate_failed",
				slog.String("error", err.Error()))
		}
	}

	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "session_created",
		TargetType:  "session",
		TargetId:    sessionId,
		ActorUserId: row.UserId,
		Outcome:     identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"clientId": row.ClientId,
		},
	})

	expiresIn := int(live.AccessTokenTTL / time.Second)
	if expiresIn <= 0 {
		expiresIn = int(time.Until(accessExp) / time.Second)
	}

	// Set the refresh-token cookie on the initial sign-in response.
	// This was missing for a long time, and the SPA's silent-rotate
	// flow depends on it -- /auth/refresh reads the cookie first
	// before falling back to body / Authorization. Without this set
	// at /oauth/token time, the SPA hit /auth/refresh on every
	// access-token expiry, got 401 (no refresh token presented),
	// classified the response as `unauthenticated`, and bumped the
	// user back to the sign-in page silently. The audit log showed
	// the symptom: zero `session_refreshed` events ever, only
	// repeated `session_created` from re-logins. The cookie also
	// rides on the JSON body (RefreshToken field) so non-browser
	// clients that don't track cookies (CLI / SDK callers) can
	// still drive the refresh flow off the body value.
	setRefreshCookie(w, refreshPlain, s.Cfg.BaseURL, live.RefreshCookieSameSite)

	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		RefreshToken: refreshPlain,
	})
}

// readTokenRequest accepts both application/json (memQL/CoPresent
// SPAs) and the OAuth-canonical x-www-form-urlencoded body so external
// integrations can call the endpoint with a stock OAuth client.
func readTokenRequest(r *http.Request) (*tokenRequest, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body tokenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		return &body, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return &tokenRequest{
		GrantType:    r.Form.Get("grant_type"),
		Code:         r.Form.Get("code"),
		ClientId:     r.Form.Get("client_id"),
		RedirectURI:  r.Form.Get("redirect_uri"),
		CodeVerifier: r.Form.Get("code_verifier"),
	}, nil
}

// hashCode is a thin alias around SHA-256 hex; mirrors the algorithm
// the magic-link verifier uses when stamping codeHash.
func hashCode(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// constantTimeEq compares two strings in constant time so an attacker
// cannot probe the redirect_uri / client_id mismatch path.
func constantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
