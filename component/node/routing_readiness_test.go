package node

import (
	"strings"
	"testing"
)

// The OS keeps a live feed of readiness rows on every replica, and the rows
// are written by whichever node evaluated. Without these rules default-deny
// leaves the feed correct on load and frozen after, which looks like it works.
func TestModuleReadinessRowsBroadcast(t *testing.T) {
	for _, topic := range []string{
		"graph.node.created.v1:platform:moduleReadiness",
		"graph.node.updated.v1:platform:moduleReadiness",
	} {
		d := evaluateRouting(defaultRoutingRules(), topic)
		if !d.Forward || !d.Broadcast {
			t.Errorf("%s: want forward+broadcast, got %+v", topic, d)
		}
	}
	// The negative control: a platform concept with no rule stays local.
	if d := evaluateRouting(defaultRoutingRules(), "graph.node.created.v1:platform:moduleVerdict"); d.Forward {
		t.Errorf("moduleVerdict is virtual and must not be routed, got %+v", d)
	}
}

// memql#5325's D4 asked for a deleted rule beside the two above, and it is
// deliberately not taken. Both halves of that decision are pinned here,
// because a missing rule and a forgotten one look identical in routing.go.
func TestTheReadinessDeleteIsExcludedOnPurposeRatherThanForgotten(t *testing.T) {
	const topic = "graph.node.deleted.v1:platform:moduleReadiness"

	if d := evaluateRouting(defaultRoutingRules(), topic); d.Forward {
		t.Errorf("%s must stay local: the fold stops counting a stopped node's row "+
			"inside NodeLiveWindow, so the delete changes no verdict anywhere, got %+v", topic, d)
	}

	// ...and the reason is written down. An exclusion is what separates this
	// from an oversight, and RoutingExclusions requires a non-empty one.
	var reason string
	for _, x := range RoutingExclusions() {
		if x.Pattern == topic {
			reason = x.Reason
			break
		}
	}
	if reason == "" {
		t.Fatalf("%s is not forwarded and carries no recorded reason -- the next reader "+
			"cannot tell the decision from an omission", topic)
	}
	// The rows really are deleted; the exclusion is about the EVENT. A reason
	// that did not say where the deleting happens would read as "readiness
	// rows are never removed", which stopped being true in memql#5325.
	if !strings.Contains(reason, "readiness_row_purge.go") {
		t.Errorf("the reason must name what removes the rows, got: %s", reason)
	}
}
