// component/edge/identity_proxy.go
package edge

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// identityXHRPaths are the identity JSON endpoints a browser must hit
// SAME-ORIGIN (docs/public/operate/auth/identity-service.md). A top-level
// navigation to /authorize still goes to identity.<domain>; these four
// are fetch() calls. Cross-origin credentialed XHR to a sibling host that
// shares a wildcard cert + IP is the Safari/Chrome HTTP/2 coalescing
// failure: the POST lands on this site's SPA fallback (200 HTML) and the
// portal reports "identity returned no access token (invalid_response)"
// (memql#4154).
//
// Exact paths only. /auth/callback is a site-owned SPA route and must
// never be forwarded.
var identityXHRPaths = map[string]struct{}{
	"/oauth/token":           {},
	"/auth/refresh":          {},
	"/auth/logout":           {},
	"/.well-known/jwks.json": {},
}

func isIdentityXHRPath(p string) bool {
	_, ok := identityXHRPaths[p]
	return ok
}

// serveIdentityXHR forwards an identity JSON endpoint to the identity
// binary. Every live site gets this: runtime-config.json now publishes
// an empty identityApiBaseUrl so fetch() stays on the site origin.
// Not gated on apiProxy -- that flag is the bff relay, a different
// surface (and an open relay if it were on by default).
func (h *Handler) serveIdentityXHR(w http.ResponseWriter, r *http.Request, site *Site) {
	if strings.TrimSpace(h.identityTarget) == "" {
		http.Error(w, "identity upstream unavailable", http.StatusBadGateway)
		return
	}
	target, err := url.Parse(h.identityTarget)
	if err != nil {
		h.logger.Error("edge: identity target is not a URL",
			"component", "edge", "target", h.identityTarget, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// Keep the path identity mounted: /oauth/token stays /oauth/token.
			pr.Out.Host = target.Host
		},
		// Identity sets two Set-Cookie headers (memql_refresh + memql_session).
		// Do not add a ModifyResponse that copies headers with Header.Set --
		// that is the ReverseProxy footgun that drops every cookie but the first
		// (memql#4158). The default copy uses Header.Add and keeps both.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			h.logger.Error("edge: proxying to identity failed",
				"component", "edge", "site", site.ID, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
