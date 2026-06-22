package memql

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// result_cache_default_on_test.go covers the 5.6 default-on caching
// policy (memql#1970): a pure read caches WITHOUT a @cache annotation,
// the @cache(ttl=0) / @nocache opt-out still wins, the identity denylist
// is enforced at the cache-set site, and the cross-user cache-key folds
// the actor identity so an owned query can't leak rows across callers.

func TestCacheTTLForBundle_DefaultOnWithoutHints(t *testing.T) {
	e := &MemQLEngine{}
	// No hints (no @cache annotation): default-on returns the backstop TTL.
	ttl := e.cacheTTLForBundle(&memqlv1.GraphBundle{}, nil)
	want := time.Duration(defaultResultCacheTTLSeconds) * time.Second
	if ttl != want {
		t.Fatalf("default-on TTL = %v, want %v", ttl, want)
	}
}

func TestCacheTTLForBundle_ExplicitHintWinsOverDefault(t *testing.T) {
	e := &MemQLEngine{}
	// A positive explicit hint overrides the default (here: shorter).
	ttl := e.cacheTTLForBundle(&memqlv1.GraphBundle{}, map[string]int64{"v1:x:y": 5})
	if ttl != 5*time.Second {
		t.Fatalf("explicit @cache(ttl=5) TTL = %v, want 5s", ttl)
	}
	// A longer explicit hint likewise wins.
	ttl = e.cacheTTLForBundle(&memqlv1.GraphBundle{}, map[string]int64{"v1:x:y": 600})
	if ttl != 600*time.Second {
		t.Fatalf("explicit @cache(ttl=600) TTL = %v, want 600s", ttl)
	}
}

func TestCacheTTLForBundle_OptOutZeroNeverCaches(t *testing.T) {
	e := &MemQLEngine{}
	// @cache(ttl=0) / @nocache stamps a 0 hint => never cache, even under
	// default-on.
	ttl := e.cacheTTLForBundle(&memqlv1.GraphBundle{}, map[string]int64{"v1:x:y": 0})
	if ttl != 0 {
		t.Fatalf("opt-out (0 hint) TTL = %v, want 0", ttl)
	}
}

func TestCacheTTLForBundle_DefaultClampedToGlobalMax(t *testing.T) {
	e := &MemQLEngine{config: engineConfig{CacheMaxTTLSeconds: 10}}
	// Default (60s) clamps down to the configured global max (10s).
	ttl := e.cacheTTLForBundle(&memqlv1.GraphBundle{}, nil)
	if ttl != 10*time.Second {
		t.Fatalf("default-on TTL with global max 10s = %v, want 10s", ttl)
	}
}

func TestCacheDenylist_IdentityNeverDefaultCached(t *testing.T) {
	if !isCacheDenylistedConcept("v1:identity:user") {
		t.Error("v1:identity:user must be denylisted (auth must read live)")
	}
	if !isCacheDenylistedConcept("v1:identity:authSession") {
		t.Error("v1:identity:authSession must be denylisted")
	}
	if isCacheDenylistedConcept("v1:cognition:space") {
		t.Error("v1:cognition:space must NOT be denylisted (it is a normal cacheable read)")
	}
	// A dependency set containing any identity concept is denylisted.
	if !anyConceptCacheDenylisted([]string{"v1:cognition:space", "v1:identity:user"}) {
		t.Error("a dep set containing an identity concept must be denylisted")
	}
	if anyConceptCacheDenylisted([]string{"v1:cognition:space", "v1:agents:agent"}) {
		t.Error("a dep set of non-identity concepts must NOT be denylisted")
	}
}

func ownerComparison() *ComparisonExpression {
	// payload.ownerUserId == actor.userId
	return &ComparisonExpression{
		Field:    FieldReference{Raw: "payload.ownerUserId", Parts: []string{"payload", "ownerUserId"}},
		Operator: OpEq,
		Value:    &ActorReference{Path: "userId"},
	}
}

func TestPlanReferencesActor(t *testing.T) {
	e := &MemQLEngine{}

	// Owned query: payload.ownerUserId == actor.userId -> actor-dependent.
	if !e.planReferencesActor(ownerComparison()) {
		t.Error("payload.ownerUserId == actor.userId must be detected as actor-dependent")
	}

	// actor.role == "admin" (LHS accessor) -> actor-dependent.
	roleCmp := &ComparisonExpression{
		Field:    FieldReference{Raw: "actor.role", Parts: []string{"actor", "role"}},
		Operator: OpEq,
		Value:    "admin",
	}
	if !e.planReferencesActor(roleCmp) {
		t.Error("actor.role == \"admin\" must be detected as actor-dependent")
	}

	// Nested under directives + AND -> still detected.
	nested := &SortExpression{Target: &LogicalExpression{
		Op:    LogicalAnd,
		Left:  conceptCmp("v1:cognition:space"),
		Right: ownerComparison(),
	}}
	if !e.planReferencesActor(nested) {
		t.Error("actor reference nested under directives must be detected")
	}

	// A plain concept/payload filter with no actor reference -> NOT
	// actor-dependent (shared cache key across callers).
	plain := &LogicalExpression{
		Op:   LogicalAnd,
		Left: conceptCmp("v1:agents:skill"),
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.active", Parts: []string{"payload", "active"}},
			Operator: OpEq,
			Value:    true,
		},
	}
	if e.planReferencesActor(plain) {
		t.Error("a concept/payload-only filter must NOT be actor-dependent")
	}
}

func TestActorCacheKeyComponent_DistinguishesCallers(t *testing.T) {
	ctxA := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-A", Role: auth.RoleWriter, IdentityId: "id-A",
	})
	ctxB := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-B", Role: auth.RoleWriter, IdentityId: "id-B",
	})

	keyA := actorCacheKeyComponent(ctxA)
	keyB := actorCacheKeyComponent(ctxB)
	if keyA == keyB {
		t.Fatalf("two different callers produced the SAME actor cache component %q -- owned reads would collide cross-user", keyA)
	}
	// Same caller -> stable key (cache hits work within a caller).
	if actorCacheKeyComponent(ctxA) != keyA {
		t.Error("actor cache component must be stable for the same caller")
	}
	// No access context -> anon sentinel (one shared bucket, no actor rows).
	if got := actorCacheKeyComponent(context.Background()); got != "anon" {
		t.Errorf("no-access actor component = %q, want \"anon\"", got)
	}
}
