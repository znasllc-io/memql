package web

import "net/http"

// cspBase is the strict policy enforced on every web-UI route. The
// only relaxation against `default-src 'self'` is `img-src 'self' data:`
// to allow the brand-logo data URI inlined into the layout. No remote
// hosts, no `unsafe-eval`.
//
// `style-src 'self'` with no 'unsafe-inline'. That concession existed for one
// reason -- the layout injected the brand colour as an inline style attribute
// on <body> -- and memql#4269 retired it: the same values are served as
// /static/brand.css, a :root override block rendered from cluster settings
// (component/identity/web/brand.go).
//
// Keep it that way. Reaching for an inline style attribute again re-weakens
// the directive for every page to move a value that has a stylesheet.
const cspBase = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self'; " +
	"script-src 'self'; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// upgradeDirective is appended to the policy on TLS responses. The
// `upgrade-insecure-requests` directive forces ALL subresource fetches
// to https — necessary in production, fatal on a local-dev http://
// origin (the browser tries to fetch https://localhost:8081/static/
// app.css, fails because no TLS, and the page renders unstyled).
const upgradeDirective = "; upgrade-insecure-requests"

// policyFor picks the right CSP for the request. HTTPS responses get
// the full policy with upgrade-insecure-requests; HTTP responses skip
// it so localhost dev doesn't get its stylesheets killed by the
// implicit upgrade-then-fail.
func policyFor(r *http.Request) string {
	if r != nil && r.TLS != nil {
		return cspBase + upgradeDirective
	}
	return cspBase
}

// CSPMiddleware sets the Content-Security-Policy header on every
// response. Wraps any handler.
func CSPMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", policyFor(r))
		next.ServeHTTP(w, r)
	})
}

// CSPHandlerFunc is the handler-shaped wrapper used by mounter
// callsites that want to keep the http.HandlerFunc signature.
func CSPHandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", policyFor(r))
		next(w, r)
	}
}
