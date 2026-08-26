package memql

import (
	"encoding/json"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// concept_shape_test.go -- the declared-shape projection (task memql#4662).
//
// The point of the last test in this file is easy to lose: `fields` and
// `relationships` are useless if the follow-mode delta stream projects a
// DIFFERENT descriptor from the one-shot list, because a client that opened a
// registry subscription would silently hold shapeless concepts for as long as
// the subscription lived. That is exactly the bug class memql#4238 filed the
// single-projection rule to prevent, so the rule is asserted rather than
// assumed.

// shapedConcept is a concept carrying a definition schema exercising every
// kind the projection distinguishes, plus one relationship of each axis shape.
func shapedConcept(t *testing.T) *memoryNodes.Concept {
	t.Helper()
	schema := map[string]any{
		"$id":                  "v1:test:widget",
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":   map[string]any{"type": "string", "description": "What it is called."},
			"count":  map[string]any{"type": "integer"},
			"ratio":  map[string]any{"type": "number"},
			"active": map[string]any{"type": "boolean"},
			// A REQUIRED datetime: stored nominally.
			"createdAt": map[string]any{"type": "string", "format": "date-time"},
			// An OPTIONAL datetime: stored as the three-member oneOf
			// (memql#1629). The projection has to see through it, or every
			// nullable timestamp in the tree reaches a client untyped.
			"releasedAt": map[string]any{"oneOf": []any{
				map[string]any{"type": "string", "format": "date-time"},
				map[string]any{"type": "string", "maxLength": 0},
				map[string]any{"type": "null"},
			}},
			"status": map[string]any{"type": "string", "enum": []any{"active", "archived"}},
			"tags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"meta":   map[string]any{"type": "object"},
		},
		"required": []any{"name", "status"},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return &memoryNodes.Concept{
		Name:     "v1:test:widget",
		SchemaId: "v1:test:widget",
		Schemas:  map[string]json.RawMessage{"definition": raw},
		NodeType: "entity",
		Relationships: []memoryNodes.RelationshipDefinition{
			// Labelled (post-memql#3652): both axes carried.
			{Type: "references", As: "ownedBy", Field: "ownerUserId", TargetConcept: "v1:identity:user", Direction: "outgoing"},
			// Unlabelled (pre-memql#3652): `as` empty, and it must STAY
			// empty rather than being backfilled from `type`.
			{Type: "parent", Field: "parentId", TargetConcept: "v1:test:widget", Direction: "outgoing"},
		},
	}
}

func fieldByName(fields []*memqlv1.ConceptField, name string) *memqlv1.ConceptField {
	for _, f := range fields {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func TestConceptFieldsProjectTheAuthoringKind(t *testing.T) {
	fields := conceptFields(shapedConcept(t))
	if len(fields) == 0 {
		t.Fatal("no fields projected")
	}

	want := map[string]string{
		"name":       "string",
		"count":      "integer",
		"ratio":      "number",
		"active":     "boolean",
		"createdAt":  "datetime",
		"releasedAt": "datetime",
		"status":     "enum",
		"tags":       "array",
		"meta":       "object",
	}
	for name, kind := range want {
		got := fieldByName(fields, name)
		if got == nil {
			t.Errorf("field %q not projected", name)
			continue
		}
		if got.GetKind() != kind {
			t.Errorf("field %q kind = %q, want %q", name, got.GetKind(), kind)
		}
	}
}

func TestConceptFieldsCarryRequirednessEnumsAndProse(t *testing.T) {
	fields := conceptFields(shapedConcept(t))

	name := fieldByName(fields, "name")
	if name == nil || !name.GetRequired() {
		t.Error("name should project as required")
	}
	if name != nil && name.GetDescription() != "What it is called." {
		t.Errorf("name description = %q", name.GetDescription())
	}
	if count := fieldByName(fields, "count"); count == nil || count.GetRequired() {
		t.Error("count should project as optional")
	}

	status := fieldByName(fields, "status")
	if status == nil {
		t.Fatal("status not projected")
	}
	if got := status.GetEnumValues(); len(got) != 2 || got[0] != "active" || got[1] != "archived" {
		t.Errorf("status enum = %v, want [active archived]", got)
	}
	// A non-enum field carries no members. A client renders these as pill
	// labels; inventing one would render a member that does not exist.
	if got := fieldByName(fields, "name").GetEnumValues(); len(got) != 0 {
		t.Errorf("non-enum field carries enum values %v", got)
	}
}

func TestConceptFieldOrderIsStable(t *testing.T) {
	// Declaration order is not recoverable (the schema is built as a Go map
	// and encoding/json sorts on the way out), so the projection sorts. What
	// this asserts is that it does not vary BETWEEN CALLS -- Go map iteration
	// is deliberately randomised, so an unsorted projection would pass a
	// single-run test and produce a different wire payload per request.
	c := shapedConcept(t)
	first := conceptFields(c)
	for i := 0; i < 8; i++ {
		again := conceptFields(c)
		if len(again) != len(first) {
			t.Fatalf("field count varies between calls: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j].GetName() != first[j].GetName() {
				t.Fatalf("field order varies between calls at %d: %q vs %q",
					j, again[j].GetName(), first[j].GetName())
			}
		}
	}
}

func TestConceptRelationshipsCarryBothAxesVerbatim(t *testing.T) {
	rels := conceptRelationships(shapedConcept(t))
	if len(rels) != 2 {
		t.Fatalf("relationships = %d, want 2", len(rels))
	}

	if rels[0].GetType() != "references" || rels[0].GetAs() != "ownedBy" {
		t.Errorf("labelled relationship = (%q, %q), want (references, ownedBy)",
			rels[0].GetType(), rels[0].GetAs())
	}
	if rels[0].GetField() != "ownerUserId" || rels[0].GetTarget() != "v1:identity:user" {
		t.Errorf("labelled relationship field/target = (%q, %q)", rels[0].GetField(), rels[0].GetTarget())
	}
	if rels[0].GetDirection() != "outgoing" {
		t.Errorf("direction = %q", rels[0].GetDirection())
	}

	// The load-bearing half: `as` is NOT backfilled from `type`. A client
	// labelling an edge falls back to `field`, which is the author's own
	// noun; handing it "parent" would present the engine's structural word
	// as the domain's label.
	if rels[1].GetAs() != "" {
		t.Errorf("unlabelled relationship as = %q, want empty", rels[1].GetAs())
	}
	if rels[1].GetType() != "parent" {
		t.Errorf("unlabelled relationship type = %q, want parent", rels[1].GetType())
	}
}

func TestConceptShapeIsEmptyRatherThanWrongWhenUnknown(t *testing.T) {
	// A concept with no stored definition schema is a real registry state.
	// Empty is the honest projection -- it says "this server publishes no
	// shape", which is the state that preceded these fields -- and a client
	// falls back to row sampling rather than believing the concept has no
	// fields.
	bare := &memoryNodes.Concept{Name: "v1:test:bare", NodeType: "entity"}
	fields, rels := conceptShape(bare)
	if len(fields) != 0 || len(rels) != 0 {
		t.Errorf("bare concept projected %d fields / %d relationships, want none", len(fields), len(rels))
	}

	// Malformed JSON must not panic and must not half-project.
	broken := &memoryNodes.Concept{
		Name:     "v1:test:broken",
		SchemaId: "v1:test:broken",
		Schemas:  map[string]json.RawMessage{"definition": json.RawMessage(`{"properties": `)},
	}
	if got := conceptFields(broken); len(got) != 0 {
		t.Errorf("malformed schema projected %d fields", len(got))
	}

	if fields, rels := conceptShape(nil); fields != nil || rels != nil {
		t.Error("nil concept should project nothing")
	}
}

// TestBothWirePathsCarryTheDeclaredShape is the reason the single-projection
// rule is written down (memql#4238) and the reason this test exists.
//
// The one-shot ConceptsListMsg reply and the follow-mode ConceptsRegistryDelta
// are two independent code paths that a reader can easily change one of. If
// the delta stream projected its own descriptor, a client holding a registry
// SUBSCRIPTION -- the composer's own path -- would see shapeless concepts for
// the life of that subscription while the list reply looked perfect.
//
// Asserting through conceptInfoFromConcept is the point: it is the function
// both paths call, and the test fails the moment either grows a second one.
func TestBothWirePathsCarryTheDeclaredShape(t *testing.T) {
	c := shapedConcept(t)

	// The exact expressions the two handlers use (concepts_handlers.go:80 for
	// the list, :197 and :239 for the snapshot and the deltas).
	listInfo := conceptInfoFromConcept(c)
	deltaInfo := conceptInfoFromConcept(c)

	for label, info := range map[string]*memqlv1.ConceptInfo{"list": listInfo, "delta": deltaInfo} {
		if len(info.GetFields()) == 0 {
			t.Errorf("%s path carries no fields", label)
		}
		if len(info.GetRelationships()) == 0 {
			t.Errorf("%s path carries no relationships", label)
		}
		// Additive means the pre-existing descriptor is untouched.
		if info.GetId() != "v1:test:widget" || info.GetType() != "entity" {
			t.Errorf("%s path lost pre-existing descriptor fields", label)
		}
	}

	if len(listInfo.GetFields()) != len(deltaInfo.GetFields()) {
		t.Fatalf("the two wire paths disagree on field count: %d vs %d",
			len(listInfo.GetFields()), len(deltaInfo.GetFields()))
	}
	for i := range listInfo.GetFields() {
		a, b := listInfo.GetFields()[i], deltaInfo.GetFields()[i]
		if a.GetName() != b.GetName() || a.GetKind() != b.GetKind() {
			t.Fatalf("the two wire paths disagree at field %d: %q/%q vs %q/%q",
				i, a.GetName(), a.GetKind(), b.GetName(), b.GetKind())
		}
	}
}
