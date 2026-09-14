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

// TestFilterWalkSeesV1StartsWithPredicates: the same clause as the edition-2026
// codemod writes it (epic memql#5363). The walk reads a v1 clause's leaves off
// the parsed tree, so each comparison arrives with the parameter root removed
// -- the head the gates inspect is still the field, and hasFilterOperator still
// sees the keyword.
func TestFilterWalkSeesV1StartsWithPredicates(t *testing.T) {
	src := "query codeMetric q {\n" +
		"  filter  row => row.bucket == args.bucket\n" +
		"          && ((args.codeReference == nil || row.codeReference == args.codeReference) || row.codeReference startsWith args.prefixes)\n" +
		"  shape   codeMetricFull\n}\n"
	var preds []string
	walkFilterPredicates("fixture/queries.memql", src, func(_ string, _ int, pred string) {
		preds = append(preds, pred)
	})
	var heads []string
	sawStartsWith := false
	for _, p := range preds {
		head, _ := splitFilterRef(p)
		heads = append(heads, head)
		if p == "codeReference startsWith args.prefixes" && hasFilterOperator(p) {
			sawStartsWith = true
		}
	}
	for _, want := range []string{"bucket", "codeReference"} {
		if !containsHead(heads, want) {
			t.Errorf("walk heads = %v (predicates %q), missing %q", heads, preds, want)
		}
	}
	if !sawStartsWith {
		t.Errorf("the startsWith comparison did not arrive as one predicate: %q", preds)
	}
}
