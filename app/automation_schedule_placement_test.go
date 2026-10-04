package app

import "testing"

// placementNodeTypes is every node type the placement can see, the empty one
// included: an untagged build with no MEMQL_NODE_TYPE.
var placementNodeTypes = []string{"identity", "bff", "agent", "planner", "mcp", "edge", "workbench", ""}

// assertPlacedOnlyOn holds one placed automation to its node type: it fires
// exactly there while that replica holds its own lease, general leadership
// decides every other scheduled automation unchanged, and an absent lease
// never borrows general leadership.
func assertPlacedOnlyOn(t *testing.T, automation, placedOn string) {
	t.Helper()
	for _, nodeType := range placementNodeTypes {
		for _, generalLeader := range []bool{false, true} {
			for _, leaseHeld := range []bool{false, true} {
				leases := map[string]func() bool{automation: func() bool { return leaseHeld }}
				gate := scheduledAutomationGateForNode(nodeType, func() bool { return generalLeader }, leases)
				if got, want := gate(automation), nodeType == placedOn && leaseHeld; got != want {
					t.Fatalf("%s on %q general=%v lease=%v: allowed=%v want=%v", automation, nodeType, generalLeader, leaseHeld, got, want)
				}
				if gate("logsRetentionSweep") != generalLeader {
					t.Fatalf("%s's lease changed general cron leadership on %q", automation, nodeType)
				}
			}
		}
	}
	if scheduledAutomationGateForNode(placedOn, func() bool { return true }, nil)(automation) {
		t.Fatalf("%s: an absent dedicated lease must not borrow general leadership", automation)
	}
}

func TestRepositoryPollRunsOnlyOnElectedWorkbench(t *testing.T) {
	assertPlacedOnlyOn(t, packagePollAutomation, "workbench")
}

// The pipelines poll recovers runs by claiming and driving them, and only an
// agent drives a run (epic memql#5477, plan decision 10).
func TestPipelinesPollRunsOnlyOnElectedAgent(t *testing.T) {
	assertPlacedOnlyOn(t, pipelinesPollAutomation, "agent")
}

// TestALeaseRunsOnlyItsOwnAutomation: the leases are independent, so holding
// one never fires the other placed automation, even where general leadership
// would admit everything.
func TestALeaseRunsOnlyItsOwnAutomation(t *testing.T) {
	for _, c := range []struct{ nodeType, held, other string }{
		{"agent", pipelinesPollAutomation, packagePollAutomation},
		{"workbench", packagePollAutomation, pipelinesPollAutomation},
	} {
		gate := scheduledAutomationGateForNode(c.nodeType, func() bool { return true },
			map[string]func() bool{c.held: func() bool { return true }})
		if !gate(c.held) {
			t.Errorf("%s holding its lease did not run %s", c.nodeType, c.held)
		}
		if gate(c.other) {
			t.Errorf("%s holding the %s lease ran %s", c.nodeType, c.held, c.other)
		}
	}
}

func TestOnlyTheirNodeTypesCreateThePlacedLeases(t *testing.T) {
	for _, nodeType := range placementNodeTypes {
		t.Run(nodeType, func(t *testing.T) {
			t.Setenv("MEMQL_NODE_TYPE", nodeType)
			a := &App{}
			gate := a.scheduledAutomationGate(func() bool { return true })
			expected := 0
			if nodeType == "workbench" || nodeType == "agent" {
				expected = 1
			}
			if len(a.Dependencies) != expected {
				t.Fatalf("%s registered %d scoped leases, want %d", nodeType, len(a.Dependencies), expected)
			}
			// Nothing has started the leaders, so no lease is held anywhere.
			if gate(packagePollAutomation) {
				t.Fatal("a node with no acquired workbench lease must not run the repository poll")
			}
			if gate(pipelinesPollAutomation) {
				t.Fatal("a node with no acquired agent lease must not run the pipelines poll")
			}
		})
	}
}
