package web

import "net/http"

// cspBase is the strict policy enforced on every web-UI route. The
// only relaxation against `default-src 'self'` is `img-src 'self' data:`
// to allow the brand-logo data URI inlined into the layout. No remote
// hosts, no `unsafe-eval`.
//
// `style-src 'self' 'unsafe-inline'` is the single concession to the
// "no unsafe-inline" target documented in the implementation plan §2.11
// — the layout uses an inline <style> tag to inject the brand-primary
// CSS variable so templates don't have to be re-rendered when the brand
// color changes. We can drop 'unsafe-inline' once the brand CSS is
// exposed via a dedicated /static/brand.css endpoint that serves
// :root vars.
const cspBase = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; " +
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
// identity.css, fails because no TLS, and the page renders unstyled).
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
