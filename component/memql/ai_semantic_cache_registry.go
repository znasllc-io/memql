package memql

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Per-namespace enablement registry for the semantic AI cache.
//
// DISABLED BY DEFAULT. Shipping the primitive does NOT change any AI-call
// behaviour: every classification namespace starts disabled, so the AI path
// is exact-hash/fresh exactly as before. A namespace becomes eligible for
// semantic hits ONLY when it is explicitly enabled here (or via env) with a
// threshold.
//
// THE ENABLEMENT PATH (how to turn a namespace on safely)
//
//  1. Confirm the call type is a CLASSIFICATION / structured-output call with
//     a stable input->label mapping (e.g. message-intent, affirmation,
//     domain-shape). NEVER a free-form generation call (agent prose) -- a
//     near-duplicate prompt there does not imply the same correct output.
//  2. Run the false-positive eval (ai_semantic_cache_eval_test.go) over a
//     corpus of that namespace's near-duplicate pairs (must HIT) and
//     lexically-close-but-opposite pairs (must NOT hit). Pick the lowest
//     threshold whose false-positive rate stays under the agreed bound.
//  3. Register the namespace below (or set MEMQL_SI_SEMANTIC_CACHE_NAMESPACES)
//     with that threshold. Start ABOVE the eval-chosen threshold in production
//     and tune down only with telemetry (SemanticCacheStats.BelowThreshold is
//     the guard-firing counter).
//
// As shipped in 5.9 the registry is EMPTY (enabled for none). The eval proves
// the primitive + threshold separate near-duplicates from opposites; the first
// live enablement is a deliberate, measured follow-up rather than a risk taken
// to satisfy acceptance criteria.

// Candidate namespace identifiers. Kept as named constants so a future
// enablement references the canonical string rather than a stray literal.
const (
	// SemanticNSMessageIntent is cognition's message-intent / affirmation
	// classification (integrations/cognition/message_classifier.go,
	// "messageClassification" prompt). The strongest first-enablement
	// candidate: structured output, stable input->label, high redundancy
	// ("thanks"/"thx"/"thank you"). NOT enabled by default -- enabling it
	// changes a live cognition decision path and must clear the eval +
	// owner sign-off first.
	SemanticNSMessageIntent = "cognition.message_intent"
)

// envSemanticCacheNamespaces lets an operator enable namespaces without a
// rebuild. Format: comma-separated "namespace:threshold[:ttlSeconds]" items,
// e.g. "cognition.message_intent:0.96:86400". A bare "namespace" enables it at
// the default threshold + TTL.
const envSemanticCacheNamespaces = "MEMQL_SI_SEMANTIC_CACHE_NAMESPACES"

// defaultSemanticNamespaces is the compiled-in enablement registry. EMPTY by
// design (enabled for none) -- see the package doc above. To enable a
// namespace at build time, add an entry here, e.g.:
//
//	SemanticNSMessageIntent: {Enabled: true, Threshold: 0.96, TTL: 24 * time.Hour},
//
// after its eval clears the false-positive bound.
func defaultSemanticNamespaces() map[string]SemanticNamespaceConfig {
	return map[string]SemanticNamespaceConfig{}
}

// loadSemanticNamespacesFromEnv merges the compiled-in defaults with any
// env-supplied overrides. Env entries win, so an operator can enable a
// namespace (or retune its threshold) on a running deployment.
func loadSemanticNamespacesFromEnv() map[string]SemanticNamespaceConfig {
	ns := defaultSemanticNamespaces()

	raw := strings.TrimSpace(os.Getenv(envSemanticCacheNamespaces))
	if raw == "" {
		return ns
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		cfg := SemanticNamespaceConfig{Enabled: true}
		if len(parts) > 1 {
			if t, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil && t > 0 {
				cfg.Threshold = t
			}
		}
		if len(parts) > 2 {
			if secs, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil && secs > 0 {
				cfg.TTL = time.Duration(secs) * time.Second
			}
		}
		ns[name] = cfg
	}
	return ns
}
