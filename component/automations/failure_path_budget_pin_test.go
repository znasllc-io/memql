package automations

import (
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// failure_path_test.go spells the default retry budget as its value so that
// file compiles against the unfixed tree it was first run against; this pins
// the spelling to the constant, so the two cannot drift apart.
func TestTheSpelledDefaultRetryBudgetIsTheConstant(t *testing.T) {
	if defaultRetryBudget != work.DefaultMaxRetries {
		t.Fatalf("failure_path_test.go spells the default budget %d; work.DefaultMaxRetries is %d", defaultRetryBudget, work.DefaultMaxRetries)
	}
}
