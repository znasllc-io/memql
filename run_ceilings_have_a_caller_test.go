package main

import (
	"os"
	"strings"
	"testing"
)

// run_ceilings_have_a_caller_test.go -- memql#5580.
//
// ===========================================================================
// THE DEFECT THIS EXISTS TO PREVENT RECURRING
// ===========================================================================
// component/work.CheckCeilings was written, documented and unit-tested, with
// the dollar-versus-loop split the design record calls for -- and it had NO
// PRODUCTION CALLER. Every reference outside its own package was a test. So a
// goal's costCeiling, tokenBudget, maxModelCalls and wallClockMs were settable
// fields nothing read: an operator who set one to bound a run's model spend
// bounded nothing, and the only real stop was the process-wide rate ceiling
// and circuit breaker at the provider transport, which one runaway run shares
// with every other caller on the node.
//
// It read as covered, which is what made it expensive. The root CLAUDE.md said
// "the per-Plan budget is REPLACED by component/work/budget.go reading the
// RUN's ceilings", and cited a test that passes whether or not anything calls
// the function. A reviewer who consulted the docs found the claim, found the
// test, and found both true.
//
// ===========================================================================
// WHY THREE LINKS AND NOT ONE
// ===========================================================================
// "CheckCeilings has a caller" is not the property. The property is that a
// model call inside a work run is refused when the run is past a ceiling, and
// that rests on a CHAIN -- the decision, the guard that installs it, and the
// seam that asks. Each link can break on its own, silently, and each break
// leaves the other two green:
//
//	the decision   work.CheckCeilings is called from a non-test file outside
//	               component/work. Delete the caller and the function is inert
//	               again, exactly as it was.
//	the wiring     SetRunCeilingGuard is called from a non-test file outside
//	               component/memql. Without it the engine holds a nil guard,
//	               every admit is a no-op, and every unit test still passes
//	               because they install the guard themselves.
//	the seam       component/memql asks the guard before it serves. Remove the
//	               two admit calls and the guard is wired to nothing.
//
// It lives in the root package for TestNoStaleRowAuthzInertClaim's reason: the
// links span four modules, the root package is where a sweep across them is
// cheap, and it runs on every Go change.

// ceilingChainLinks is the chain, as (what to look for, where it must NOT be
// the only place it appears, what breaks if it goes).
var ceilingChainLinks = []struct {
	name string
	// needle is the source text that proves the link.
	needle string
	// excludePrefix is the package that DEFINES the thing; a reference there
	// does not prove a caller.
	excludePrefix string
	// consequence is what a reader sees when this link is the one missing.
	consequence string
}{
	{
		name:          "the decision is called",
		needle:        "CheckCeilings(",
		excludePrefix: "component/work/",
		consequence:   "a run's ceilings are settable fields nothing reads, which is memql#5580 exactly",
	},
	{
		name:          "the guard is installed",
		needle:        "SetRunCeilingGuard(",
		excludePrefix: "component/memql/",
		consequence:   "the engine holds a nil guard, every admit is a no-op, and every unit test still passes because they install a guard themselves",
	},
	{
		name:          "the run's spend is folded",
		needle:        "work.AddCall(",
		excludePrefix: "component/work/",
		consequence:   "nothing counts what a run spent, so every ceiling is compared against zero forever",
	},
}

// TestTheRunCeilingChainHasAProductionCaller fails when any link is test-only.
func TestTheRunCeilingChainHasAProductionCaller(t *testing.T) {
	// trackedGoFiles is dsl_call_quoting_test.go's, shared rather than
	// copied: it already fails on an empty answer, which is the vacuous pass
	// a sweep like this dies of.
	files := trackedGoFiles(t)
	if len(files) < 500 {
		t.Fatalf("only %d tracked .go files; the sweep did not run, so it proved nothing", len(files))
	}

	for _, link := range ceilingChainLinks {
		var callers []string
		for _, rel := range files {
			if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, link.excludePrefix) {
				continue
			}
			b, err := os.ReadFile(rel)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), link.needle) {
				callers = append(callers, rel)
			}
		}
		if len(callers) == 0 {
			t.Errorf("%s: no non-test file outside %s contains %q.\n"+
				"Without it, %s.\n"+
				"If the enforcement moved, point this link at its new spelling rather than deleting it.",
				link.name, link.excludePrefix, link.needle, link.consequence)
		}
	}
}

// TestTheModelSeamAsksTheRunCeilingGuard pins the third link, which is a
// within-package call and so cannot be found by the sweep above.
//
// TWO CALL SITES, AND BOTH ARE LOAD-BEARING. The seam's own `serve` covers
// every provider call and every journal hit; the ai() runtime's covers the
// in-process response caches, which answer ABOVE the seam and would otherwise
// let a run past its loop cap be served warm answers forever.
func TestTheModelSeamAsksTheRunCeilingGuard(t *testing.T) {
	for path, why := range map[string]string{
		"component/memql/model_journal.go": "the seam every provider call and every journal hit passes through",
		"component/memql/ai_runtime.go":    "the ai() runtime, whose exact-hash and semantic caches answer ABOVE the seam",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(b), ".admit(ctx") {
			t.Errorf("%s does not ask the run-ceiling guard, and it is %s. "+
				"A guard nothing asks is the defect memql#5580 was about, one layer down.", path, why)
		}
	}
	// And the charge, without which every ceiling is compared against a spend
	// that never rises.
	for _, path := range []string{"component/memql/model_journal.go", "component/memql/ai_runtime.go"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(b), ".charge(ctx") {
			t.Errorf("%s never charges the run, so its ceilings are compared against a spend that stays at zero", path)
		}
	}
}
