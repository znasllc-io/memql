package memql

import "time"

// harness_consolidation.go holds the pure, table-testable decision
// helpers for memory consolidation (#586) -- the episodic -> semantic
// distillation pass driven by the consolidateMemory automation in
// dsl/harness/automations.memql.
//
// These helpers isolate the consolidation DECISIONS that the MemQL DSL
// cannot express (arithmetic on confidence + datetime ages, the
// reinforce-vs-create branch, the prune cutoff) so they can be unit-
// tested without a database -- mirroring stepTransitionAllowed in
// harness_step_validation.go. The scheduled automation + LLM-distill
// path is integration-covered; the math + branching that determines
// "reinforce vs create", "how confidence moves", and "when to prune"
// lives here and is covered by harness_consolidation_test.go.
//
// The Go consolidation handler (registered by the agent-node init
// path) calls these helpers and feeds their results into the harness
// mutations (createHarnessSemanticMemory /
// reinforceHarnessSemanticMemory /
// decayHarnessSemanticMemory / pruneHarnessSemanticMemory
// / advanceHarnessConsolidationCursor).

// Consolidation tuning defaults. The handler reads the matching env
// vars at runtime (MEMQL_HARNESS_CONSOLIDATION_*) and falls back to
// these; the helpers below take the resolved values as parameters so
// they stay pure and testable.
const (
	// ConsolidationDefaultDedupThreshold is the cosine-similarity floor
	// at or above which a freshly distilled belief is treated as a
	// REINFORCEMENT of an existing semanticMemory rather than a new
	// belief. High enough that only genuine restatements collapse; low
	// enough that paraphrases of the same belief still dedup.
	ConsolidationDefaultDedupThreshold = 0.86

	// ConsolidationDefaultDecayDays is how long a belief may go
	// unreinforced before the decay sweep starts lowering its confidence.
	ConsolidationDefaultDecayDays = 30

	// ConsolidationDefaultDecayPerInterval is the confidence lost per
	// DecayDays window of non-reinforcement. Linear in the number of
	// elapsed windows so a long-stale belief decays faster than a fresh
	// one.
	ConsolidationDefaultDecayPerInterval = 0.1

	// ConsolidationDefaultPruneConfidence is the floor at/below which a
	// decayed belief is retired (status -> pruned). Recall + dedup
	// filter status==active, so a pruned belief drops out of both.
	ConsolidationDefaultPruneConfidence = 0.15

	// ConsolidationDefaultReinforceBump is the confidence gained per
	// reinforcement.
	ConsolidationDefaultReinforceBump = 0.15

	consolidationConfidenceCeiling = 1.0
	consolidationConfidenceFloor   = 0.0
)

// clampConsolidationConfidence keeps a confidence value inside [0,1].
func clampConsolidationConfidence(c float64) float64 {
	if c > consolidationConfidenceCeiling {
		return consolidationConfidenceCeiling
	}
	if c < consolidationConfidenceFloor {
		return consolidationConfidenceFloor
	}
	return c
}

// ConsolidationShouldReinforce reports whether a freshly distilled
// belief whose best similarTo match scored bestCosine against an
// existing belief should REINFORCE that match (true) rather than be
// written as a new belief (false). This is the dedup decision that
// backs the acceptance criterion "re-running over the same episodes
// reinforces rather than duplicates". A match only counts when the
// matched belief is still active -- a pruned belief never absorbs a
// reinforcement.
func ConsolidationShouldReinforce(bestCosine, threshold float64, matchActive bool) bool {
	if !matchActive {
		return false
	}
	return bestCosine >= threshold
}

// ConsolidationReinforcedConfidence is the new confidence after a
// reinforcement: the prior confidence plus the bump, clamped to 1. A
// belief seen again is held more strongly.
func ConsolidationReinforcedConfidence(prior, bump float64) float64 {
	return clampConsolidationConfidence(prior + bump)
}

// ConsolidationDecayedConfidence is a belief's confidence after the
// decay sweep, given how long it has gone unreinforced. Confidence is
// reduced by decayPerInterval for every full decayDays window elapsed
// since lastReinforced; a belief still inside its first window is
// untouched. Linear-in-windows so staleness compounds. Result clamped
// to [0,1].
func ConsolidationDecayedConfidence(prior float64, lastReinforced, now time.Time, decayDays int, decayPerInterval float64) float64 {
	if decayDays <= 0 {
		return clampConsolidationConfidence(prior)
	}
	age := now.Sub(lastReinforced)
	if age <= 0 {
		return clampConsolidationConfidence(prior)
	}
	window := time.Duration(decayDays) * 24 * time.Hour
	intervals := int(age / window)
	if intervals <= 0 {
		return clampConsolidationConfidence(prior)
	}
	return clampConsolidationConfidence(prior - float64(intervals)*decayPerInterval)
}

// ConsolidationShouldPrune reports whether a belief whose (already
// decayed) confidence is `confidence` should be retired. Backs the
// acceptance criterion "stale, unreinforced memories decay and are
// pruned after a configurable threshold". Only active beliefs are
// prune candidates -- an already-pruned belief is left alone
// (idempotent sweep).
func ConsolidationShouldPrune(confidence, pruneFloor float64, active bool) bool {
	if !active {
		return false
	}
	return confidence <= pruneFloor
}

// ConsolidationMergeProvenance unions a belief's existing
// sourceEpisodes with the episode ids that contributed to a new
// reinforcement, preserving first-seen order and dropping duplicates.
// This is how provenance GROWS as evidence accumulates without ever
// double-recording an episode -- the audit trail stays complete and
// deduped.
func ConsolidationMergeProvenance(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	add := func(ids []string) {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			merged = append(merged, id)
		}
	}
	add(existing)
	add(incoming)
	return merged
}

// ConsolidationNextWatermark returns the watermark a consolidation run
// should advance to: the maximum createdAt over the batch it processed.
// The next run reads only episodes newer than this, which is what makes
// consolidation incremental (cost bounded by the new-episode delta, not
// total history). When the batch is empty the watermark does not move
// (prior is returned), so an empty run is a no-op.
func ConsolidationNextWatermark(prior time.Time, batchCreatedAt []time.Time) time.Time {
	maxTime := prior
	for _, t := range batchCreatedAt {
		if t.After(maxTime) {
			maxTime = t
		}
	}
	return maxTime
}
