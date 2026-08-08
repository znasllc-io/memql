package automations

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/healing"
)

// TestPatchPreconditionShapeParity is the E4.3 follow-up the code owner asked
// for: healing.PatchPrecondition mirrors automations.Precondition by SHAPE
// rather than by import, to avoid an automations -> memql import cycle. This
// asserts field parity so the mirror cannot drift silently -- add a field to
// one and this fails until both carry it.
//
// # Why it lives HERE and not in component/healing (memql#3241)
//
// It was in component/healing/end_to_end_test.go, and it was the single reason
// that package could not become a base-tier module: a test import counts as a
// module requirement, so `component/healing` required `component/automations`
// and the boundary rejected it -- healing is base, automations is far above it.
//
// The direction is what makes this the right home rather than a workaround.
// The mirror exists to keep healing from importing automations; asserting the
// mirror from healing reintroduces exactly the edge the mirror removed. From
// automations the edge already runs downward, so the assertion costs nothing
// architecturally and covers the same drift.
func TestPatchPreconditionShapeParity(t *testing.T) {
	// Construct both with the same field set and assert each field round-trips
	// to the same value, proving the four fields are present + named-aligned
	// on both types.
	pp := healing.PatchPrecondition{
		ID:          "g",
		Check:       "exists(event.payload.x)",
		Literal:     "x",
		Description: "guard x",
	}
	ap := Precondition{
		ID:          pp.ID,
		Check:       pp.Check,
		Literal:     pp.Literal,
		Description: pp.Description,
	}
	if ap.ID != pp.ID || ap.Check != pp.Check || ap.Literal != pp.Literal || ap.Description != pp.Description {
		t.Fatalf("PatchPrecondition and automations.Precondition diverged: %+v vs %+v", pp, ap)
	}

	// Field-count parity: both types must expose exactly the same set of
	// exported fields, so adding a field to one without the other fails here.
	healingFields := exportedFieldNamesForParity(healing.PatchPrecondition{})
	automationFields := exportedFieldNamesForParity(Precondition{})
	if len(healingFields) != len(automationFields) {
		t.Fatalf("field-count drift: PatchPrecondition has %v, automations.Precondition has %v", healingFields, automationFields)
	}
	for i := range healingFields {
		if healingFields[i] != automationFields[i] {
			t.Errorf("field name drift at %d: %q vs %q", i, healingFields[i], automationFields[i])
		}
	}
}

// exportedFieldNamesForParity returns the exported field names of a struct
// value in declaration order.
func exportedFieldNamesForParity(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() {
			out = append(out, f.Name)
		}
	}
	return out
}
