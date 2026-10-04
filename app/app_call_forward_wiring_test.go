package app

import (
	"os"
	"strings"
	"testing"
)

// The agent half of the app-call hop has to be WIRED on both ends, and this is
// a source scan for TestAgentWiringInstallsTheAppSessionDelegate's reason: the
// wiring lives behind `//go:build agent`, standing up an agent node needs a
// gRPC server, a worker service and a database, and each half of the hop is
// correct on its own in its unit tests. What goes wrong is a line going away.
//
//   - Without SetAppCallServer, every app call a planner or a sibling agent
//     forwards here is refused as not_configured: the planner's route passes
//     over Claude Code again, now with a reason that blames this node.
//   - Without SetForward, a turn on the replica that does not hold the laptop
//     reports no machine for a laptop the user can see is on -- the coin flip
//     every multi-replica deployment makes on every call.
//
// The planner half is exercised for real by
// TestPlannerInstallsFleetAndAppInferenceOnItsExistingAgentDialer.
func TestAgentWiringServesAndForwardsAppCalls(t *testing.T) {
	source, err := os.ReadFile("cluster_worker.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, ".SetAppCallServer(") {
		t.Fatal("the agent's forward handler has no app door behind it: every app call a planner or " +
			"a sibling replica forwards here is refused before start")
	}
	if !strings.Contains(text, ".SetForward(forwarder") {
		t.Fatal("the agent's app door cannot forward: a turn on the replica that does not hold the " +
			"machine's stream reports no machine for a laptop that is on")
	}
}
