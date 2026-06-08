package planner

import "testing"

// The per-cycle planner iteration cap is the lone loop bound the cost-control
// audit (memql#1143) flagged as hard-coded; it is now env-tunable.
func TestMaxPlannerIterationsPerCycle(t *testing.T) {
	if got := maxPlannerIterationsPerCycle(); got != defaultMaxPlannerIterations {
		t.Fatalf("with no env override the cap must be the default %d, got %d", defaultMaxPlannerIterations, got)
	}

	t.Setenv("MEMQL_PLANNER_MAX_ITERATIONS_PER_CYCLE", "2")
	if got := maxPlannerIterationsPerCycle(); got != 2 {
		t.Fatalf("env override must apply, want 2 got %d", got)
	}

	// A non-positive / non-numeric value falls back to the default (a 0 cap
	// would strand every Plan immediately).
	for _, bad := range []string{"0", "-1", "nope", ""} {
		t.Setenv("MEMQL_PLANNER_MAX_ITERATIONS_PER_CYCLE", bad)
		if got := maxPlannerIterationsPerCycle(); got != defaultMaxPlannerIterations {
			t.Fatalf("invalid override %q must fall back to default %d, got %d", bad, defaultMaxPlannerIterations, got)
		}
	}
}
