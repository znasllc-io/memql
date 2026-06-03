// agent_loop_convergence.go
//
// Convergence / no-progress guard for the Planner Agent loop
// (memql#822). Beyond the raw per-cycle iteration cap
// (maxPlannerIterations) and the cumulative LLM ceiling (#819), this
// catches a model that OSCILLATES -- emitting the SAME non-terminal
// decision over and over without ever progressing toward a terminal
// state. When an identical non-terminal decision repeats past a small
// threshold within a cycle, the loop parks the Plan to awaitingFeedback
// instead of continuing to spin (and spend).
//
// Within-cycle is where a spin actually burns calls: the loop recurses
// up to maxPlannerIterations per event cycle. Across cycles, the #819
// cumulative invocation ceiling (persisted on the Plan) is the backstop,
// and there is no auto cross-cycle re-invoke today (HandlePlanUpdated is
// log-only), so a per-cycle tracker covers the live spin path.
package planner

import (
	"encoding/json"
	"os"
	"strconv"
)

// defaultMaxIdenticalNonTerminalDecisions is how many times the SAME
// non-terminal decision may appear in one cycle before the loop parks.
// 2 means: allow it twice, park on the 3rd identical emission. Override
// with MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS.
const defaultMaxIdenticalNonTerminalDecisions = 2

func maxIdenticalNonTerminalDecisions() int {
	if v := os.Getenv("MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxIdenticalNonTerminalDecisions
}

// convTracker counts identical non-terminal decisions within a single
// planner cycle. Created once per invokeAndDispatch entry and threaded
// through the recursion; single-goroutine per Plan, so no locking.
type convTracker struct {
	seen map[string]int
}

func newConvTracker() *convTracker {
	return &convTracker{seen: map[string]int{}}
}

// recordAndCheck records one decision and reports whether the loop
// should PARK (the same non-terminal decision has now repeated past the
// threshold). Terminal decisions (markPlanSucceeded / markPlanFailed /
// escalate) are never tracked -- they end the loop anyway.
func (c *convTracker) recordAndCheck(d plannerDecision) (park bool, count int) {
	if c == nil || !isNonTerminalDecision(d.Action) {
		return false, 0
	}
	fp := decisionFingerprint(d)
	c.seen[fp]++
	count = c.seen[fp]
	return count > maxIdenticalNonTerminalDecisions(), count
}

// isNonTerminalDecision reports whether an action keeps the loop going
// (and so could spin). Terminal / parking actions are excluded.
func isNonTerminalDecision(action string) bool {
	switch action {
	case "decompose", "dispatchTask", "createSpecialist",
		"extendSpecialist", "mintSkill", "retry", "spawnTrainingPlan":
		return true
	default:
		return false
	}
}

// decisionFingerprint is a stable key for "the same decision." Two
// decisions that differ in any decision-relevant field (e.g. a
// dispatchTask for a different task, a decompose with a different
// outline) fingerprint differently, so only a literally-repeated
// decision counts as non-progress. The canonical JSON of the decision
// IS the key -- it's an in-memory per-cycle map key, so its length is
// irrelevant and no hashing is needed (which also keeps integrations/
// free of crypto/sha256 per the conformance rule).
func decisionFingerprint(d plannerDecision) string {
	b, err := json.Marshal(d)
	if err != nil {
		return d.Action
	}
	return d.Action + ":" + string(b)
}
