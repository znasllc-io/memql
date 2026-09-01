// component/edge/resolve.go -- Host-to-Site resolution, cached. The package
// doc comment (the "why" behind this package) lives in doc.go.
package edge

import (
	"context"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Site is the projection of v1:platform:site the edge needs to serve a request.
type Site struct {
	ID          string
	Hostname    string
	Kind        string // "spa" | "static" | "shopify_storefront"
	BundleRef   string
	Status      string // "draft" | "live" | "disabled"
	Title       string
	APIProxy    bool
	SystemOwned bool

	// Binding is the site row's typed per-kind configuration, carried through
	// as the untyped object the row stores (memql#4345). Empty for every kind
	// that has none (spa, static).
	//
	// For kind="shopify_storefront" it holds {storeDomain,
	// storefrontTokenRef}. NOTE WHAT IS NOT IN IT: the token itself. The ref
	// NAMES a v1:platform:globalSecret row and runtimeconfig.go resolves it at
	// SERVE time -- so the credential lives in the secret store, and anyone
	// reading the site row (or this cached copy of it) sees only the name of
	// the thing they would have to be allowed to read.
	Binding map[string]any
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
}

// Resolver maps a request Host to a Site.
type Resolver interface {
	Resolve(ctx context.Context, hostname string) (*Site, error)
	Invalidate(hostname string)
}

type entry struct {
	site *Site
	at   time.Time
}

type resolver struct {
	exec QueryExecutor
	ttl  time.Duration

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
	return &resolver{exec: exec, ttl: ttl, cache: map[string]entry{}}
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
		site, err := r.exec.SiteByHostname(ctx, key)
		if err != nil {
			return nil, err
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
		if site == nil {
			site, err = r.exec.SiteForCustomDomain(ctx, key)
			if err != nil {
				return nil, err
			}
		}

		// A MISS IS CACHED TOO. Without this, a scanner walking random hostnames
		// drives one database query per request -- an amplifier pointed at the
		// database, reachable by anyone who can resolve the wildcard.
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
	r.mu.Unlock()
}
