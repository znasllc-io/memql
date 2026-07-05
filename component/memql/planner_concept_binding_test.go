package memql

import (
	"log/slog"
	"os"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestPlannerPlanQueriesBindConcept guards memql#759: the planner
// plan-queries (plansForSpace / allPlans) are each declared
// `query plan <name>` and must bind to v1:planner:plan. (The
// dueTrainAgentRetryPlans / runningTrainAgentPlans plan-queries that
// used to exercise this moved to the product pack with the
// training integration; the pack's tree-load gate covers them there.)
// The bare trailing segment "plan" is
// ambiguous (v1:planner:plan AND v1:harness:plan both end ":plan"), so the
// binding has to disambiguate via the file's `use planner.concepts.{ plan }`
// import. A regression leaves BoundConcept empty and every call fails the
// engine with `concept "" not found in registry` (a 60s log-spam on the
// training poll + the frontend plan queries).
func TestPlannerPlanQueriesBindConcept(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	for _, name := range []string{
		"plansForSpace",
		"allPlans",
	} {
		fn, err := registry.Get(name)
		if err != nil {
			t.Errorf("%s: not registered: %v", name, err)
			continue
		}
		if fn.BoundConcept != "v1:planner:plan" {
			t.Errorf("%s: BoundConcept=%q, want v1:planner:plan", name, fn.BoundConcept)
		}
	}
}

// TestResolveBareConceptName_NamespaceHintDisambiguates is the unit-level
// guard for the disambiguation rule: a trailing segment shared across two
// namespaces resolves with a namespace hint and stays ambiguous without one.
func TestResolveBareConceptName_NamespaceHintDisambiguates(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	resolver := NewConceptResolver(memoryNodes.DefaultRegistry())

	if _, err := resolver.resolveBareConceptName("plan"); err == nil {
		t.Fatal("resolveBareConceptName(\"plan\") with no hint: expected ambiguity error, got nil")
	}
	if id, err := resolver.resolveBareConceptNameWithNamespace("plan", "planner"); err != nil || id != "v1:planner:plan" {
		t.Fatalf("resolveBareConceptNameWithNamespace(\"plan\", \"planner\") = (%q, %v), want (v1:planner:plan, nil)", id, err)
	}
	if id, err := resolver.resolveBareConceptNameWithNamespace("plan", "harness"); err != nil || id != "v1:harness:plan" {
		t.Fatalf("resolveBareConceptNameWithNamespace(\"plan\", \"harness\") = (%q, %v), want (v1:harness:plan, nil)", id, err)
	}
	// An unhelpful hint (matches neither) stays ambiguous.
	if _, err := resolver.resolveBareConceptNameWithNamespace("plan", "nope"); err == nil {
		t.Fatal("resolveBareConceptNameWithNamespace(\"plan\", \"nope\"): expected ambiguity error, got nil")
	}
}
