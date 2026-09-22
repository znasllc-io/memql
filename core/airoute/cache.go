package airoute

import "strings"

// cache.go -- WHICH CACHE ANSWERED, when one did (memql#5581).
//
// A call that a cache answers is still a call the router resolved: the AI
// runtime's exact-hash key folds in the RESOLVED provider name, so the chain
// walk happens before the lookup and a rule, a policy and a door report exist
// for it. What did NOT happen is a provider call -- no tokens, no bill, no
// latency to speak of.
//
// So the decision record for such a call is written, and it says so here. That
// is what makes a warm page legible: N rows, every one naming the cache, every
// cost a real zero. Before this the row was simply absent, and "the cache is
// working" and "nothing is calling models" looked identical from the ledger.
//
// AN EMPTY VALUE MEANS A PROVIDER ANSWERED, which is what every v1:router:call
// row written before this epic was. It is not "unknown": a row reaches the
// ledger through an observer that wrapped a real provider call, or through the
// cache path that names its kind, and there is no third writer.
const (
	// CacheKindExact is the byte-exact response cache (ai_cache.go), keyed on
	// the template, the resolved provider and the rendered text.
	CacheKindExact = "exact"
	// CacheKindSemantic is the vector nearest-neighbour cache
	// (ai_semantic_cache.go), consulted only for an enabled namespace and
	// only after the exact-hash cache has missed.
	CacheKindSemantic = "semantic"
)

// CacheKinds is the closed set, for validation and for an error message.
func CacheKinds() []string { return []string{CacheKindExact, CacheKindSemantic} }

// ValidCacheKind reports whether kind names one of the caches. The empty
// string is NOT a cache kind -- it is the absence of one, which is a provider
// call -- so this answers false for it.
func ValidCacheKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case CacheKindExact, CacheKindSemantic:
		return true
	}
	return false
}
