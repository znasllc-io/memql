package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
)

// adminCookieName is the cookie that carries the JWT for an
// established admin session. Scoped to the identity domain (path /);
// HttpOnly + Secure + SameSite=Lax (the admin pages live on the same
// origin as identity, so Lax is correct).
const adminCookieName = "memql_admin"

// claimsCtxKey is the context-value key for the validated access-token
// claims set by requireAdmin.
type claimsCtxKey struct{}

// requireAdmin returns a middleware that enforces the role gate. On
// success the validated *identity.AccessTokenClaims is stashed on
// the request context so handlers can read the actor identity for
// audit + display.
//
// Two acceptance modes:
//
//   - Authorization: Bearer <jwt>     -- API consumers / curl debug.
//   - Cookie memql_admin=<jwt>        -- browser sessions established
//     via /admin/establish.
//
// Failure modes:
//
//   - No token       -> 302 redirect to /admin/login (HTML clients)
//     or 401 JSON for XHR requests.
//   - Invalid token  -> 401 with audit event admin_auth_failed.
//   - Wrong role     -> 403 with audit event admin_auth_forbidden.
func (s *AdminServer) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := extractAdminToken(r)
		if raw == "" {
			s.respondUnauthenticated(w, r)
			return
		}

		claims, err := s.Issuer.VerifyAccessToken(raw, time.Now().UTC())
		if err != nil {
			s.audit(r, identity.AuditEvent{
				Category:      identity.AuditCategoryAdmin,
				Action:        "admin_auth_failed",
				Outcome:       identity.AuditOutcomeFailure,
				FailureReason: err.Error(),
			})
			s.writeAuthError(w, r, http.StatusUnauthorized,
				"Your session is invalid or expired. Sign in again to continue.")
			return
		}

		role := strings.ToLower(strings.TrimSpace(claims.Role))
		if role != "owner" && role != "admin" {
			s.audit(r, identity.AuditEvent{
				Category:      identity.AuditCategoryAdmin,
				Action:        "admin_auth_forbidden",
				ActorUserId:   claims.Subject,
				ActorEmail:    claims.Email,
				ActorRole:     claims.Role,
				Outcome:       identity.AuditOutcomeBlocked,
				FailureReason: "role_not_admin",
			})
			s.writeAuthError(w, r, http.StatusForbidden,
				"Your account is signed in but does not have admin access.")
			return
		}

		ctx := context.WithValue(r.Context(), claimsCtxKey{}, claims)

		// Stamp the verified admin's identity onto the context so
		// engine mutations triggered by admin handlers
		// (mutationUpdateUser, mutationCreateAuditEvent, etc.)
		// satisfy the actor-required check. Without this the
		// mutation pipeline rejects with "no actor found in
		// context". Use the real admin's subject + email so audit
		// rows attribute changes to the operator, not to a
		// synthetic system identity.
		actorClaims := map[string]any{
			"sub":   claims.Subject,
			"email": claims.Email,
			"role":  claims.Role,
			"name":  claims.Name,
		}
		if claims.Internal {
			actorClaims["internal"] = true
		}
		token := auth.BuildTokenInfo(actorClaims)
		ctx = auth.ContextWithClaims(ctx, actorClaims)
		ctx = auth.ContextWithToken(ctx, token)

		next(w, r.WithContext(ctx))
	}
}

// claimsFromCtx pulls the validated claims off the context. Returns
// nil when missing — callers that need the actor must come through
// requireAdmin first.
func claimsFromCtx(ctx context.Context) *identity.AccessTokenClaims {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(claimsCtxKey{}).(*identity.AccessTokenClaims)
	return v
}

// extractAdminToken pulls the JWT from (in priority order):
//  1. Authorization: Bearer header
//  2. memql_admin cookie
//
// Trailing whitespace is trimmed; an empty string means no token.
func extractAdminToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		parts := strings.Fields(v)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if c, err := r.Cookie(adminCookieName); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

// respondUnauthenticated handles the no-token case. HTML browsers
// get a redirect to /admin/login so the operator can paste a token.
// XHR-style callers (Accept: application/json) get a typed JSON 401.
func (s *AdminServer) respondUnauthenticated(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"sign in required"}`))
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// writeAuthError surfaces a 401/403 with an HTML page for browsers
// or a typed JSON envelope for XHR. The HTML path drops the operator
// back on /admin/login with a flash message so they can re-paste a
// fresh token.
func (s *AdminServer) writeAuthError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"auth_failed","message":"` + jsonEscape(message) + `"}`))
		return
	}
	// Clear the bad cookie so the operator's next paste isn't shadowed.
	clearAdminCookie(w)
	target := "/admin/login?flash=" + urlQueryEscape(message) + "&flash_kind=error"
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// wantsJSON returns true when the caller signalled it wants JSON
// (XHR APIs). HTML browsers send Accept: text/html as the first
// preference; XHR clients typically send Accept: application/json
// or X-Requested-With: XMLHttpRequest.
func wantsJSON(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return true
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	// Pick the first media type. application/json beats text/html =>
	// XHR; otherwise assume HTML.
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(part)
		if mt == "" {
			continue
		}
		if strings.HasPrefix(mt, "application/json") {
			return true
		}
		if strings.HasPrefix(mt, "text/html") {
			return false
		}
		// First entry wins.
		return false
	}
	return false
}

// setAdminCookie writes the JWT-bearing cookie. SameSite=Lax because
// admin pages live on the same origin as identity; HttpOnly so JS
// cannot read it; Secure on HTTPS deployments.
func setAdminCookie(w http.ResponseWriter, jwt, baseURL string) {
	secure := strings.HasPrefix(strings.ToLower(baseURL), "https://")
	c := &http.Cookie{
		Name:     adminCookieName,
		Value:    jwt,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		// Cookie has no MaxAge so it dies with the browser session.
		// JWT exp validation prevents an extended-replay window even
		// if the cookie were persistent — short-lived tokens do the
		// real work here.
	}
	http.SetCookie(w, c)
}

// clearAdminCookie wipes the cookie. Used after a failed verify and
// on /admin/logout.
func clearAdminCookie(w http.ResponseWriter) {
	c := &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, c)
}

// jsonEscape produces a minimally-quoted JSON string body suitable
// for direct concatenation into a JSON envelope. Used because we
// build the small error envelopes by hand to avoid pulling in
// encoding/json for one-line replies.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// urlQueryEscape replaces a few characters so the message round-trips
// safely through a redirect query string. Doesn't pull in net/url just
// for this single hot path.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteByte('+')
		case r == '&', r == '=', r == '#', r == '%', r == '?':
			// Skip / drop these few characters rather than percent-
			// encode; the message body never needs them.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// audit attaches request metadata (source IP, user-agent, claims-
// derived actor identity) and forwards to the configured AuditLogger.
// Safe to call even when Audit is nil.
func (s *AdminServer) audit(r *http.Request, ev identity.AuditEvent) {
	if s == nil || s.Audit == nil {
		return
	}
	if r != nil {
		if ev.SourceIP == "" {
			ev.SourceIP = clientIP(r)
		}
		if ev.UserAgent == "" {
			ev.UserAgent = r.Header.Get("User-Agent")
		}
		if claims := claimsFromCtx(r.Context()); claims != nil {
			if ev.ActorUserId == "" {
				ev.ActorUserId = claims.Subject
			}
			if ev.ActorEmail == "" {
				ev.ActorEmail = claims.Email
			}
			if ev.ActorRole == "" {
				ev.ActorRole = claims.Role
			}
		}
	}
	s.Audit.Log(r.Context(), ev)
}

// auditActor produces an identity.AuditActor from the request claims.
// Used by handlers that need to attribute key rotations and similar
// privileged operations.
func (s *AdminServer) auditActor(r *http.Request) identity.AuditActor {
	if r == nil {
		return identity.AuditActor{}
	}
	claims := claimsFromCtx(r.Context())
	if claims == nil {
		return identity.AuditActor{}
	}
	return identity.AuditActor{
		UserId: claims.Subject,
		Email:  claims.Email,
		Role:   claims.Role,
	}
}

// clientIP pulls the originating IP off the request, honoring common
// proxy headers when set. Best-effort.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if comma := strings.IndexByte(v, ','); comma >= 0 {
			return strings.TrimSpace(v[:comma])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}
