package edge

import (
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// shopper.go -- THE EDGE HALF OF THE SHOPPER WRITE PATH (epic memql#5532,
// issue memql#5551).
//
// # What arrives here
//
// A plain HTML form post from a shopper's browser, on a merchant's own
// origin, with no JavaScript. The first product's wholesale form forbids
// every easier answer: it is method="post" so a federal tax identifier
// never reaches a query string, a browser history entry or an access log,
// and its own comment says it "must NOT be downgraded to a JS submit
// handler" because the served policy is script-src 'self'. So the endpoint
// takes application/x-www-form-urlencoded and multipart from a plain form
// and answers 303 -- and form-action 'self' already confines the post to
// this origin.
//
// # What this file owns, and what it deliberately does not
//
// It owns the four things only the EDGE can know or should do:
//
//  1. THE SWITCH. site.ShopperForms, off by default. A site that did not
//     ask for a public form endpoint must not have one -- an open one on
//     every hosted site is the relay proxy.go's own comment warns about.
//  2. THE EFFECTIVE STORE. Which is free, and that is the nicest property
//     in this epic: ServeHTTP has ALREADY replaced site with
//     previewSite(site, grant) before this runs, so site.Store IS the store
//     in effect. A review submitted while previewing carries the
//     DEVELOPMENT store's id and is invisible to every live-scoped read.
//     D9 falls out of D7 rather than being implemented a second time.
//  3. THE ABUSE CONTROLS. A per-address, per-site token bucket and a body
//     size cap, both refusing BEFORE the proxy hop so a flood never reaches
//     the bff at all.
//  4. THE STAMP. Any inbound X-MemQL-Shopper-* is DELETED before ours is
//     written, so a client cannot supply its own.
//
// It does not own what may be reached: that is the pack's declaration
// (component/memql/shopper_surface.go), read on the bff. And it does not
// own the decision that the stamp is trustworthy: the bff re-reads the site
// row the stamp NAMES and re-checks everything about it, so the header is a
// pointer rather than an assertion. See component/server/shopper_handler.go.
//
// # A cluster-owned site is refused
//
// The write borrows the site row's ownerUserId as its authority. An empty
// one -- the cluster-owned state, which the platform's own sites carry --
// has no authority to borrow, so the surface is refused rather than run
// under some synthetic actor. Refusing here rather than at the bff is
// deliberate: the answer can be a page a person reads instead of a write
// error nobody sees.

const (
	// shopperFormPrefix and shopperReadPrefix are the two reserved paths on
	// a site's own origin. They sit under the SAME /_memql/ marker the API
	// proxy uses so the origin keeps ONE reserved namespace rather than
	// three -- a bundle may not claim /_memql/ and never could.
	shopperFormPrefix = "/_memql/forms/"
	shopperReadPrefix = "/_memql/reads/"

	shopperMaxBytesEnv = "MEMQL_SHOPPER_FORM_MAX_BYTES"
	shopperRateEnv     = "MEMQL_SHOPPER_FORM_RATE_PER_MINUTE"

	// defaultShopperMaxBytes bounds a request body.
	//
	// 64 KiB, and the number is set by what a form IS rather than by what a
	// server can take: a review body, a name, an email and a product handle.
	// A form that needs more than this is carrying a file, and a file upload
	// from an unauthenticated stranger is a different decision that nothing
	// in this epic makes.
	defaultShopperMaxBytes int64 = 64 << 10

	// defaultShopperRatePerMinute bounds submissions per address per site.
	//
	// TEN, which is generous for a person and useless for a flood. The
	// bucket is per (site, address) rather than per address: one merchant's
	// storefront being hammered must not stop shoppers writing to another's.
	defaultShopperRatePerMinute = 10
)

// The headers the edge stamps and the bff reads are declared ONCE, in
// component/memql, because that is the one package both modules can import
// -- component/server cannot reach component/edge. Aliased here so this
// file reads as it did and so a reader sees immediately that the names are
// not this package's to choose.
const (
	ShopperSiteHeader  = memql.ShopperSiteHeader
	ShopperStoreHeader = memql.ShopperStoreHeader
	ShopperOwnerHeader = memql.ShopperOwnerHeader
)

// shopperStampHeaders is every header this file controls. Used to STRIP
// before setting, so a client cannot supply its own.
var shopperStampHeaders = []string{ShopperSiteHeader, ShopperStoreHeader, ShopperOwnerHeader}

func shopperMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(shopperMaxBytesEnv))
	if raw == "" {
		return defaultShopperMaxBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		// A zero or unparseable cap falls back to the DEFAULT rather than to
		// zero or to unlimited. Zero would refuse every form and say nothing
		// about the typo; unlimited is the failure the cap exists to prevent.
		return defaultShopperMaxBytes
	}
	return n
}

func shopperRatePerMinute() int {
	raw := strings.TrimSpace(os.Getenv(shopperRateEnv))
	if raw == "" {
		return defaultShopperRatePerMinute
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultShopperRatePerMinute
	}
	return n
}

// shopperBucket is a per-(site, address) token bucket.
//
// PER REPLICA, and that is honest rather than ideal: two replicas behind
// one front door each allow the configured rate, so the cluster-wide
// ceiling is rate x replicas. A shared counter would need a round trip to
// the database on the path of every public form post, which is a worse
// trade for a control whose job is to stop a flood rather than to meter
// exactly. The number is small enough that the multiple does not matter and
// this comment is here so nobody reads the knob as a cluster-wide promise.
type shopperBucket struct {
	mu      sync.Mutex
	tokens  map[string]float64
	refill  map[string]time.Time
	perMin  int
	maxSize int
}

func newShopperBucket(perMinute int) *shopperBucket {
	return &shopperBucket{
		tokens:  map[string]float64{},
		refill:  map[string]time.Time{},
		perMin:  perMinute,
		maxSize: 10000,
	}
}

// allow spends a token for key, refilling at perMin per minute with a burst
// equal to perMin.
func (b *shopperBucket) allow(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A BOUND ON THE MAP, because its key contains a client address and is
	// therefore attacker-chosen. Dropping the whole map is right for the
	// same reason it is right in previewCache: there is nothing here worth
	// an eviction policy, and the worst outcome is that a flood in progress
	// gets one more burst.
	if len(b.tokens) > b.maxSize {
		b.tokens = map[string]float64{}
		b.refill = map[string]time.Time{}
	}

	burst := float64(b.perMin)
	last, seen := b.refill[key]
	tokens := burst
	if seen {
		tokens = b.tokens[key] + now.Sub(last).Minutes()*float64(b.perMin)
		if tokens > burst {
			tokens = burst
		}
	}
	b.refill[key] = now
	if tokens < 1 {
		b.tokens[key] = tokens
		return false
	}
	b.tokens[key] = tokens - 1
	return true
}

// shopperClientAddress is the address the rate limit keys on.
//
// THE LEFTMOST X-Forwarded-For ENTRY, falling back to the peer address.
// Spoofable by the client, and that is stated rather than hidden: the edge
// sits behind an ingress that appends, so the leftmost value is whatever
// the first hop saw and a determined caller can set it. It is the right key
// anyway, because the alternative -- the peer address -- is the INGRESS on
// every request in a cluster, which would make one bucket for the whole
// internet. The controls that do not depend on a truthful address are the
// per-site switch, the size cap and the declaration registry.
func shopperClientAddress(r *http.Request) string {
	if fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); fwd != "" {
		if first, _, found := strings.Cut(fwd, ","); found {
			if v := strings.TrimSpace(first); v != "" {
				return v
			}
		} else if fwd != "" {
			return fwd
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// shopperRoute is the (pack, name) a request addresses, parsed out of the
// path.
type shopperRoute struct {
	pack string
	name string
	read bool
}

// parseShopperPath splits /_memql/forms/{pack}/{name} or
// /_memql/reads/{pack}/{name}.
//
// EXACTLY TWO SEGMENTS, no more. A third would be a path the registry can
// never name, and accepting it would mean the bff parses a shape the edge
// blessed without understanding.
func parseShopperPath(p string) (shopperRoute, bool) {
	var rest string
	var read bool
	switch {
	case strings.HasPrefix(p, shopperFormPrefix):
		rest = strings.TrimPrefix(p, shopperFormPrefix)
	case strings.HasPrefix(p, shopperReadPrefix):
		rest = strings.TrimPrefix(p, shopperReadPrefix)
		read = true
	default:
		return shopperRoute{}, false
	}
	pack, name, found := strings.Cut(rest, "/")
	if !found || pack == "" || name == "" || strings.Contains(name, "/") {
		return shopperRoute{}, false
	}
	return shopperRoute{pack: pack, name: name, read: read}, true
}

// serveShopperSurface gates a shopper form post or read and forwards it.
//
// EVERY REFUSAL BEFORE THE FORWARD ANSWERS A PAGE, not a bare status line.
// The caller is a member of the public on somebody's storefront; a blank
// 429 is indistinguishable from the site being broken, and the merchant
// hears about it rather than us.
func (h *Handler) serveShopperSurface(w http.ResponseWriter, r *http.Request, site *Site) {
	route, ok := parseShopperPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// OFF IS 404, NOT 403. A site that has not turned this on must look like
	// it has no such path rather than like it has one it is guarding --
	// a 403 tells a prober the endpoint exists and is worth coming back to.
	if !site.ShopperForms {
		http.NotFound(w, r)
		return
	}

	// A CLUSTER-OWNED SITE HAS NO AUTHORITY TO BORROW. The platform's own
	// sites are the population, and none of them wants a public form; the
	// refusal is a page rather than a silent 404 because if somebody DOES
	// turn this on on a cluster-owned site, "nothing happens" is the least
	// useful answer available.
	owner := strings.TrimSpace(site.OwnerUserID)
	if owner == "" {
		h.logger.Warn("edge: refusing a shopper request to a site with no owner to borrow",
			"component", "edge", "site", site.ID, "path", r.URL.Path)
		writeShopperRefusal(w, shopperRefusalNotAvailable)
		return
	}

	if strings.TrimSpace(h.apiTarget) == "" {
		h.logger.Error("edge: a shopper surface is on but MEMQL_EDGE_API_TARGET is unset",
			"component", "edge", "site", site.ID)
		writeShopperRefusal(w, shopperRefusalNotAvailable)
		return
	}

	// The method split is the GET/POST rule /unsubscribe already keeps, and
	// for the same reason: a GET must never have the side effect, because
	// link scanners and prefetchers follow links.
	if route.read && r.Method != http.MethodGet {
		writeShopperRefusal(w, shopperRefusalNotAvailable)
		return
	}
	if !route.read && r.Method != http.MethodPost {
		writeShopperRefusal(w, shopperRefusalNotAvailable)
		return
	}

	// SIZE FIRST, because it is the cheapest refusal and the only one that
	// can be made without spending a token. Content-Length is advisory --
	// the bff enforces the real cap while reading -- but refusing an
	// honestly-declared oversize body here saves the hop.
	if !route.read && r.ContentLength > shopperMaxBytes() {
		writeShopperRefusal(w, shopperRefusalTooLarge)
		return
	}

	if !h.shopperRate.allow(site.ID+"|"+shopperClientAddress(r), time.Now()) {
		writeShopperRefusal(w, shopperRefusalRateLimited)
		return
	}

	// THE EFFECTIVE STORE, which is whatever previewSite already put here.
	// A submit made under a preview grant carries the DEVELOPMENT store's
	// id; a public one carries the live store's; an unbound storefront
	// carries none and the bff refuses, because a shopper-written row with
	// no store is a row no storefront read can ever scope to.
	storeID := ""
	if site.Store != nil {
		storeID = strings.TrimSpace(site.Store.ID)
	}

	h.proxyShopperRequest(w, r, site, owner, storeID)
}

// proxyShopperRequest forwards to the bff with the stamp applied.
func (h *Handler) proxyShopperRequest(w http.ResponseWriter, r *http.Request, site *Site, owner, storeID string) {
	proxy := h.newBFFProxy(site, func(pr *httputil.ProxyRequest) {
		// STRIP, THEN SET. A request arriving with its own
		// X-MemQL-Shopper-* must not survive the hop -- this is the whole
		// reason the bff may treat the header as a pointer at all. Deleting
		// from pr.Out is what matters: the shared rewrite has already copied
		// the inbound headers across by the time this runs.
		for _, name := range shopperStampHeaders {
			pr.Out.Header.Del(name)
		}
		pr.Out.Header.Set(ShopperSiteHeader, site.ID)
		pr.Out.Header.Set(ShopperOwnerHeader, owner)
		if storeID != "" {
			pr.Out.Header.Set(ShopperStoreHeader, storeID)
		}
	})
	if proxy == nil {
		writeShopperRefusal(w, shopperRefusalNotAvailable)
		return
	}
	proxy.ServeHTTP(w, r)
}
