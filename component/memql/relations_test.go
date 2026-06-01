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
		"dependsOn":     relationshipTypeDependsOn,
		"formedFrom":    relationshipTypeFormedFrom,
		// case / underscore insensitivity (normalized lower + strip "_").
		"DEPENDS_ON":  relationshipTypeDependsOn,
		"dependson":   relationshipTypeDependsOn,
		"formed_from": relationshipTypeFormedFrom,
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

	for _, input := range []string{"", "references", "bogus", "depends"} {
		if got, ok := canonicalRelationshipType(input); ok {
			t.Errorf("canonicalRelationshipType(%q) = (%q, true), want invalid", input, got)
		}
	}
}
