package memoryNodes

import "testing"

// TestCloneDefaultRegistry_Isolation: a clone is seeded from the default
// snapshot, and mutating the clone (MergeAll) never touches the default
// registry -- the core contract the authoring sandbox relies on (#956).
func TestCloneDefaultRegistry_Isolation(t *testing.T) {
	// Seed the default with a known baseline concept so the clone has a
	// non-empty snapshot to copy.
	ReplaceAll(map[string]*Concept{
		"v1:test:base": {Name: "v1:test:base"},
	})
	t.Cleanup(func() { ReplaceAll(nil) })

	clone := CloneDefaultRegistry()

	// The clone sees the baseline...
	if _, err := clone.Get("v1:test:base"); err != nil {
		t.Fatalf("clone should be seeded with the default's concepts: %v", err)
	}

	// ...and overlaying a candidate concept onto the clone is visible on
	// the clone.
	clone.MergeAll(map[string]*Concept{
		"v1:test:candidate": {Name: "v1:test:candidate"},
	})
	if _, err := clone.Get("v1:test:candidate"); err != nil {
		t.Fatalf("candidate concept should be visible on the clone: %v", err)
	}

	// ...but NOT on the default registry. This is the no-mutation
	// guarantee the sandbox depends on.
	if _, err := Get("v1:test:candidate"); err == nil {
		t.Fatal("candidate overlaid on the clone leaked into the default registry")
	}
	if got := len(List()); got != 1 {
		t.Fatalf("default registry size changed: want 1, got %d", got)
	}
}

// TestCloneDefaultRegistry_DefaultMutationDoesNotLeakIntoClone: a clone
// taken earlier does not pick up concepts registered into the default
// afterwards -- the two share no underlying map.
func TestCloneDefaultRegistry_DefaultMutationDoesNotLeakIntoClone(t *testing.T) {
	ReplaceAll(map[string]*Concept{
		"v1:test:base": {Name: "v1:test:base"},
	})
	t.Cleanup(func() { ReplaceAll(nil) })

	clone := CloneDefaultRegistry()

	// Register a new concept into the default AFTER the clone was taken.
	MergeAll(map[string]*Concept{
		"v1:test:late": {Name: "v1:test:late"},
	})

	if _, err := clone.Get("v1:test:late"); err == nil {
		t.Fatal("a concept added to the default after cloning leaked into the clone")
	}
}
