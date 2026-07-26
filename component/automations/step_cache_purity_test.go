package automations

import (
	"testing"
)

// step_cache_purity_test.go -- memql#2869.
//
// Step.IsCacheable() made StepTypeFunction cacheable BY DEFAULT, on the
// assumption that a "function step" is pure. Nothing checked that, and a DSL
// `logic` call compiles to exactly this step type (logic_runner.go) -- so a
// SIDE-EFFECTING logic was cacheable unless its author remembered
// `cache.enabled = false`.
//
// The live example is `logic revokeExpiredDelegations ( asOf: now )` in
// dsl/identity/automations.memql: a sweep that REVOKES rows, servable from cache
// and therefore skippable for the entry's TTL.
//
// Why it had not bitten, and why that was luck: the cache key carried a
// per-second wall-clock reading, so it churned every second and an entry almost
// never survived to be reused. #2867 removed wall-clock values from the key --
// correctly, because a key changing every nanosecond is a cache that can never
// hit -- which widened the window from under a second to the full TTL. The
// protection was never the classification; it was key churn, and a
// cache-invalidation strategy made of clock granularity narrows a window rather
// than closing it.

// TestFunctionStepIsNotCacheableByDefault is the fix.
func TestFunctionStepIsNotCacheableByDefault(t *testing.T) {
	step := &Step{ID: "s1", Name: "sweep", Type: StepTypeFunction}
	if step.IsCacheable() {
		t.Error("a function step is cacheable by default. A DSL `logic` call compiles to this " +
			"step type, so a side-effecting sweep (revokeExpiredDelegations) can be served from " +
			"cache and SKIPPED for the entry's TTL (memql#2869). Caching must be opt-in here.")
	}
}

// TestFunctionStepCachingIsOptIn keeps the escape hatch working: a genuinely
// pure logic can still be cached, deliberately.
func TestFunctionStepCachingIsOptIn(t *testing.T) {
	yes := true
	step := &Step{ID: "s1", Name: "pure", Type: StepTypeFunction, Cache: &CacheConfig{Enabled: &yes}}
	if !step.IsCacheable() {
		t.Error("`cache.enabled = true` no longer opts a function step INTO caching -- the fix " +
			"must change the DEFAULT, not remove the capability, or an author with a genuinely " +
			"pure logic has no way to cache it")
	}
}

// TestPureStepTypesStayCacheable guards against over-correcting. Queries read
// and shapes project; neither can have a side effect, so making them opt-in
// would cost the cache its entire purpose.
func TestPureStepTypesStayCacheable(t *testing.T) {
	for _, tt := range []StepType{StepTypeQuery, StepTypeShape} {
		step := &Step{ID: "s1", Name: "pure", Type: tt}
		if !step.IsCacheable() {
			t.Errorf("%v is no longer cacheable by default; only the FUNCTION default was meant "+
				"to change (memql#2869)", tt)
		}
	}
}

// TestSideEffectingStepTypesStayUncacheable pins what was already correct, so a
// future edit to the switch cannot widen it by accident.
func TestSideEffectingStepTypesStayUncacheable(t *testing.T) {
	for _, tt := range []StepType{StepTypeMutation, StepTypeWebhook, StepTypeEvent} {
		step := &Step{ID: "s1", Name: "effect", Type: tt}
		if step.IsCacheable() {
			t.Errorf("%v became cacheable by default", tt)
		}
	}
}

// TestExplicitDisableStillWinsOverAnOptIn pins the precedence order: an explicit
// disable must beat an explicit enable is NOT the rule -- enable wins -- but an
// explicit disable must beat the type default for every type. Both directions
// are asserted because the two `if` blocks that implement them are adjacent and
// easy to reorder.
func TestExplicitDisableStillWinsOverTheTypeDefault(t *testing.T) {
	no := false
	for _, tt := range []StepType{StepTypeQuery, StepTypeShape, StepTypeFunction} {
		step := &Step{ID: "s1", Name: "x", Type: tt, Cache: &CacheConfig{Enabled: &no}}
		if step.IsCacheable() {
			t.Errorf("%v with cache.enabled=false is cacheable; an explicit disable must always win", tt)
		}
	}
}

// TestTheLiveSweepLogicIsNotCacheable is the end-to-end half: the actual
// authored automation that motivated the issue must not be cacheable.
//
// It resolves the step from the REAL tree rather than constructing a fixture,
// because the claim being made is about a shipped construct -- and if
// revokeExpiredDelegations ever stops compiling to a function step, a fixture
// test would keep passing while the property it stands for silently moved.
func TestTheLiveSweepLogicIsNotCacheable(t *testing.T) {
	loader := NewLoader(LoaderOptions{Logger: nil})
	all, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	var found int
	var walk func(steps []*Step)
	walk = func(steps []*Step) {
		for _, s := range steps {
			if s == nil {
				continue
			}
			if s.Type == StepTypeFunction && s.Function != nil &&
				s.Function.Name == "revokeExpiredDelegations" {
				found++
				if s.IsCacheable() {
					t.Errorf("the live delegation-revocation sweep step is CACHEABLE. It revokes "+
						"rows; served from cache it skips the sweep for the entry's TTL "+
						"(memql#2869). cache=%+v", s.Cache)
				}
			}
			if s.Parallel != nil {
				walk(s.Parallel.Branches)
			}
			if s.ForEach != nil {
				walk(s.ForEach.Do)
			}
			if s.Switch != nil {
				for _, c := range s.Switch.Cases {
					if c != nil {
						walk(c.Steps)
						walk([]*Step{c.Step})
					}
				}
				if s.Switch.Default != nil {
					walk(s.Switch.Default.Steps)
					walk([]*Step{s.Switch.Default.Step})
				}
			}
		}
	}
	for _, a := range all {
		if a != nil {
			walk(a.Steps)
		}
	}

	if found == 0 {
		t.Skip("revokeExpiredDelegations is not a function step in the tree any more; this test " +
			"stands for a shipped construct, so re-point it rather than deleting it")
	}
}
