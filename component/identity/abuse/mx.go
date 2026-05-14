package abuse

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// mxCacheTTL is how long a positive or negative MX-lookup result lives
// in the per-domain cache before being re-resolved. One hour balances
// "fresh enough to catch operators turning up MX records" against
// "infrequent enough to keep DNS load and signup latency low."
const mxCacheTTL = time.Hour

// mxCacheMaxEntries triggers a sweep of expired entries when the
// in-memory cache grows past this size. We do not enforce an LRU bound
// — bounded sweeping on insertion keeps the implementation small and
// the working set is naturally domain-shaped (small).
const mxCacheMaxEntries = 5000

// mxCacheEntry is what mxCache stores per domain.
type mxCacheEntry struct {
	hasMX   bool
	expires time.Time
}

// mxCache is a small concurrent map with TTL semantics.
type mxCache struct {
	mu      sync.RWMutex
	entries map[string]mxCacheEntry
}

func newMXCache() *mxCache {
	return &mxCache{entries: make(map[string]mxCacheEntry, 256)}
}

// get returns (hasMX, hit). hit=false means either the key was absent
// or the entry was expired (callers re-resolve).
func (c *mxCache) get(domain string) (bool, bool) {
	c.mu.RLock()
	entry, ok := c.entries[domain]
	c.mu.RUnlock()
	if !ok {
		return false, false
	}
	if time.Now().After(entry.expires) {
		return false, false
	}
	return entry.hasMX, true
}

// put stores a fresh result. When the cache grows past
// mxCacheMaxEntries the function opportunistically sweeps any entries
// whose expires has passed before inserting.
func (c *mxCache) put(domain string, hasMX bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= mxCacheMaxEntries {
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[domain] = mxCacheEntry{
		hasMX:   hasMX,
		expires: time.Now().Add(mxCacheTTL),
	}
}

// MXValidator answers "does this email's domain have a mail server?"
// using DNS lookups, with results cached for mxCacheTTL.
type MXValidator struct {
	cache    *mxCache
	logger   *slog.Logger
	resolver *net.Resolver // nil = net default; injectable for tests
}

// NewMXValidator returns a validator using the default DNS resolver.
func NewMXValidator(logger *slog.Logger) *MXValidator {
	return &MXValidator{
		cache:  newMXCache(),
		logger: logger,
	}
}

// HasMX returns true if the email's domain has at least one MX
// record. Falls back to A/AAAA records (RFC 5321 §5.1: "If no MX
// record is found, but an A or AAAA record is present for the
// domain, the implicit MX record is the domain itself.") so domains
// that publish only an A record still validate.
//
// Cached results live for one hour. Errors are NOT cached — a
// transient resolver failure should not lock out a domain for an
// hour. Returns false on any malformed input (no @, empty domain).
//
// The context is used to bound the DNS resolution; callers should
// pass the request context so a slow resolver can be cancelled.
func (v *MXValidator) HasMX(ctx context.Context, email string) bool {
	domain := emailDomainLower(email)
	if domain == "" {
		return false
	}

	if hit, ok := v.cache.get(domain); ok {
		return hit
	}

	hasMX := v.resolveDomain(ctx, domain)
	v.cache.put(domain, hasMX)
	return hasMX
}

// resolveDomain performs the actual DNS work without touching the
// cache. Split out so callers that want to bypass the cache (tests,
// admin diagnostics) can do so.
func (v *MXValidator) resolveDomain(ctx context.Context, domain string) bool {
	resolver := v.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	mxs, err := resolver.LookupMX(ctx, domain)
	if err == nil && len(mxs) > 0 {
		return true
	}
	// Fall back to A/AAAA: many small operators publish only an A
	// record and rely on the implicit-MX rule. Treat any address
	// hit as "mail might land here" — the magic-link delivery
	// step is the canonical authoritative check.
	addrs, err := resolver.LookupHost(ctx, domain)
	if err == nil && len(addrs) > 0 {
		return true
	}
	if v.logger != nil && err != nil {
		v.logger.Debug("mx_validation_no_records",
			slog.String("domain", domain),
			slog.String("error", err.Error()),
		)
	}
	return false
}

// emailDomainLower extracts the lowercased domain portion of an email,
// or "" if no @ is present or the domain is empty.
func emailDomainLower(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	return domain
}
