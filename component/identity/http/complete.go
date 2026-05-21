package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
)

// adminCookieName mirrors component/identity/admin.adminCookieName.
// Duplicated rather than imported to avoid an http <-> admin cycle —
// admin already imports http for the SecurityHeaders middleware.
const adminCookieName = "memql_admin"

// handleComplete is the user-clicks-the-link endpoint:
// GET /auth/complete?ml=<plainToken>&state=<x>
//
// On success it 302-redirects to the original OAuth client's
// redirectURI with ?code=<authCode>&state=<echo>. On failure it
// renders an HTML error page so users see a friendly message instead
// of a JSON blob.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	plain := strings.TrimSpace(q.Get("ml"))
	state := strings.TrimSpace(q.Get("state"))

	if plain == "" {
		s.renderCompleteError(w, "Missing magic link token. Please use the link from your email.")
		return
	}

	res, err := s.MLVerifier.Verify(r.Context(), magiclink.VerifyInput{
		PlainToken: plain,
		State:      state,
		SourceIP:   clientIP(r),
		UserAgent:  r.Header.Get("User-Agent"),
	})
	if err != nil {
		switch {
		case errors.Is(err, magiclink.ErrInvalidToken):
			s.renderCompleteError(w, "This sign-in link is invalid. Please request a new one.")
		case errors.Is(err, magiclink.ErrTokenExpired):
			s.renderCompleteError(w, "This sign-in link has expired. Please request a new one.")
		case errors.Is(err, magiclink.ErrTokenAlreadyUsed):
			s.renderCompleteError(w, "This sign-in link has already been used. Please request a new one.")
		case errors.Is(err, magiclink.ErrStateMismatch), errors.Is(err, magiclink.ErrOAuthCtxCorrupted):
			s.renderCompleteError(w, "We could not complete sign-in. Please request a new link.")
		default:
			eid := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("auth_complete_failed",
					slog.String("error_id", eid),
					slog.String("error", err.Error()))
			}
			s.renderCompleteError(w, fmt.Sprintf("Internal error completing sign-in (reference %s).", eid))
		}
		return
	}

	// Admin path: skip the OAuth code flow entirely. Two trust
	// origins land here:
	//   - res.Bootstrap     -- the /setup wizard's owner-mint click
	//   - res.AdminSession  -- a returning admin who hit /login
	//                          without a relying-party in scope
	// Both flags are server-stamped at issue time and decoded from
	// the magic-link row's oauthCtx; neither is request-derived.
	//
	// Subtlety: a Bootstrap link can ALSO carry an OAuth client +
	// redirect URI when /setup was reached from a relying-party-
	// driven /login (the cockpit case: it opens /login with
	// return_to=<loopback>, the pre-bootstrap branch redirects to
	// /setup preserving the query string, the wizard re-emits the
	// OAuth context as hidden fields, and IssueMagicLink stamps
	// clientId/redirectURI/state into the row's oauthCtx). For
	// those, we want the click to land at the cockpit's loopback
	// callback (`code=...&state=...`) so the cockpit can finish its
	// login automatically -- not /admin/. The presence of a non-
	// empty RedirectURI on the VerifyResult is the discriminator;
	// when it's set we fall through to buildClientCallback below.
	if (res.Bootstrap || res.AdminSession) && res.RedirectURI == "" {
		if err := s.startAdminSession(w, r, res); err != nil {
			eid := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("auth_complete_admin_session_failed",
					slog.String("error_id", eid),
					slog.String("user_id", res.UserId),
					slog.String("error", err.Error()))
			}
			s.renderCompleteError(w, "Internal error starting admin session (reference "+eid+").")
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}

	target, err := buildClientCallback(res.RedirectURI, res.AuthCode, res.State)
	if err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("auth_complete_redirect_build_failed",
				slog.String("error_id", eid),
				slog.String("redirectURI", res.RedirectURI),
				slog.String("error", err.Error()))
		}
		s.renderCompleteError(w, "Internal error building redirect target (reference "+eid+").")
		return
	}

	http.Redirect(w, r, target, http.StatusFound)
}

// startAdminSession mints an access token and stamps the memql_admin
// cookie. The cookie matches the shape component/identity/admin
// .setAdminCookie writes, so the admin middleware accepts it on the
// next request.
//
// Role is looked up off the user record so a non-admin user clicking
// an admin-session link gets a token reflecting their actual role.
// The /admin/ middleware will then reject them at the door, but we
// don't forge an owner claim into their JWT. Bootstrap-path users
// are always owner by construction (the verifier just minted them
// with role=owner) so this path coincides with the previous
// hard-coded "owner" for that case.
func (s *Server) startAdminSession(w http.ResponseWriter, r *http.Request, res *magiclink.VerifyResult) error {
	if s == nil || s.Issuer == nil {
		return errors.New("startAdminSession: nil issuer")
	}
	role := "reader"
	internal := false
	displayName := ""
	firstName := ""
	lastName := ""
	var revocationEpoch int64
	if s.Store != nil {
		if user, err := s.Store.LookupUserByEmail(r.Context(), res.Email); err == nil && user != nil {
			role = user.Role
			internal = user.Internal
			displayName = user.DisplayName
			firstName = user.FirstName
			lastName = user.LastName
			revocationEpoch = user.RevocationEpoch
		}
	}
	jwt, _, err := s.Issuer.IssueAccessToken(identity.IssueInput{
		UserId:          res.UserId,
		Email:           res.Email,
		Name:            displayName,
		GivenName:       firstName,
		FamilyName:      lastName,
		Role:            role,
		Internal:        internal,
		RevocationEpoch: revocationEpoch,
	}, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("startAdminSession: mint token: %w", err)
	}
	secure := strings.HasPrefix(strings.ToLower(s.Cfg.BaseURL), "https://")
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    jwt,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	if s.Audit != nil {
		action := "admin_session_started"
		if res.Bootstrap {
			action = "admin_bootstrap_session_started"
		}
		s.Audit.Log(r.Context(), identity.AuditEvent{
			Category:    identity.AuditCategoryAuth,
			Action:      action,
			ActorUserId: res.UserId,
			ActorEmail:  res.Email,
			ActorRole:   role,
			TargetType:  "user",
			TargetId:    res.UserId,
			SourceIP:    clientIP(r),
			UserAgent:   r.Header.Get("User-Agent"),
			Outcome:     identity.AuditOutcomeSuccess,
		})
	}
	return nil
}

// buildClientCallback returns the OAuth-style redirect target the
// browser should follow after a successful magic-link consume.
func buildClientCallback(redirectURI, code, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// renderCompleteError renders a tiny inline HTML page when something
// goes wrong on the consume path. Phase 3 will replace this with a
// proper template living under component/identity/web/.
func (s *Server) renderCompleteError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Sign-in error</title></head>
<body style="font-family:Helvetica,Arial,sans-serif;background:#f7f7f8;padding:48px;">
  <div style="max-width:480px;margin:0 auto;background:#ffffff;border-radius:8px;padding:32px;">
    <h1 style="font-size:18px;color:#111827;margin:0 0 12px;">Sign-in error</h1>
    <p style="font-size:14px;color:#374151;margin:0 0 8px;">%s</p>
  </div>
</body>
</html>`, escapeHTML(message))
}

// escapeHTML is a minimal escape used by renderCompleteError.
// (html/template would also work but pulling it in just for the inline
// error page is overkill.)
func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
