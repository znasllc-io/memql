package memql

import "testing"

// canonicalRelationshipType gates which @relationship types a concept may
// declare. The harness state model (v1:harness:step, #582) declares a
// `dependsOn` DAG edge; this pins that it is accepted (the engine rejected it
// at bootstrap before, taking the whole cluster down -- identity-first), plus
// the existing valid set and a clear rejection for unknown types.
func TestCanonicalRelationshipType(t *testing.T) {
	valid := map[string]string{
		"parent":     relationshipTypeParent,
		"alias":      relationshipTypeAlias,
		"equals":     relationshipTypeEquals,
		"references": relationshipTypeReferences,
		"contains":   relationshipTypeContains,
		"owns":       relationshipTypeOwns,
		"createdBy":  relationshipTypeCreatedBy,
		// case / underscore insensitivity (normalized lower + strip "_").
		"CREATED_BY": relationshipTypeCreatedBy,
		"createdby":  relationshipTypeCreatedBy,
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
	// and now live on the `as` label axis, so they belong here. `interactsWith`
	// joined them in memql#3663, which renamed it to `references` -- a clean
	// break with no alias, so the old spelling must be REFUSED rather than
	// quietly normalised, or a stale declaration would load and behave
	// differently from what it says.
	for _, input := range []string{"", "interactsWith", "interacts_with", "bogus", "depends", "dependsOn", "formedFrom", "DEPENDS_ON", "formed_from"} {
		if got, ok := canonicalRelationshipType(input); ok {
			t.Errorf("canonicalRelationshipType(%q) = (%q, true), want invalid", input, got)
		}
	}
}
