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

	"github.com/znasllc-io/memql/component/memql"
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
	// PreviewExec resolves the v1:platform:sitePreviewGrant a request presents
	// (epic memql#5531, issue memql#5545). NIL IS THE OFF SWITCH and it is the
	// ordinary state for a handler built by a test: without one no request is
	// ever a preview, so every existing test's expectations about what the
	// public sees are untouched by this feature's existence. See preview.go for
	// why it is separate from Resolver rather than folded into it.
	PreviewExec PreviewExecutor
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
	previewExec    PreviewExecutor
	// previews caches resolved grants for previewCacheTTL and throttles the
	// lastSeenAt write. Per replica, like hashCache beside it.
	previews *previewCache
	// hashCache holds one document's inline-script hash list per asset
	// validator (scripthash.go). Same LRU as the bundle byte cache, and
	// immutable for the same reason: the key names exact bytes, so a
	// republish is a new key and there is no invalidation path.
	hashCache *bundleCache
	// hashSF coalesces concurrent misses on that cache, the way blob.go and
	// resolve.go already guard their own cold paths.
	hashSF hashGroup
	// shopperRate is the per-(site, address) token bucket in front of the
	// shopper surface (shopper.go). Built once here rather than lazily on
	// first use, because a lazy build on a public, unauthenticated path is a
	// data race with a public trigger.
	shopperRate *shopperBucket
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
		previewExec:    opts.PreviewExec,
		previews:       newPreviewCache(),
		hashCache:      newBundleCache(cspHashCacheBytes),
		shopperRate:    newShopperBucket(shopperRatePerMinute()),
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
	// THE PREVIEW ENTRY POINTS, AHEAD OF EVERYTHING (epic memql#5531). They are
	// answered by the edge ITSELF rather than proxied, and they sit above the
	// status switch, so a link to a DRAFT deployable's preview is redeemable --
	// which is the whole point, since a draft is 404 for everybody including
	// its operator.
	//
	// THEY ARE ABOVE THE /_memql/ PROXY BRANCH TOO, deliberately. They share
	// that prefix so the origin keeps ONE reserved namespace rather than two,
	// but a preview has to work on a deployable whose apiProxy is off -- and
	// with the proxy on, forwarding them to the bff would hand the token to a
	// service that has no route for it.
	switch r.URL.Path {
	case memql.PreviewEnterPath:
		h.servePreviewEnter(w, r, site)
		return pathClassConfig
	case memql.PreviewLeavePath:
		h.servePreviewLeave(w, r)
		return pathClassConfig
	}

	// IS THIS A PREVIEW? Answered once, here, and the answer becomes a SITE
	// rather than a flag -- a copy carrying the candidate version and the
	// preview binding (previewSite). Nothing below this line knows a preview is
	// happening, which is design D7 made literal: the engine chooses a bundle
	// and a binding and does nothing else differently.
	grant := h.previewGrantFor(r, site)
	if grant != nil {
		// A PREVIEW IS REFUSED FOR A DEPLOYABLE THAT IS NOT DRAFT OR LIVE.
		// `disabled` is a deliberate pause and `archived` is decommissioned; a
		// preview that served through either would be a way to bring a site
		// back that does not touch its status, and an operator reading the row
		// would have no idea anything was being served.
		if site.Status != siteStatusDraftValue && site.Status != siteStatusLiveValue {
			grant = nil
		}
	}
	if grant != nil {
		h.notePreviewSeen(grant)
		previewHeaders(w)
		site = previewSite(site, grant)
		// AND THE STATUS SWITCH IS SKIPPED, which is the one place a preview
		// changes an answer -- a draft deployable is 404 for everybody, and an
		// authorized operator being served its candidate is exactly what this
		// epic is. It is skipped rather than widened so the switch below goes
		// on naming WHAT SERVES for the public, which is the inversion
		// memql#4794 made load-bearing.
		return h.serveResolved(w, r, site)
	}

	// A testing origin never falls through to Production when access expires.
	if site.StorefrontTesting {
		previewHeaders(w)
		http.Error(w, "Open this testing website from MemQL OS: Visit → Testing.", http.StatusUnauthorized)
		return pathClassUnserved
	}

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

	return h.serveResolved(w, r, site)
}

// serveResolved is everything after the status decision: the runtime document,
// the proxies, and the bundle.
//
// IT IS THE WHOLE OF WHAT A PREVIEW SHARES WITH A PUBLIC REQUEST, and the split
// exists so that sharing is structural rather than asserted. A preview reaches
// it with a Site whose BundleRef is the candidate and whose Store is the
// preview binding's, and from here there is no code path that can tell the
// difference.
func (h *Handler) serveResolved(w http.ResponseWriter, r *http.Request, site *Site) string {
	// Cluster-wide identity discovery, ahead of the bundle lookup and for
	// every site alike -- see runtimeconfig.go. Not a new entry in
	// component/server.EdgePaths(): the edge's declared surface is exactly
	// "/", and this path lives under it.
	if r.URL.Path == runtimeConfigPath {
		h.serveRuntimeConfig(w, r, site)
		return pathClassConfig
	}

	// THE SHOPPER SURFACE, above the /_memql/ proxy branch because it shares
	// that marker (epic memql#5532, issue memql#5551). It must be ABOVE:
	// these paths would otherwise be forwarded to the bff as ordinary
	// authenticated API calls, arriving with no stamp, no rate limit and no
	// size cap -- and the bff would refuse them, so the failure would look
	// like the feature not working rather than like a routing mistake.
	//
	// BELOW the preview substitution, equally deliberately, and that is what
	// makes design D9 free: `site` is already the preview copy when a grant
	// is in force, so site.Store IS the store this write belongs to.
	if strings.HasPrefix(r.URL.Path, shopperFormPrefix) ||
		strings.HasPrefix(r.URL.Path, shopperReadPrefix) {
		h.serveShopperSurface(w, r, site)
		return pathClassProxy
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

	// The last rung of D11's order, and the only place the tail is decided.
	if fallsBackToIndex(site) {
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

// fallsBackToIndex answers the last rung of D11's resolution order: when a
// request matched NO file in the bundle, does this site serve index.html or
// 404?
//
// THE SITE DECIDES, AND THE KIND IS THE DEFAULT (memql#5535). Before this
// function the kind decided outright -- static 404s so a mistyped path in a
// multi-page site is visible, spa falls back so client-side routing survives
// a hard reload, and a shopify_storefront took the fallback because the
// kind's own description says it IS a spa bundle. That last one is where it
// broke: the first storefront actually built was a multi-page prerendered
// tree, and it declared kind "static" precisely to get the 404 back -- which
// also gave up the store binding, the storefront block in its runtime
// document, and the policy that admits Shopify. Neither tail is right for
// every storefront, so the tail became a property of the SITE and the kind
// enum stayed at exactly three values.
//
// EMPTY IS THE DEFAULT AND MEANS "ASK THE KIND", which is what every row
// written before the field existed carries. AN UNRECOGNISED VALUE READS THE
// SAME WAY, and that direction is deliberate: the enum is validated on the
// write path, so anything else is a raw write that bypassed it, and reading
// an unknown value as not_found would take a live site's every client-side
// route dark on a typo. Fail toward what the site did yesterday.
func fallsBackToIndex(site *Site) bool {
	if site == nil {
		return false
	}
	switch site.ResolutionTail {
	case resolutionTailFallback:
		return true
	case resolutionTailNotFound:
		return false
	}
	return site.Kind == "spa" || site.Kind == storefrontKind
}

// The two site statuses a preview may be served under (epic memql#5531).
// `disabled` and `archived` are deliberately absent -- see serve().
const (
	siteStatusDraftValue = "draft"
	siteStatusLiveValue  = "live"
)

// The two tails a site may name. A third state -- the empty string -- is the
// absence of a choice, not a value, and is deliberately unnamed here.
const (
	resolutionTailFallback = "fallback"
	resolutionTailNotFound = "not_found"
)

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

	// A PRIVATE RESPONSE IS NEVER WIDENED (epic memql#5531). A preview writes
	// `no-store` before the response is built, and the two policies below would
	// otherwise overwrite it -- `public, no-cache, must-revalidate` for a
	// document, and `public, max-age=31536000, immutable` for a
	// content-addressed asset, which also deletes the Pragma and Expires
	// headers that came with it.
	//
	// THE CONSEQUENCE IS THE WHOLE REASON THE HEADER EXISTS: an unpublished
	// version of somebody's storefront, storable by every shared cache between
	// this cluster and the operator's browser, and for a year on any asset
	// whose name carries a digest. The policy is set EARLY because it must
	// cover the 304 path too, so the check belongs here rather than a reorder.
	//
	// It reads the header rather than taking a parameter, so the rule binds
	// every caller of serveFile including any added later -- a `no-store`
	// somebody sets anywhere upstream is honoured for the same reason.
	if !alreadyPrivate(w) {
		// Every mutable URL must reach this handler again after a release. Only
		// a filename digest VERIFIED against its bytes earns immutable caching.
		w.Header().Set("Cache-Control", "public, no-cache, must-revalidate")
		if contentAddressedAsset(fsys, name) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Del("Pragma")
			w.Header().Del("Expires")
		}
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

// alreadyPrivate reports whether something upstream has already declared this
// response uncacheable.
//
// `no-store` is the token tested rather than `private`, and the difference
// matters: `private` permits a browser cache, which a preview is fine with, and
// `no-store` is the one that forbids every store including the shared ones this
// guards against. A caller that meant `private` did not mean "do not apply the
// bundle's freshness policy".
func alreadyPrivate(w http.ResponseWriter) bool {
	return strings.Contains(strings.ToLower(w.Header().Get("Cache-Control")), "no-store")
}
