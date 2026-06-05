package annotations

import "testing"

// TestEveryByReceiverNameHasDoc locks the registry's internal invariant
// at the source: every annotation any receiver offers carries a Docs
// entry. (sense relies on this for hover/completion; holding it here
// keeps the registry self-consistent regardless of who consumes it.)
func TestEveryByReceiverNameHasDoc(t *testing.T) {
	for receiver, names := range ByReceiver {
		label := receiver
		if label == "" {
			label = "<concept>"
		}
		for _, n := range names {
			if _, ok := Docs[n]; !ok {
				t.Errorf("%s: annotation @%s is offered but has no Docs entry", label, n)
			}
		}
	}
}

// TestSetReturnsMembership confirms Set mirrors ByReceiver as a
// membership map and yields an empty (non-nil) set for unknown
// receivers.
func TestSetReturnsMembership(t *testing.T) {
	q := Set("Query")
	for _, n := range ByReceiver["Query"] {
		if !q[n] {
			t.Errorf("Set(\"Query\") missing %q", n)
		}
	}
	if len(q) != len(ByReceiver["Query"]) {
		t.Errorf("Set(\"Query\") size = %d, want %d", len(q), len(ByReceiver["Query"]))
	}
	if got := Set("DoesNotExist"); got == nil || len(got) != 0 {
		t.Errorf("Set(unknown) = %v, want empty non-nil map", got)
	}
}

// TestSetReturnsFreshMap confirms the caller can mutate the returned
// set without corrupting the registry.
func TestSetReturnsFreshMap(t *testing.T) {
	s := Set("Logic")
	s["injected"] = true
	if Set("Logic")["injected"] {
		t.Error("Set returned a shared map; mutation leaked back into the registry")
	}
}
