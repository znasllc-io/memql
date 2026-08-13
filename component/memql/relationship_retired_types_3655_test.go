package memql

import (
	"strings"
	"testing"
)

// TestRetiredStructuralTypesAreRejected pins memql#3655: `dependsOn` and
// `formedFrom` are no longer structural relationship types.
//
// They were never structural. Their own registration comments said graph
// expansion was deliberately unwired and the field was read directly by the
// controller -- the engine did nothing with either. They existed as structural
// types only because, at the time, there was no other way to say the word, and
// adding each one cost an engine release after a boot refusal took the cluster
// down (d45efaa2, 42aeff3b).
//
// With `as` (memql#3652) they are exactly what a domain label is for.
func TestRetiredStructuralTypesAreRejected(t *testing.T) {
	for _, retired := range []string{"dependsOn", "formedFrom", "depends_on", "FORMED_FROM"} {
		t.Run(retired, func(t *testing.T) {
			if got, ok := canonicalRelationshipType(retired); ok {
				t.Errorf("canonicalRelationshipType(%q) = (%q, true), want rejected", retired, got)
			}
		})
	}
}

// TestRetiredStructuralTypeErrorPointsAtTheLabel pins that an author who writes
// the old spelling is told where it moved, rather than only that it is invalid.
// This is the whole reason the break is safe to take: the error carries the fix.
func TestRetiredStructuralTypeErrorPointsAtTheLabel(t *testing.T) {
	def := validAsLabelDef("")
	def.Type = "dependsOn"

	_, err := normalizeRelationshipDefinition(def)
	if err == nil {
		t.Fatal("dependsOn was accepted as a structural type, want a load error")
	}
	for _, want := range []string{"dependsOn", `as="dependsOn"`, "references"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestRetiredTypesSurviveAsLabels is the other half of the migration: the words
// keep working, just on the axis that was always right for them.
func TestRetiredTypesSurviveAsLabels(t *testing.T) {
	for _, label := range []string{"dependsOn", "formedFrom"} {
		t.Run(label, func(t *testing.T) {
			got, err := normalizeRelationshipDefinition(validAsLabelDef(label))
			if err != nil {
				t.Fatalf("as=%q was rejected: %v", label, err)
			}
			if got.As != label {
				t.Errorf("As = %q, want %q", got.As, label)
			}
		})
	}
}
