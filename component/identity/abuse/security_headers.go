package abuse

import "net/http"

// SecurityHeadersMiddleware sets a baseline of common hardening
// headers on every API response. The same baseline is set on the web
// UI by web.SecurityHeadersMiddleware; we keep a copy here so the
// abuse package doesn't need to import the web package (which is a
// heavier surface — embedded templates, CSP, etc.).
//
// Headers set:
//
//   - Strict-Transport-Security: long max-age + subdomains. Identity
//     is always served over HTTPS in any non-local deployment.
//   - X-Frame-Options: DENY. The auth API never renders inside a
//     frame; clickjacking on token endpoints is a real concern.
//   - X-Content-Type-Options: nosniff. Belt-and-suspenders for any
//     JSON response served from this surface.
//   - Referrer-Policy: strict-origin-when-cross-origin. Avoids
//     leaking magic-link URLs in Referer headers.
//   - Permissions-Policy: deny camera / microphone / geolocation by
//     default — these have no business on an auth API.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, r)
		next.ServeHTTP(w, r)
	})
}

// SecurityHeadersHandlerFunc is the handler-shaped variant for
// per-route mounting (matches web.SecurityHeadersHandlerFunc's
// signature so callers can swap one for the other).
func SecurityHeadersHandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, r)
		next(w, r)
	}
}

// setSecurityHeaders writes the hardening header set. HSTS is gated
// on TLS — emitting it over plain HTTP is a no-op per the spec AND
// (importantly) Safari + Chrome aggressively cache it for the host,
// which on localhost forces every future request to upgrade to
// https://localhost and fail. Skipping it on HTTP keeps localhost
// dev usable.
func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
	if r != nil && r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}
