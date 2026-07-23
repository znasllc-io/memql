package sense

// Tests for the two remaining reference-completion positions (#2740, follow-up
// to #2733): a @relationship target concept, and a shape body's `include`.

import "testing"

func refShapeRegistry() *stubRegistry {
	return &stubRegistry{
		concepts: map[string]*ConceptInfo{
			"v1:reference:objectExample": {Name: "v1:reference:objectExample"},
			"v1:reference:node":          {Name: "v1:reference:node"},
			"v1:orders:order":            {Name: "v1:orders:order"},
		},
		shapes: []string{"participantFull", "actorEnvelope", "roleFull"},
	}
}

func TestRelationshipTarget_OffersConcepts(t *testing.T) {
	s := New(refShapeRegistry())
	got := completeAtEnd(s, "concept x {\n  @relationship(type=\"contains\", target=\"")
	for _, want := range []string{"v1:reference:objectExample", "v1:reference:node", "v1:orders:order"} {
		if !got[want] {
			t.Errorf("@relationship target should offer concept %q, got %v", want, got)
		}
	}
	// Not a dump: shapes / keywords must not leak in here.
	if got["participantFull"] {
		t.Errorf("@relationship target must not offer shapes")
	}
}

func TestRelationshipTarget_PrefixFilter(t *testing.T) {
	got := completeAtEnd(New(refShapeRegistry()), "concept x {\n  @relationship(target=\"v1:reference:")
	if !got["v1:reference:objectExample"] || !got["v1:reference:node"] {
		t.Errorf("prefix `v1:reference:` should offer the reference concepts, got %v", got)
	}
	if got["v1:orders:order"] {
		t.Errorf("prefix `v1:reference:` must exclude v1:orders:order, got %v", got)
	}
}

func TestRelationshipTarget_NoConceptNoiseAtOtherArgs(t *testing.T) {
	// type= / field= / direction= values are enums/strings/field-names, NOT
	// concepts -- concepts must not be offered there.
	for _, src := range []string{
		"concept x {\n  @relationship(type=\"con",
		"concept x {\n  @relationship(field=\"mem",
		"concept x {\n  @relationship(type=\"contains\", direction=\"out",
	} {
		got := completeAtEnd(New(refShapeRegistry()), src)
		if got["v1:reference:node"] {
			t.Errorf("concept offered at a non-target arg value: %q -> %v", src, got)
		}
	}
}

func TestRelationshipTarget_BareOffersShortNames(t *testing.T) {
	// The common form: `target=user` (bare) binds the SHORT concept name.
	got := completeAtEnd(New(refShapeRegistry()), "concept x {\n  @relationship(type=\"contains\", target=")
	for _, want := range []string{"objectExample", "node", "order"} {
		if !got[want] {
			t.Errorf("bare target= should offer short name %q, got %v", want, got)
		}
	}
	// Not the canonical form here.
	if got["v1:reference:node"] {
		t.Errorf("bare target= must offer short names, not canonical ids: %v", got)
	}
}

func TestRelationshipTarget_BarePrefixFilter(t *testing.T) {
	got := completeAtEnd(New(refShapeRegistry()), "concept x {\n  @relationship(target=no")
	if !got["node"] || got["order"] || got["objectExample"] {
		t.Errorf("bare `target=no` should offer only node, got %v", got)
	}
}

func TestRelationshipTarget_NotPoisonedAcrossLines(t *testing.T) {
	// An unclosed @relationship( upstream, or one inside a comment, must NOT make
	// a later target= elsewhere offer concepts (the regex is single-line).
	for _, src := range []string{
		"concept x {\n  @relationship(type=\"x\"\n  @foo(target=\"bar",
		"// @relationship(type=\nconcept y {\n  @index(target=\"z",
	} {
		got := completeAtEnd(New(refShapeRegistry()), src)
		if got["v1:reference:node"] || got["node"] {
			t.Errorf("relationship-target poisoned a later position: %q -> %v", src, got)
		}
	}
}

func TestShapeInclude_OffersShapes(t *testing.T) {
	got := completeAtEnd(New(refShapeRegistry()), "shape participant p {\n  include ")
	for _, want := range []string{"participantFull", "actorEnvelope", "roleFull"} {
		if !got[want] {
			t.Errorf("shape include should offer shape %q, got %v", want, got)
		}
	}
	// Not a dump: concepts / keywords must not leak in.
	if got["v1:reference:node"] {
		t.Errorf("shape include must not offer concepts")
	}
}

func TestShapeInclude_PrefixFilter(t *testing.T) {
	got := completeAtEnd(New(refShapeRegistry()), "shape participant p {\n  include participant")
	if !got["participantFull"] || got["actorEnvelope"] {
		t.Errorf("prefix `participant` should offer only participantFull, got %v", got)
	}
}

func TestShapeInclude_OnlyInShapeBody(t *testing.T) {
	// `include` in a non-shape body is not the shape-include verb.
	got := completeAtEnd(New(refShapeRegistry()), "logic foo {\n  body {\n    include ")
	if got["participantFull"] {
		t.Errorf("shape include fired outside a shape body, got %v", got)
	}
}
