package http

import (
	"errors"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// handleLogout ends both sessions held by this browser: the relying-party
// refresh session and the first-party SSO session. Leaving SSO alive makes
// the next /authorize silently sign the caller straight back in.
//
// Logout is idempotent: missing token / unknown session / already
// revoked all return 204. Storage failures return 503, but cookies are always
// cleared before the response is committed.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	plain := strings.TrimSpace(extractRefreshToken(r))
	live := s.effectiveTokenSettings(r.Context())
	// A deferred Set-Cookie runs AFTER WriteHeader and never reaches the
	// browser. Clear every cookie now, including when no session resolves.
	clearRefreshCookie(w, s.Cfg.BaseURL, live.RefreshCookieSameSite)
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: strings.HasPrefix(strings.ToLower(s.Cfg.BaseURL), "https://"),
		SameSite: http.SameSiteLaxMode,
	})
	r = r.WithContext(memqlengine.ContextWithFreshRead(r.Context()))
	seen := map[string]bool{}
	var failures error
	revoke := func(row *identity.AuthSessionRow, err error) {
		if err != nil {
			failures = errors.Join(failures, err)
			return
		}
		if row == nil || !row.RevokedAt.IsZero() || seen[row.ID] {
			return
		}
		seen[row.ID] = true
		if err := s.Store.RevokeAuthSession(r.Context(), row.ID, "user_action"); err != nil {
			failures = errors.Join(failures, err)
			return
		}
		s.audit(r, identity.AuditEvent{
			Category: identity.AuditCategoryAuth, Action: "session_revoked",
			TargetType: "session", TargetId: row.ID, ActorUserId: row.UserId,
			Outcome: identity.AuditOutcomeSuccess, Detail: map[string]any{"reason": "user_action"},
		})
	}

	if plain != "" {
		tokenHash := refresh.HashRefreshToken(plain)
		row, err := s.Store.LookupAuthSessionByRefreshTokenHash(r.Context(), tokenHash)
		if err == nil && row == nil {
			// A concurrent refresh may already have rotated this credential.
			row, err = s.Store.LookupAuthSessionByPreviousRefreshTokenHash(r.Context(), tokenHash)
		}
		revoke(row, err)
	}
	if cookie, err := r.Cookie(adminCookieName); err == nil && cookie.Value != "" {
		// Match the exact persisted credential hash, never an unverified sid
		// supplied by a caller. This also allows an expired cookie to sign out.
		revoke(s.Store.LookupAuthSessionByTokenHash(r.Context(), identity.HashSessionToken(cookie.Value)))
	}
	if failures != nil {
		s.logErr("logout: session revocation failed", failures)
		s.writeJSONError(w, http.StatusServiceUnavailable, "logout_failed", "Some sessions could not be ended. Please try again.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
