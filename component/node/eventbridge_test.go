package node

import (
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestEventBridge_SuppressInboundChatReply asserts the asymmetric gate that
// memql#1264 adds: with SuppressInboundChatReply(true) (set only on the bff),
// the INBOUND mesh copy of a chat-reply topic is dropped before it hits the
// local bus -- so the browser receives those topics exactly once via the
// durable substrate -- while a NON-chat-reply inbound event still publishes.
// The outbound forward is unaffected (covered by the other forward tests).
func TestEventBridge_SuppressInboundChatReply(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())
	eb.SuppressInboundChatReply(true)

	var mu sync.Mutex
	got := map[string]int{}
	bus.Subscribe("#", func(event events.Event) {
		mu.Lock()
		got[event.Topic]++
		mu.Unlock()
	})

	chatReply := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance)
	other := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, "v1:cluster:node")

	eb.HandleInbound(&nodev1.EventForward{
		EventId: "u-1", Topic: chatReply, Kind: int32(events.KindNodeCreated),
		Ts: timestamppb.New(time.Now()), OriginNodeId: "remote", Ttl: 3,
	})
	eb.HandleInbound(&nodev1.EventForward{
		EventId: "n-1", Topic: other, Kind: int32(events.KindNodeCreated),
		Ts: timestamppb.New(time.Now()), OriginNodeId: "remote", Ttl: 3,
	})

	// Let the async bus deliver.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := got[other] >= 1
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if got[chatReply] != 0 {
		t.Fatalf("chat-reply topic must be suppressed inbound on the bff; got %d", got[chatReply])
	}
	if got[other] != 1 {
		t.Fatalf("non-chat-reply inbound must still publish; got %d", got[other])
	}
}

func TestEventBridge_HandleInbound_PublishesLocally(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	identity := testIdentity()

	eb := NewEventBridge(identity, bus, pm, testLogger())

	// Subscribe to the local bus to capture the forwarded event
	var received events.Event
	var wg sync.WaitGroup
	wg.Add(1)
	bus.Subscribe("graph.node.created.v1:cluster:node", func(event events.Event) {
		received = event
		wg.Done()
	})

	payload, _ := structpb.NewStruct(map[string]any{"nodeType": "cognition"})

	eb.HandleInbound(&nodev1.EventForward{
		EventId:      "evt-123",
		Topic:        "graph.node.created.v1:cluster:node",
		Kind:         int32(events.KindNodeCreated),
		Ts:           timestamppb.New(time.Now()),
		Payload:      payload,
		OriginNodeId: "remote-node",
		Ttl:          3,
	})

	wg.Wait()

	if received.Topic != "graph.node.created.v1:cluster:node" {
		t.Errorf("expected topic graph.node.created.v1:cluster:node, got %s", received.Topic)
	}
	if received.OriginNodeId != "remote-node" {
		t.Errorf("expected OriginNodeId remote-node, got %s", received.OriginNodeId)
	}
	if !received.IsRemote() {
		t.Error("expected IsRemote() to be true")
	}
	if received.Payload["nodeType"] != "cognition" {
		t.Errorf("expected payload nodeType=cognition, got %v", received.Payload["nodeType"])
	}
}

func TestEventBridge_HandleInbound_DedupDuplicates(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	var count int
	var mu sync.Mutex
	bus.Subscribe("#", func(event events.Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	evt := &nodev1.EventForward{
		EventId:      "dup-evt",
		Topic:        "graph.node.created.v1:cluster:node",
		Kind:         int32(events.KindNodeCreated),
		Ts:           timestamppb.New(time.Now()),
		OriginNodeId: "remote",
		Ttl:          3,
	}

	eb.HandleInbound(evt)
	eb.HandleInbound(evt) // duplicate
	eb.HandleInbound(evt) // duplicate

	// Give async handlers time to process
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if count != 1 {
		t.Errorf("expected 1 delivery, got %d (duplicates not deduped)", count)
	}
	mu.Unlock()
}

func TestEventBridge_HandleInbound_TTLExpired(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	var count int
	var mu sync.Mutex
	bus.Subscribe("#", func(event events.Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	eb.HandleInbound(&nodev1.EventForward{
		EventId:      "ttl-evt",
		Topic:        "graph.node.created.v1:cluster:node",
		Kind:         int32(events.KindNodeCreated),
		Ts:           timestamppb.New(time.Now()),
		OriginNodeId: "remote",
		Ttl:          0, // expired
	})

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if count != 0 {
		t.Errorf("expected 0 deliveries for expired TTL, got %d", count)
	}
	mu.Unlock()
}

func TestEventBridge_LocalEventsNotReforwarded(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	identity := testIdentity()
	eb := NewEventBridge(identity, bus, pm, testLogger())

	// Simulate an inbound event (has OriginNodeId set)
	remoteEvent := events.NewEvent(
		"graph.node.created.v1:cluster:node",
		events.KindNodeCreated,
		map[string]any{"test": true},
	)
	remoteEvent.OriginNodeId = "other-node"

	// onLocalEvent should not forward remote events
	eb.onLocalEvent(remoteEvent)

	// No crash, no forward -- test passes if we get here
}

func TestEventBridge_OnLocalEvent_BlockedTopics(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	// These should be blocked by default routing rules
	blockedTopics := []string{
		"automation.started",
		"telemetry.metrics",
		"session.opened",
		"query.executed",
	}

	for _, topic := range blockedTopics {
		event := events.NewEvent(topic, events.KindUnspecified, nil)
		eb.onLocalEvent(event) // should not panic or forward
	}
}

func TestEvent_IsRemote(t *testing.T) {
	local := events.NewEvent("test", events.KindUnspecified, nil)
	if local.IsRemote() {
		t.Error("local event should not be remote")
	}

	remote := events.NewEvent("test", events.KindUnspecified, nil)
	remote.OriginNodeId = "other-node"
	if !remote.IsRemote() {
		t.Error("event with OriginNodeId should be remote")
	}
}

func TestEvent_Clone_PreservesOriginNodeId(t *testing.T) {
	original := events.NewEvent("test", events.KindNodeCreated, map[string]any{"a": 1})
	original.OriginNodeId = "node-42"

	cloned := original.Clone()

	if cloned.OriginNodeId != "node-42" {
		t.Errorf("Clone should preserve OriginNodeId, got %q", cloned.OriginNodeId)
	}
}

// TestEventBridge_PayloadSurvivesCrossNodeRoundTrip (memql#1065 diagnostic)
//
// On every login the IDENTITY node inserts a v1:identity:authSession row;
// executeInsert publishes graph.node.created with the flattened payload
// (subject at the top level AND under the nested "payload" key). The
// ensureDailySpaceOnAuthSession automation runs on the cognition/BFF node,
// so the event has to ride the EventBridge structpb serialization to get
// there. This test mirrors that exact payload shape and proves the nested
// "payload.subject" the logic reads survives the structpb.NewStruct ->
// AsMap round-trip the bridge performs.
func TestEventBridge_PayloadSurvivesCrossNodeRoundTrip(t *testing.T) {
	const subject = "v1:identity:user:11111111-2222-3333-4444-555555555555"
	// Exactly the eventPayload shape executor_mutation.go builds for an
	// authSession node.created: top-level flattened fields + nested payload.
	nestedPayload := map[string]any{
		"userId":   "", // optional, raced ahead of user-row insert
		"subject":  subject,
		"source":   "bff_exchange",
		"revoked?": false,
	}
	eventPayload := map[string]any{
		"id":        "v1:identity:authSession:abc",
		"nodeId":    "v1:identity:authSession:abc",
		"concept":   "v1:identity:authSession",
		"actor":     "system:identity",
		"nodeType":  "authSession",
		"createdAt": "2026-06-08T00:00:00Z",
		"subject":   subject,
		"userId":    "",
		"payload":   nestedPayload,
	}

	// The bridge serializes via structpb.NewStruct on the producer node.
	st, err := structpb.NewStruct(eventPayload)
	if err != nil {
		t.Fatalf("structpb.NewStruct dropped the whole payload: %v", err)
	}
	// ... and rehydrates via AsMap on the consumer node.
	got := st.AsMap()

	nested, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("nested payload missing after round-trip; got %#v", got["payload"])
	}
	if nested["subject"] != subject {
		t.Fatalf("payload.subject did not survive cross-node hop: got %#v, want %q", nested["subject"], subject)
	}
	if got["subject"] != subject {
		t.Fatalf("top-level subject did not survive cross-node hop: got %#v", got["subject"])
	}
}
