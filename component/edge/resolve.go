// Package edge serves this cluster's web surfaces: every hosted SPA and
// website, and the memQL Portal, which is site #1 and takes no special path.
//
// It is component/portal generalized. That package serves exactly one bundle
// from a directory named by an env var; this one resolves the request Host to
// a v1:platform:site row and serves the bundle that row names. The portal
// keeps working because its row's bundleRef is file:///app/portal -- the same
// directory, reached through the general mechanism.
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
	Kind        string // "spa" | "static"
	BundleRef   string
	Status      string // "draft" | "live" | "disabled"
	Title       string
	APIProxy    bool
	SystemOwned bool
}

// QueryExecutor is the narrow read the resolver needs. Narrow deliberately:
// the edge should not be able to reach the rest of the graph.
type QueryExecutor interface {
	SiteByHostname(ctx context.Context, hostname string) (*Site, error)
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
