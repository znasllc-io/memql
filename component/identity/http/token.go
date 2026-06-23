package http

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
)

// tokenRequest is the body of POST /oauth/token. We support the
// authorization_code grant (initial code redemption) and the
// refresh_token grant (silent re-issue of an expired access token).
// The password / client_credentials grants are out of scope.
type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	ClientId     string `json:"client_id"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
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

	switch body.GrantType {
	case "authorization_code":
		// Handled inline below.
	case "refresh_token":
		s.handleRefreshTokenGrant(w, r, body)
		return
	default:
		s.writeJSONError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
		return
	}
	if body.Code == "" || body.ClientId == "" || body.RedirectURI == "" {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "code, client_id, and redirect_uri are required")
		return
	}

	// Validate registered client + redirect URI. Resolve through the
	// unified resolver so dynamically-registered (RFC 7591) clients in
	// the store work exactly like static MEMQL_IDENTITY_REGISTERED_CLIENTS.
	if identity.ResolveClient(r.Context(), s.Cfg, s.Store, body.ClientId) == nil ||
		!identity.ClientAllowsRedirectURI(r.Context(), s.Cfg, s.Store, body.ClientId, body.RedirectURI) {
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

	// PKCE (RFC 7636): when a challenge was bound to the code, a matching
	// verifier is REQUIRED. Validated before the code is consumed.
	if row.CodeChallenge != "" {
		if err := verifyPKCE(row.CodeChallenge, row.CodeChallengeMethod, body.CodeVerifier); err != nil {
			s.audit(r, identity.AuditEvent{
				Category:      identity.AuditCategoryAuth,
				Action:        "auth_code_redemption_blocked",
				TargetType:    "authCode",
				TargetId:      row.ID,
				Outcome:       identity.AuditOutcomeBlocked,
				FailureReason: "pkce_failed",
			})
			s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
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
	// forward via rotateAuthSession on every /auth/refresh.
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

// handleRefreshTokenGrant implements the OAuth 2.1 refresh_token grant
// at the token endpoint (RFC 6749 §6). claude.ai (and any stock OAuth
// client) posts grant_type=refresh_token + refresh_token=<...> when its
// 15-minute access token expires; without this branch the client falls
// back to a full re-authorization and the connector session drops. The
// rotation logic is shared with /auth/refresh via s.Rotator: the
// presented token is validated against the stored hash, a fresh access +
// rotated refresh token are minted, and the OLD refresh token is
// invalidated (its hash no longer points at the row outside the short
// reuse-grace window).
//
// Unlike /auth/refresh we read the refresh token ONLY from the request
// body -- an OAuth client tracks the token itself and presents it
// explicitly; there is no cookie to fall back to on this path.
func (s *Server) handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request, body *tokenRequest) {
	if s.Rotator == nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("token_refresh_no_rotator", slog.String("error_id", eid))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "refresh unavailable; reference "+eid)
		return
	}

	presented := strings.TrimSpace(body.RefreshToken)
	if presented == "" {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	// When a client_id is presented (claude.ai sends the id it received
	// at registration), confirm it still resolves. Public clients only
	// (token_endpoint_auth_method=none); there is no client secret to
	// check. A missing client_id is tolerated -- the refresh token itself
	// is the credential and is bound to the session row.
	if body.ClientId != "" &&
		identity.ResolveClient(r.Context(), s.Cfg, s.Store, body.ClientId) == nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_client", "client_id is not registered")
		return
	}

	res, err := s.Rotator.Rotate(r.Context(), refresh.RotateInput{
		PresentedRefreshToken: presented,
		SourceIP:              clientIP(r),
		UserAgent:             r.Header.Get("User-Agent"),
	})
	if err != nil {
		switch {
		case errors.Is(err, refresh.ErrSessionNotFound),
			errors.Is(err, refresh.ErrSessionRevoked),
			errors.Is(err, refresh.ErrSessionExpired),
			errors.Is(err, refresh.ErrTokenMismatch):
			// Stale / revoked / stolen refresh token. RFC 6749 §5.2:
			// invalid_grant. The client re-runs the full authorization
			// flow from here.
			s.writeJSONError(w, http.StatusBadRequest, "invalid_grant", "refresh token is no longer valid")
			return
		case errors.Is(err, refresh.ErrNoToken):
			s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
			return
		default:
			eid := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("token_refresh_failed",
					slog.String("error_id", eid),
					slog.String("error", err.Error()))
			}
			s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "refresh failed; reference "+eid)
			return
		}
	}

	// Roll the refresh-token cookie forward too, mirroring /auth/refresh,
	// so a browser-based OAuth client keeps a live cookie. Harmless for
	// non-browser clients (claude.ai) -- they read the rotated token off
	// the JSON body.
	live := s.effectiveTokenSettings(r.Context())
	setRefreshCookie(w, res.RefreshToken, s.Cfg.BaseURL, live.RefreshCookieSameSite)

	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  res.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    res.ExpiresIn,
		RefreshToken: res.RefreshToken,
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
		RefreshToken: r.Form.Get("refresh_token"),
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

// verifyPKCE checks a PKCE code_verifier (RFC 7636) against the stored
// challenge. method defaults to "S256" when empty. S256 compares
// base64url(sha256(verifier)) (no padding) to the challenge in constant
// time; "plain" compares the verifier directly. Any other method, or an
// empty verifier, is an error.
func verifyPKCE(challenge, method, verifier string) error {
	verifier = strings.TrimSpace(verifier)
	if verifier == "" {
		return errors.New("code_verifier required")
	}
	switch strings.TrimSpace(method) {
	case "", "S256":
		sum := sha256.Sum256([]byte(verifier))
		computed := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) != 1 {
			return errors.New("code_verifier does not match code_challenge")
		}
		return nil
	case "plain":
		if subtle.ConstantTimeCompare([]byte(verifier), []byte(challenge)) != 1 {
			return errors.New("code_verifier does not match code_challenge")
		}
		return nil
	default:
		return fmt.Errorf("unsupported code_challenge_method %q", method)
	}
}
