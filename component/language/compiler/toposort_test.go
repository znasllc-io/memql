package compiler

import (
	"strings"
	"testing"
)

func TestTopoSortSteps_NoDeps(t *testing.T) {
	order := []string{"a", "b", "c"}
	deps := map[string]map[string]struct{}{
		"a": {},
		"b": {},
		"c": {},
	}
	sorted, err := topoSortSteps("test", order, deps)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	if strings.Join(sorted, ",") != "a,b,c" {
		t.Errorf("source order not preserved: got %v", sorted)
	}
}

func TestTopoSortSteps_ForwardReferenceReorders(t *testing.T) {
	// Step A references B, but A appears first in source.
	// Toposort should emit B before A.
	order := []string{"a", "b"}
	deps := map[string]map[string]struct{}{
		"a": {"b": {}},
		"b": {},
	}
	sorted, err := topoSortSteps("test", order, deps)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	if strings.Join(sorted, ",") != "b,a" {
		t.Errorf("want b,a got %v", sorted)
	}
}

func TestTopoSortSteps_DiamondDeps(t *testing.T) {
	// D depends on B and C; B and C depend on A.
	// Valid order: A, B, C, D (or A, C, B, D).
	order := []string{"d", "b", "c", "a"}
	deps := map[string]map[string]struct{}{
		"a": {},
		"b": {"a": {}},
		"c": {"a": {}},
		"d": {"b": {}, "c": {}},
	}
	sorted, err := topoSortSteps("test", order, deps)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	// A must come before B and C; D must come last.
	idx := map[string]int{}
	for i, id := range sorted {
		idx[id] = i
	}
	if idx["a"] > idx["b"] || idx["a"] > idx["c"] {
		t.Errorf("A must precede B and C: %v", sorted)
	}
	if idx["b"] > idx["d"] || idx["c"] > idx["d"] {
		t.Errorf("B and C must precede D: %v", sorted)
	}
}

func TestTopoSortSteps_CycleDetected(t *testing.T) {
	order := []string{"a", "b"}
	deps := map[string]map[string]struct{}{
		"a": {"b": {}},
		"b": {"a": {}},
	}
	_, err := topoSortSteps("test", order, deps)
	if err == nil {
		t.Fatalf("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle: %v", err)
	}
}

func TestTopoSortSteps_PreservesSourceOrderForIndependentSteps(t *testing.T) {
	// A, B, C are independent; D depends on A.
	// Expected: A, B, C, D (A moves before D, others stay in order).
	order := []string{"b", "c", "d", "a"}
	deps := map[string]map[string]struct{}{
		"a": {},
		"b": {},
		"c": {},
		"d": {"a": {}},
	}
	sorted, err := topoSortSteps("test", order, deps)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	idx := map[string]int{}
	for i, id := range sorted {
		idx[id] = i
	}
	// B and C should maintain their relative order.
	if idx["b"] > idx["c"] {
		t.Errorf("B should precede C (source order): %v", sorted)
	}
	// A must precede D.
	if idx["a"] > idx["d"] {
		t.Errorf("A must precede D: %v", sorted)
	}
}

// The order is a STABLE topological sort: of the steps ready to run, the one
// written first runs first. These pin exact orders, which the relative-order
// checks above cannot: a breadth-first sort satisfies every one of them.

// TestTopoSortSteps_SourceOrderWhenDependenciesPointBack is forge's
// routeRequest: `advance` reads `steps.decide`, `persistRouted` reads neither.
// The source is already in dependency order, so it is the order. A
// breadth-first sort ran persistRouted (ready in the first round) ahead of
// advance (ready in the second) -- the reverse of what was written
// (memql#5367).
func TestTopoSortSteps_SourceOrderWhenDependenciesPointBack(t *testing.T) {
	order := []string{"decide", "advance", "persistRouted"}
	deps := map[string]map[string]struct{}{
		"decide":        {},
		"advance":       {"decide": {}},
		"persistRouted": {},
	}
	sorted, err := topoSortSteps("routeRequest", order, deps)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	if got := strings.Join(sorted, ","); got != "decide,advance,persistRouted" {
		t.Errorf("got %s, want the source order decide,advance,persistRouted", got)
	}
}

// TestTopoSortSteps_StepsReleasedTogetherKeepSourceOrder: several steps that
// become ready when the same provider runs keep their source order. A
// breadth-first sort released them in the iteration order of a Go map, so
// the order differed between runs; the loop gives that a chance to show.
func TestTopoSortSteps_StepsReleasedTogetherKeepSourceOrder(t *testing.T) {
	order := []string{"a", "b", "c", "d", "e"}
	deps := map[string]map[string]struct{}{
		"a": {},
		"b": {"a": {}},
		"c": {"a": {}},
		"d": {"a": {}},
		"e": {"a": {}},
	}
	for i := 0; i < 100; i++ {
		sorted, err := topoSortSteps("test", order, deps)
		if err != nil {
			t.Fatalf("topoSort: %v", err)
		}
		if got := strings.Join(sorted, ","); got != "a,b,c,d,e" {
			t.Fatalf("run %d: got %s, want a,b,c,d,e", i, got)
		}
	}
}

// TestTopoSortSteps_ForwardReferenceMovesOnlyTheProvider: a step that reads a
// step written after it pulls that provider ahead of itself, and nothing else
// moves.
func TestTopoSortSteps_ForwardReferenceMovesOnlyTheProvider(t *testing.T) {
	order := []string{"x", "a", "b", "y"}
	deps := map[string]map[string]struct{}{
		"x": {},
		"a": {"b": {}},
		"b": {},
		"y": {},
	}
	sorted, err := topoSortSteps("test", order, deps)
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	if got := strings.Join(sorted, ","); got != "x,b,a,y" {
		t.Errorf("got %s, want x,b,a,y: b moves ahead of a, and y stays last", got)
	}
}
