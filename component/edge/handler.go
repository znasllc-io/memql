// component/edge/handler.go
package edge

import (
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Options configures a Handler. APITarget is consumed in Task 7; a Handler
// built without it simply refuses /_memql/* for every site.
type Options struct {
	Resolver  Resolver
	Opener    BundleOpener
	Logger    *slog.Logger
	APITarget string
}

// Handler serves whichever site the request's Host names.
type Handler struct {
	resolver  Resolver
	opener    BundleOpener
	logger    *slog.Logger
	apiTarget string
}

var _ http.Handler = (*Handler)(nil)

func NewHandler(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		resolver:  opts.Resolver,
		opener:    opts.Opener,
		logger:    logger,
		apiTarget: opts.APITarget,
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

	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		h.serveAPI(w, r, site)
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
	w.Header().Set("Content-Security-Policy", policyForSite(r, site))

	if name, ok := resolveAsset(fsys, r.URL.Path); ok {
		h.serveFile(w, r, fsys, name)
		return
	}

	// The last rung of D11's order, and the only place kind is consulted. A
	// static site 404s so a mistyped path in a multi-page site is visible;
	// an spa falls back so client-side routing works.
	if site.Kind == "spa" {
		if _, err := fs.Stat(fsys, "index.html"); err == nil {
			h.serveFile(w, r, fsys, "index.html")
			return
		}
	}
	noCache(w)
	http.NotFound(w, r)
}

// serveAPI answers requests under apiPrefix ("/_memql/*"). Task 7 (#3712)
// replaces this body with a reverse proxy to apiTarget; until that lands --
// and for any site that has not opted in via APIProxy -- the prefix is
// refused outright rather than falling through to static-file resolution,
// because a path reserved for a site's API must never be servable as a file.
func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, site *Site) {
	if !site.APIProxy || h.apiTarget == "" {
		http.NotFound(w, r)
		return
	}
	// TODO(#3712): reverse proxy the request to h.apiTarget.
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
