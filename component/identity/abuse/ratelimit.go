package abuse

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

// ipRateLimiterMaxBuckets triggers GC of the buckets map when it
// exceeds this size. We deliberately don't enforce a hard cap:
// genuine traffic spikes shouldn't surface as failed lookups.
const ipRateLimiterMaxBuckets = 50000

// ipRateLimiterIdleSweep is the idle window past which a bucket is
// removed during the GC sweep. Two hours covers the natural rhythm
// of a normal user re-trying within the same session.
const ipRateLimiterIdleSweep = 2 * time.Hour

// ipBucket is a single per-IP token bucket. Refill rate is encoded
// at the limiter level (perHour); the bucket only carries state.
type ipBucket struct {
	tokens   float64
	updated  time.Time
	lastSeen time.Time // for the idle-sweep pass
}

// IPRateLimiter caps requests-per-IP using a simple token bucket.
// Bucket size = perHour; refill rate = perHour tokens per hour
// (linear, fractional). Concurrency: a single mutex around the
// buckets map. At our scale (one mutation per request) the cost is
// negligible and the implementation stays small.
//
// In-memory only — Phase 4 doesn't carry a Redis dependency. Multi-
// node identity deployments will need either sticky sessions on the
// load balancer or a Redis-backed bucket; out of scope here.
type IPRateLimiter struct {
	perHour int
	mu      sync.Mutex
	buckets map[string]*ipBucket
	logger  *slog.Logger
}

// NewIPRateLimiter constructs a limiter with bucket size = perHour
// and refill rate = perHour/hour. perHour <= 0 disables the limiter
// (Allow always returns true) — useful in tests and for explicit
// "no rate limit" deployments.
func NewIPRateLimiter(perHour int, logger *slog.Logger) *IPRateLimiter {
	return &IPRateLimiter{
		perHour: perHour,
		buckets: make(map[string]*ipBucket, 256),
		logger:  logger,
	}
}

// Allow consumes one token for the given IP. Returns (true, 0) when
// allowed; (false, retryAfterSeconds) when over budget. retryAfter is
// the time until the bucket has at least one token, rounded up to the
// nearest second so callers can stamp Retry-After cleanly.
//
// Empty ip is treated as a single shared bucket — better than letting
// unidentifiable callers bypass the limiter. The cost is one shared
// budget for "we couldn't extract an IP" requests, which in practice
// only happens on misconfigured proxies.
func (l *IPRateLimiter) Allow(ip string) (bool, int) {
	if l == nil || l.perHour <= 0 {
		return true, 0
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[ip]
	if !ok {
		// Lazy GC: when the map is large, drop stale entries
		// before allocating a new bucket. Keeps memory bounded
		// without a separate sweeper goroutine.
		if len(l.buckets) >= ipRateLimiterMaxBuckets {
			cutoff := now.Add(-ipRateLimiterIdleSweep)
			for k, v := range l.buckets {
				if v.lastSeen.Before(cutoff) {
					delete(l.buckets, k)
				}
			}
		}
		bucket = &ipBucket{
			tokens:   float64(l.perHour),
			updated:  now,
			lastSeen: now,
		}
		l.buckets[ip] = bucket
	}

	// Refill: tokens added since last touch, at perHour per hour.
	elapsed := now.Sub(bucket.updated)
	if elapsed > 0 {
		refill := elapsed.Seconds() * float64(l.perHour) / 3600.0
		bucket.tokens = math.Min(float64(l.perHour), bucket.tokens+refill)
		bucket.updated = now
	}
	bucket.lastSeen = now

	if bucket.tokens >= 1 {
		bucket.tokens--
		return true, 0
	}

	// Compute how long until 1 token is available.
	deficit := 1 - bucket.tokens
	secondsPerToken := 3600.0 / float64(l.perHour)
	retryAfter := int(math.Ceil(deficit * secondsPerToken))
	if retryAfter < 1 {
		retryAfter = 1
	}
	return false, retryAfter
}

// Snapshot returns a tiny diagnostic snapshot of the limiter for
// admin telemetry / tests. Not part of the request path.
func (l *IPRateLimiter) Snapshot() (bucketCount int, perHour int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets), l.perHour
}
