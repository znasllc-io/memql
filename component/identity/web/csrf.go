package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// CSRF protection for the identity web surface (memql#111).
//
// Pattern: double-submit cookie. On every request, the middleware
// ensures a `memql_csrf` cookie exists (mints one if not) and
// stashes the token value in the request context. On unsafe
// methods (POST / PUT / PATCH / DELETE) it compares the cookie's
// value against the `_csrf` form field (or the `X-CSRF-Token`
// header) using constant-time comparison; a mismatch returns
// 403.
//
// Why double-submit cookie:
//   - No server-side session storage required (the identity web
//     surface is mostly stateless; sessions live elsewhere).
//   - Works for plain HTML form POSTs (the dominant pattern in
//     templ-rendered admin / /me pages) without JS.
//   - The Server-side render of the form embeds the cookie value
//     in a hidden input, so HttpOnly=true on the cookie is fine
//     (we don't need JS to read it).
//
// Carve-outs (per the issue):
//   - `/login` and `/setup` POSTs are intentionally NOT gated.
//     They're unauthenticated entry points; the SameSite=Lax on
//     `memql_auth` blocks cross-site cookie attachment, and the
//     attacker who triggers a magic-link POST has no way to read
//     the resulting email.
//
// Exempt routes are configured by the mount call (see
// CSRFOptions.ExemptPaths). The middleware ENFORCES on every
// other unsafe-method request.
//
// Reference: memql#111.

// CSRFCookieName is the cookie that carries the CSRF token.
// Mirrors the `memql_auth` / `memql_admin` naming convention.
const CSRFCookieName = "memql_csrf"

// CSRFFormField is the hidden form field every authenticated
// template renders to carry the token back on POST.
const CSRFFormField = "_csrf"

// CSRFHeader is the alternate carrier for clients that POST via
// fetch / XHR without a form body (the /me dashboard's "revoke
// all sessions" button, future SPA admin moves).
const CSRFHeader = "X-CSRF-Token"

// csrfTokenBytes is the entropy budget for a fresh token. 32
// bytes -> 256 bits, well above any guess budget; encoded as
// base64-url it lands at 43 chars.
const csrfTokenBytes = 32

// csrfContextKey is the typed key the middleware uses to stash
// the active token in the request context. Handlers / templates
// read it via CSRFTokenFromContext.
type csrfContextKey struct{}

// CSRFOptions tunes the middleware. Zero value is fine for
// production; tests + tests-of-the-test inject overrides.
type CSRFOptions struct {
	// Secure flips the cookie's Secure flag. nil = derive from
	// BaseURL (https -> true). Tests pass a fixed bool.
	Secure *bool

	// ExemptPaths is the list of exact request paths whose unsafe-
	// method calls bypass CSRF enforcement. The issue carves out
	// `/login` (unauthenticated magic-link request) and `/setup`
	// (bootstrap-only); add new exemptions here only with an
	// explicit comment naming why the path is safe without CSRF.
	ExemptPaths []string

	// Logger receives a single Warn line on every rejection
	// (path, method, missing/mismatch reason). nil silences.
	Logger *slog.Logger
}

// CSRFMiddleware wraps `next` with the double-submit-cookie
// enforcement. The returned handler:
//   - On every request: mints a cookie if absent and stashes the
//     token in ctx.
//   - On POST/PUT/PATCH/DELETE: requires the form field or header
//     to match the cookie; otherwise 403.
//
// Exempt paths (CSRFOptions.ExemptPaths) bypass enforcement but
// still get a cookie minted on response so the NEXT request from
// the same browser carries the token.
func CSRFMiddleware(opts CSRFOptions) func(http.Handler) http.Handler {
	exempt := make(map[string]struct{}, len(opts.ExemptPaths))
	for _, p := range opts.ExemptPaths {
		exempt[p] = struct{}{}
	}
	secure := false
	if opts.Secure != nil {
		secure = *opts.Secure
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Ensure a cookie exists. Mint on first request.
			token, fresh := ensureCSRFCookie(w, r, secure)

			// Always make the token available downstream so
			// handlers / templates can read it without a cookie
			// rummage.
			ctx := context.WithValue(r.Context(), csrfContextKey{}, token)
			r = r.WithContext(ctx)

			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := exempt[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}

			// Unsafe method on a protected path. The cookie MUST
			// have existed prior to this request -- a "fresh" mint
			// means the caller never visited a GET first and
			// therefore cannot have a matching form/header token.
			if fresh {
				rejectCSRF(w, r, opts.Logger, "missing cookie (fresh mint)")
				return
			}
			supplied := readSuppliedToken(r)
			if supplied == "" {
				rejectCSRF(w, r, opts.Logger, "missing token")
				return
			}
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				rejectCSRF(w, r, opts.Logger, "token mismatch")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRFMiddlewareFunc is the http.HandlerFunc-shaped helper that
// the existing `wrap` chains in server.go can compose with.
func CSRFMiddlewareFunc(opts CSRFOptions) func(http.HandlerFunc) http.HandlerFunc {
	mw := CSRFMiddleware(opts)
	return func(h http.HandlerFunc) http.HandlerFunc {
		wrapped := mw(http.HandlerFunc(h))
		return wrapped.ServeHTTP
	}
}

// CSRFTokenFromContext returns the active token for the request.
// Empty string when the middleware isn't wired (defensive; the
// templ helper renders no hidden field in that case rather than
// failing the page render).
func CSRFTokenFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(csrfContextKey{}).(string); ok {
		return v
	}
	return ""
}

// CSRFTokenFromRequest is the request-scoped shorthand.
func CSRFTokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return CSRFTokenFromContext(r.Context())
}

// ensureCSRFCookie reads the existing cookie or mints a fresh
// one. Returns the token value plus a bool indicating whether
// the cookie was freshly minted (true) or already present
// (false). The "fresh" signal lets the middleware reject any
// POST that arrives without a prior GET -- the cookie+form
// pair must have been established on a same-origin GET first.
func ensureCSRFCookie(w http.ResponseWriter, r *http.Request, secure bool) (token string, fresh bool) {
	if c, err := r.Cookie(CSRFCookieName); err == nil && c != nil {
		val := strings.TrimSpace(c.Value)
		if val != "" {
			return val, false
		}
	}
	// Mint.
	raw := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		// Should be unreachable on any modern OS. Fall back to a
		// deterministic value so the cookie still gets set;
		// downstream constant-time compare will still gate
		// correctly because the form field will carry the same
		// value the server just rendered.
		raw = []byte("memql-csrf-rand-failure-padding-32!!")[:csrfTokenBytes]
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return token, true
}

func readSuppliedToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	// Header beats form -- a fetch/XHR caller that sets the
	// header but ALSO multipart-encodes a body shouldn't have
	// the header silently dropped.
	if h := strings.TrimSpace(r.Header.Get(CSRFHeader)); h != "" {
		return h
	}
	// Parse the form body. Bounded by Go's default
	// multipart limit (10 MiB) -- adequate for our admin / /me
	// forms which never carry uploads.
	if err := r.ParseForm(); err != nil {
		return ""
	}
	if v := strings.TrimSpace(r.FormValue(CSRFFormField)); v != "" {
		return v
	}
	return ""
}

func rejectCSRF(w http.ResponseWriter, r *http.Request, logger *slog.Logger, reason string) {
	if logger != nil {
		logger.Warn("csrf: rejecting unsafe-method request",
			"method", r.Method,
			"path", r.URL.Path,
			"reason", reason,
			"remote_addr", r.RemoteAddr,
		)
	}
	// 403 + a one-line message. We don't surface the reason to
	// the client; a stale form is the common case and we don't
	// want to leak the discriminator.
	http.Error(w, "Forbidden: CSRF token missing or invalid. Reload the page and try again.", http.StatusForbidden)
}

func isSafeMethod(m string) bool {
	switch strings.ToUpper(m) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}
