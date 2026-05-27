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

// revocation.go implements the per-NodeService.Stream-open revocation
// check that closes the last operational gap memql#343 set out to
// fill. NodeClassStreamInterceptor (in auth.go) handles JWT verification
// + class-pin + (nodeId, nodeType) binding extraction. This file layers
// the row-state check on top: after the JWT is valid + the class is
// right + the binding is extracted, look the row up and reject when
// it's been revoked.
//
// Why a separate interceptor variant: the JWT validation path is
// JWKS-only (no store reads) and we want to keep it that way for the
// no-auth single-node dev case + for any future bff-side fast path
// that doesn't need persistence. NodeClassStreamInterceptorWithRevocation
// is opt-in -- callers that wire it accept the per-stream-open store
// read; callers that don't get the legacy pre-#343 behavior.

// NodeTokenRevocationResolver is the narrow port the interceptor
// uses to check whether a node token has been revoked. Implementations
// should be fast + cache-friendly -- the function is called on every
// NodeService.Stream open from every cluster peer.
//
// Returns (true, nil) when the token is revoked (verifier rejects
// stream open with codes.PermissionDenied).
// Returns (false, nil) when the token is live (verifier proceeds).
// Returns (false, error) when the lookup itself failed; the interceptor
// logs and short-circuits to PermissionDenied to avoid admitting traffic
// on a partially-failed revocation check.
type NodeTokenRevocationResolver interface {
	IsNodeTokenRevoked(ctx context.Context, identityId string) (bool, error)
}

// NodeRevocationCheck wraps a resolver with the per-call cache the
// interceptor consults. CacheTTL controls how long a "not revoked"
// answer is reused before re-asking the resolver; the default of 5s
// trades a small revocation-propagation lag for protection against
// DB-saturating fan-out when many nodes restart concurrently.
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
const DefaultNodeRevocationCacheTTL = 5 * time.Second

// NodeClassStreamInterceptorWithRevocation is NodeClassStreamInterceptor
// plus the memql#343 revocation gate. When check == nil or
// check.Resolver == nil, it behaves identically to NodeClassStreamInterceptor.
// When wired, every stream open looks up the node token's identity row
// (keyed by the canonical id computed from the JWT's NodeId + NodeType
// claims) and rejects when the row carries revokedAt or active=false.
//
// The lookup goes through a short-TTL in-process cache to keep the
// stream-open path fast under concurrent peer reconnects. Cache key
// is the canonical IdentityId; cache value is "revoked-or-not at
// time of last lookup". The cache does NOT memoise errors -- a failed
// resolver call always retries on the next stream open.
func NodeClassStreamInterceptorWithRevocation(v *verifier.Verifier, check *NodeRevocationCheck, logger *slog.Logger) grpc.StreamServerInterceptor {
	base := NodeClassStreamInterceptor(v, logger)
	if check == nil || check.Resolver == nil {
		// No revocation gate wired -- fall back to base behaviour.
		return base
	}
	ttl := check.CacheTTL
	if ttl <= 0 {
		ttl = DefaultNodeRevocationCacheTTL
	}
	cache := newRevocationCache(ttl)
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// Two-stage interceptor: let the base verifier do its work
		// first (token extraction + JWT verify + class pin + binding
		// extraction), then add the revocation check inside the
		// downstream handler.
		return base(srv, ss, info, func(srv interface{}, wrapped grpc.ServerStream) error {
			ctx := wrapped.Context()
			nodeId, nodeType, ok := NodeBindingFromContext(ctx)
			if !ok {
				// Base interceptor must have run and stamped the
				// binding -- if it didn't, something is wrong with
				// the verifier pipeline. Fail closed.
				if logger != nil {
					logger.Warn("node revocation: binding missing from context (verifier pipeline broken?)",
						"method", info.FullMethod)
				}
				return status.Error(codes.Internal, "node binding missing")
			}
			identityId := canonicalNodeIdentityId(nodeType, nodeId)

			revoked, err := cache.lookup(ctx, identityId, check.Resolver)
			if err != nil {
				if logger != nil {
					logger.Warn("node revocation: resolver lookup failed (rejecting open)",
						"identity_id", identityId,
						"method", info.FullMethod,
						"error", err)
				}
				return status.Error(codes.Unauthenticated, "node token revocation check failed")
			}
			if revoked {
				if logger != nil {
					logger.Info("node revocation: rejected revoked token",
						"identity_id", identityId,
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

// canonicalNodeIdentityId mirrors nodetoken.CanonicalIdentityIdFor but
// is duplicated here to avoid a node -> nodetoken import (the verifier
// + node packages live upstream of the nodetoken store in the
// dependency graph; pulling nodetoken in would invert that).
func canonicalNodeIdentityId(nodeType, nodeId string) string {
	return "v1:identity:identity:node:" + nodeType + ":" + nodeId
}

// revocationCache is a single-key-per-id sync.Map style cache with TTL.
// Holds positive ("not revoked") AND negative ("revoked") answers --
// once a token is revoked, every subsequent open from that node hits
// the cache and short-circuits; the operator-CLI revoke click trickles
// to all peers within ttl.
type revocationCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]revocationCacheEntry
}

type revocationCacheEntry struct {
	revoked bool
	expires time.Time
}

func newRevocationCache(ttl time.Duration) *revocationCache {
	return &revocationCache{
		ttl:     ttl,
		entries: make(map[string]revocationCacheEntry),
	}
}

// lookup consults the cache; on miss / expiry, calls the resolver
// and memoises the result. Concurrent lookups against the same id
// during a cache miss may each call the resolver once (the cache
// uses a read-lock for the fast path + a write-lock for the store);
// for the expected access pattern (a few nodes opening streams to
// the BFF every ~30s, plus occasional reconnects) this is fine.
// A singleflight wrapper would be the next step under heavier load.
func (c *revocationCache) lookup(ctx context.Context, identityId string, resolver NodeTokenRevocationResolver) (bool, error) {
	now := time.Now()
	c.mu.RLock()
	entry, ok := c.entries[identityId]
	c.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.revoked, nil
	}
	revoked, err := resolver.IsNodeTokenRevoked(ctx, identityId)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	c.entries[identityId] = revocationCacheEntry{
		revoked: revoked,
		expires: now.Add(c.ttl),
	}
	c.mu.Unlock()
	return revoked, nil
}
