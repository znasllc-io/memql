package web

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
)

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
// formActionSelf is the form-action directive in its strict form. It is a
// named constant because clientFormActionPolicy EXTENDS exactly this
// directive and nothing else; building cspBase from it means the extension
// point cannot silently drift out of the base policy.
const formActionSelf = "form-action 'self'"

const cspBase = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self'; " +
	"script-src 'self'; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	formActionSelf + "; " +
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

// ---------------------------------------------------------------------------
// form-action and the registered-client redirect (memql#4302 regression)
// ---------------------------------------------------------------------------
//
// The device-bound magic-link flow ends in a form POST answered with a 303 to
// the OAuth client's redirect URI -- a DIFFERENT origin (the portal). Chrome
// enforces form-action against every URL in a form submission's REDIRECT
// CHAIN, not only the submission target (w3c/webappsec-csp#8; Firefox and
// Safari check the submission URL only). Under `form-action 'self'` Chrome
// therefore CANCELS the final navigation: the server has already consumed the
// link and minted the auth code, the 303 is issued in milliseconds, and the
// user watches the Confirm button sit on "Confirming..." forever while the
// one-time code is stranded. Nothing reaches the client; the only trace is a
// DevTools console violation.
//
// Go handler tests see the 303 and pass -- no browser in CI enforces CSP --
// which is exactly how this shipped. Hence the guard tests beside this file.
//
// The pre-#4302 flow was immune BY SHAPE: it consumed on a top-level GET, and
// form-action does not govern GET-navigation redirects. #4302 deliberately
// moved every state change onto form POSTs (mail-scanner safety), which put
// the redirect inside form-action's Chrome-interpreted scope.
//
// THE EXTENSION IS THE REGISTERED CLIENTS' ORIGINS AND NOTHING ELSE. A
// magic-link row's redirect URI is validated against the registered-client
// list at ISSUE time, so that list is precisely the set of places a sign-in
// form's redirect chain can legitimately end. It is boot-static
// configuration, not request data: nothing an attacker submits can add an
// origin here. Extending form-action (where the forms may SUBMIT) is not
// comparable to relaxing script-src or connect-src -- injecting a form that
// abuses the wider directive would require HTML injection, which script-src
// 'self' and the templates' escaping already stand against.

// clientFormActionOrigins reduces the registered clients' redirect URIs to a
// deduplicated, sorted list of origins. Relative or unparseable URIs
// contribute nothing.
func clientFormActionOrigins(clients []identity.RegisteredClient) []string {
	seen := map[string]struct{}{}
	for _, c := range clients {
		for _, raw := range c.RedirectURIs {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || u.Scheme == "" || u.Host == "" {
				continue
			}
			seen[u.Scheme+"://"+u.Host] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// policyForOrigins is policyFor with form-action extended by the given
// origins. Empty origins yields the base policy unchanged.
func policyForOrigins(r *http.Request, origins []string) string {
	p := policyFor(r)
	if len(origins) == 0 {
		return p
	}
	return strings.Replace(p, formActionSelf, formActionSelf+" "+strings.Join(origins, " "), 1)
}
