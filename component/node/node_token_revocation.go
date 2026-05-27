package node

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity/verifier"
)

// node_token_revocation.go closes the last operational gap memql#343
// set out to fill. NodeClassStreamInterceptor (in auth.go) handles JWT
// verification + class-pin + (nodeId, nodeType) binding extraction --
// JWKS-only, no store reads. memql#347 added the persistence side: a
// v1:identity:identity[node_token] row exists for every bootstrap-
// minted credential, with the row's `active` flag flipping to false
// when the operator revokes via /admin/tokens. But the verifier was
// still JWKS-only -- a revoked row meant the operator had to wait up
// to the JWT TTL (30 days) for outstanding tokens to lapse.
//
// memql#349 (this file) layers the row-state check on top of the
// existing interceptor: after the JWT is valid + the class is right
// + the binding is extracted, look the row up by (nodeType, nodeId)
// and reject when Active == false. Short-TTL in-process cache keeps
// the stream-open path fast under concurrent peer reconnects.
//
// Why a separate interceptor variant (not modifying the base): the
// base interceptor is JWKS-only and used by callers that explicitly
// don't want store reads (single-node dev, tests). The variant is
// opt-in. NodeClassStreamInterceptorWithRevocation(v, check, logger)
// with check == nil OR check.Resolver == nil falls back to the base
// no-revocation behaviour, so wiring the variant doesn't bind the
// caller to a live store.
//
// Why no separate revokedAt field (vs. the EpochCheck mechanism #106
// uses): memql#347 chose to encode revocation purely as the top-level
// `active` flag on v1:identity:identity, with the revocation
// timestamp captured in the audit event row rather than on the
// identity itself. This file follows that convention -- the resolver
// returns a bare bool, and "revoked" means "exists + Active == false."
// A non-existent row is treated as not-revoked (the operator-CLI mint
// path doesn't go through #347's persistence; only bootstrap-minted
// rows persist today, and operator-CLI mints can still be valid).

// NodeTokenRevocationResolver is the narrow port the interceptor
// uses to check whether a node token's identity row has been
// revoked by the operator. Implementations should be fast +
// cache-friendly -- the function is called on every NodeService.Stream
// open from every cluster peer.
//
// (nodeType, nodeId) are extracted from the verified JWT's claims;
// the resolver looks up the v1:identity:identity[node_token] row by
// that binding (matches memql#347's
// `identity.Store.LookupNodeTokenIdentityByBinding`).
//
// Returns (true, nil) when the row exists AND Active == false
// (verifier rejects the stream open with codes.PermissionDenied).
// Returns (false, nil) when the row is live or doesn't exist (the
// "operator-CLI mint that pre-dates persistence" case is treated as
// not-revoked; verifier proceeds).
// Returns (false, error) when the lookup itself failed; the
// interceptor logs and short-circuits to codes.Unauthenticated to
// avoid admitting traffic on a partially-failed revocation check.
type NodeTokenRevocationResolver interface {
	IsNodeTokenRevoked(ctx context.Context, nodeType, nodeId string) (bool, error)
}

// NodeRevocationCheck wraps a resolver with the per-call cache the
// interceptor consults. CacheTTL controls how long an answer is
// reused before re-asking the resolver; the default of 5s trades a
// small revocation-propagation lag for protection against DB-
// saturating fan-out when many nodes restart concurrently.
//
// Resolver == nil disables the whole check (the interceptor falls
// back to NodeClassStreamInterceptor's no-revocation behaviour).
type NodeRevocationCheck struct {
	Resolver NodeTokenRevocationResolver
	CacheTTL time.Duration
}

// DefaultNodeRevocationCacheTTL is the per-call cache TTL used when
// NodeRevocationCheck.CacheTTL is zero. 5s is the sweet spot the
// memql#343 issue notes called out: the operator's revoke click
// propagates to all nodes within a single cache-window of seconds,
// and the verifier holds the line via JWKS-only auth in the meantime.
// Production deploys that want tighter propagation can pass 1s; tests
// pass 0 (treated as default) or a small duration to exercise the
// "expired entry" branch.
const DefaultNodeRevocationCacheTTL = 5 * time.Second

// NodeClassStreamInterceptorWithRevocation is NodeClassStreamInterceptor
// plus the memql#349 revocation gate. When check == nil or
// check.Resolver == nil, it behaves identically to the base. When
// wired, every stream open looks up the node-token row (keyed by
// nodeType + nodeId from the verified JWT claims) and rejects when
// the row is Active == false.
//
// The lookup goes through a short-TTL in-process cache to keep the
// stream-open path fast under concurrent peer reconnects. Cache key
// is (nodeType, nodeId); cache value is "revoked-or-not at time of
// last lookup". The cache does NOT memoise errors -- a failed
// resolver call always retries on the next stream open.
func NodeClassStreamInterceptorWithRevocation(v *verifier.Verifier, check *NodeRevocationCheck, logger *slog.Logger) grpc.StreamServerInterceptor {
	base := NodeClassStreamInterceptor(v, logger)
	if check == nil || check.Resolver == nil {
		// No revocation gate wired -- fall back to base behaviour.
		// Preserves the pre-#349 contract for deployments that
		// haven't opted in.
		return base
	}
	ttl := check.CacheTTL
	if ttl <= 0 {
		ttl = DefaultNodeRevocationCacheTTL
	}
	cache := newNodeRevocationCache(ttl)
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// Two-stage interceptor: let the base verifier do its work
		// first (token extraction + JWT verify + class pin + binding
		// extraction), then add the revocation check inside the
		// downstream handler. NodeBindingFromContext is the read-back
		// of what NodeClassStreamInterceptor stamped onto ctx.
		return base(srv, ss, info, func(srv interface{}, wrapped grpc.ServerStream) error {
			ctx := wrapped.Context()
			nodeId, nodeType, ok := NodeBindingFromContext(ctx)
			if !ok {
				// Base interceptor must have stamped the binding --
				// if it didn't, something is wrong with the verifier
				// pipeline. Fail closed; this isn't a state callers
				// should observe.
				if logger != nil {
					logger.Warn("node revocation: binding missing from context (verifier pipeline broken?)",
						"method", info.FullMethod)
				}
				return status.Error(codes.Internal, "node binding missing")
			}

			revoked, err := cache.lookup(ctx, nodeType, nodeId, check.Resolver)
			if err != nil {
				if logger != nil {
					logger.Warn("node revocation: resolver lookup failed (rejecting open)",
						"node_type", nodeType,
						"node_id", nodeId,
						"method", info.FullMethod,
						"error", err)
				}
				return status.Error(codes.Unauthenticated, "node token revocation check failed")
			}
			if revoked {
				if logger != nil {
					logger.Info("node revocation: rejected revoked token",
						"node_type", nodeType,
						"node_id", nodeId,
						"method", info.FullMethod)
				}
				return status.Error(codes.PermissionDenied, "node token has been revoked")
			}
			return handler(srv, wrapped)
		})
	}
}

// nodeRevocationCache is a small (cacheKey, revoked, expires) table
// indexed by the binding pair. Holds positive ("not revoked") AND
// negative ("revoked") answers -- once a token is revoked, every
// subsequent open from that node hits the cache and short-circuits;
// the operator-CLI revoke click trickles to all peers within ttl.
type nodeRevocationCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[nodeRevocationCacheKey]nodeRevocationCacheEntry
}

// nodeRevocationCacheKey is the index pair. (nodeType, nodeId) is
// the same binding the verifier extracted from the JWT + the same
// signature the resolver takes; using it as the cache key avoids a
// string-concat step.
type nodeRevocationCacheKey struct {
	nodeType string
	nodeId   string
}

type nodeRevocationCacheEntry struct {
	revoked bool
	expires time.Time
}

func newNodeRevocationCache(ttl time.Duration) *nodeRevocationCache {
	return &nodeRevocationCache{
		ttl:     ttl,
		entries: make(map[nodeRevocationCacheKey]nodeRevocationCacheEntry),
	}
}

// lookup consults the cache; on miss / expiry, calls the resolver
// and memoises the result. Concurrent lookups against the same key
// during a cache miss may each call the resolver once (the cache
// uses a read-lock for the fast path + a write-lock for the store).
// For the expected access pattern -- a small number of cluster peers
// each opening a stream every ~30s, plus occasional reconnects --
// this is fine. A singleflight wrapper would be the next step under
// heavier load.
func (c *nodeRevocationCache) lookup(ctx context.Context, nodeType, nodeId string, resolver NodeTokenRevocationResolver) (bool, error) {
	now := time.Now()
	key := nodeRevocationCacheKey{nodeType: nodeType, nodeId: nodeId}
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.revoked, nil
	}
	revoked, err := resolver.IsNodeTokenRevoked(ctx, nodeType, nodeId)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	c.entries[key] = nodeRevocationCacheEntry{
		revoked: revoked,
		expires: now.Add(c.ttl),
	}
	c.mu.Unlock()
	return revoked, nil
}
