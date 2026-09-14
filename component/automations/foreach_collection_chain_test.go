package automations

import "testing"

// TestForEachSourceCollectionChain locks Story 4 (#2302 / ADR §2.2)
// forEach support: a forEach source that is a collection-method chain
// evaluates over the run -- the chain's base receiver (a step result here)
// resolves and the chain runs over it.
func TestForEachSourceCollectionChain(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("loadUsers", &StepResult{
		Result: []any{
			map[string]any{"name": "alice", "active": true},
			map[string]any{"name": "bob", "active": false},
			map[string]any{"name": "carol", "active": true},
		},
	})

	// where(active) over the step result -> 2 active users.
	val, err := evalV1(e, `loadUsers.result.where(u => u.active)`)
	if err != nil {
		t.Fatalf("evaluate chain source: %v", err)
	}
	items, ok := val.([]any)
	if !ok {
		t.Fatalf("source resolved to %T, want []any", val)
	}
	if len(items) != 2 {
		t.Fatalf("active users = %d, want 2", len(items))
	}

	// A plain step path (no chain) still resolves normally.
	plain, err := evalV1(e, `loadUsers.result`)
	if err != nil {
		t.Fatalf("evaluate plain source: %v", err)
	}
	if all, ok := plain.([]any); !ok || len(all) != 3 {
		t.Fatalf("plain source = %v, want 3 items", plain)
	}
}
