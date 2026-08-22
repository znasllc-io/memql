package automations

import (
	"strings"
	"testing"
)

// TestEvaluateCondition_StartsWithIsRefused pins the fail-loud posture for the
// `startsWith` predicate (memql#4208) on the one surface that does NOT carry
// it. The runtime condition-string grammar knows `==` `!=` `<` `>` and the
// truthy tail; a condition written with `startsWith` has no comparison
// operator this evaluator recognises, so without the named refusal it would
// reach the truthy fallthrough as a non-empty string and fire on every event
// -- the memql#2819 / memql#1396 class.
func TestEvaluateCondition_StartsWithIsRefused(t *testing.T) {
	e := NewEvaluator()
	for _, condition := range []string{
		`payload.codeReference startsWith "integration."`,
		`event.payload.codeReference startsWith ["a.", "b."]`,
		`a == "x" && payload.codeReference startsWith "integration."`,
	} {
		_, err := e.EvaluateCondition(condition)
		if err == nil {
			t.Fatalf("%q: expected a refusal, got nil (the condition would fire on every event)", condition)
		}
		if !strings.Contains(err.Error(), "startsWith") {
			t.Errorf("%q: error should name startsWith: %v", condition, err)
		}
	}
}
