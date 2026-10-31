package gamespack

import (
	"testing"

	"github.com/znasllc-io/memql/component/node"
)

// A live memory view subscribes to these concepts on whichever replica it is
// attached to; a row written through another replica must reach it.
//
// The rules are registered through Register, the production entry point, so
// the test sees the wiring Register performs. Calling the helper that Register
// invokes would pass with that wiring deleted.
func TestEveryLiveConceptCrossesReplicas(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	for _, concept := range routedConcepts {
		for _, verb := range []string{"created", "updated"} {
			topic := node.GraphEventTopic(verb, concept)
			if !node.ForwardsGraphEvent(topic) {
				t.Errorf("%s is not forwarded; a live view on another replica goes dark", topic)
			}
		}
	}
}

func TestPlanTracesStayOnTheirReplica(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	// Positive control: a player row must cross, or nothing is registered and
	// the plan-trace checks below would pass for the wrong reason.
	if topic := node.GraphEventTopic("created", conceptPlayer); !node.ForwardsGraphEvent(topic) {
		t.Fatalf("%s is not forwarded, so nothing is registered and the plan-trace checks below prove nothing", topic)
	}
	for _, verb := range []string{"created", "updated"} {
		if node.ForwardsGraphEvent(node.GraphEventTopic(verb, conceptPlanTrace)) {
			t.Errorf("plan trace %s events are forwarded; they are one row per plan and nothing watches them live", verb)
		}
	}
}
