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
