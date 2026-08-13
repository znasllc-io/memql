package memql

import (
	"strings"
	"testing"
)

// TestReferencesIsTheStructuralType pins memql#3663: the plain foreign-key edge
// type is `references`.
//
// Once `as` carries the domain meaning (memql#3652), the structural type only
// has to denote "a plain foreign-key edge -- the engine canonicalizes the id and
// nothing more". `interactsWith` implied an interaction that does not exist.
//
// `references` rather than `reference` because NodeTypeReference already claims
// "reference" as a concept NODE TYPE, and one word in two unrelated roles on the
// same construct is the confusion this epic exists to remove.
func TestReferencesIsTheStructuralType(t *testing.T) {
	for _, spelling := range []string{"references", "REFERENCES", "References"} {
		got, ok := canonicalRelationshipType(spelling)
		if !ok {
			t.Errorf("canonicalRelationshipType(%q): ok=false, want valid", spelling)
			continue
		}
		if got != "references" {
			t.Errorf("canonicalRelationshipType(%q) = %q, want %q", spelling, got, "references")
		}
	}
}

// TestInteractsWithIsRetired is the other half of the break.
func TestInteractsWithIsRetired(t *testing.T) {
	for _, spelling := range []string{"interactsWith", "interactswith", "INTERACTS_WITH"} {
		if got, ok := canonicalRelationshipType(spelling); ok {
			t.Errorf("canonicalRelationshipType(%q) = (%q, true), want rejected", spelling, got)
		}
	}
}

// TestRetiredInteractsWithErrorNamesTheRename pins that the error tells an
// author the word MOVED, not merely that it is invalid.
//
// This case needs its own hint. The generic unknown-type message suggests
// `as="<whatever you wrote>"`, which for a renamed STRUCTURAL type is actively
// misleading -- it would send the author to write as="interactsWith" on an edge
// whose structural type is what actually changed.
func TestRetiredInteractsWithErrorNamesTheRename(t *testing.T) {
	def := validAsLabelDef("")
	def.Type = "interactsWith"

	_, err := normalizeRelationshipDefinition(def)
	if err == nil {
		t.Fatal("interactsWith was accepted, want a load error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "references") {
		t.Errorf("error %q does not name the new spelling", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "renamed") {
		t.Errorf("error %q does not say the type was renamed", msg)
	}
}

// TestEdgeLabelFollowsTheRename pins that the wire value moved with the type.
//
// memql#3657 made the edge label derive from the type constant precisely so the
// two cannot drift; this asserts that derivation still holds after the rename,
// rather than trusting it.
func TestEdgeLabelFollowsTheRename(t *testing.T) {
	if GraphEdgeLabelReferences != "references" {
		t.Errorf("GraphEdgeLabelReferences = %q, want %q", GraphEdgeLabelReferences, "references")
	}
	for _, label := range GraphEdgeLabels() {
		if label == "interactsWith" {
			t.Error("GraphEdgeLabels still advertises the retired \"interactsWith\" label")
		}
	}
}
