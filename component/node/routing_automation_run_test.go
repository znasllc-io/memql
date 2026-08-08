package node

import "testing"

// The automation-run relay topics must cross the mesh, and they sit one
// character away from a tree that must NOT (defaultRoutingRules blocks
// "automation.#"). Block rules evaluate first, so a topic that drifted under
// that prefix would stop forwarding with no test failing anywhere else --
// the relay would simply stop working in cluster mode and keep working
// locally. These assertions are the guard for that.
func TestAutomationRunTopicsAreForwarded(t *testing.T) {
	for _, topic := range []string{
		"automationrun.request",
		"automationrun.trace",
	} {
		forward, broadcast, target := ForwardDecisionFor(topic)
		if !forward {
			t.Fatalf("topic %q must forward across the mesh -- the automation run relay "+
				"(memql#3310) dies silently in cluster mode without it", topic)
		}
		if !broadcast {
			t.Fatalf("topic %q must broadcast: the target node type is a runtime value carried "+
				"in the event, and the trace has to reach whichever node asked (got target %q)",
				topic, target)
		}
	}
}

// The block rule for automation lifecycle chatter must still hold: this pair
// of assertions together says "these two trees are adjacent and behave
// oppositely, on purpose".
func TestAutomationLifecycleTopicsStayLocal(t *testing.T) {
	for _, topic := range []string{
		"automation.started",
		"automation.completed",
		"automation.failed",
	} {
		if forward, _, _ := ForwardDecisionFor(topic); forward {
			t.Fatalf("topic %q must stay node-local", topic)
		}
	}
}
