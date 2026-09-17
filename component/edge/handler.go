// component/edge/handler.go
package edge

import (
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
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
	// RequestLog records one row per served request, which is what the
	// traffic figure on a deployable's Live stop is folded from (epic
	// memql#4906). NIL IS THE OFF SWITCH and it is the ordinary state for a
	// handler built by a test: recording costs one nil check per request
	// when it is absent. See requestlog.go for why it must not block.
	RequestLog RequestRecorder
}

// Handler serves whichever site the request's Host names.
type Handler struct {
	resolver       Resolver
	opener         BundleOpener
	logger         *slog.Logger
	apiTarget      string
	identityTarget string
	secretResolver SecretResolver
	requestLog     RequestRecorder
	// hashCache holds one document's inline-script hash list per asset
	// validator (scripthash.go). Same LRU as the bundle byte cache, and
	// immutable for the same reason: the key names exact bytes, so a
	// republish is a new key and there is no invalidation path.
	hashCache *bundleCache
	// hashSF coalesces concurrent misses on that cache, the way blob.go and
	// resolve.go already guard their own cold paths.
	hashSF hashGroup
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
		requestLog:     opts.RequestLog,
		hashCache:      newBundleCache(cspHashCacheBytes),
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

	// AN UNKNOWN HOST IS NOT RECORDED. There is no deployable to attribute
	// the request to, and inventing one would put requests for hostnames
	// nobody in this cluster owns into somebody's figure.
	if site == nil {
		http.NotFound(w, r)
		return
	}

	// THE CLUSTER'S OWN SURFACES ARE EXCLUDED, and it is the ROW FIELD that
	// excludes them rather than a name this package recognises -- the portal
	// and the OS are `systemOwned`, so a third one added tomorrow is excluded
	// by the same line. A deployable's traffic figure is about somebody's
	// app; folding the console an operator is reading it in into the same
	// table would measure the act of looking.
	// Synthetic homepage probes do not count as visitor traffic. This is a
	// metrics classification only, not an authentication or security-log bypass.
	probe := r.Method == http.MethodGet && r.URL.Path == "/" && r.UserAgent() == "MemQL-Site-Health/1.0"
	if h.requestLog == nil || site.SystemOwned || probe {
		h.serve(w, r, site)
		return
	}

	started := time.Now()
	rec := &recordingWriter{ResponseWriter: w}
	pathClass := h.serve(rec, r, site)
	// AFTER the response, so nothing a visitor waits for happens here beyond
	// a channel send the recorder promises not to block on.
	h.requestLog.Record(RequestRecord{
		SiteId:     site.ID,
		ServedAt:   time.Now().UTC(),
		Status:     rec.statusOr(http.StatusOK),
		PathClass:  pathClass,
		Bytes:      rec.bytes,
		DurationNs: time.Since(started).Nanoseconds(),
	})
}

// serve is the whole of what the edge does with a resolved site, and it
// returns the path class of what it did -- the one fact the record needs that
// cannot be read off the response.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, site *Site) string {
	// STATUS BEFORE ANY FILE LOOKUP. An unknown host and a draft site are both
	// 404 -- neither exists as far as the internet is concerned. A DISABLED
	// site is 503, deliberately: a deliberately paused site and a typo'd
	// hostname are different situations, and the operator debugging one needs
	// to tell them apart from the status code alone.
	//
	// THE SWITCH NAMES WHAT SERVES, not what does not, and that inversion is
	// load-bearing (epic memql#4794). It used to 404 draft, 503 disabled and
	// serve everything else -- which meant the D10 lifecycle adding `archived`
	// to the enum would have made every archived site keep answering 200,
	// silently, with the row correctly marked archived on the operator's own
	// page. A serve-by-default tail cannot be audited: it grants serving to
	// values that do not exist yet. Now a status this build does not recognise
	// resolves for nobody, so the next value added to the enum is inert here
	// until somebody decides what it should do.
	switch {
	case site.Status == "live":
		// serves; fall through
	case site.Status == "disabled":
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return pathClassUnserved
	default:
		// draft, archived, and any status a future release adds.
		http.NotFound(w, r)
		return pathClassUnserved
	}

	// Cluster-wide identity discovery, ahead of the bundle lookup and for
	// every site alike -- see runtimeconfig.go. Not a new entry in
	// component/server.EdgePaths(): the edge's declared surface is exactly
	// "/", and this path lives under it.
	if r.URL.Path == runtimeConfigPath {
		h.serveRuntimeConfig(w, r, site)
		return pathClassConfig
	}

	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		h.serveAPI(w, r, site)
		return pathClassProxy
	}

	// Exact-path identity JSON, ahead of the bundle / SPA fallback.
	// A miss here is how a Mac browser's coalesced POST /oauth/token
	// used to get index.html 200 (memql#4154).
	if isIdentityXHRPath(r.URL.Path) {
		h.serveIdentityXHR(w, r, site)
		return pathClassProxy
	}

	fsys, err := h.opener.Open(site.BundleRef)
	if err != nil {
		h.logger.Error("edge: opening the bundle failed",
			"component", "edge", "site", site.ID, "bundleRef", site.BundleRef, "err", err)
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return pathClassUnserved
	}

	securityHeaders(w, r)

	// THE POLICY IS SET AFTER RESOLUTION, NOT BEFORE, and the ordering is
	// the whole of what makes inline-script hashes work (scripthash.go).
	// Which hashes belong in script-src is a property of the DOCUMENT being
	// served, so the header cannot be written until resolveAsset has said
	// which file that is.
	//
	// It must still be written BEFORE serveFile, because serveFile answers a
	// conditional request with 304 and returns -- and a 304's headers
	// REPLACE the stored ones, so a 304 that omitted the hashes would
	// overwrite the good policy its own 200 established. The page would work
	// on a first visit and be inert on every one after, which is the failure
	// TestNotModifiedRepeatsTheInlineScriptHashes exists to catch.
	if name, ok := resolveAsset(fsys, r.URL.Path); ok {
		// ONE VALIDATOR, TWO CONSUMERS. On the file:// path assetETagFor
		// reads the whole file and SHA-256s it (contentETag), so letting the
		// policy and serveFile each derive it would hash every served asset
		// twice per request.
		etag, hasETag := assetETagFor(fsys, name, site.BundleRef)
		h.setContentSecurityPolicy(w, r, site, fsys, name, etag, hasETag)
		h.serveFile(w, r, fsys, name, etag, hasETag)
		return classifyServed(name, false)
	}

	// The last rung of D11's order, and the only place kind is consulted. A
	// static site 404s so a mistyped path in a multi-page site is visible;
	// an spa falls back so client-side routing works. A shopify_storefront
	// IS a spa bundle -- design D4 says so in as many words -- so it takes
	// the same fallback; without it every client-side storefront route
	// (/products/x, /cart) 404s on a hard reload.
	if site.Kind == "spa" || site.Kind == storefrontKind {
		if _, err := fs.Stat(fsys, "index.html"); err == nil {
			// The fallback serves a DOCUMENT, so it needs its hashes exactly
			// as the root does. Without this every client-side route breaks
			// on a hard reload while the root keeps working.
			etag, hasETag := assetETagFor(fsys, "index.html", site.BundleRef)
			h.setContentSecurityPolicy(w, r, site, fsys, "index.html", etag, hasETag)
			h.serveFile(w, r, fsys, "index.html", etag, hasETag)
			return pathClassFallback
		}
	}
	h.setContentSecurityPolicy(w, r, site, fsys, "", "", false)
	noCache(w)
	http.NotFound(w, r)
	return pathClassUnserved
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

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name, etag string, hasETag bool) {
	// THE VALIDATOR IS COMPUTED BEFORE THE FILE IS OPENED (memql#4545) --
	// by the CALLER now, which also hands it to the CSP builder so it is
	// derived once rather than twice (see the call site).
	//
	// On the blob path that ordering is the entire point: a bundle's
	// version prefix is a content hash, so (prefix, path) names the bytes
	// without reading them, and a conditional request for an ASSET is
	// answered 304 with ZERO downloads.
	//
	// A DOCUMENT is the exception, and it is a cost rather than a
	// correctness problem: the CSP builder must know the document's inline
	// scripts, so on a cold hash cache it reads the document even for a
	// response that will carry no body. Bounded to once per (bundle
	// version, document, replica) by that cache, and coalesced across
	// concurrent requests by its singleflight.
	//
	// The file:// path hashes content because build timestamps can be preserved.

	// Every mutable URL must reach this handler again after a release. Only
	// a filename digest VERIFIED against its bytes earns immutable caching.
	w.Header().Set("Cache-Control", "public, no-cache, must-revalidate")
	if contentAddressedAsset(fsys, name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Del("Pragma")
		w.Header().Del("Expires")
	}
	if hasETag {
		w.Header().Set("ETag", etag)
		// Repeat the current freshness policy on conditional responses too.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	f, err := fsys.Open(name)
	if err != nil {
		// Clear the cache headers set above: they describe a body that is
		// not being sent, and `immutable` on a 404 is a browser caching the
		// absence of a file for a year.
		w.Header().Del("ETag")
		noCache(w)
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}); ok {
		// HTTP dates have one-second precision and archive mtimes often
		// survive a release unchanged. Use the content/version ETag alone;
		// an old If-Modified-Since must never suppress a changed release.
		http.ServeContent(w, r, path.Base(name), time.Time{}, rs)
		return
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		noCache(w)
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(data))
}

func noCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}
