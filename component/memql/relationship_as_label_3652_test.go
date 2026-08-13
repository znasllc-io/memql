package memql

import (
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptForAsLabelTest is a concept carrying exactly one labelled
// relationship, so an assertion about the label cannot be satisfied by
// anything else in the payload.
func conceptForAsLabelTest() *concept.Concept {
	return &concept.Concept{
		Name: "v1:cognition:participant",
		Relationships: []RelationshipDefinition{{
			Type:          "interactsWith",
			Field:         "agentId",
			TargetConcept: "v1:agents:agent",
			Direction:     "outgoing",
			As:            "assignedTo",
		}},
	}
}

// validAsLabelDef returns a definition that differs from a known-good one only
// in its `as` label, so a failure can only be attributable to that label.
func validAsLabelDef(as string) RelationshipDefinition {
	return RelationshipDefinition{
		Type:          "interactsWith",
		Field:         "agentId",
		TargetConcept: "v1:agents:agent",
		Direction:     "outgoing",
		As:            as,
	}
}

// TestRelationshipAsLabelRejectsMalformed pins the FORM half of the `as`
// contract (memql#3652): a label must be a lowerCamelCase identifier within the
// length cap. Form is the only thing ever checked -- see
// TestRelationshipAsLabelAcceptsAnyWellFormedVerb for the other half.
func TestRelationshipAsLabelRejectsMalformed(t *testing.T) {
	malformed := map[string]string{
		"underscore":   "assigned_to",
		"capitalised":  "AssignedTo",
		"leadingDigit": "1assigned",
		"hyphen":       "assigned-to",
		"space":        "assigned to",
		"dot":          "assigned.to",
		"overLengthCap": strings.Repeat("a", 65),
	}

	for name, as := range malformed {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeRelationshipDefinition(validAsLabelDef(as)); err == nil {
				t.Errorf("as=%q was accepted, want a load error", as)
			}
		})
	}
}

// TestRelationshipAsLabelAcceptsAnyWellFormedVerb is the reason memql#3652
// exists. It is a GUARD: it passes today and its job is to start failing the
// moment somebody adds a membership check to `as`.
//
// A closed vocabulary is what made an unrecognised relationship verb a boot
// refusal that took the whole mesh down -- twice (d45efaa2, 42aeff3b), each
// fixed by adding one word to a Go switch and cutting an engine release. A
// product repo mounting its own tree at MEMQL_DSL_PATH cannot patch this
// engine, so for them that failure has no remedy at all. Every verb below is
// one no engine author would think to allow-list, which is the point.
func TestRelationshipAsLabelAcceptsAnyWellFormedVerb(t *testing.T) {
	unanticipated := []string{
		"assignedTo",
		"repliesTo",
		"producedBy",
		"dependsOn",  // retired as a structural type in memql#3655; lives on as a label
		"formedFrom", // same
		"escalatesTo",
		"supersedes",
		"wasBilledUnder",
		"zzTopLevelNonsense42",
	}

	for _, as := range unanticipated {
		t.Run(as, func(t *testing.T) {
			got, err := normalizeRelationshipDefinition(validAsLabelDef(as))
			if err != nil {
				t.Fatalf("as=%q was rejected (%v) -- `as` must never be membership-checked", as, err)
			}
			if got.As != as {
				t.Errorf("As = %q, want %q", got.As, as)
			}
		})
	}
}

// TestRelationshipAsLabelReachesConceptsAPI pins that the label is visible to
// CLIENTS, not just to the engine.
//
// buildConceptPayload hand-builds a map[string]any per relationship for the
// concepts API rather than marshalling the struct, so a new field is dropped on
// the wire unless it is added here by hand. A label a client cannot read is
// write-only metadata -- the same defect in a new place.
func TestRelationshipAsLabelReachesConceptsAPI(t *testing.T) {
	eng := &MemQLEngine{}
	payload := eng.buildConceptPayload(conceptForAsLabelTest())

	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want a map", payload["metadata"])
	}
	rels, ok := metadata["relationships"].([]any)
	if !ok || len(rels) != 1 {
		t.Fatalf("relationships = %#v, want one entry", metadata["relationships"])
	}
	rel, ok := rels[0].(map[string]any)
	if !ok {
		t.Fatalf("relationship entry = %#v, want a map", rels[0])
	}
	if got := rel["as"]; got != "assignedTo" {
		t.Errorf("as = %#v, want %q", got, "assignedTo")
	}
}

// TestUnknownRelationshipTypeErrorNamesTheAsForm pins that the error a client
// repo's author hits is an INSTRUCTION, not a dead end.
//
// An unknown structural type still refuses boot, which is correct and matches
// the strict-boot convention. What was missing is any hint that a domain verb
// has a legitimate home. Without it, the author's only visible options are to
// guess another type or to patch an engine they may not own -- which is exactly
// how `dependsOn` and `formedFrom` each cost an engine release.
func TestUnknownRelationshipTypeErrorNamesTheAsForm(t *testing.T) {
	def := validAsLabelDef("")
	def.Type = "assignedTo" // a domain verb written in the structural slot

	_, err := normalizeRelationshipDefinition(def)
	if err == nil {
		t.Fatal("an unknown structural type was accepted, want a load error")
	}

	msg := err.Error()
	for _, want := range []string{"assignedTo", `as="assignedTo"`, "interactsWith"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// TestRelationshipAsLabelIsOptional pins that `as` is additive: every existing
// declaration in this tree and in every downstream bundle is unlabelled, and
// must keep loading untouched.
func TestRelationshipAsLabelIsOptional(t *testing.T) {
	got, err := normalizeRelationshipDefinition(validAsLabelDef(""))
	if err != nil {
		t.Fatalf("an unlabelled relationship was rejected: %v", err)
	}
	if got.As != "" {
		t.Errorf("As = %q, want empty", got.As)
	}
}

// TestRelationshipAsLabelIsPreserved pins memql#3652: a well-formed `as` label
// survives normalization intact.
//
// `as` carries the DOMAIN meaning of an edge, independent of the structural
// `type` the engine acts on. Normalization rebuilds the definition field by
// field, so a new field silently vanishes there unless it is carried across --
// which is the "declaration theatre" failure this whole epic exists to remove.
func TestRelationshipAsLabelIsPreserved(t *testing.T) {
	def := RelationshipDefinition{
		Type:          "interactsWith",
		Field:         "agentId",
		TargetConcept: "v1:agents:agent",
		Direction:     "outgoing",
		As:            "assignedTo",
	}

	got, err := normalizeRelationshipDefinition(def)
	if err != nil {
		t.Fatalf("normalizeRelationshipDefinition: %v", err)
	}

	if got.As != "assignedTo" {
		t.Errorf("As = %q, want %q", got.As, "assignedTo")
	}
}
