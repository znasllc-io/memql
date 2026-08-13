package memoryNodes

import "testing"

// TestParseConcept_RelationshipAsLabel pins the PARSE half of memql#3652: an
// `as` label written in source must survive all the way into the
// RelationshipDefinition.
//
// The annotation parser has no kwarg allow-list, so `as="assignedTo"` already
// lands in attr.Args today -- and is then dropped, because
// attributeToRelationshipDecl builds its struct from a hardcoded list of keys.
// A label that parses and is silently discarded is the exact "declaration
// theatre" shape this epic exists to remove, so it is worth a test of its own
// rather than trusting the field-copy chain.
func TestParseConcept_RelationshipAsLabel(t *testing.T) {
	src := []byte(`
@description("A participant that points at an agent and at a user.")
concept participant {
  agentId    string  @description("The responding agent.")
  forUserId  string  @description("The user being acted for.")

  @relationship(type="interactsWith", as="respondsAs", field="agentId", target="v1:agents:agent", direction="outgoing")
  @relationship(type="interactsWith", as="actsFor", field="forUserId", target="v1:identity:user", direction="outgoing")
}
`)

	c, err := ParseConceptMemQL(src, "v1/cognition/participant")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	if len(c.Relationships) != 2 {
		t.Fatalf("got %d relationships, want 2", len(c.Relationships))
	}

	// Two edges that are identical in every structural respect. Before `as`
	// they were indistinguishable -- that is the defect.
	byField := map[string]string{}
	for _, rel := range c.Relationships {
		byField[rel.Field] = rel.As
	}

	if got := byField["agentId"]; got != "respondsAs" {
		t.Errorf("agentId as = %q, want %q", got, "respondsAs")
	}
	if got := byField["forUserId"]; got != "actsFor" {
		t.Errorf("forUserId as = %q, want %q", got, "actsFor")
	}
}

// TestRelationshipDefinition_UnmarshalJSONKeepsAsLabel covers the highest-risk
// silent dropper: RelationshipDefinition has a CUSTOM UnmarshalJSON whose
// private alias struct lists every key explicitly. Adding a Go struct field is
// not enough -- any concept loaded from JSON loses the label unless the alias
// learns it too.
func TestRelationshipDefinition_UnmarshalJSONKeepsAsLabel(t *testing.T) {
	var def RelationshipDefinition
	raw := []byte(`{
		"type": "interactsWith",
		"field": "agentId",
		"targetConcept": "v1:agents:agent",
		"direction": "outgoing",
		"as": "respondsAs"
	}`)

	if err := def.UnmarshalJSON(raw); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if def.As != "respondsAs" {
		t.Errorf("As = %q, want %q", def.As, "respondsAs")
	}
}
