package memql

import "testing"

// canonicalRelationshipType gates which @relationship types a concept may
// declare. This pins the accepted set and a clear rejection for everything
// else, including the three spellings that were once accepted and no longer
// are: `dependsOn` and `formedFrom`, retired to `as` labels in memql#3655, and
// `interactsWith`, renamed to `references` in memql#3663.
//
// The rejections matter as much as the acceptances. Each retired spelling was
// once a live word, and re-admitting one silently -- as an alias or a
// normalization -- would leave two spellings for a single concept, which is
// exactly the drift memql#3657 removed from the edge-label vocabulary.
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
		"REFERENCES": relationshipTypeReferences,
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
	// and now live on the `as` label axis. `interactsWith` was RENAMED to
	// `references` in memql#3663 -- it is rejected outright rather than
	// normalized, because a silent alias would leave two spellings for one
	// concept. All belong here.
	for _, input := range []string{"", "bogus", "depends", "dependsOn", "formedFrom", "DEPENDS_ON", "formed_from", "interactsWith", "interacts_with"} {
		if got, ok := canonicalRelationshipType(input); ok {
			t.Errorf("canonicalRelationshipType(%q) = (%q, true), want invalid", input, got)
		}
	}
}
