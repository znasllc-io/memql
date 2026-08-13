package memql

import "testing"

// canonicalRelationshipType gates which @relationship types a concept may
// declare. The harness state model (v1:harness:step, #582) declares a
// `dependsOn` DAG edge; this pins that it is accepted (the engine rejected it
// at bootstrap before, taking the whole cluster down -- identity-first), plus
// the existing valid set and a clear rejection for unknown types.
func TestCanonicalRelationshipType(t *testing.T) {
	valid := map[string]string{
		"parent":        relationshipTypeParent,
		"alias":         relationshipTypeAlias,
		"equals":        relationshipTypeEquals,
		"interactsWith": relationshipTypeInteracts,
		"contains":      relationshipTypeContains,
		"owns":          relationshipTypeOwns,
		"createdBy":     relationshipTypeCreatedBy,
		// case / underscore insensitivity (normalized lower + strip "_").
		"CREATED_BY":     relationshipTypeCreatedBy,
		"createdby":      relationshipTypeCreatedBy,
		"interacts_with": relationshipTypeInteracts,
	}
	for input, want := range valid {
		got, ok := canonicalRelationshipType(input)
		if !ok {
			t.Errorf("canonicalRelationshipType(%q): ok=false, want valid", input)
			continue
		}
		if got != want {
			t.Errorf("canonicalRelationshipType(%q) = %q, want %q", input, got, want)
		}
	}

	// `dependsOn` / `formedFrom` were retired as structural types in memql#3655
	// and now live on the `as` label axis, so they belong here.
	for _, input := range []string{"", "references", "bogus", "depends", "dependsOn", "formedFrom", "DEPENDS_ON", "formed_from"} {
		if got, ok := canonicalRelationshipType(input); ok {
			t.Errorf("canonicalRelationshipType(%q) = (%q, true), want invalid", input, got)
		}
	}
}
