package dslconformance

import "testing"

// TestSplitterSeesStartsWithPredicates pins the conformance splitter on the
// `startsWith` predicate (memql#4208): the head the gates inspect is the
// field on the left, and hasFilterOperator recognises the keyword so the
// predicate is classified as a comparison rather than as a bare spec
// reference.
func TestSplitterSeesStartsWithPredicates(t *testing.T) {
	clause := `bucket==args.bucket && (when(args.codeReference) { codeReference==args.codeReference } || codeReference startsWith args.prefixes)`
	heads := predicateHeads(clause)
	for _, want := range []string{"bucket", "codeReference"} {
		if !containsHead(heads, want) {
			t.Errorf("predicateHeads(%q) = %v, missing %q", clause, heads, want)
		}
	}
	if !hasFilterOperator(`codeReference startsWith args.prefixes`) {
		t.Error("hasFilterOperator must recognise `startsWith` as a comparison operator")
	}
	if !hasFilterOperator(`codeReference startsWith "integration."`) {
		t.Error("hasFilterOperator must recognise `startsWith` against a literal")
	}
}
