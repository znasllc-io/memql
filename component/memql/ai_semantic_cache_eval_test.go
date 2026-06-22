package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// 5.9 FALSE-POSITIVE EVAL HARNESS  --  the core safety artifact.
//
// The wrong-answer risk is the whole concern: a semantic cache that returns a
// cached label for a prompt whose CORRECT label is different is a silent bug.
// This harness measures that risk directly at the chosen threshold and asserts
// it stays under an agreed bound.
//
//   - SHOULD-HIT corpus: near-duplicate phrasings of the same classification
//     input ("thanks" / "thx" / "thank you"). A miss here is a lost saving
//     (acceptable); we report the hit rate but the gate is on false positives.
//   - MUST-NOT-HIT corpus: lexically-close-but-semantically-DIFFERENT prompts
//     ("cancel the meeting" vs "confirm the meeting"). A hit here is a FALSE
//     POSITIVE -- the failure mode we ship the threshold to prevent.
//
// Bound: false-positive rate must be 0% at the conservative default threshold
// over this corpus. (We assert == 0 rather than "< small epsilon": at 0.95
// the curated opposite pairs are well-separated, and a non-zero FP rate here
// would mean the threshold is mis-set for any real enablement.)
//
// Uses the same hermetic conceptEmbedder as ai_semantic_cache_test.go, so the
// eval runs with no network and no DB. The geometry it models (synonyms close,
// opposites far) is the property a real embedding model provides; the harness
// is reusable against the real provider behind a build tag when tuning a
// specific namespace's threshold.
// ----------------------------------------------------------------------------

// evalPair is one corpus entry: a stored "anchor" prompt + the probe phrasings
// that should (or must not) resolve to the anchor's cached label.
type evalPair struct {
	anchor string
	label  string
	probes []string
}

// shouldHitCorpus: near-duplicates of the anchor. Each probe SHOULD return the
// anchor's label.
var shouldHitCorpus = []evalPair{
	{anchor: "thanks", label: "gratitude", probes: []string{"thank you", "thx", "thanks!", "ty", "thank you so much"}},
	{anchor: "yes", label: "affirmation", probes: []string{"yeah", "yep", "ok", "okay", "sounds good"}},
	{anchor: "bye", label: "farewell", probes: []string{"goodbye", "goodnight", "see you later"}},
	{anchor: "hi", label: "greeting", probes: []string{"hello", "hey there"}},
	{anchor: "send it", label: "send", probes: []string{"ship it", "deliver it"}},
}

// mustNotHitCorpus: each probe is lexically close to the anchor but has a
// DIFFERENT correct label. A hit is a false positive.
var mustNotHitCorpus = []evalPair{
	{anchor: "cancel the meeting", label: "cancel", probes: []string{"confirm the meeting", "keep the meeting"}},
	{anchor: "delete it", label: "delete", probes: []string{"keep it", "save it", "retain it"}},
	{anchor: "yes", label: "affirmation", probes: []string{"bye", "cancel"}},
	{anchor: "thanks", label: "gratitude", probes: []string{"send it", "delete it"}},
	{anchor: "confirm the meeting", label: "confirm", probes: []string{"cancel the meeting"}},
}

// runEvalAtThreshold loads each anchor into its own namespace-isolated cache,
// then probes. Returns (hitRate over should-hit, falsePositiveRate over
// must-not-hit).
func runEvalAtThreshold(t *testing.T, threshold float64) (hitRate, fpRate float64, falsePositives []string) {
	t.Helper()
	ctx := context.Background()
	const ns = "eval.ns"

	// SHOULD-HIT: one fresh cache per anchor so cross-anchor neighbours can't
	// interfere -- we're measuring near-duplicate recall in isolation.
	hits, total := 0, 0
	for _, p := range shouldHitCorpus {
		c := newTestSemanticCache(t, ns, threshold)
		c.Store(ctx, ns, p.anchor, p.label)
		for _, probe := range p.probes {
			total++
			if _, ok := c.Lookup(ctx, ns, probe); ok {
				hits++
			}
		}
	}
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}

	// MUST-NOT-HIT: same isolation. A returned value is a false positive.
	fp, totalNeg := 0, 0
	for _, p := range mustNotHitCorpus {
		c := newTestSemanticCache(t, ns, threshold)
		c.Store(ctx, ns, p.anchor, p.label)
		for _, probe := range p.probes {
			totalNeg++
			if val, ok := c.Lookup(ctx, ns, probe); ok {
				fp++
				falsePositives = append(falsePositives, probe+" -> "+anyToString(val))
			}
		}
	}
	if totalNeg > 0 {
		fpRate = float64(fp) / float64(totalNeg)
	}
	return hitRate, fpRate, falsePositives
}

func anyToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "?"
}

// TestSemanticCacheFalsePositiveEval is the gate: at the conservative default
// threshold the FP rate over the opposite-pair corpus must be ZERO, and the
// near-duplicate hit rate must be high (the cache is actually useful).
func TestSemanticCacheFalsePositiveEval(t *testing.T) {
	hitRate, fpRate, fps := runEvalAtThreshold(t, defaultSemanticThreshold)

	t.Logf("eval @ threshold %.3f: near-duplicate hitRate=%.1f%%  falsePositiveRate=%.1f%%",
		defaultSemanticThreshold, hitRate*100, fpRate*100)
	if len(fps) > 0 {
		t.Logf("false positives: %v", fps)
	}

	// THE SAFETY BOUND: no false positives at the conservative threshold.
	require.Equal(t, 0.0, fpRate,
		"false-positive rate must be 0%% at the conservative threshold; got %.1f%% (%v)",
		fpRate*100, fps)

	// Usefulness floor: the cache must actually hit near-duplicates, else it's
	// dead weight. Bound is generous -- the point of the gate is the FP rate.
	require.GreaterOrEqual(t, hitRate, 0.8,
		"near-duplicate hit rate should be high at the conservative threshold; got %.1f%%", hitRate*100)
}

// TestSemanticCacheThresholdSweep documents the FP/hit tradeoff across
// thresholds so a future namespace enablement can pick its threshold from data.
// A LOWER threshold admits more near-duplicates (higher hit rate) at the risk
// of more false positives; this sweep proves the curated opposites stay
// separated across the usable range and that the FP rate never exceeds an
// agreed loose bound even when we relax the threshold substantially.
func TestSemanticCacheThresholdSweep(t *testing.T) {
	// A loose upper bound on FP rate across the whole sweep. Opposites in this
	// corpus are well-separated (different concept axes), so even at a permissive
	// threshold they should not collide.
	const looseFPBound = 0.0

	for _, threshold := range []float64{0.99, 0.97, 0.95, 0.90, 0.80, 0.70} {
		hitRate, fpRate, fps := runEvalAtThreshold(t, threshold)
		t.Logf("threshold %.2f -> hitRate=%.1f%%  fpRate=%.1f%%", threshold, hitRate*100, fpRate*100)
		require.LessOrEqual(t, fpRate, looseFPBound,
			"FP rate exceeded the loose bound at threshold %.2f: %.1f%% (%v)", threshold, fpRate*100, fps)
	}
}

// TestSemanticCacheTokenSavingVsExactHash demonstrates the measured token
// saving vs the 5.8 exact-hash baseline. Same near-duplicate workload through
// each cache; the exact-hash cache misses every near-duplicate (re-paying the
// LLM), the semantic cache hits them (paying once). The delta in
// "provider calls avoided" * prompt-tokens is the saving.
func TestSemanticCacheTokenSavingVsExactHash(t *testing.T) {
	ctx := context.Background()
	const ns = "saving.ns"

	// The workload: a first occurrence ("thanks") + N near-duplicate restatings
	// that an exact-hash cache can never collapse.
	anchor := "thanks"
	nearDupes := []string{"thank you", "thx", "thanks!", "ty", "thank you so much", "cheers"}

	// --- Exact-hash baseline (the 5.8 cache) ---
	exact := newAIResponseCache()
	exactProviderCalls := 0
	exactTokens := 0
	feedExact := func(prompt string) {
		key := buildAICacheKey("classify", "mock", prompt)
		if _, ok := exact.get(key); ok {
			return // hit, no LLM
		}
		// miss -> LLM call + store
		exactProviderCalls++
		exactTokens += estimatePromptTokens(prompt)
		exact.set(key, "LABEL:gratitude", 24*60*60*1e9)
	}
	feedExact(anchor)
	for _, p := range nearDupes {
		feedExact(p)
	}

	// --- Semantic cache (5.9) over the SAME workload ---
	sem := newTestSemanticCache(t, ns, defaultSemanticThreshold)
	semProviderCalls := 0
	semTokens := 0
	feedSem := func(prompt string) {
		if _, ok := sem.Lookup(ctx, ns, prompt); ok {
			return // semantic hit, no LLM
		}
		semProviderCalls++
		semTokens += estimatePromptTokens(prompt)
		sem.Store(ctx, ns, prompt, "LABEL:gratitude")
	}
	feedSem(anchor)
	for _, p := range nearDupes {
		feedSem(p)
	}

	t.Logf("exact-hash baseline: %d provider calls, ~%d prompt tokens", exactProviderCalls, exactTokens)
	t.Logf("semantic cache:      %d provider calls, ~%d prompt tokens", semProviderCalls, semTokens)
	t.Logf("provider calls avoided by semantic cache: %d (~%d tokens saved)",
		exactProviderCalls-semProviderCalls, exactTokens-semTokens)

	// The exact-hash cache misses every distinct near-duplicate string.
	require.Equal(t, 1+len(nearDupes), exactProviderCalls,
		"exact-hash cache must miss every near-duplicate (the 5.9 motivation)")

	// The semantic cache pays only for the anchor; near-duplicates hit.
	require.Equal(t, 1, semProviderCalls,
		"semantic cache should pay the LLM cost once for the whole near-duplicate cluster")

	// Concrete demonstrated saving.
	require.Greater(t, exactProviderCalls-semProviderCalls, 0)
	require.Greater(t, exactTokens-semTokens, 0, "semantic cache must demonstrate a token saving")

	stats := sem.Stats()
	require.Equal(t, int64(len(nearDupes)), stats.Hits)
	require.Greater(t, stats.TokensSavedApprox, int64(0))
}
