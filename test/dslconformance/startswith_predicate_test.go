package dslconformance

import "testing"

// TestFilterWalkSeesV1StartsWithPredicates pins the filter walk on the
// `startsWith` predicate (memql#4208). The walk reads a clause's leaves off the
// parsed tree, so each comparison arrives with the parameter root removed: the
// head the gates inspect is the field on the left, and hasFilterOperator
// recognises the keyword, so the predicate is classified as a comparison
// rather than as a predicate application.
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
	if !hasFilterOperator(`codeReference startsWith "integration."`) {
		t.Error("hasFilterOperator must recognise `startsWith` against a literal")
	}
}

func containsHead(heads []string, want string) bool {
	for _, h := range heads {
		if h == want {
			return true
		}
	}
	return false
}
