package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/identity/refresh"
)

// refreshRequest is the body of POST /auth/refresh. The refresh token
// rides in the request body as a fallback, but the canonical channel
// is the httpOnly cookie set by /oauth/token.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
}

// refreshResponse mirrors the /oauth/token shape so SPA refresh logic
// can reuse the same parser.
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

const (
	refreshCookieName = "memql_refresh"
	// sessionMarkerCookieName is a JS-readable companion to
	// memql_refresh. It carries no token and grants no auth on its
	// own; it exists so the SPA can decide -- before making a
	// network call -- whether a session might exist. Without it the
	// SPA has to probe /auth/refresh on every cold load just to
	// find out the user is logged out, which surfaces in the
	// browser network log as a recurring "401 on /auth/refresh"
	// even though the SPA's JS handles it cleanly. With the marker,
	// the SPA skips the probe when the cookie is absent and only
	// hits /auth/refresh when there's actually a session to rotate.
	//
	// Lifecycle mirrors the refresh cookie: set on /oauth/token +
	// /auth/refresh rotation, cleared on /auth/logout + on a refresh
	// rejection. Value is constant ("1") -- the marker is purely a
	// presence signal, never a credential.
	sessionMarkerCookieName = "memql_session"
)

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	plain := strings.TrimSpace(extractRefreshToken(r))
	if plain == "" {
		// No cookie / body / header => the caller has no session
		// at all. Return 401 so SPA refresh logic classifies this
		// as "unauthenticated, please sign in" rather than a
		// transient infrastructure failure. RFC 6749 isn't crisp
		// on missing-credentials for refresh; we follow the same
		// posture as the rest of the bearer-auth surface.
		s.writeJSONError(w, http.StatusUnauthorized, "invalid_grant", "no refresh token presented")
		return
	}

	res, err := s.Rotator.Rotate(r.Context(), refresh.RotateInput{
		PresentedRefreshToken: plain,
		SourceIP:              clientIP(r),
		UserAgent:             r.Header.Get("User-Agent"),
	})
	if err != nil {
		switch {
		case errors.Is(err, refresh.ErrSessionNotFound),
			errors.Is(err, refresh.ErrSessionRevoked),
			errors.Is(err, refresh.ErrSessionExpired),
			errors.Is(err, refresh.ErrTokenMismatch):
			// Cookie is stale -- clear it AND the session marker so
			// the SPA stops probing this endpoint until the user
			// signs back in. Without the clear, every subsequent
			// page load would see the marker, hit /auth/refresh,
			// and get another 401 -- the loop the marker is
			// designed to break.
			live := s.effectiveTokenSettings(r.Context())
			clearRefreshCookie(w, s.Cfg.BaseURL, live.RefreshCookieSameSite)
			s.writeJSONError(w, http.StatusUnauthorized, "invalid_grant", "refresh token is no longer valid")
			return
		case errors.Is(err, refresh.ErrNoToken):
			s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "refresh token required")
			return
		default:
			eid := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("refresh_failed",
					slog.String("error_id", eid),
					slog.String("error", err.Error()))
			}
			s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "refresh failed; reference "+eid)
			return
		}
	}

	// Roll the refresh-token cookie forward. SameSite override comes
	// from the runtime clusterSettings row; empty falls through to
	// IDENTITY_REFRESH_COOKIE_SAMESITE env (which itself defaults
	// to "lax").
	live := s.effectiveTokenSettings(r.Context())
	setRefreshCookie(w, res.RefreshToken, s.Cfg.BaseURL, live.RefreshCookieSameSite)

	writeJSON(w, http.StatusOK, refreshResponse{
		AccessToken:  res.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    res.ExpiresIn,
		RefreshToken: res.RefreshToken,
	})
}

// extractRefreshToken pulls the refresh token from (in priority order)
// the cookie, the JSON request body, or the Authorization header.
func extractRefreshToken(r *http.Request) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if r.Body != nil && r.ContentLength > 0 {
		var body refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.RefreshToken != "" {
			return body.RefreshToken
		}
	}
	if v := r.Header.Get("Authorization"); v != "" {
		parts := strings.Fields(v)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return parts[1]
		}
	}
	return ""
}

// setRefreshCookie writes the refresh-token cookie with the right
// SameSite setting for the deployment topology. See cookieAttrs for
// the matrix. sameSiteOverride is non-empty when the runtime
// clusterSettings row provides a value; empty falls through to the
// env var read by cookieAttrs.
func setRefreshCookie(w http.ResponseWriter, value, baseURL, sameSiteOverride string) {
	secure, sameSite := cookieAttrs(baseURL, sameSiteOverride)
	c := &http.Cookie{
		Name:     refreshCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	}
	http.SetCookie(w, c)
	// JS-readable companion -- presence flag for the SPA bootstrap.
	// Same Path / Secure / SameSite as the refresh cookie so the two
	// always travel together on the same network paths; HttpOnly is
	// false so document.cookie can see it. Constant value -- never
	// inspected by the server.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionMarkerCookieName,
		Value:    "1",
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: sameSite,
	})
}

// clearRefreshCookie wipes the refresh cookie. Used by /auth/logout.
// Same SameSite resolution as setRefreshCookie -- the cleared cookie
// must carry the same attributes as the one being replaced or the
// browser keeps the old one.
func clearRefreshCookie(w http.ResponseWriter, baseURL, sameSiteOverride string) {
	secure, sameSite := cookieAttrs(baseURL, sameSiteOverride)
	c := &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	}
	http.SetCookie(w, c)
	// Companion presence marker -- cleared together with the refresh
	// cookie so the SPA's bootstrap doesn't probe /auth/refresh after
	// a logout.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionMarkerCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   secure,
		SameSite: sameSite,
	})
}

// cookieAttrs picks (Secure, SameSite) for the refresh cookie. Two
// inputs:
//
//  1. The BaseURL scheme. HTTPS gets Secure=true; HTTP gets
//     Secure=false. Non-negotiable -- Secure cookies are dropped on
//     HTTP, and SameSite=None requires Secure.
//
//  2. The SameSite preference. Three sources, in priority order:
//     (a) override -- the runtime clusterSettings value, when the
//     admin has explicitly picked one. Empty string falls
//     through to (b).
//     (b) IDENTITY_REFRESH_COOKIE_SAMESITE env var. Operator-set
//     on the identity binary's environment. Empty falls
//     through to (c).
//     (c) "lax" default.
//
//     Valid values:
//     - "lax": the cookie rides on top-level navigations and on
//     cross-origin XHR-with-credentials when the requesting
//     origin shares an eTLD+1 with the cookie's domain. Right
//     choice for the common "same-site self-hosted" topology
//     (app.acme.com + identity.acme.com); avoids Safari ITP
//     scrutiny that SameSite=None invites.
//     - "none": the cookie rides on every cross-site request as
//     long as Secure is set. Required for true cross-site
//     deployments where the SPA and identity service live under
//     different eTLD+1s (app.copresent.ai + auth.znasllc.io).
//     Browsers (especially Safari) treat the cookie as
//     third-party and apply ITP / cross-site-tracking rules.
//
// Default flipped from "none" to "lax" on 2026-05-09 -- the previous
// default was wrong for the most common self-hosted topology and
// was silently breaking refresh flows on Safari without any error
// path.
func cookieAttrs(baseURL, override string) (bool, http.SameSite) {
	secure := strings.HasPrefix(strings.ToLower(baseURL), "https://")
	sameSiteName := strings.ToLower(strings.TrimSpace(override))
	if sameSiteName == "" {
		sameSiteName = strings.ToLower(strings.TrimSpace(os.Getenv("IDENTITY_REFRESH_COOKIE_SAMESITE")))
	}
	switch sameSiteName {
	case "none":
		// None requires Secure=true. If we're on HTTP dev, the
		// browser will reject the Set-Cookie outright; downgrade
		// to Lax to keep dev flows usable. Operators with HTTP
		// production deployments are out of scope.
		if !secure {
			return false, http.SameSiteLaxMode
		}
		return true, http.SameSiteNoneMode
	case "", "lax":
		return secure, http.SameSiteLaxMode
	default:
		// Unknown value -- log-and-default rather than fail-closed.
		// Could also be Strict here in the future; not yet exposed
		// because Strict would break the post-redirect SPA bootstrap
		// (browser drops the cookie on the cross-origin POST).
		return secure, http.SameSiteLaxMode
	}
}
