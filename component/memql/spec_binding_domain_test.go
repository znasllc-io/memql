package memql

import (
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A spec's bound name resolves in the spec's own domain first, as a query's
// signature concept does (memql#5369). Two domains each declaring a `ticket`
// left a spec bound to `ticket` resolving to neither -- the bare name was
// ambiguous across the tree -- so the load refused it as "resolves to neither
// an imported shape nor a concept", while a query over `ticket` in either
// domain loaded. The conformance corpus, whose every case domain declares one,
// found it.
func TestSpecBindingResolvesInTheSpecsOwnDomainFirst(t *testing.T) {
	registry := concept.NewRegistry(map[string]*concept.Concept{
		"v1:alpha:ticket": {Name: "v1:alpha:ticket"},
		"v1:beta:ticket":  {Name: "v1:beta:ticket"},
		"v1:beta:note":    {Name: "v1:beta:note"},
	})

	spec := &Spec{Name: "isOpen", BoundName: "ticket", Origin: "alpha/specs.memql"}
	got, err := specBindingConcept(registry, spec)
	if err != nil || got == nil || got.Name != "v1:alpha:ticket" {
		t.Fatalf("a spec in alpha bound to ticket = %v, %v; want v1:alpha:ticket", got, err)
	}
	spec.Origin = "beta/specs.memql"
	if got, err = specBindingConcept(registry, spec); err != nil || got == nil || got.Name != "v1:beta:ticket" {
		t.Fatalf("a spec in beta bound to ticket = %v, %v; want v1:beta:ticket", got, err)
	}

	// A name the spec's domain does not declare still resolves across the
	// tree when it is unique, and stays refused when it is not.
	spec = &Spec{Name: "isNoted", BoundName: "note", Origin: "alpha/specs.memql"}
	if got, err = specBindingConcept(registry, spec); err != nil || got == nil || got.Name != "v1:beta:note" {
		t.Fatalf("a spec in alpha bound to beta's unique note = %v, %v; want v1:beta:note", got, err)
	}
	spec = &Spec{Name: "isOpen", BoundName: "ticket", Origin: "gamma/specs.memql"}
	if got, err = specBindingConcept(registry, spec); err == nil {
		t.Fatalf("a spec in gamma bound to the ambiguous ticket resolved to %v; want the ambiguity refused", got)
	}

	// The shape half: the spec's own domain's shape wins over another
	// domain's shape of the same name.
	shapes := newShapeRegistry()
	for _, s := range []*ShapeDefinition{
		{Name: "caller", Origin: "alpha/shapes.memql", KindActor: true},
		{Name: "caller", Origin: "beta/shapes.memql", KindRow: true},
	} {
		if err := shapes.Upsert(s); err != nil {
			t.Fatalf("register shape: %v", err)
		}
	}
	bound := &Spec{Name: "callerIsAdmin", BoundName: "caller", Origin: "alpha/specs.memql"}
	shape, ok := specBindingShape(shapes, bound)
	if !ok || shape == nil || shape.Origin != "alpha/shapes.memql" {
		t.Fatalf("a spec in alpha bound to caller = %v, %v; want alpha's shape", shape, ok)
	}
}
