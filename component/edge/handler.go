// component/edge/handler.go
package edge

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

// Options configures a Handler. APITarget names the bff's plain-HTTP address
// (e.g. "http://bff-http:8085") that serveAPI (proxy.go) reverse-proxies
// /_memql/* to; a Handler built without it simply refuses /_memql/* for
// every site. IdentityTarget names the identity binary (typically
// MEMQL_IDENTITY_VERIFIER_BASE_URL, e.g. "https://identity:8085") that
// serveIdentityXHR reverse-proxies the four same-origin JSON paths to;
// empty refuses those paths with 502 rather than SPA-fallback HTML.
// SecretResolver, when supplied, is how a shopify_storefront site's
// runtime-config document resolves the v1:platform:globalSecret its binding
// names (memql#4345); a Handler built without one serves every other field of
// that document and an empty storefrontToken.
type Options struct {
	Resolver       Resolver
	Opener         BundleOpener
	Logger         *slog.Logger
	APITarget      string
	IdentityTarget string
	SecretResolver SecretResolver
}

// Handler serves whichever site the request's Host names.
type Handler struct {
	resolver       Resolver
	opener         BundleOpener
	logger         *slog.Logger
	apiTarget      string
	identityTarget string
	secretResolver SecretResolver
}

var _ http.Handler = (*Handler)(nil)

func NewHandler(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		resolver:       opts.Resolver,
		opener:         opts.Opener,
		logger:         logger,
		apiTarget:      opts.APITarget,
		identityTarget: opts.IdentityTarget,
		secretResolver: opts.SecretResolver,
	}
}

const apiPrefix = "/_memql/"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	site, err := h.resolver.Resolve(r.Context(), r.Host)
	if err != nil {
		h.logger.Error("edge: resolving the host failed",
			"component", "edge", "host", r.Host, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// STATUS BEFORE ANY FILE LOOKUP. An unknown host and a draft site are both
	// 404 -- neither exists as far as the internet is concerned. A DISABLED
	// site is 503, deliberately: a deliberately paused site and a typo'd
	// hostname are different situations, and the operator debugging one needs
	// to tell them apart from the status code alone.
	switch {
	case site == nil, site.Status == "draft":
		http.NotFound(w, r)
		return
	case site.Status == "disabled":
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return
	}

	// Cluster-wide identity discovery, ahead of the bundle lookup and for
	// every site alike -- see runtimeconfig.go. Not a new entry in
	// component/server.EdgePaths(): the edge's declared surface is exactly
	// "/", and this path lives under it.
	if r.URL.Path == runtimeConfigPath {
		h.serveRuntimeConfig(w, r, site)
		return
	}

	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		h.serveAPI(w, r, site)
		return
	}

	// Exact-path identity JSON, ahead of the bundle / SPA fallback.
	// A miss here is how a Mac browser's coalesced POST /oauth/token
	// used to get index.html 200 (memql#4154).
	if isIdentityXHRPath(r.URL.Path) {
		h.serveIdentityXHR(w, r, site)
		return
	}

	fsys, err := h.opener.Open(site.BundleRef)
	if err != nil {
		h.logger.Error("edge: opening the bundle failed",
			"component", "edge", "site", site.ID, "bundleRef", site.BundleRef, "err", err)
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return
	}

	securityHeaders(w, r)
	w.Header().Set("Content-Security-Policy", policyForSite(r, site, os.Getenv))

	if name, ok := resolveAsset(fsys, r.URL.Path); ok {
		h.serveFile(w, r, fsys, name)
		return
	}

	// The last rung of D11's order, and the only place kind is consulted. A
	// static site 404s so a mistyped path in a multi-page site is visible;
	// an spa falls back so client-side routing works. A shopify_storefront
	// IS a spa bundle -- design D4 says so in as many words -- so it takes
	// the same fallback; without it every client-side storefront route
	// (/products/x, /cart) 404s on a hard reload.
	if site.Kind == "spa" || site.Kind == storefrontKind {
		if _, err := fs.Stat(fsys, "index.html"); err == nil {
			h.serveFile(w, r, fsys, "index.html")
			return
		}
	}
	noCache(w)
	http.NotFound(w, r)
}

// resolveAsset walks D11's resolution order and returns the first name that
// exists: the exact file, then <path>/index.html, then <path>.html.
//
// The .html rung is what makes prerendering worth doing. Without it the spa
// fallback would serve index.html for every route, so a crawler would see one
// page no matter how many the build emitted.
func resolveAsset(fsys fs.FS, urlPath string) (string, bool) {
	clean := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if clean == "" || clean == "." {
		clean = "index.html"
	}
	for _, candidate := range []string{clean, path.Join(clean, "index.html"), clean + ".html"} {
		// fs.ValidPath is the backstop behind the rooted filesystem: it
		// rejects "..", leading slashes and empty segments outright, and
		// there is no legitimate request outside the bundle to repair.
		if !fs.ValidPath(candidate) {
			continue
		}
		if info, err := fs.Stat(fsys, candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	f, err := fsys.Open(name)
	if err != nil {
		noCache(w)
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	// index.html is NEVER cached: it is how a deploy reaches a returning
	// visitor. Everything else is content-addressed by the build, so it may
	// be cached hard.
	if name == "index.html" || strings.HasSuffix(name, "/index.html") {
		noCache(w)
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	if rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}); ok {
		info, _ := fs.Stat(fsys, name)
		var modTime = info.ModTime()
		http.ServeContent(w, r, path.Base(name), modTime, rs)
		return
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		noCache(w)
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func noCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}
