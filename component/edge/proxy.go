// component/edge/proxy.go
package edge

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
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

	// DEFENCE IN DEPTH, NOT THE PRIMARY CONTROL. The only reason
	// "/_memql/../admin" cannot reach this function today is
	// http.ServeMux.Handler(): it unconditionally 301-redirects any request
	// whose path does not survive its own cleanPath (dot-segments, doubled
	// slashes, a decoded %2e%2e) before ANY registered handler -- including
	// this one -- ever runs. That is a property of the router this Handler
	// happens to be mounted behind, not of this file. If the router is ever
	// swapped, or this Handler is ever wired up some other way that skips
	// ServeMux, that redirect disappears silently -- and Go's http.Client
	// never cleans dot-segments when it writes the request line, so a path
	// like "/memql/../admin" would go out to the bff VERBATIM: a path
	// traversal into the cluster's API surface.
	//
	// So this refuses rather than trusts the mux, and refuses rather than
	// sanitises: a request whose path needed repair was not a legitimate
	// request. Same call resolveAsset makes in handler.go via fs.ValidPath,
	// same reasoning ("there is no legitimate request outside the bundle to
	// repair") -- true here even more sharply, since there is no legitimate
	// /_memql/ request containing a dot-segment or a doubled slash at all.
	//
	// The comparison uses cleanPathLikeMux below, NOT bare path.Clean: the
	// two differ on a trailing slash (path.Clean("/_memql/ws/") strips it;
	// ServeMux's own cleanPath puts it back), and a guard that claims to
	// backstop the mux has to refuse exactly what the mux would have
	// rewritten -- no more, no less. Refusing a bare trailing slash too
	// would turn a client library's harmless URL-normalization choice into
	// a 400 about dot-segments the request never had.
	if cleaned := cleanPathLikeMux(r.URL.Path); cleaned != r.URL.Path {
		h.logger.Warn("edge: refusing a /_memql/* path that does not survive cleaning",
			"component", "edge", "site", site.ID, "path", r.URL.Path)
		http.Error(w, "bad request", http.StatusBadRequest)
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

// cleanPathLikeMux reproduces net/http's own unexported cleanPath
// (server.go, the helper behind ServeMux's path canonicalization) exactly,
// rather than relying on bare path.Clean. serveAPI's guard has to refuse
// precisely what the mux would have rewritten -- no stricter, no looser --
// or the comment describing it becomes false. The one place this departs
// from path.Clean alone: a trailing slash survives ("/_memql/ws/" stays
// "/_memql/ws/"), because path.Clean strips a trailing slash and ServeMux's
// cleanPath puts it back. Duplicated rather than imported because it is
// unexported.
func cleanPathLikeMux(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes a trailing slash except for root; restore it if
	// the original had one, exactly as net/http's cleanPath does.
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}
