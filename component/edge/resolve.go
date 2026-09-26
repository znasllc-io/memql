// component/edge/resolve.go -- Host-to-Site resolution, cached. The package
// doc comment (the "why" behind this package) lives in doc.go.
package edge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// Site is the projection of v1:platform:site the edge needs to serve a request.
type Site struct {
	ID        string
	Hostname  string
	Kind      string // "spa" | "static" | "shopify_storefront"
	BundleRef string
	Status    string // "draft" | "live" | "disabled" | "archived" | "archived"
	Title     string
	APIProxy  bool

	// OwnerUserID is the site row's owner, or EMPTY for a cluster-owned
	// site (the platform's own surfaces).
	//
	// IT IS AUTHORITY, not decoration. The shopper surface runs its write
	// under this user as borrowed authority -- the merchant owns what is
	// written through their storefront and the shopper is data on it -- so
	// an empty value means there is nobody to borrow and the surface is
	// refused rather than run under a synthetic actor. Nothing else on the
	// serving path reads it.
	OwnerUserID string

	// ShopperForms mounts this deployable's SHOPPER SURFACE on its own
	// origin (epic memql#5532, issue memql#5551): the forms and reads the
	// loaded packs declare, under /_memql/forms/ and /_memql/reads/.
	//
	// OFF IS THE DEFAULT AND ABSENT READS AS OFF. rowBool collapses absent
	// and false, which is exactly right here and worth saying because it
	// usually is not: every site row written before this field existed must
	// resolve with no shopper surface, and there is no third state to
	// preserve.
	//
	// INDEPENDENT OF APIProxy. That switch is for a bundle holding a MemQL
	// cookie; a shopper holds none. A storefront that wants a review form
	// and no authenticated API says exactly that, and one that wants the
	// API and no public form says the opposite.
	ShopperForms bool
	SystemOwned  bool

	// StorefrontTesting is a private sibling origin serving BundleRef with PreviewStore.
	StorefrontTesting bool

	// Binding is the site row's typed per-kind configuration, carried through
	// as the untyped object the row stores (memql#4345). Empty for every kind
	// that has none (spa, static).
	//
	// For kind="shopify_storefront" it holds exactly {storeId}, which NAMES a
	// v1:shopify:store row (epic memql#5530, issue memql#5538). It used to
	// hold {storeDomain, storefrontTokenRef} -- the same two values that store
	// row already held, edited in two places at two authorization tiers -- and
	// that duplication is what the binding stopped being.
	//
	// It is the id's arrival, and Store below is its resolution. Runtime
	// config uses its presence to distinguish unbound from unavailable;
	// the policy and connection credentials read Store. Keeping the raw object is what lets the resolver see whether
	// there is a store to look up at all without a second projection.
	Binding map[string]any

	// CandidateRef is the CANDIDATE VERSION: a published bundle version this
	// deployable is NOT serving (epic memql#5531). Empty is the ordinary state.
	//
	// NOTHING READS IT ON THE PUBLIC PATH. It reaches the serving path only
	// through previewSite (preview.go), which returns a COPY of this Site with
	// the candidate substituted into BundleRef -- so every consumer downstream
	// opens a bundle and builds a policy exactly as it always has, and there is
	// no "is this a preview" branch anywhere below that one function. That is
	// design D7 made literal: the engine chooses a bundle and a binding, and
	// does nothing else differently.
	CandidateRef string

	// PreviewBinding is the site row's preview binding -- {storeId} naming the
	// DEVELOPMENT store a candidate is exercised against (design D8). Carried
	// as the raw object for Binding's reason: it is the id's arrival, and
	// PreviewStore below is its resolution.
	PreviewBinding map[string]any

	// PreviewStore is the row PreviewBinding.storeId names, resolved, or nil.
	//
	// IT IS RESOLVED FOR EVERY STOREFRONT THAT HAS ONE, whether or not the
	// request is a preview, because the Site is CACHED and a request cannot
	// wait for a second lookup. That costs one extra read per cache fill on
	// storefronts with a development store attached, and it buys the property
	// that matters: a preview request is served from exactly the same cached
	// value a public request is, so the two can never disagree about what this
	// deployable is.
	//
	// IT NEVER REACHES A DOCUMENT OR A POLICY EXCEPT UNDER A VALID GRANT.
	// previewSite is the only thing that moves it into Store, and
	// TestTheRuntimeConfigNeverCarriesThePreviewBindingWithoutAGrant greps the
	// SERVED document for it, the way the Admin token's absence is asserted.
	PreviewStore *BoundStore

	// Store is the row binding.storeId names, resolved. Nil for every kind
	// that has no binding, for an unbound storefront, and for a storefront
	// whose store cannot be read -- three states the serving path treats
	// alike, because the answer in all three is the same: serve the bundle,
	// serve no storefront block, and name no store in the policy.
	Store *BoundStore

	// Account is the account whose reserved front door this request arrived
	// through, or nil (epic memql#5168, design D4).
	//
	// PRESENT ONLY ON THE THIRD RESOLUTION PATH. A request to a site's own
	// hostname or to a live custom domain leaves it nil, and the
	// runtime-config document then omits the key entirely -- so a document
	// served on the cluster's own host is byte-identical to what it was before
	// this field existed, which is the additive-only rule RuntimeConfig
	// already documents.
	//
	// It is CONTEXT, never authorization. What a client's member may read is
	// decided by each concept's own @rowAuthz declaration and by record A's
	// account grant, on the server, for every request regardless of which host
	// it arrived on. This field is what lets the shell say whose door you came
	// through and default its pickers to that account -- and nothing else
	// reads it to decide anything.
	Account *SiteAccount

	// ResolutionTail is what this site answers for a path matching NO file in
	// its bundle: "fallback" (index.html), "not_found" (404), or EMPTY --
	// which is the default and means Kind decides, exactly as it did before
	// this field existed (memql#5535).
	//
	// EMPTY IS NOT A THIRD BEHAVIOUR, it is the absence of a choice. Every
	// site row in every cluster carries no value today and must keep
	// resolving as it always has, which is what makes the field additive; and
	// an unrecognised value reads as empty rather than as 404, because a typo
	// must not take a live site's every client-side route dark.
	//
	// It is a property of the SITE and not of the Kind, and that is the
	// decision. The kind enum says a shopify_storefront IS a spa bundle,
	// while the first storefront built was a multi-page prerendered tree that
	// declared kind "static" precisely to get the 404 -- and thereby gave up
	// the binding, the storefront block in its runtime document, and the
	// policy that admits Shopify. Neither tail is right for every storefront.
	ResolutionTail string

	// Settings is the site row's runtime settings (epic memql#4906, P7): the
	// plain string values a bundle reads at load, merged into the site's
	// runtime-config document under `settings` by runtimeconfig.go. Empty
	// for a row that carries none; never nil on the document, which always
	// carries the key. Only STRING values survive the projection
	// (siteFromRow): the write guard admits nothing else, and a raw row that
	// slipped one past it must not put a number where a bundle reads a
	// string.
	Settings map[string]string
}

// BoundStore is the v1:shopify:store row as the SERVING PATH sees it: the
// serving fields it needs and not one more (epic memql#5530, issue memql#5538).
//
// The store row also holds adminTokenRef and webhookSecretRef. Neither is
// here, and the omission is the safety property rather than an economy: a
// credential reference the edge was never handed cannot reach a response
// header, a served document or a log line.
// TestTheBoundStoreCarriesOnlyWhatTheServingPathNeeds pins the field list so
// adding one is a decision.
//
// It is resolved at SITE-RESOLUTION time, under the same synthetic
// cluster-owner actor the site read uses, and cached with the site. That is
// what makes "an edit to the store reaches the site with no second write"
// true: the site row is never rewritten, and the store's own event flushes
// the cache.
type BoundStore struct {
	// ID is the bare v1:shopify:store row id -- what binding.storeId names.
	ID string
	// Domain is the myshopify.com host: the Storefront API origin, and the one
	// source admitted into connect-src / img-src / media-src.
	Domain string
	// StorefrontTokenRef NAMES a v1:platform:globalSecret row. The edge
	// resolves it at serve time into the runtime-config document, and that is
	// still the only place it is dereferenced.
	StorefrontTokenRef string
	// APIVersion supplies the Storefront API version unless the site explicitly
	// pins its own. It follows the connected store without rewriting the site.
	APIVersion string
}

// SiteAccount is the account behind a reserved front door, as the served page
// needs to know it.
//
// TWO FIELDS, AND THE MISSING ONE IS THE DECISION. There is no account NAME
// here. The obvious third field would be denormalized onto the door row and
// then be wrong the first time somebody renamed the client -- and it would be
// served in an UNAUTHENTICATED document, telling any passer-by which company
// this cluster serves at this name. The OS reads the display name through its
// own authorized query once somebody has signed in, which is both fresher and
// narrower.
type SiteAccount struct {
	// ID is the v1:accounts:account row id.
	ID string
	// ReservedName is the name the door serves beneath (memql.acme.com), not
	// the host the request arrived on. The shell composes what it needs from
	// it through frontdoor.AccountRoleHost.
	ReservedName string
}

// QueryExecutor is the narrow read the resolver needs. Narrow deliberately:
// the edge should not be able to reach the rest of the graph.
type QueryExecutor interface {
	SiteByHostname(ctx context.Context, hostname string) (*Site, error)
	// SiteForCustomDomain resolves a hostname through a LIVE
	// v1:platform:customDomain binding to the site it names (epic memql#4805,
	// design D8). A miss -- no binding, or one that is not live -- is
	// (nil, nil), exactly like SiteByHostname's.
	SiteForCustomDomain(ctx context.Context, hostname string) (*Site, error)
	// SiteForAccountFrontDoor resolves an `app.<reservedName>` host through a
	// LIVE v1:platform:accountFrontDoor row to the OS site, with the account
	// attached (epic memql#5168, design D4/F). A miss is (nil, nil), exactly
	// like the two above.
	SiteForAccountFrontDoor(ctx context.Context, hostname string) (*Site, error)
	// StoreByID resolves the v1:shopify:store row a storefront binding names
	// (epic memql#5530, issue memql#5538). Returns (nil, nil) for a miss -- a
	// store that is gone is not an error, it is a storefront with nothing to
	// talk to, and the resolver depends on that distinction to leave the
	// bundle serving.
	StoreByID(ctx context.Context, storeId string) (*BoundStore, error)
}

// Resolver maps a request Host to a Site.
type Resolver interface {
	Resolve(ctx context.Context, hostname string) (*Site, error)
	Invalidate(hostname string)
	// InvalidateAll drops every cached resolution -- what a v1:shopify:store
	// event triggers, since that row carries no hostname to evict by. See the
	// implementation for why flushing beats a reverse index.
	InvalidateAll()
}

type entry struct {
	site *Site
	at   time.Time
}

type resolver struct {
	exec QueryExecutor
	ttl  time.Duration

	// logger is where a failed BOUND-STORE read goes. It is the only thing in
	// this file that has something to say and no caller to say it to: a store
	// that cannot be read is deliberately not an error (the bundle keeps
	// serving), so without a line here the condition would be invisible.
	// Defaulted rather than taken as a parameter, so NewResolver's signature
	// -- which app/transport_edge.go wires -- is unchanged.
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]entry

	// sf collapses concurrent cold-cache resolutions for the SAME hostname
	// into one query, the same shape as integrations/cognition's cache-miss
	// singleflight groups (e.g. recentUtterSF / spaceInfoSF in
	// prompt_context_cache.go: "singleflight to avoid herd"). Without it, N
	// concurrent first-requests for an uncached hostname each drive their
	// own query before any of them can populate the cache -- a residual
	// amplification window on top of miss-caching, and one a burst is
	// exactly how someone would exercise.
	sf singleflight.Group
}

// TWO CACHE LAYERS, and both are worth knowing about (memql#4534). This
// per-replica map is the OUTER one; behind it the engine's result cache also
// holds the siteByHostname read. Both are invalidated on a site write -- this
// one off graph.node.*.v1:platform:site (invalidation_subscriber.go), the
// engine's off cache.invalidate.v1:platform:site -- so the TTLs are backstops
// for a missed invalidation rather than the freshness mechanism. The DSL read
// carries an explicit @cache(30) so the inner backstop matches this one; see
// dsl/platform/queries.memql for why a looser inner TTL would make this one's
// bound an illusion.
//
// NewResolver returns a caching Resolver. The TTL bounds staleness after a
// site row changes on ANOTHER node -- the write lands wherever the portal is
// served from, and this cache lives on every edge replica, so the TTL is the
// backstop behind the change-feed invalidation in Task 9.
func NewResolver(exec QueryExecutor, ttl time.Duration) Resolver {
	return &resolver{exec: exec, ttl: ttl, cache: map[string]entry{}, logger: slog.Default()}
}

// normalizeHost strips the port and lowercases. A Host header carries a port
// whenever the listener is not on the scheme's default, and browsers do not
// agree on case.
func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

func (r *resolver) Resolve(ctx context.Context, hostname string) (*Site, error) {
	key := normalizeHost(hostname)

	r.mu.RLock()
	e, ok := r.cache[key]
	r.mu.RUnlock()
	if ok && time.Since(e.at) < r.ttl {
		return e.site, nil
	}

	// Slow path: the engine, singleflighted per hostname so concurrent
	// misses for the same key collapse into one query instead of each
	// driving their own.
	anySite, err, _ := r.sf.Do(key, func() (any, error) {
		lookup := key
		production, testing := frontdoor.StorefrontProductionHost(key)
		if testing {
			lookup = production
		}
		site, err := r.exec.SiteByHostname(ctx, lookup)
		if testing && site != nil && (site.Kind != storefrontKind || normalizeHost(site.Hostname) != production || site.SystemOwned) {
			site = nil
		}
		if err != nil {
			return nil, err
		}

		if site != nil {
			copied := *site
			site = &copied
		}

		// THE CUSTOM-DOMAIN ALIAS (epic memql#4805, design D8). One extra
		// step, asked ONLY on a miss, so a hostname this cluster serves as
		// <slug>.<domain> costs exactly what it did before -- and a site's own
		// hostname always wins, which is the ordering that matters: a binding
		// can never take traffic away from a deployable already answering on
		// that name.
		//
		// Everything downstream is unchanged. The Site this returns is the
		// same row, so per-site CSP, the runtime-config document and the
		// /_memql/* apiProxy behave identically on the client's origin -- and
		// because the API is same-origin there, a custom domain has no CORS
		// story at all.
		//
		// A MISS HERE IS ALSO CACHED, by the shared write below: without that,
		// a scanner walking random hostnames would drive TWO queries per
		// request instead of one, which would make the alias step an
		// amplifier rather than a lookup.
		if site == nil && !testing {
			site, err = r.exec.SiteForCustomDomain(ctx, key)
			if err != nil {
				return nil, err
			}
		}

		// THE ACCOUNT FRONT DOOR (epic memql#5168, design D4/F). The third and
		// last step, asked only after both misses, and the ORDER is the whole
		// of its safety.
		//
		// A site's own hostname wins first, because a deployable already
		// answering on a name must never lose traffic to a door. A live custom
		// domain wins second, because a client's OWN domain is a more specific
		// claim than a name reserved under ours -- and if the two ever collide
		// it is the client's that a person typed deliberately.
		//
		// It resolves to the OS site, which is not a lookup: the shell's row id
		// is the frontdoor.OsSite constant, known before any operator creates
		// anything. So this is ONE query on a miss, not two, and it returns the
		// account context in the same read.
		//
		// The miss is cached by the shared write below, for the reason the
		// alias step gives: without it, a scanner walking hostnames against the
		// wildcard would drive THREE queries per request instead of one.
		if site == nil && !testing {
			site, err = r.exec.SiteForAccountFrontDoor(ctx, key)
			if err != nil {
				return nil, err
			}
		}

		// THE BOUND STORE (epic memql#5530, issue memql#5538), resolved with
		// the site and cached with it.
		//
		// Here rather than in siteFromRow because it is a SECOND read, and here
		// rather than per-request because the policy is built on every asset
		// response. Caching it with the site is what makes the TTL and the
		// invalidation apply to both: a store event flushes this map
		// (InvalidateAll), which is how an edit to a store reaches every site
		// bound to it without the site row being written at all.
		//
		// A FAILED STORE READ LEAVES THE SITE SERVABLE. The bundle is not the
		// store's to take down: an unreadable store means no storefront block
		// and no store named in the policy, which is what a storefront nobody
		// has bound yet already gets. It is LOGGED rather than returned, because
		// the alternative is a live storefront going dark over a row it merely
		// references.
		if site != nil && site.Kind == storefrontKind {
			if storeId := strings.TrimSpace(bindingStoreId(site.Binding)); storeId != "" {
				store, serr := r.exec.StoreByID(ctx, storeId)
				if serr != nil {
					r.logger.Warn("edge: could not resolve the bound store",
						"component", "edge", "siteId", site.ID, "storeId", storeId, "err", serr)
				} else {
					site.Store = store
				}
			}
			// THE PREVIEW BINDING'S STORE, resolved beside the serving one and
			// cached with it (epic memql#5531). A second read on a cache fill
			// for storefronts that have a development store attached, and it
			// buys the property that matters: a preview request and a public
			// request are served from the SAME cached value, so the two can
			// never disagree about what this deployable is.
			//
			// A FAILED READ LEAVES THE SITE SERVABLE, exactly as the serving
			// store's does, and the consequence is narrower: a preview whose
			// store could not be read gets an unavailable connection state and
			// no store origin in its policy. The public path is untouched.
			if storeId := strings.TrimSpace(bindingStoreId(site.PreviewBinding)); storeId != "" {
				store, serr := r.exec.StoreByID(ctx, storeId)
				if serr != nil {
					r.logger.Warn("edge: could not resolve the preview binding's store",
						"component", "edge", "siteId", site.ID, "storeId", storeId, "err", serr)
				} else {
					site.PreviewStore = store
				}
			}
		}

		// A MISS IS CACHED TOO. Without this, a scanner walking random hostnames
		// drives one database query per request -- an amplifier pointed at the
		// database, reachable by anyone who can resolve the wildcard.
		if testing && site != nil {
			copied := *site
			copied.Hostname = key
			copied.StorefrontTesting = true
			site = &copied
		}
		r.mu.Lock()
		r.cache[key] = entry{site: site, at: time.Now()}
		r.mu.Unlock()

		return site, nil
	})
	if err != nil {
		return nil, err
	}
	// site may legitimately be a nil *Site (a cached miss); the type
	// assertion still succeeds because singleflight boxes the concrete
	// *Site the closure returned, nil or not.
	site, _ := anySite.(*Site)
	return site, nil
}

func (r *resolver) Invalidate(hostname string) {
	key := normalizeHost(hostname)
	r.mu.Lock()
	delete(r.cache, key)
	delete(r.cache, frontdoor.StorefrontTestingHost(key))
	r.mu.Unlock()
}

// InvalidateAll drops every cached resolution.
//
// It is what a v1:shopify:store event triggers. The cache is keyed by HOSTNAME
// and a store row carries none, so there is no entry to evict by name -- the
// edge would need a reverse index from store to site to do better, and a store
// edit is an operator act measured in ones per day while the cache is a 30s
// TTL over a handful of hostnames. Flushing it is cheaper than the index.
func (r *resolver) InvalidateAll() {
	r.mu.Lock()
	r.cache = map[string]entry{}
	r.mu.Unlock()
}
