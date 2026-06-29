package automations

import "testing"

// TestLogicCollectionChain_IntermediateStepThenReturn pins gap 2 (#2317): a
// multi-statement logic body whose intermediate `:=` step is a collection
// chain over a prior step result, followed by a `return active.count()` over
// that bound collection. The intermediate chain evaluates in-memory (the base
// receiver -- a query-result Bundle step -- is unwrapped to its node slice),
// the result binds as the step value, and the trailing step-accessor count
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

	// `active := rows.where(r => r.status == "active")` -- the intermediate
	// chain RHS short-circuit binds a []any of the two active rows.
	val, handled, err := tryEvaluateCollectionChainLocally(`rows.where(r => r.status == "active")`, e)
	if err != nil {
		t.Fatalf("intermediate chain: %v", err)
	}
	if !handled {
		t.Fatalf("intermediate chain `rows.where(...)` must be handled by the collection evaluator")
	}
	items, ok := val.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("active rows = %#v, want a 2-element []any", val)
	}
	e.SetStepResult("active", &StepResult{Status: "success", Result: val})

	// `return active.count()` -- a step-result accessor over the []any-valued
	// step. GetStepNodes now treats a collection step result as the node list,
	// so the count is correct (was 0 before the #2317 fix).
	got, handled, err := EvaluateLocalExpr("active.count()", e)
	if err != nil {
		t.Fatalf("return active.count(): %v", err)
	}
	if !handled {
		t.Fatalf("`active.count()` must resolve locally")
	}
	if got != 2 {
		t.Errorf("active.count() = %#v, want 2", got)
	}
}

// TestLogicCollectionChain_FullChainReturn pins gap 2 (#2317): a `return`
// expression that is a full collection chain (`rows.where(...).count()`) --
// which the single step-method short-circuit does not handle -- resolves
// through the collection evaluator via EvaluateLocalExpr.
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

	got, handled, err := EvaluateLocalExpr(`rows.where(r => r.role == "admin").count()`, e)
	if err != nil {
		t.Fatalf("full chain return: %v", err)
	}
	if !handled {
		t.Fatalf("full chain `rows.where(...).count()` must resolve locally")
	}
	if got != 2 {
		t.Errorf("admin count = %#v, want 2", got)
	}
}

// TestLogicCollectionChain_LegacyStepAccessorUnchanged pins that a bare
// single step-method accessor over a query-result Bundle keeps its legacy
// resolution (NOT routed into the collection evaluator), so #2317 does not
// regress the `return X.count()` / `.empty()` / `.first()` family.
func TestLogicCollectionChain_LegacyStepAccessorUnchanged(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("expiredDelegations", &StepResult{
		Status: "success",
		Result: map[string]any{"Bundle": map[string]any{"nodes": []any{
			map[string]any{"id": "d1"},
			map[string]any{"id": "d2"},
			map[string]any{"id": "d3"},
		}}},
	})

	// isGenuineCollectionChain rejects a bare accessor, so it never reaches the
	// new collection branch.
	if isGenuineCollectionChain("expiredDelegations.count()") {
		t.Fatalf("a bare step accessor must not be treated as a genuine collection chain")
	}
	got, handled, err := EvaluateLocalExpr("expiredDelegations.count()", e)
	if err != nil {
		t.Fatalf("legacy accessor: %v", err)
	}
	if !handled || got != 3 {
		t.Errorf("expiredDelegations.count() = (handled=%v val=%#v), want (true, 3)", handled, got)
	}
}

// TestCollectionChain_SourceOuterArgSubstitution pins gap 3b (#2318): a
// collection chain whose lambda body references an outer `args.X` (not the
// bound element) resolves it against the caller args threaded into the
// in-memory evaluator. This is the forEach-source resolution path
// (EvaluateStepReference), so it covers both the source and the threading.
func TestCollectionChain_SourceOuterArgSubstitution(t *testing.T) {
	e := NewEvaluator()
	e.SetCustom("args", map[string]any{
		"members": []any{
			map[string]any{"name": "alice", "score": 5.0},
			map[string]any{"name": "bob", "score": 1.0},
			map[string]any{"name": "carol", "score": 9.0},
		},
		"threshold": 3.0,
	})

	val, err := e.EvaluateStepReference(`args.members.where(m => m.score > args.threshold)`)
	if err != nil {
		t.Fatalf("outer-arg chain: %v", err)
	}
	items, ok := val.([]any)
	if !ok {
		t.Fatalf("chain resolved to %T, want []any", val)
	}
	if len(items) != 2 {
		t.Fatalf("members over threshold = %d, want 2 (alice, carol)", len(items))
	}

	// Sanity: the same chain with a HIGHER threshold leaves only carol.
	e.SetCustom("args", map[string]any{
		"members":   e.custom["args"].(map[string]any)["members"],
		"threshold": 6.0,
	})
	val, err = e.EvaluateStepReference(`args.members.where(m => m.score > args.threshold)`)
	if err != nil {
		t.Fatalf("outer-arg chain (raised threshold): %v", err)
	}
	if items, ok := val.([]any); !ok || len(items) != 1 {
		t.Errorf("members over raised threshold = %#v, want 1 (carol)", val)
	}
}

// TestEvaluateForEachFilter_ChainAndLegacy pins gap 3a (#2318): a forEach
// filter that is a genuine collection chain over the bound item evaluates
// through the in-memory surface, while a plain string-condition filter keeps
// the legacy EvaluateCondition path.
func TestEvaluateForEachFilter_ChainAndLegacy(t *testing.T) {
	e := NewEvaluator()
	e.SetItem(map[string]any{
		"id":   "x",
		"tags": []any{"vip", "beta"},
	}, "item")

	// Collection-chain filter: item has a "vip" tag.
	match, err := e.EvaluateForEachFilter(`item.tags.any(t => t == "vip")`)
	if err != nil {
		t.Fatalf("chain filter: %v", err)
	}
	if !match {
		t.Errorf("item.tags.any(t => t == \"vip\") = false, want true")
	}

	// Same chain, a tag the item does NOT carry.
	match, err = e.EvaluateForEachFilter(`item.tags.any(t => t == "gold")`)
	if err != nil {
		t.Fatalf("chain filter (absent tag): %v", err)
	}
	if match {
		t.Errorf("item.tags.any(t => t == \"gold\") = true, want false")
	}

	// Legacy string-condition filter still works unchanged.
	match, err = e.EvaluateForEachFilter(`item.id == "x"`)
	if err != nil {
		t.Fatalf("legacy condition filter: %v", err)
	}
	if !match {
		t.Errorf("legacy `item.id == \"x\"` filter = false, want true")
	}
}

// TestIsGenuineCollectionChain pins the gate that keeps the #2317/#2318
// short-circuits from hijacking legacy single step-result accessors: only a
// lambda or a non-accessor collection operator counts as genuine.
func TestIsGenuineCollectionChain(t *testing.T) {
	genuine := []string{
		`rows.where(r => r.active)`,
		`args.members.select(m => m.id)`,
		`item.tags.any(t => t == "vip")`,
		`xs.orderBy(x => x.n).take(3)`,
		`xs.contains("y")`,
	}
	for _, s := range genuine {
		if !isGenuineCollectionChain(s) {
			t.Errorf("isGenuineCollectionChain(%q) = false, want true", s)
		}
	}
	notGenuine := []string{
		`rows.count()`,
		`existing.first()`,
		`rows.empty()`,
		`rows.last()`,
		`rows.nodes()`,
		`coalesce(a, b)`,
		`item.status == "active"`,
		``,
	}
	for _, s := range notGenuine {
		if isGenuineCollectionChain(s) {
			t.Errorf("isGenuineCollectionChain(%q) = true, want false", s)
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
