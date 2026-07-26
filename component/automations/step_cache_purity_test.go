package automations

import (
	"testing"
)

// step_cache_purity_test.go -- memql#2869.
//
// Step.IsCacheable() made StepTypeFunction cacheable BY DEFAULT, on the
// assumption that a "function step" is pure. Nothing checked that.
//
// THE HOLE WAS WIDER THAN THE ISSUE DESCRIBES. Every authored `mutate`
// construct invoked as an automation step compiles to StepTypeFunction, not
// StepTypeMutation -- compileStep has no mutation branch. Measured on the live
// tree: 54 function steps and ZERO mutation steps, so the type the old comment
// named as side-effecting was unused while the type carrying every mutation was
// marked cacheable. Also zero query and zero shape steps, which is why the
// "pure types stay cacheable" half of this change protects nothing real.
//
// AND THE ISSUE'S EXAMPLE IS WRONG, which is worth pinning because a first cut
// of this test enshrined it. #2869 names `logic revokeExpiredDelegations` as "a
// sweep that revokes rows"; its body is `return query
// expiredActiveDelegations(...)` -- a pure READ. The write is a separate step,
// `revokeDelegation`, inside expireDelegations.apply's forEach. Two stale DSL
// comments said otherwise and the issue inherited them; both are corrected in
// this change.
//
// So the live-tree test below asserts on steps that ACTUALLY WRITE, which is
// also what the issue asked for -- "a test that a `logic` step with a mutation
// in its body is not served from cache". Pinning the pure read would have been
// the one shape that is genuinely safe to cache.

// TestFunctionStepIsNotCacheableByDefault is the fix.
func TestFunctionStepIsNotCacheableByDefault(t *testing.T) {
	step := &Step{ID: "s1", Name: "sweep", Type: StepTypeFunction}
	if step.IsCacheable() {
		t.Error("a function step is cacheable by default. A DSL `logic` call compiles to this " +
			"step type, so a side-effecting sweep (revokeExpiredDelegations) can be served from " +
			"cache and SKIPPED for the entry's TTL (memql#2869). Caching must be opt-in here.")
	}
}

// TestFunctionStepCachingIsOptIn covers the Go-level opt-in.
//
// SCOPE, because the PR body first overclaimed this: `cache.enabled = true`
// works on a Step built in GO, and there is no `cache` clause in the DSL
// grammar, so no AUTHOR can reach it. This is not an escape hatch an author
// has; it is the mechanism a future `cache` clause would compile to. Plumbing
// that through StepDef is tracked separately.
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

// TestExplicitDisableStillWinsOverTheTypeDefault pins that an explicit
// `cache.enabled = false` beats the type default for EVERY step type -- including
// the two that are cacheable by default.
//
// (An earlier comment here described a precedence conflict between an explicit
// disable and an explicit enable. There is none: Enabled is a single *bool, so
// the two branches are mutually exclusive by construction.)
func TestExplicitDisableStillWinsOverTheTypeDefault(t *testing.T) {
	no := false
	for _, tt := range []StepType{StepTypeQuery, StepTypeShape, StepTypeFunction} {
		step := &Step{ID: "s1", Name: "x", Type: tt, Cache: &CacheConfig{Enabled: &no}}
		if step.IsCacheable() {
			t.Errorf("%v with cache.enabled=false is cacheable; an explicit disable must always win", tt)
		}
	}
}

// TestNoWritingStepInTheLiveTreeIsCacheable is the end-to-end half, and it is
// the assertion #2869 actually asked for.
//
// It resolves steps from the REAL tree rather than fixtures, because the claim
// is about shipped constructs -- and it names WRITES specifically, not a logic
// that happens to be invoked by a sweep. All three of these were cacheable
// before this change:
//
//	revokeDelegation             a `mutate` -- soft-revokes a delegation row
//	deleteUserHard               a `mutate` -- hard-deletes a user
//	workbenchTeardownDirectory   a builtin  -- rm -rf-class directory teardown
//
// The walk includes OnComplete and OnError, which an earlier version missed.
// No live automation uses them today, so the gap was latent -- but the whole
// design rationale here is "resolve from the real tree so the property cannot
// silently move", and moving a step into an onComplete is precisely such a move.
func TestNoWritingStepInTheLiveTreeIsCacheable(t *testing.T) {
	loader := NewLoader(LoaderOptions{Logger: nil})
	all, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// Named writes that must never be served from cache. Each is a `mutate` or
	// a side-effecting builtin reached as a function step.
	writers := map[string]string{
		"revokeDelegation":           "mutate -- soft-revokes a delegation row",
		"deleteUserHard":             "mutate -- hard-deletes a user",
		"workbenchTeardownDirectory": "builtin -- rm -rf-class directory teardown",
	}

	found := map[string]bool{}
	var walk func(steps []*Step)
	walk = func(steps []*Step) {
		for _, s := range steps {
			if s == nil {
				continue
			}
			if s.Type == StepTypeFunction && s.Function != nil {
				if why, ok := writers[s.Function.Name]; ok {
					found[s.Function.Name] = true
					if s.IsCacheable() {
						t.Errorf("step %q is CACHEABLE (%s). Served from cache it SKIPS the write "+
							"for the entry's TTL (memql#2869). cache=%+v", s.Function.Name, why, s.Cache)
					}
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
		if a == nil {
			continue
		}
		walk(a.Steps)
		walk([]*Step{a.OnComplete})
		walk([]*Step{a.OnError})
	}

	// A t.Skip here would make the whole test vacuous, so it is a FAILURE.
	// These are shipped constructs; if one is renamed or removed, the fixture
	// must be re-pointed at whatever writes in its place, not silently dropped.
	for name, why := range writers {
		if !found[name] {
			t.Errorf("%q (%s) was not found as a function step in the tree. Re-point this test at "+
				"a step that still writes rather than letting it assert on nothing.", name, why)
		}
	}
}
