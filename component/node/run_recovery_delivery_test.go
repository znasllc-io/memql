package node

import (
	"context"
	"github.com/znasllc-io/memql/component/events"
	"testing"
	"time"
)

func TestRunRecoveryHintSurvivesRouteGapWithoutBecomingAGraphWrite(t *testing.T) {
	route := evaluateRouting(defaultRoutingRules(), events.TopicWorkRunRecovery)
	if !route.Forward || route.TargetType != NodeTypeAgent {
		t.Fatalf("recovery cannot reach the executing peers: %+v", route)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeOutboxStore()
	sink := &busSink{}
	consumer := newPlannerConsumerDelivery(t, ctx, "agent-recovery", store, sink)
	defer consumer.cleanup()
	producer := newPlanProducer(t, "bff-recovery", NodeTypeBFF, store)
	producer.onLocalEvent(ctx, events.Event{Topic: events.TopicWorkRunRecovery, Kind: events.KindMessage, Timestamp: time.Now(), Payload: map[string]any{"id": "run", "ownerUserId": "owner", "snapshot": "exact-stale-state"}})
	got := waitForEvents(t, sink, 1)
	if got[0].Topic != events.TopicWorkRunRecovery || got[0].Kind != events.KindMessage || got[0].Payload["snapshot"] != "exact-stale-state" || !got[0].IsRemote() {
		t.Fatalf("recovery hint changed across replica: %+v", got[0])
	}
	if err := waitForCursor(t, store, runLifecycleKey, "agent-recovery", 1); err != nil {
		t.Fatal(err)
	}
}
