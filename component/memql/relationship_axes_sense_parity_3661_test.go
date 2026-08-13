package memql

// relationship_axes_sense_parity_3661_test.go pins the editor's @relationship
// diagnostics against the engine's own load gate.
//
// sense CANNOT import this package -- the dependency runs the other way, since
// component/memql imports component/memql/sense -- so sense restates the `as`
// label form and the structural `type` set rather than calling
// validateRelationshipAsLabel and canonicalRelationshipType. A restatement is
// a second opinion waiting to happen: the editor could quietly start accepting
// a label the loader refuses, which is worse than no squiggle at all, because
// the author is told the thing is fine right up until boot.
//
// So this test runs the SAME inputs through both layers and fails on any
// disagreement. It is the same arrangement actorUnknownPropertyRule uses for
// the actor envelope, and it is why the duplication is acceptable.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/sense"
)

// asLabelCases spans the form rule's edges in both directions.
var asLabelCases = []string{
	// Well-formed.
	"assignedTo", "respondsAs", "actsFor", "a", "a1", "frobnicatesWith2",
	"belongsToSpace", "x9y8z7",
	// Malformed, one rule each.
	"AssignedTo",             // leading uppercase
	"assigned_to",            // underscore
	"assigned-to",            // hyphen
	"1assigned",              // leading digit
	"assigned to",            // space
	"assigned.to",            // dot
	"",                       // empty is LEGAL -- as is optional
	strings.Repeat("a", 64),  // exactly at the cap
	strings.Repeat("a", 65),  // one over
	strings.Repeat("ab", 40), // well over
}

// TestSenseAsLabelDiagnosticMatchesTheLoadGate asserts the editor rejects
// exactly the labels the loader rejects -- no more, no less.
func TestSenseAsLabelDiagnosticMatchesTheLoadGate(t *testing.T) {
	svc := sense.New(nil)

	for _, label := range asLabelCases {
		engineRefuses := validateRelationshipAsLabel(label) != nil

		src := "concept t {\n  @relationship(type=\"references\", as=\"" + label +
			"\", field=\"a\", target=b, direction=\"outgoing\")\n}"
		editorRefuses := false
		for _, d := range svc.Diagnose(src, "concepts.memql") {
			if d.Code == "relationship-as-malformed" {
				editorRefuses = true
			}
		}

		if engineRefuses != editorRefuses {
			t.Errorf("as=%q: engine refuses=%v but editor refuses=%v -- the editor's "+
				"restated form rule has drifted from validateRelationshipAsLabel",
				shortLabel(label), engineRefuses, editorRefuses)
		}
	}
}

// structuralTypeCases spans the closed set plus the shapes that are not in it.
var structuralTypeCases = []string{
	// Every member, in its canonical spelling.
	"parent", "alias", "equals", "references", "contains", "owns", "createdBy",
	// The normalisation canonicalRelationshipType applies.
	"REFERENCES", "created_by", "createdby",
	// Not members. "references" is the one the authoring reference used to
	// teach (memql#3660); "dependsOn" / "formedFrom" were structural types
	// until memql#3655 retired them to `as` labels -- and this pin is what
	// caught that the moment it landed; the rest are plausible domain verbs
	// that belong on `as` instead.
	"references", "dependsOn", "formedFrom", "DEPENDS_ON", "formed_from",
	"assignedTo", "bogus", "depends", "child", "",
}

// TestSenseStructuralTypeDiagnosticMatchesTheLoadGate asserts the same for the
// closed `type` set, including its case/underscore normalisation.
func TestSenseStructuralTypeDiagnosticMatchesTheLoadGate(t *testing.T) {
	svc := sense.New(nil)

	for _, value := range structuralTypeCases {
		_, engineAccepts := canonicalRelationshipType(value)

		src := "concept t {\n  @relationship(type=\"" + value +
			"\", field=\"a\", target=b, direction=\"outgoing\")\n}"
		editorRefuses := false
		for _, d := range svc.Diagnose(src, "concepts.memql") {
			if d.Code == "relationship-type-unknown" {
				editorRefuses = true
			}
		}

		if engineAccepts == editorRefuses {
			t.Errorf("type=%q: engine accepts=%v but editor refuses=%v -- the editor's "+
				"restated structural set has drifted from canonicalRelationshipType",
				value, engineAccepts, editorRefuses)
		}
	}
}

// shortLabel keeps a failure message readable when the case is a 65-character
// label. (Named around the package's existing truncate.)
func shortLabel(s string) string {
	if len(s) <= 20 {
		return s
	}
	return fmt.Sprintf("%s...(len %d)", s[:20], len(s))
}
