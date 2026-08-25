package memql

import (
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The pinned result-cache posture (memql#4533, owner decision D1,
// 2026-08-25).
//
// THE DECISION. Default-on caching at a 60-second backstop, with
// `v1:identity:` denylisted from the default path, IS the product's posture.
// It has been the shipped behaviour since memql#1970, and on 2026-08-25 the
// owner ruled that it stays -- the public docs, which described an opt-in
// model, were the thing that was wrong and were corrected to match.
//
// WHY THE POSTURE NEEDS A TEST AT ALL. It is three constants and a slice.
// Any of them can be changed in a one-line diff that looks like a tuning
// tweak and reads as obviously reasonable in review ("60s is aggressive",
// "the denylist is over-broad"), and NOTHING else in the tree would go red --
// the cache would simply start answering differently, and the failure would
// surface as somebody's stale read weeks later. This is the
// TestOAuthDCRDefaultsToDisabled mold (memql#3719): pin the decision so that
// reversing it must be deliberate, and put the owner and the date in the
// failure message so whoever trips it knows whose call it was.
//
// This is NOT a claim that 60s is optimal forever. It is a claim that
// changing it is a product decision rather than a refactor, and that the
// person changing it should update this test and say so.
func TestResultCachePostureIsPinned(t *testing.T) {
	if defaultResultCacheTTLSeconds != 60 {
		t.Errorf("defaultResultCacheTTLSeconds = %d, want 60.\n"+
			"The default-on 60s backstop for hint-free pure reads is a pinned product posture "+
			"(owner decision D1, 2026-08-25, memql#4533), not a tuning constant. It bounds how "+
			"stale any uncached-by-choice read can be if an invalidation is ever missed, and the "+
			"public language docs state the number. Changing it means changing those docs and "+
			"this pin together, deliberately.", defaultResultCacheTTLSeconds)
	}

	if len(cacheDenylistedConceptPrefixes) != 1 || cacheDenylistedConceptPrefixes[0] != "v1:identity:" {
		t.Errorf("cacheDenylistedConceptPrefixes = %v, want exactly [\"v1:identity:\"].\n"+
			"EMPTYING it would let authn/authz state ride the 60s default: a revoked session, a "+
			"downgraded role or a deleted credential served from cache for up to a minute. "+
			"GROWING it is not free either -- every added prefix silently erodes the hit ratio -- "+
			"so an addition belongs in the reviewed adoption table with a reason, not here as a "+
			"quiet edit (owner decision D1/D4, 2026-08-25, memql#4533).",
			cacheDenylistedConceptPrefixes)
	}
}

// The BEHAVIOURAL half. Pinning the constant alone would let the code path
// drift away from it -- someone could stop consulting
// defaultResultCacheTTLSeconds entirely and the constant test would stay
// green while nothing was cached by default. This asserts that a hint-free
// pure read actually RESOLVES to that TTL through the real policy function.
func TestHintFreeReadResolvesToTheDefaultTTL(t *testing.T) {
	e := &MemQLEngine{}
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: "row-1", Concept: "v1:test:widget"}},
	}

	got := e.cacheTTLForBundle(bundle, nil)
	want := time.Duration(defaultResultCacheTTLSeconds) * time.Second
	if got != want {
		t.Fatalf("a hint-free pure read resolved to TTL %v, want %v.\n"+
			"Default-on caching is the pinned posture; a zero here means hint-free reads stopped "+
			"caching, which is the silent flip this test exists to catch.", got, want)
	}
}

// The three overrides an author has, exercised through the same policy
// function, so "the docs say @nocache opts out" is a checked claim.
func TestExplicitCacheHintsOverrideTheDefault(t *testing.T) {
	e := &MemQLEngine{}
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: "row-1", Concept: "v1:test:widget"}},
	}

	if got := e.cacheTTLForBundle(bundle, map[string]int64{"ttl": 300}); got != 300*time.Second {
		t.Errorf("@cache(300) resolved to %v, want 5m", got)
	}
	// @nocache is compiled to a 0 hint by the parser; 0 must mean NEVER, not
	// "fall through to the default", or the documented opt-out does nothing.
	if got := e.cacheTTLForBundle(bundle, map[string]int64{"ttl": 0}); got != 0 {
		t.Errorf("@nocache / @cache(0) resolved to %v, want 0 -- the documented opt-out does not opt out", got)
	}
}

// MEMQL_CACHE_MAX_TTL is a CEILING, and 0 -- its default -- means NO CLAMP.
// This is the misreading the docs carried in three places until memql#4533:
// an operator who sets it to 0 believing they have disabled caching gets the
// exact opposite, a fully-caching engine with no ceiling at all.
func TestGlobalCacheMaxTTLZeroMeansNoClamp(t *testing.T) {
	e := &MemQLEngine{}
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: "row-1", Concept: "v1:test:widget"}},
	}

	e.config.CacheMaxTTLSeconds = 0
	if got := e.cacheTTLForBundle(bundle, map[string]int64{"ttl": 3600}); got != time.Hour {
		t.Errorf("with MEMQL_CACHE_MAX_TTL=0 a 1h hint resolved to %v, want 1h (0 = no clamp)", got)
	}
	if got := e.cacheTTLForBundle(bundle, nil); got != time.Duration(defaultResultCacheTTLSeconds)*time.Second {
		t.Errorf("with MEMQL_CACHE_MAX_TTL=0 a hint-free read resolved to %v -- 0 does NOT disable caching", got)
	}

	// Set, it clamps both the hint and the default.
	e.config.CacheMaxTTLSeconds = 10
	if got := e.cacheTTLForBundle(bundle, map[string]int64{"ttl": 3600}); got != 10*time.Second {
		t.Errorf("a 1h hint under a 10s ceiling resolved to %v, want 10s", got)
	}
	if got := e.cacheTTLForBundle(bundle, nil); got != 10*time.Second {
		t.Errorf("the 60s default under a 10s ceiling resolved to %v, want 10s", got)
	}
}

// The denylist gates the DEFAULT path only. An author who writes @cache(N)
// on an identity read has made a decision the engine does not second-guess;
// the denylist governs what the engine does on its own. Pinning both
// directions keeps a future "harden the denylist" change from silently
// turning it into a prohibition.
func TestIdentityDenylistGatesOnlyTheDefaultPath(t *testing.T) {
	if !anyConceptCacheDenylisted([]string{"v1:cognition:space", "v1:identity:user"}) {
		t.Error("an identity concept in the dependency set was not detected as denylisted")
	}
	if anyConceptCacheDenylisted([]string{"v1:cognition:space", "v1:worker:registration"}) {
		t.Error("a non-identity dependency set was wrongly reported as denylisted")
	}
	if !isCacheDenylistedConcept("v1:identity:authSession") {
		t.Error("v1:identity:authSession must be denylisted -- session revocation cannot be served stale")
	}
	if isCacheDenylistedConcept("v1:identityish:thing") {
		t.Error("prefix matching is too loose")
	}
}
