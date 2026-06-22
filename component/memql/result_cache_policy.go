package memql

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// Default-on result caching policy (epic 5, issue 5.6 / memql#1970).
//
// 5.5 proved the @cache(ttl=) mechanism on hot reads; 5.6 graduates pure
// reads to cache BY DEFAULT, without a per-query annotation, relying on
// 5.4's invalidation primitive + the dedicated cache.invalidate.*
// broadcast channel. The default is conservative on three axes:
//
//  1. A backstop TTL (defaultResultCacheTTLSeconds) bounds staleness even
//     in the (impossible-by-construction) event an invalidation is
//     missed -- the cache self-heals within the TTL.
//  2. The existing 5.4 invariant still gates caching: a result is cached
//     ONLY when at least one dependency concept can be named
//     (dependencyConceptsForResult returns >= 1). A result whose
//     dependencies can't be named is never cached, because it could never
//     be invalidated. (Guard lives in engine.go's execute path; 5.6 does
//     not change it.)
//  3. A staleness denylist (cacheDenylistedConceptPrefixes) names concept
//     prefixes that must NEVER default-cache -- auth/identity above all,
//     where even brief staleness is a correctness/security problem. If
//     ANY of a result's dependency concepts is denylisted, the result is
//     not cached.
//
// Opt-out for an individual query stays `@cache(ttl="0")` (an explicit
// "never cache" escape that wins over the default). An explicit positive
// `@cache(ttl=N)` likewise wins over the default (longer or shorter TTL).
const (
	// defaultResultCacheTTLSeconds is the conservative backstop TTL
	// applied to a pure read that carries NO @cache annotation (5.6
	// default-on). 60s balances a useful hit window against bounded
	// worst-case staleness; invalidation (5.4 + the cache.invalidate.*
	// broadcast channel) normally evicts long before this lapses, so the
	// TTL is a safety backstop, not the primary freshness mechanism.
	// Clamped to the global CACHE_MAX_TTL when that is configured.
	defaultResultCacheTTLSeconds = 60
)

// cacheDenylistedConceptPrefixes lists concept-id prefixes whose reads
// must NEVER be cached by the 5.6 default-on path. A result is skipped if
// ANY of its dependency concepts has one of these prefixes.
//
// Why these are on the list:
//   - "v1:identity:" -- authn/authz state (users, identities, sessions,
//     magic links, audit, partition access, delegations). Auth decisions
//     must read live: a revoked session, a downgraded role, or a deleted
//     credential cannot be served from a 60s-stale cache. This is the
//     non-negotiable minimum the issue calls out.
//
// The denylist is a DEFAULT-CACHING guard only. It does NOT forbid an
// explicit per-query @cache(ttl=N) -- if an author deliberately caches a
// denylisted concept they accept the staleness; default-on simply won't
// do it for them. (No identity query currently carries @cache.)
//
// Keep this list short and well-justified; over-denylisting silently
// erodes the cache hit ratio. Documented in
// docs/internal/planning/cache-audit-phase-0.md.
var cacheDenylistedConceptPrefixes = []string{
	"v1:identity:",
}

// isCacheDenylistedConcept reports whether a single concept id falls under
// any denylisted prefix.
func isCacheDenylistedConcept(concept string) bool {
	for _, prefix := range cacheDenylistedConceptPrefixes {
		if strings.HasPrefix(concept, prefix) {
			return true
		}
	}
	return false
}

// anyConceptCacheDenylisted reports whether any concept in deps is
// denylisted for default-on caching.
func anyConceptCacheDenylisted(deps []string) bool {
	for _, c := range deps {
		if isCacheDenylistedConcept(c) {
			return true
		}
	}
	return false
}

// planReferencesActor reports whether a query plan's filter expression
// depends on the calling actor's identity -- either an `*ActorReference`
// comparison value (e.g. `payload.ownerUserId == actor.userId`) or an
// `actor.<field>` left-hand accessor (e.g. `actor.role == "admin"`).
//
// CORRECTNESS (epic 5, issue 5.6 / memql#1970). The result-cache key is
// built from the plan's CANONICAL expression, which renders an actor
// reference identically for every caller (the userId/role is resolved
// later, against a NEW tree, and never reaches the signature). With
// default-on caching this would let two different users share one cache
// entry for an owned query (`payload.ownerUserId == actor.userId`) and
// the second caller would be served the first caller's rows. So when a
// plan references the actor, the resolved actor identity MUST be folded
// into the cache key (see actorCacheKeyComponent). Actor-independent
// plans keep sharing one key across callers (the common, safe case).
//
// Spec references are expanded so a context-spec that gates on
// `actor.<field>` (e.g. requiresAdmin on actor.role) is detected too; a
// missing spec registry conservatively treats the spec as
// actor-dependent (fold the actor identity rather than risk a collision).
func (e *MemQLEngine) planReferencesActor(expr ExpressionNode) bool {
	switch node := expr.(type) {
	case nil:
		return false
	case *LogicalExpression:
		return e.planReferencesActor(node.Left) || e.planReferencesActor(node.Right)
	case *RelationshipExpression:
		return e.planReferencesActor(node.Target)
	case *SortExpression:
		return e.planReferencesActor(node.Target)
	case *SelectExpression:
		return e.planReferencesActor(node.Target)
	case *CountExpression:
		return e.planReferencesActor(node.Target)
	case *SpecReferenceExpression:
		if e == nil || e.specs == nil {
			// Can't expand the spec to inspect it -- fold the actor in to
			// stay safe (never under-key a possibly actor-dependent read).
			return true
		}
		spec, err := e.specs.Get(node.Name)
		if err != nil || spec == nil {
			return true
		}
		return e.planReferencesActor(spec.Expr)
	case *ComparisonExpression:
		if node == nil {
			return false
		}
		// RHS: `payload.X == actor.Y` carries an *ActorReference value.
		if _, ok := node.Value.(*ActorReference); ok {
			return true
		}
		// LHS: `actor.<field> <op> <value>`.
		if len(node.Field.Parts) >= 2 &&
			strings.EqualFold(strings.TrimSpace(node.Field.Parts[0]), "actor") {
			return true
		}
		return false
	default:
		return false
	}
}

// actorCacheKeyComponent returns the cache-key fragment identifying the
// calling actor, for folding into the result-cache key of an
// actor-dependent plan (planReferencesActor). It pins the resolved
// identity (UserId + Role + IdentityId) so two different callers can
// never share a cached result for an owned/role-gated query. Returns a
// stable "anon" sentinel when no AccessContext is present (a single
// shared bucket for unauthenticated reads, which carry no actor rows).
func actorCacheKeyComponent(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return "anon"
	}
	// IdentityId distinguishes two credentials of the same user (PAT vs
	// session vs delegation) in case a query ever gates on it; UserId +
	// Role cover the owned/role-gated cases.
	return ac.UserId + "|" + string(ac.Role) + "|" + ac.IdentityId
}
