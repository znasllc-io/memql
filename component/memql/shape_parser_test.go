package memql

import (
	"testing"
)

// Phase G.3.g pt 4 declared-must-be-used rule applied to shapes:
// every concept listed in `@useConcept(...)` must have at least one
// `<conceptName>.X` body path. Stale entries fail loud at parse time.

func TestParseShapeMemQL_RejectsStaleUseConceptEntry(t *testing.T) {
	src := []byte(`@row
@useConcept(spaceA, spaceB)
shape testShape {
  row.id
  spaceA.name
}`)
	_, err := parseShapeMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error for unused @useConcept entry")
	}
	wantSubstr := "spaceB"
	if got := err.Error(); !contains(got, wantSubstr) {
		t.Fatalf("expected error to mention %q, got %q", wantSubstr, got)
	}
}

func TestParseShapeMemQL_AcceptsFullyUsedUseConcept(t *testing.T) {
	src := []byte(`@row
@useConcept(spaceA, spaceB)
shape testShape {
  row.id
  spaceA.name
  spaceB.title
}`)
	_, err := parseShapeMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
