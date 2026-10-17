package node

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// A BFF opens a pipeline run locally; agent replicas hear it through their
// own buses and the real NodeService transport, with no shared subscriptions.
func TestPipelineRunDeliverySurvivesSubscriptionChurnOnEveryReplica(t *testing.T) {
	m := newWireMesh(t)
	m.node("bff-a", NodeTypeBFF)
	m.node("agent-a", NodeTypeAgent)
	m.node("agent-b", NodeTypeAgent)
	m.dials("bff-a", "agent-a", "agent-b")
	m.start()
	m.awaitLinks()

	const topic = "graph.node.created.v1:pipelines:run"
	counts := map[string]*atomic.Int32{}
	for _, id := range []string{"agent-a", "agent-b"} {
		n := &atomic.Int32{}
		counts[id] = n
		t.Cleanup(m.nodes[id].bus.Subscribe(topic, func(event events.Event) {
			if event.Payload["id"] == "v1:pipelines:run:after-churn" {
				n.Add(1)
			}
		}))
	}
	for _, replica := range m.nodes {
		for range 2 * 26 * 10 * 26 {
			unsubscribe := replica.bus.Subscribe("graph.node.created.v1:worker:registration", func(events.Event) {})
			unsubscribe()
		}
	}
	m.nodes["bff-a"].bus.Publish(events.NewEvent(topic, events.KindNodeCreated,
		map[string]any{"id": "v1:pipelines:run:after-churn"}))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if counts["agent-a"].Load() == 1 && counts["agent-b"].Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for id, n := range counts {
		if got := n.Load(); got != 1 {
			t.Errorf("%s received %d pipeline events after subscription churn, want 1", id, got)
		}
	}
}
