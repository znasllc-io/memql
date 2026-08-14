// component/edge/proxy.go
package edge

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// serveAPI forwards /_memql/* to the bff so a hosted site is same-origin with
// its own API.
//
// WHY THIS EXISTS. component/server/token_cookie.go sets the auth cookie
// SameSite=Lax, which is not sent on a cross-site request in any browser
// today -- before third-party cookie deprecation is even considered. A site
// on its own origin calling api.<domain> would therefore have no cookie, and
// would also need CORS and the cluster domain compiled into its bundle. Going
// through the site's own origin removes all three problems at once.
//
// OPT-IN PER SITE. A site that did not ask for an API path must not get one:
// otherwise every hosted site is an open relay to the cluster's API surface,
// including sites belonging to whoever else this cluster serves.
func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, site *Site) {
	if !site.APIProxy || strings.TrimSpace(h.apiTarget) == "" {
		http.NotFound(w, r)
		return
	}

	target, err := url.Parse(h.apiTarget)
	if err != nil {
		h.logger.Error("edge: MEMQL_EDGE_API_TARGET is not a URL",
			"component", "edge", "target", h.apiTarget, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// /_memql/ws -> /memql/ws. The prefix is the edge's marker, not
			// part of the API's path space -- swap the marker for the bff's
			// real "/memql" route root rather than deleting it outright, or
			// every proxied path loses its leading segment along with it.
			pr.Out.URL.Path = "/memql" + strings.TrimPrefix(pr.In.URL.Path, "/_memql")
			pr.Out.URL.RawPath = ""
			// SetXForwarded, plus the site's own Host, so the bff sees which
			// origin the request actually arrived on rather than the edge's
			// service name.
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			h.logger.Error("edge: proxying to the API failed",
				"component", "edge", "site", site.ID, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}

	// ReverseProxy handles the WebSocket upgrade itself when the request
	// carries Upgrade/Connection -- it detects a 101 and switches to a raw
	// byte pipe. TestAPIProxyPreservesTheUpgradeHeaders proves it rather than
	// trusting it, because a plain GET test would pass while every WebSocket
	// silently died.
	proxy.ServeHTTP(w, r)
}
