package events

import (
	"sync/atomic"
	"testing"
)

// Browser streams repeatedly subscribe and unsubscribe during a node's life.
// That churn must not replace the automation subscriptions registered at boot.
func TestBusPersistentSubscriptionSurvivesClientChurn(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	var deliveries atomic.Int32
	stop := bus.Subscribe("graph.node.created.v1:platform:inboundRequest", func(Event) {
		deliveries.Add(1)
	}, WithSubscriberName("automation:triggerPipelinesOnGitHubDelivery"))
	defer stop()

	// Cross the former three-character ID's entire 26*10*26 namespace twice.
	for range 2 * 26 * 10 * 26 {
		unsubscribe := bus.Subscribe("graph.node.created.v1:worker:registration", func(Event) {})
		unsubscribe()
	}
	bus.PublishSync(NewEvent("graph.node.created.v1:platform:inboundRequest", KindNodeCreated, map[string]any{"id": "delivery"}))
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("persistent webhook subscription received %d events after browser subscription churn, want 1", got)
	}
	stop()
	bus.PublishSync(NewEvent("graph.node.created.v1:platform:inboundRequest", KindNodeCreated, nil))
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("unsubscribed webhook handler still ran: %d deliveries", got)
	}
}
