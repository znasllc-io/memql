package web

import "net/http"

// securityHeaders writes the baseline hardening header set onto w.
//
// HSTS is only emitted on TLS requests. HSTS over plain HTTP is a
// no-op per the spec AND, more importantly, browsers (especially
// Safari) cache it aggressively for the host — once seen on
// http://localhost it forces every future localhost request to
// upgrade to https, which then fails because dev runs over HTTP.
// Skipping it on HTTP keeps localhost development from being
// poisoned in the browser's STS store.
func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
	if r != nil && r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// SecurityHeadersMiddleware sets a baseline of common hardening
// headers on every response. Pairs with CSPMiddleware (which carries
// the policy) — split out so callers can opt into one or the other
// independently in the rare case they need to.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w, r)
		next.ServeHTTP(w, r)
	})
}

// SecurityHeadersHandlerFunc is the handler-shaped variant used at
// per-route mount points.
func SecurityHeadersHandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w, r)
		next(w, r)
	}
}
