package app

import "testing"

func TestRepositoryPollRunsOnlyOnElectedWorkbench(t *testing.T) {
	for _, nodeType := range []string{"identity", "bff", "agent", "planner", "mcp", "edge", "workbench", ""} {
		for _, generalLeader := range []bool{false, true} {
			for _, pollLeader := range []bool{false, true} {
				gate := scheduledAutomationGateForNode(nodeType, func() bool { return generalLeader }, func() bool { return pollLeader })
				if got, want := gate(packagePollAutomation), nodeType == "workbench" && pollLeader; got != want {
					t.Fatalf("%s general=%v poll=%v: poll allowed=%v want=%v", nodeType, generalLeader, pollLeader, got, want)
				}
				if gate("logsRetentionSweep") != generalLeader {
					t.Fatalf("%s changed general cron leadership", nodeType)
				}
			}
		}
	}
	if scheduledAutomationGateForNode("workbench", func() bool { return true }, nil)(packagePollAutomation) {
		t.Fatal("an absent dedicated lease must not borrow general leadership")
	}
}

func TestOnlyWorkbenchCreatesRepositoryPollLease(t *testing.T) {
	for _, nodeType := range []string{"identity", "bff", "agent", "planner", "mcp", "edge", "workbench", ""} {
		t.Run(nodeType, func(t *testing.T) {
			t.Setenv("MEMQL_NODE_TYPE", nodeType)
			a := &App{}
			gate := a.scheduledAutomationGate(func() bool { return true })
			expected := 0
			if nodeType == "workbench" {
				expected = 1
			}
			if len(a.Dependencies) != expected {
				t.Fatalf("%s registered %d scoped leases, want %d", nodeType, len(a.Dependencies), expected)
			}
			if gate(packagePollAutomation) {
				t.Fatal("a node with no acquired workbench lease must not run the poll")
			}
		})
	}
}
