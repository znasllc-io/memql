package edge

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// Preview access is a short-lived, site-scoped credential. Storefront Testing
// uses a separate stable hostname and the current published bundle, with only
// Store replaced by the Testing binding. Production ignores preview cookies.
// A missing, invalid, expired or revoked grant on Testing returns 401; it never
// falls through to Production. Publishing a design updates both destinations
// without ending an otherwise valid Testing session.
//
// Non-storefront deployables retain candidate previews on their own origin:
// a valid grant substitutes both BundleRef and Store, and must still name the
// current candidate. Invalid grants fall through to the ordinary public path.

// PreviewExecutor is the engine seam this file needs, and it is DELIBERATELY
// SEPARATE from QueryExecutor.
//
// A grant read is not a site read, and it must not be cached with one. The
// resolver's cache is keyed by HOSTNAME and holds an answer for the site TTL; a
// grant is REVOCABLE and a revoke names no hostname, so a grant cached that way
// could not be invalidated at all. Keeping the two interfaces apart is what
// stops somebody helpfully folding the grant read into the resolver later and
// making "end this preview" mean "in thirty seconds, maybe".
//
// A handler built without one simply never honours a preview, which is what
// every test that does not care about previews gets for free.
type PreviewExecutor interface {
	// PreviewGrantByToken resolves a v1:platform:sitePreviewGrant by the
	// SHA-256 of the token presented (epic memql#5531, issue memql#5545). A
	// miss is (nil, nil) -- a cookie naming no grant is not an error, it is a
	// request that gets exactly what an unauthenticated one gets.
	PreviewGrantByToken(ctx context.Context, tokenHash string) (*PreviewGrant, error)
	// TouchPreviewGrant records that the edge honoured a grant. Throttled by
	// the caller; see notePreviewSeen.
	TouchPreviewGrant(ctx context.Context, grantId string, at time.Time) error
}

// PreviewGrant is the projection of v1:platform:sitePreviewGrant the serving
// path needs (epic memql#5531, issue memql#5545).
//
// THE TOKEN DIGEST IS NOT HERE. The edge FILTERS on it and never reads it back
// -- there is no request this package serves that needs the stored digest in
// hand, and a field carried for no reader is one more place a credential's
// stored form can reach a log line.
type PreviewGrant struct {
	// ID is the bare grant row id.
	ID string
	// SiteID is the deployable this grant previews. The edge compares it
	// against the site it just resolved: a grant is for ONE deployable, and a
	// cookie carried to a sibling hostname must not grant access.
	SiteID string
	// CandidateRef records the version at mint time. Non-storefront previews
	// must still match the current candidate; storefront Testing follows the
	// shared published version throughout the authorized session.
	CandidateRef string
	// ExpiresAt is when it stops resolving. ZERO IS EXPIRED, never eternal: an
	// unreadable or absent expiry on a bearer credential must fail toward the
	// refusal.
	ExpiresAt time.Time
	// RevokedAt is non-empty once an operator ended this preview.
	RevokedAt string
	// LastSeenAt is what the throttle compares against before writing again.
	LastSeenAt time.Time
}

// previewCacheTTL bounds how long a resolved grant is reused without a read.
//
// FIFTEEN SECONDS, and the number is set by revocation rather than by load. A
// preview loads a document and every asset under it, so a read per request
// would put the graph in the serving path of a page; but "end this preview" has
// to actually end it, and this window is how long a revoked grant can still be
// honoured on a replica that already saw it. Fifteen seconds is shorter than
// anybody can act on a leaked link and long enough to serve a page from one
// read.
const previewCacheTTL = 15 * time.Second

// previewSeenThrottle bounds how often lastSeenAt is written.
//
// The field is evidence that a URL WAS USED, not a request counter, so one
// write per minute of active preview says everything it claims to say. Without
// the throttle a single page load would write once per asset -- a graph write
// per image, on the serving path.
const previewSeenThrottle = time.Minute

// previewCookieMaxAge is how long the browser keeps the cookie.
//
// It is deliberately NOT the grant's lifetime: the cookie is a convenience and
// the GRANT is the credential, so a cookie outliving its grant costs a fallthrough
// to the public site (or a Testing refusal), never extra access. Capped at the
// maximum a grant can be configured for, so no browser holds one longer than a
// grant could ever be valid.
var previewCookieMaxAge = int(memql.MaxPreviewGrantTTL / time.Second)

// previewResolution is one cached answer about a token digest. A NEGATIVE
// ANSWER IS CACHED TOO -- without it, a bad or stale cookie drives a query per
// asset request, which is an amplifier anyone holding an old link could point
// at the database.
type previewResolution struct {
	grant *PreviewGrant
	at    time.Time
}

// previewCache is the per-replica cache and the lastSeenAt throttle.
type previewCache struct {
	mu       sync.RWMutex
	byDigest map[string]previewResolution
	seenAt   map[string]time.Time
}

func newPreviewCache() *previewCache {
	return &previewCache{byDigest: map[string]previewResolution{}, seenAt: map[string]time.Time{}}
}

func (c *previewCache) get(digest string) (*PreviewGrant, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.byDigest[digest]
	if !ok || time.Since(e.at) >= previewCacheTTL {
		return nil, false
	}
	return e.grant, true
}

func (c *previewCache) put(digest string, grant *PreviewGrant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A BOUND ON THE MAP, because its key is attacker-chosen: a cookie carries
	// any string, so an unbounded map keyed by digest is a memory leak with a
	// public trigger. Previews are a handful per cluster; a thousand entries is
	// far past any real population, and dropping the whole map is right because
	// there is nothing here worth an eviction policy.
	if len(c.byDigest) > 1000 {
		c.byDigest = map[string]previewResolution{}
	}
	c.byDigest[digest] = previewResolution{grant: grant, at: time.Now()}
}

// shouldTouch reports whether lastSeenAt is due a write, and records the intent
// in the same critical section so concurrent requests cannot each decide yes.
func (c *previewCache) shouldTouch(grantID string, last time.Time) bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.seenAt[grantID]; ok && now.Sub(at) < previewSeenThrottle {
		return false
	}
	if !last.IsZero() && now.Sub(last) < previewSeenThrottle {
		c.seenAt[grantID] = last
		return false
	}
	if len(c.seenAt) > 1000 {
		c.seenAt = map[string]time.Time{}
	}
	c.seenAt[grantID] = now
	return true
}

// previewGrantFor resolves the grant a request carries, or nil.
//
// NIL IS THE ORDINARY ANSWER and it is never an error. Every way this can fail
// -- no cookie, a malformed one, no grant, the wrong deployable, a moved
// candidate, an expired or revoked grant -- means the same thing to the caller:
// serve the public site, or refuse a private Testing request.
func (h *Handler) previewGrantFor(r *http.Request, site *Site) *PreviewGrant {
	if site == nil || h.resolver == nil || (site.Kind == storefrontKind && !site.StorefrontTesting) {
		return nil
	}
	cookie, err := r.Cookie(memql.PreviewCookieName)
	if err != nil || cookie == nil {
		return nil
	}
	token := strings.TrimSpace(cookie.Value)
	if !memql.WellFormedPreviewToken(token) {
		return nil
	}
	digest := memql.PreviewTokenDigest(token)

	grant, cached := h.previews.get(digest)
	if !cached {
		if h.previewExec == nil {
			return nil
		}
		resolved, err := h.previewExec.PreviewGrantByToken(r.Context(), digest)
		if err != nil {
			// LOGGED AND TREATED AS ABSENT. A grant that cannot be read must
			// not take the public site down, and it must not grant either --
			// the public answer is the safe one in both directions.
			h.logger.Warn("edge: could not resolve a preview grant",
				"component", "edge", "site", site.ID, "err", err)
			return nil
		}
		h.previews.put(digest, resolved)
		grant = resolved
	}
	if grant == nil {
		return nil
	}
	if grant.SiteID != site.ID {
		return nil
	}
	// Storefront access belongs to the stable testing destination, so a
	// redeploy updates its design without ending the testing session. Other
	// deployables keep candidate-scoped previews.
	if !site.StorefrontTesting && (strings.TrimSpace(grant.CandidateRef) == "" ||
		strings.TrimSpace(grant.CandidateRef) != strings.TrimSpace(site.CandidateRef)) {
		return nil
	}

	if strings.TrimSpace(grant.RevokedAt) != "" {
		return nil
	}
	// A ZERO EXPIRY IS EXPIRED. An unreadable or absent expiry on a bearer
	// credential fails toward the refusal, never toward forever.
	if grant.ExpiresAt.IsZero() || !time.Now().UTC().Before(grant.ExpiresAt) {
		return nil
	}
	return grant
}

// previewSite returns the Site to serve under a grant: a COPY with the
// candidate version and the preview binding substituted.
//
// A COPY, NOT A MUTATION. The original is the resolver's CACHED value, shared
// by every request to this hostname on this replica -- writing through it would
// serve the candidate to the public, from one preview, until the cache expired.
// That is the single worst bug this feature could have, and taking a copy is
// the whole of what prevents it.
//
// THE PREVIEW STORE REPLACES THE SERVING STORE OUTRIGHT, including when there
// is no preview store: a storefront previewing without a development store
// attached gets NO store rather than falling back to the live one. Falling back
// would point a preview at the store shoppers reach, which is exactly what the
// guard one level up refuses -- and a fallback here would quietly undo it.
func previewSite(site *Site, grant *PreviewGrant) *Site {
	if site == nil || grant == nil {
		return site
	}
	copied := *site
	if !site.StorefrontTesting {
		copied.BundleRef = strings.TrimSpace(site.CandidateRef)
	}
	copied.Store = site.PreviewStore
	copied.Binding = site.PreviewBinding
	return &copied
}

// servePreviewEnter redeems a token from a link and turns it into a cookie.
//
// THE REDIRECT IS THE POINT, not a nicety. The token is in the address bar for
// exactly one navigation and is a cookie thereafter, so it never reaches a
// Referer header sent by the page's own assets, never lands in a browser
// history entry anybody shares, and never appears in a screenshot of a working
// preview.
//
// IT ANSWERS 303 TO "/" WHETHER OR NOT THE TOKEN IS ANY GOOD, and the identical
// answer is deliberate: a distinguishable failure here would turn this path
// into an oracle for which tokens exist. A bad token simply sets no cookie, and
// the person lands on the public site -- which for a draft deployable is the
// 404 anybody else gets.
func (h *Handler) servePreviewEnter(w http.ResponseWriter, r *http.Request, site *Site) {
	token := strings.TrimSpace(r.URL.Query().Get(memql.PreviewGrantParam))
	if memql.WellFormedPreviewToken(token) {
		http.SetCookie(w, &http.Cookie{
			Name:  memql.PreviewCookieName,
			Value: token,
			Path:  "/",
			// HttpOnly: no script on the previewed page can read the
			// credential that is showing it -- and a candidate version is
			// precisely the code nobody has reviewed in production yet.
			HttpOnly: true,
			// Secure: it is a bearer credential. Every host this cluster serves
			// is behind TLS in the cloud and in the local parity cluster alike.
			Secure: true,
			// Lax rather than Strict: Shopify's hosted checkout redirects BACK
			// to this origin at the end of the walk, and Strict would drop the
			// cookie on that top-level navigation -- so the person would land
			// on the public site at the one moment the preview matters most.
			SameSite: http.SameSiteLaxMode,
			MaxAge:   previewCookieMaxAge,
		})
	}
	// Host-only by omitting Domain: the cookie belongs to this deployable's own
	// hostname and to no sibling under the cluster domain.
	previewNoStore(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// servePreviewLeave clears the cookie in this browser.
//
// IT IS NOT REVOCATION, and the difference is worth being plain about: the
// grant row is untouched, so a link somebody else holds goes on working until
// it expires or an operator revokes it. This is the "show me what the public
// sees" button, not the "end this preview" one.
func (h *Handler) servePreviewLeave(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     memql.PreviewCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	previewNoStore(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// previewHeaders marks a previewed response as private and unindexable.
//
// BOTH ARE LOAD-BEARING AND NEITHER IS OBVIOUS. `no-store` keeps a candidate
// version out of every shared cache between here and the operator's browser --
// without it a CDN or a corporate proxy could serve an unpublished storefront
// to the next person through. `X-Robots-Tag: noindex, nofollow` stops a crawler
// that somehow reached a preview from putting an unpublished version, and its
// development-store prices, into a search index.
//
// `Vary: Cookie` is the same statement to a cache that does pay attention: this
// response DEPENDS on the cookie, so it may not be reused for a request without
// one.
func previewHeaders(w http.ResponseWriter) {
	previewNoStore(w)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Add("Vary", "Cookie")
}

func previewNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// notePreviewSeen records that a grant was honoured, throttled and off the
// request's path.
//
// FIRE AND FORGET, ON ITS OWN CONTEXT. The request's context is cancelled the
// moment the response finishes, so a write started on it would be cancelled
// mid-flight most of the time -- and this is evidence, not something a visitor
// waits for. A failure is logged and nothing else: a preview must not fail
// because the record of it could not be written.
func (h *Handler) notePreviewSeen(grant *PreviewGrant) {
	if h.previewExec == nil || grant == nil {
		return
	}
	if !h.previews.shouldTouch(grant.ID, grant.LastSeenAt) {
		return
	}
	go func(id string) {
		ctx, cancel := contextWithPreviewWriteTimeout()
		defer cancel()
		if err := h.previewExec.TouchPreviewGrant(ctx, id, time.Now().UTC()); err != nil {
			h.logger.Warn("edge: could not record that a preview was used",
				"component", "edge", "grantId", id, "err", err)
		}
	}(grant.ID)
}

// contextWithPreviewWriteTimeout bounds the detached lastSeenAt write.
func contextWithPreviewWriteTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
