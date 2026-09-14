package memql

// flip_fixture_concept_test.go -- a stub registry's concept, built from a real
// declaration (memql#5368).
//
// Since the flip a query filter is a lambda, and the load lowers it against
// its concept's declared fields (Lower). A concept stubbed as a bare name,
// `{Name: "v1:x:y"}`, has no definition to read, so the load refuses the
// filter before the test reaches what it is about. A fixture that loads a
// filter over a stub concept declares the fields the filter reads, here.

import (
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// fixtureConcept builds the concept id from decl, a `concept` declaration.
func fixtureConcept(t testing.TB, id, decl string) *memoryNodes.Concept {
	t.Helper()
	file, err := languageParser.ParseFile(decl)
	if err != nil {
		t.Fatalf("parse the fixture concept %s: %v", id, err)
	}
	for _, d := range file.Definitions {
		if cd, ok := d.(*languageParser.ConceptDecl); ok {
			c, err := memoryNodes.BuildConceptFromDecl(cd, id)
			if err != nil {
				t.Fatalf("build the fixture concept %s: %v", id, err)
			}
			return c
		}
	}
	t.Fatalf("the fixture for %s declares no concept", id)
	return nil
}
