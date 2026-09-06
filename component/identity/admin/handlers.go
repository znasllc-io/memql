package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// ---------------------------------------------------------------------------
// Auth-establishment handlers (no requireAdmin gate)
// ---------------------------------------------------------------------------

// handleLoginGet renders the paste-token form. The form is the only
// way to seed an admin session in a vanilla browser today; a future
// commit can wire a "Open admin" button on the SPA that POSTs the
// live access token to /admin/establish.
func (s *AdminServer) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	data := webtempl.AdminLoginData{
		Layout: s.layoutData(r, "Admin sign-in", true),
		Flash:  readFlash(r),
	}
	s.render(w, r, "admin/login", webtempl.AdminLogin(data))
}

// handleEstablish accepts a JWT (form field `token` or Authorization
// header), validates it, requires role=owner|admin, and sets the
// admin cookie. On success: redirects to /admin/. On failure: drops
// the operator back to /admin/login with an error flash.
func (s *AdminServer) handleEstablish(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/login?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	tok := strings.TrimSpace(r.PostForm.Get("token"))
	if tok == "" {
		// Try the Authorization header for API-style consumers.
		tok = extractAdminToken(r)
	}
	if tok == "" {
		http.Redirect(w, r, "/admin/login?flash=Paste+a+JWT+to+continue&flash_kind=error", http.StatusSeeOther)
		return
	}

	claims, err := s.Issuer.VerifyAccessToken(tok, time.Now().UTC())
	if err != nil {
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAdmin,
			Action:        "admin_establish_failed",
			Outcome:       identity.AuditOutcomeFailure,
			FailureReason: err.Error(),
		})
		http.Redirect(w, r, "/admin/login?flash=Token+is+invalid+or+expired&flash_kind=error", http.StatusSeeOther)
		return
	}
	role := strings.ToLower(strings.TrimSpace(claims.Role))
	if role != "owner" && role != "admin" {
		s.audit(r, identity.AuditEvent{
			Category:      identity.AuditCategoryAdmin,
			Action:        "admin_establish_forbidden",
			ActorUserId:   claims.Subject,
			ActorEmail:    claims.Email,
			ActorRole:     claims.Role,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "role_not_admin",
		})
		http.Redirect(w, r, "/admin/login?flash=Token+is+valid+but+lacks+admin+role&flash_kind=error", http.StatusSeeOther)
		return
	}

	setAdminCookie(w, tok, s.Cfg.BaseURL)
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "admin_established",
		ActorUserId: claims.Subject,
		ActorEmail:  claims.Email,
		ActorRole:   claims.Role,
		Outcome:     identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r, s.shellHome(), http.StatusSeeOther)
}

// handleLogout clears the admin cookie and bounces to the login page.
func (s *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if claims := claimsFromRequestToken(r, s.Issuer); claims != nil {
		s.audit(r, identity.AuditEvent{
			Category:    identity.AuditCategoryAdmin,
			Action:      "admin_logout",
			ActorUserId: claims.Subject,
			ActorEmail:  claims.Email,
			ActorRole:   claims.Role,
			Outcome:     identity.AuditOutcomeSuccess,
		})
	}
	clearAdminCookie(w)
	http.Redirect(w, r, "/admin/login?flash=Signed+out&flash_kind=info", http.StatusSeeOther)
}

// claimsFromRequestToken decodes (without re-validating beyond the
// signature check) the JWT carried by the request. Used by logout to
// stamp an audit entry without rejecting the call when the token is
// already expired.
func claimsFromRequestToken(r *http.Request, issuer *identity.JWTIssuer) *identity.AccessTokenClaims {
	tok := extractAdminToken(r)
	if tok == "" || issuer == nil {
		return nil
	}
	claims, err := issuer.VerifyAccessToken(tok, time.Now().UTC())
	if err != nil {
		return nil
	}
	return claims
}

// ---------------------------------------------------------------------------
// Root
// ---------------------------------------------------------------------------

// handleRoot answers /admin/ now that the console serves no pages.
//
// Everything it served is in MemQL OS: the dashboard, users, tokens, audit,
// JWKS and settings moved to the portal in memql#3324, Deployments followed in
// memql#3380, and epic memql#4984 retired the portal in turn and moved them
// again -- Users, and Settings' Tokens / Keys / Cluster / AI providers
// sections.
//
// The route survives its pages deliberately. /admin/ is where handleEstablish
// lands an operator after sign-in and where a bookmark points, and a bare 404
// there reads as an outage. 410 Gone is the accurate status -- the resource
// existed, it is deliberately retired, and the reply says where it went --
// where a redirect would not be: the shell is served by the edge, on a
// different origin from this one, so this server cannot name a URL that is
// correct for every deployment.
func (s *AdminServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	dest := s.shellHome()
	msg := "The server-rendered admin console has been retired.\n\n" +
		"Every surface it served -- users, tokens, audit, JWKS, settings and " +
		"Deployments -- now lives in MemQL OS"
	if dest != "" && dest != "/me" {
		msg += " at " + dest + ".\n"
	} else {
		msg += ".\n"
	}
	_, _ = w.Write([]byte(msg))
}

// shellHome is the post-establish dest (memql#4144). Never /admin/.
func (s *AdminServer) shellHome() string {
	base := ""
	if s != nil {
		base = s.Cfg.BaseURL
	}
	return identity.DefaultPostLoginLanding("", base)
}
