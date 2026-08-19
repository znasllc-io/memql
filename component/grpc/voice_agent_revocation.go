package memql

import (
	"context"
	"sync"
	"time"
)

// voice_agent_revocation.go closes the gap memql#4111 recorded: a
// compromised class="voice_agent" JWT could not be killed before its
// natural expiry, and that expiry is 90 days
// (identity.DefaultVoiceAgentTokenTTLSeconds).
//
// What was there before: nothing on the verify path read row state.
// VerifyBearer -> verifyJWT is a pure JWKS check with no DB lookup, and
// the session-revocation middleware only covers bearers backed by a
// v1:identity:authSession row -- which this class never creates. So
// soft-deleting the identity row (active=false) recorded the operator's
// INTENT and changed nothing about what the mesh would admit. The only
// working mitigation was rotating the identity signing key, which
// invalidates every JWT of every class at once.
//
// Why this shape rather than verifier.EpochCheck: EpochCheck (memql#106)
// keys on v1:identity:user.revocationEpoch. A voice-agent credential is a
// machine identity with no user row to bump -- the thing an operator
// revokes is the v1:identity:identity[voice_agent_token] row's `active`
// flag. So this mirrors the NODE class's working mechanism
// (component/node/node_token_revocation.go, memql#349) instead: same
// row-state question, same short-TTL cache, same fail-closed posture on a
// lookup error.
//
// The class="voice_agent" JWT's `sub` IS the identity row id
// (identity.VoiceAgentIssueInput.IdentityId -> RegisteredClaims.Subject),
// so the lookup needs nothing the verified token does not already carry.
//
// SERVICE ACCOUNTS ARE DELIBERATELY NOT COVERED HERE, and that is not an
// oversight to fix later. A class="service_account" subject is explicitly
// NOT required to be a persisted row (identity.ServiceAccountIssueInput
// documents this), so there is no row whose state could be read. Its
// answer is the TTL: 1 hour by default
// (identity.DefaultServiceAccountTokenTTLSeconds) versus the voice-agent
// class's 90 days. Revoking one means letting it expire or rotating the
// key, and a one-hour worst case is the reason that is tolerable. Adding a
// row lookup for a class that has no row would fail open on every call.

// VoiceAgentRevocationResolver is the narrow port the interceptor uses to
// ask whether a voice-agent credential's identity row has been revoked.
// Implementations should be fast and cache-friendly -- it is consulted on
// every voice-agent stream open.
//
// identityId is the verified JWT's subject.
//
// Returns (true, nil) when the row exists AND active == false: the
// interceptor rejects the open with codes.PermissionDenied.
// Returns (false, nil) when the row is live OR does not exist. A missing
// row reads as not-revoked, matching the node-class convention: an
// operator-CLI mint that pre-dates row persistence must not be locked out
// by a lookup that finds nothing.
// Returns (false, error) when the lookup itself failed; the interceptor
// fails CLOSED rather than admitting traffic on a half-answered
// revocation check.
type VoiceAgentRevocationResolver interface {
	IsVoiceAgentTokenRevoked(ctx context.Context, identityId string) (bool, error)
}

// VoiceAgentRevocationCheck wraps a resolver with the short-TTL cache the
// interceptor consults. Resolver == nil disables the check entirely, so
// wiring the parameter does not bind a caller to a live store (tests and
// single-node dev builds pass nil).
type VoiceAgentRevocationCheck struct {
	Resolver VoiceAgentRevocationResolver
	// CacheTTL is how long an answer is reused. Zero uses
	// DefaultVoiceAgentRevocationCacheTTL.
	CacheTTL time.Duration
}

// DefaultVoiceAgentRevocationCacheTTL bounds how long a revoked
// credential keeps being admitted after the operator flips the row. 5s,
// matching node.DefaultNodeRevocationCacheTTL -- the same trade (a few
// seconds of propagation lag against DB fan-out when agents reconnect
// together), and the same number so an operator has one figure to
// remember rather than two.
const DefaultVoiceAgentRevocationCacheTTL = 5 * time.Second

// voiceAgentRevocationCache memoises both answers. Caching the REVOKED
// answer is the point as much as caching the live one: once a credential
// is revoked, every subsequent open short-circuits without touching the
// store.
type voiceAgentRevocationCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]voiceAgentRevocationCacheEntry
}

type voiceAgentRevocationCacheEntry struct {
	revoked bool
	expires time.Time
}

func newVoiceAgentRevocationCache(ttl time.Duration) *voiceAgentRevocationCache {
	if ttl <= 0 {
		ttl = DefaultVoiceAgentRevocationCacheTTL
	}
	return &voiceAgentRevocationCache{
		ttl:     ttl,
		entries: make(map[string]voiceAgentRevocationCacheEntry),
	}
}

// lookup consults the cache, falling through to the resolver on a miss or
// an expired entry. Errors are never memoised -- a failed lookup retries
// on the next open rather than pinning a wrong answer for the TTL.
func (c *voiceAgentRevocationCache) lookup(ctx context.Context, identityId string, resolver VoiceAgentRevocationResolver) (bool, error) {
	now := time.Now()
	c.mu.RLock()
	entry, ok := c.entries[identityId]
	c.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.revoked, nil
	}
	revoked, err := resolver.IsVoiceAgentTokenRevoked(ctx, identityId)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	c.entries[identityId] = voiceAgentRevocationCacheEntry{revoked: revoked, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return revoked, nil
}
