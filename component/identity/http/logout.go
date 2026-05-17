package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
)

// handleLogout revokes the current session (the one tied to the
// presented refresh token) and clears the refresh-token cookie.
//
// Logout is idempotent: missing token / unknown session / already
// revoked all return 204 so the SPA can blindly call this on its
// "sign out" click without retry logic.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	plain := strings.TrimSpace(extractRefreshToken(r))

	// Even on no-token, clear the cookie so the browser stops sending
	// it on subsequent requests. SameSite must match what the cookie
	// was originally set with or the browser keeps the old cookie
	// alongside the cleared one.
	live := s.effectiveTokenSettings(r.Context())
	defer clearRefreshCookie(w, s.Cfg.BaseURL, live.RefreshCookieSameSite)

	if plain == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	tokenHash := refresh.HashRefreshToken(plain)
	row, err := s.Store.LookupAuthSessionByRefreshTokenHash(r.Context(), tokenHash)
	if err != nil || row == nil {
		// Not found — succeed silently (idempotent).
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if row.RevokedAt.IsZero() {
		// Best-effort revoke. Failures don't block the response — the
		// cookie is already cleared.
		_ = s.Store.RevokeAuthSession(
			r.Context(),
			row.ID,
			row.Subject,
			row.TokenHash,
			row.Source,
			row.ExpiresAt.Format(time.RFC3339Nano),
			"user_action",
			row.UserId,
		)
		s.audit(r, identity.AuditEvent{
			Category:    identity.AuditCategoryAuth,
			Action:      "session_revoked",
			TargetType:  "session",
			TargetId:    row.ID,
			ActorUserId: row.UserId,
			Outcome:     identity.AuditOutcomeSuccess,
			Detail:      map[string]any{"reason": "user_action"},
		})
	}

	w.WriteHeader(http.StatusNoContent)
}
