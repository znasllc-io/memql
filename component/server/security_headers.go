package server

import "net/http"

// SecurityHeadersMiddleware writes the baseline hardening header set
// onto every response. Matches the shape of the identity-service
// header middleware (component/identity/web/security_headers.go) so
// the posture is uniform across the binary's HTTP surface; this copy
// lives in the server package so it can be mounted on every HTTP
// router (BFF / gRPC-gateway / etc.) without taking on a dependency
// on the identity package.
//
//   - X-Frame-Options: DENY — block iframe embedding (clickjacking).
//   - X-Content-Type-Options: nosniff — block content-type sniffing.
//   - Referrer-Policy: strict-origin-when-cross-origin — limit
//     referrer leakage on cross-origin navigations.
//   - Permissions-Policy: geolocation=(), microphone=(), camera=() —
//     deny the three most-sensitive sensor permissions by default.
//   - Strict-Transport-Security: max-age=31536000; includeSubDomains
//     — TLS-only. HSTS over plaintext is a no-op per the spec AND
//     poisons the browser's STS store for the host (especially Safari
//     on localhost). Skipping the header on HTTP keeps dev unbroken.
//
// CSP and frame-ancestors are intentionally NOT set here — they are
// HTML-specific and live on the identity web UI's per-page middleware.
// API responses (JSON, gRPC-Web) don't need them.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if r != nil && r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
