package automations

import (
	"fmt"
	"testing"
)

// collection_chain_test.go -- collection chains over step results and args
// (#2317 / #2318), evaluated over the run the way a logic step, a return, a
// forEach source and a forEach filter are.

// TestLogicCollectionChain_IntermediateStepThenReturn pins gap 2 (#2317): a
// multi-statement logic body whose intermediate `:=` step is a collection
// chain over a prior step result, followed by a `return active.count()` over
// that bound collection. The query-result Bundle step stands for its node
// list, the chain's value binds as the step value, and the trailing count
// reads it correctly.
func TestLogicCollectionChain_IntermediateStepThenReturn(t *testing.T) {
	e := NewEvaluator()
	// `rows := someActiveQuery()` -> a query-result Bundle.
	e.SetStepResult("rows", &StepResult{
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{"nodes": []any{
				map[string]any{"id": "a", "status": "active"},
				map[string]any{"id": "b", "status": "inactive"},
				map[string]any{"id": "c", "status": "active"},
			}},
		},
	})

	// `active := rows.where(r => r.status == "active")`
	val, err := evalV1(e, `rows.where(r => r.status == "active")`)
	if err != nil {
		t.Fatalf("intermediate chain: %v", err)
	}
	items, ok := val.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("active rows = %#v, want a 2-element []any", val)
	}
	e.SetStepResult("active", &StepResult{Status: "success", Result: val})

	// `return active.count()` over the []any-valued step.
	got, err := evalV1(e, "active.count()")
	if err != nil {
		t.Fatalf("return active.count(): %v", err)
	}
	if fmt.Sprint(got) != "2" {
		t.Errorf("active.count() = %#v, want 2", got)
	}
}

// TestLogicCollectionChain_FullChainReturn pins gap 2 (#2317): a `return`
// expression that is a full collection chain (`rows.where(...).count()`).
func TestLogicCollectionChain_FullChainReturn(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("rows", &StepResult{
		Status: "success",
		Result: []any{
			map[string]any{"id": "a", "role": "admin"},
			map[string]any{"id": "b", "role": "member"},
			map[string]any{"id": "c", "role": "admin"},
		},
	})

	got, err := evalV1(e, `rows.where(r => r.role == "admin").count()`)
	if err != nil {
		t.Fatalf("full chain return: %v", err)
	}
	if fmt.Sprint(got) != "2" {
		t.Errorf("admin count = %#v, want 2", got)
	}
}

// TestLogicCollectionChain_StepAccessorOverBundle: a bare step accessor over
// a query-result Bundle -- the `return X.count()` / `.empty()` / `.first()`
// family -- reads the step's node list.
func TestLogicCollectionChain_StepAccessorOverBundle(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("expiredDelegations", &StepResult{
		Status: "success",
		Result: map[string]any{"Bundle": map[string]any{"nodes": []any{
			map[string]any{"id": "d1"},
			map[string]any{"id": "d2"},
			map[string]any{"id": "d3"},
		}}},
	})

	got, err := evalV1(e, "expiredDelegations.count()")
	if err != nil {
		t.Fatalf("accessor: %v", err)
	}
	if fmt.Sprint(got) != "3" {
		t.Errorf("expiredDelegations.count() = %#v, want 3", got)
	}
}

// TestCollectionChain_SourceOuterArgSubstitution pins gap 3b (#2318): a
// collection chain whose lambda body references an outer `args.X` (not the
// bound element) resolves it against the run's args -- the forEach-source
// position.
func TestCollectionChain_SourceOuterArgSubstitution(t *testing.T) {
	members := []any{
		map[string]any{"name": "alice", "score": 5.0},
		map[string]any{"name": "bob", "score": 1.0},
		map[string]any{"name": "carol", "score": 9.0},
	}
	for threshold, want := range map[float64]int{3.0: 2, 6.0: 1} {
		e := NewEvaluator()
		e.SetCustom("args", map[string]any{"members": members, "threshold": threshold})
		val, err := evalV1(e, `args.members.where(m => m.score > args.threshold)`)
		if err != nil {
			t.Fatalf("outer-arg chain (threshold %v): %v", threshold, err)
		}
		items, ok := val.([]any)
		if !ok || len(items) != want {
			t.Errorf("members over threshold %v = %#v, want %d", threshold, val, want)
		}
	}
}

// TestForEachFilter_ChainAndComparison pins gap 3a (#2318): a forEach filter
// that is a collection chain over the bound item, and one that is a plain
// comparison, both decide over the item.
func TestForEachFilter_ChainAndComparison(t *testing.T) {
	e := NewEvaluator()
	e.SetItem(map[string]any{
		"id":   "x",
		"tags": []any{"vip", "beta"},
	}, "item")

	for cond, want := range map[string]bool{
		`item.tags.any(t => t == "vip")`:  true,
		`item.tags.any(t => t == "gold")`: false,
		`item.id == "x"`:                  true,
	} {
		got, err := evalV1Cond(t, e, cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		if got != want {
			t.Errorf("%s = %v, want %v", cond, got, want)
		}
	}
}

// TestGetStepNodes_CollectionResult pins the GetStepNodes change behind #2317:
// a collection-valued step result (a `:= rows.where(...)` bind) is treated as
// the node list directly, so step accessors over it return correct values
// (was zero before -- GetStepNodes only understood the Bundle envelope).
func TestGetStepNodes_CollectionResult(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("active", &StepResult{
		Status: "success",
		Result: []any{
			map[string]any{"id": "a"},
			map[string]any{"id": "b"},
		},
	})
	nodes, ok := e.GetStepNodes("active")
	if !ok {
		t.Fatalf("GetStepNodes over a []any step result must succeed")
	}
	if len(nodes) != 2 {
		t.Errorf("nodes = %d, want 2", len(nodes))
	}

	// A Bundle-shaped result keeps the envelope-aware path.
	e.SetStepResult("bundled", &StepResult{
		Status: "success",
		Result: map[string]any{"Bundle": map[string]any{"nodes": []any{
			map[string]any{"id": "z"},
		}}},
	})
	if nodes, ok := e.GetStepNodes("bundled"); !ok || len(nodes) != 1 {
		t.Errorf("bundled nodes = (ok=%v len=%d), want (true, 1)", ok, len(nodes))
	}
}
