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
			pr.Out.URL.Path = upstreamPath(pr.In.URL.Path)
			pr.Out.URL.RawPath = ""
			// SetXForwarded, plus the site's own Host, so the bff sees which
			// origin the request actually arrived on rather than the edge's
			// service name.
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
			copyWebsocketSubprotocols(pr)
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

// bffRootPrefixes are the bff route roots that are NOT under its "/memql"
// multiplexed root, so the marker is stripped for them rather than swapped.
//
//   - "/artifacts"  -- the Library's byte-bearing endpoints
//     (server.ArtifactPaths(): POST /artifacts, GET /artifacts/{id}/content
//     and the chunked-session family, memql#4341). This is what puts the
//     knowledge pipeline within reach of a hosted surface: the MemQL OS
//     Training app uploads through it and the analyzer runs from there.
//
// "/spaces" was the second member until the space attachment endpoints went
// with the space concept (epic memql#4988). Named, not spelled inline, so the
// one place that has to agree with component/server says which.
//
// A LIST RATHER THAN A CONSTANT because the rule is one rule over a set, and
// the day a second bff root reappears the alternative is a second
// `strings.HasPrefix` pair somebody has to notice is the same test twice.
var bffRootPrefixes = []string{"/artifacts"}

// isBffRootPath reports whether a marker-stripped path addresses one of the
// bff's own roots.
//
// The prefix must end the path or be followed by "/" -- never a bare
// HasPrefix. "/artifactsomething" is a different route from "/artifacts", and
// treating it as the same one would silently make any future
// "/memql/artifacts*" path unreachable through this proxy while every
// manifest looked correct.
func isBffRootPath(rest string) bool {
	for _, prefix := range bffRootPrefixes {
		if rest == prefix || strings.HasPrefix(rest, prefix+"/") {
			return true
		}
	}
	return false
}

// upstreamPath maps the edge's same-origin marker onto the bff's real path
// space.
//
// TWO ROOTS, because the bff has two. The multiplexed API lives under
// "/memql" ("/_memql/ws" -> "/memql/ws"), and the marker is SWAPPED for it
// rather than deleted -- deleting it outright would make every proxied path
// lose its leading segment along with the marker. The Library's byte routes
// live at the bff's own root instead ("/artifacts", "/artifacts/{id}/content"),
// so for those the marker is stripped and nothing put back.
//
// WHY THE PORTAL CANNOT JUST CALL "/artifacts" DIRECTLY, which is the obvious
// question. It is served BY this handler, and a path that resolves to no file
// in the bundle takes the SPA fallback -- so a bare same-origin POST
// /artifacts would be answered with index.html and a 200, which is a silent
// success for an upload that stored nothing. Routing it through the marker is
// also what keeps a hosted site that has its own /artifacts page from having
// it swallowed by this proxy: the marker is the one prefix a bundle may not
// claim.
//
// Nothing new is EXPOSED by this. serveAPI already requires site.APIProxy, and
// every one of these routes is authenticated: the Library's two decide each
// read under the caller's own actor (component/server/artifact_handler.go),
// and the attachment handler checks space ownership BEFORE it parses a
// multipart body, answering a generic 404 rather than a 403 so probing cannot
// tell "not yours" from "not there" (component/server/attachment_handler.go).
// Strictly narrower, in both cases, than the "/memql/ws" bridge this proxy has
// always carried -- that one is the whole multiplexed API.
func upstreamPath(in string) string {
	rest := strings.TrimPrefix(in, "/_memql")
	if isBffRootPath(rest) {
		return rest
	}
	return "/memql" + rest
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

// copyWebsocketSubprotocols keeps the bearer/guest JWT pair on the
// hop to bff (memql#4160). Values(), not Get: browsers may send two
// Sec-WebSocket-Protocol lines.
func copyWebsocketSubprotocols(pr *httputil.ProxyRequest) {
	protos := pr.In.Header.Values("Sec-WebSocket-Protocol")
	if len(protos) == 0 {
		return
	}
	pr.Out.Header.Del("Sec-WebSocket-Protocol")
	for _, proto := range protos {
		pr.Out.Header.Add("Sec-WebSocket-Protocol", proto)
	}
}
