package memql

import (
	"testing"
)

// TestValidateSpecBindings_RejectsMissingShape locks the post-load
// validation: a spec that binds via @useShape(N) where N is not in
// the shape registry must error at engine startup.
func TestValidateSpecBindings_RejectsMissingShape(t *testing.T) {
	specs := newSpecRegistry()
	shapes := newShapeRegistry()

	// Register a spec that references a shape that doesn't exist.
	bad := &Spec{
		Name:         "specMissingBinding",
		UseShapeName: "nonExistentShape",
	}
	if err := specs.add(bad); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	if err := ValidateSpecBindings(specs, shapes); err == nil {
		t.Fatal("expected error for spec with missing @useShape binding, got nil")
	}
}

// TestValidateSpecBindings_PassesWithExistingShape locks the happy
// path: when the spec's @useShape(N) resolves to a registered shape,
// validation succeeds.
func TestValidateSpecBindings_PassesWithExistingShape(t *testing.T) {
	specs := newSpecRegistry()
	shapes := newShapeRegistry()

	if err := shapes.Upsert(&ShapeDefinition{Name: "knownShape"}); err != nil {
		t.Fatalf("seed shape: %v", err)
	}
	good := &Spec{
		Name:         "specGoodBinding",
		UseShapeName: "knownShape",
	}
	if err := specs.add(good); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	if err := ValidateSpecBindings(specs, shapes); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestValidateSpecBindings_SkipsTraits locks that traits bypass the
// binding check (traits forbid @useShape, so they can't have one).
func TestValidateSpecBindings_SkipsTraits(t *testing.T) {
	specs := newSpecRegistry()
	shapes := newShapeRegistry()

	trait := &Spec{
		Name:    "traitNoBinding",
		IsTrait: true,
	}
	if err := specs.add(trait); err != nil {
		t.Fatalf("seed trait: %v", err)
	}

	if err := ValidateSpecBindings(specs, shapes); err != nil {
		t.Fatalf("expected nil for trait, got %v", err)
	}
}
