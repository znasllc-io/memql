package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// busSink records every event the consumer bff re-publishes onto its local bus.
// It stands in for the browser's gRPC subscription, which consumes the local
// bus -- the moment a deliverable lands here, the WS-owning bff would forward it
// out over /memql/ws.
type busSink struct {
	mu     sync.Mutex
	events []events.Event
}

func (s *busSink) publish(e events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *busSink) snapshot() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Event(nil), s.events...)
}

// newConsumerDelivery builds a bff-side ChatReplyDelivery wired to a shared
// store + bus sink, with the run-loop context primed so ensureSubscribed works
// without going through the full component lifecycle.
func newConsumerDelivery(t *testing.T, ctx context.Context, nodeID string, store outboxStore, sink *busSink) *ChatReplyDelivery {
	t.Helper()
	d := NewChatReplyDelivery(
		&Identity{ID: nodeID, Type: NodeTypeBFF},
		NewSubstrate(store, time.Minute, nil, nil),
		events.NewBus(),
		true, // isBFF
		nil,
	)
	if d == nil {
		t.Fatal("NewChatReplyDelivery returned nil")
	}
	d.republish = sink.publish
	d.ctx = ctx
	return d
}

// TestChatReplyCrossReplicaDelivery is the focused proof for memql#1264: a chat
// reply PRODUCED ON ONE NODE is delivered exactly once to a subscriber anchored
// on a DIFFERENT bff replica -- the live cross-replica drop the star mesh caused.
//
// It mirrors the #1261 cluster-gate intent in-process: producer node A
// (cognition/agent/worker) and consumer node B (the bff owning the browser WS)
// share ONLY the durable outbox store -- there is NO mesh connection between
// them, exactly the star-topology blind spot where the old path drops. With the
// substrate, B still receives the reply, in order, exactly once.
//
// On the PRE-#1264 path this fails: the reply lands on A's local bus and is
// pushed over the mesh, which on the star cannot reach B (B is not A's parent,
// and #1245 skips it) -- B's browser never sees it. Here B subscribes to the
// space's durable stream and receives it regardless of who produced it.
func TestChatReplyCrossReplicaDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One durable store, shared across replicas (the Postgres backbone). No mesh
	// link between producer and consumer -- the substrate is the only path.
	store := newFakeOutboxStore()
	const spaceID = "v1:cognition:space:abc"
	key := RoutingKey{Kind: "space", ID: spaceID}

	// Consumer: bff-2 owns the browser's WebSocket for this space. It subscribes
	// to the space (as it would on observing the user's own local WS traffic)
	// and fans the durable stream back onto its local bus.
	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-2", store, sink)
	consumer.ensureSubscribed(key)

	// Producer: a reply is produced on cognition (a different node entirely),
	// which durably Publishes it keyed by the space.
	producer := NewChatReplyDelivery(
		&Identity{ID: "cognition-1", Type: NodeTypeCognition},
		NewSubstrate(store, time.Minute, nil, nil),
		events.NewBus(),
		false, // not bff: producer-only
		nil,
	)
	if producer == nil {
		t.Fatal("producer NewChatReplyDelivery returned nil")
	}

	replyTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance)
	producer.publishDeliverable(ctx, Deliverable{
		EventID:    "utterance:reply-1",
		Key:        key,
		Topic:      replyTopic,
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "utterance:reply-1", "spaceId": spaceID, "text": "Hi from Sofia"},
		OriginNode: "cognition-1",
	})

	// The reply must reach bff-2's local bus exactly once.
	got := waitForEvents(t, sink, 1)
	if got[0].Topic != replyTopic {
		t.Fatalf("delivered wrong topic: got %q want %q", got[0].Topic, replyTopic)
	}
	if id, _ := got[0].Payload["id"].(string); id != "utterance:reply-1" {
		t.Fatalf("delivered wrong payload id: got %q", id)
	}
	// Stamped remote so the local EventBridge will NOT loop it back onto the mesh.
	if !got[0].IsRemote() {
		t.Fatalf("re-published event must be marked remote (OriginNodeId set), got %q", got[0].OriginNodeId)
	}

	// Cursor advanced past the delivered seq -- a reconnect would not replay it.
	if err := waitForCursor(t, store, key, "bff-2", 1); err != nil {
		t.Fatalf("cursor did not advance after delivery: %v", err)
	}
}

// TestPresenceTransitionsAllDeliverDespiteStableRowID is the regression proof
// for memql#1324 ("assistant stuck at Idle on the cluster"). Presence is an
// upsert-style concept: every state transition for a participant re-inserts
// under the SAME deterministic row id, and the outbox enforces uniqueness on
// EventID. With EventID = bare row id, only the FIRST write per participant
// ever landed durably -- every later transition (idle -> thinking ->
// responding -> idle) hit ON CONFLICT DO NOTHING and silently vanished, so the
// browser's presence pill froze on whatever state arrived first.
//
// The producer side must therefore key the deliverable per OCCURRENCE (row id
// + emission timestamp), so each transition of the same logical row is its own
// durable row and all of them reach the consumer bff, in order.
func TestPresenceTransitionsAllDeliverDespiteStableRowID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	const spaceID = "v1:cognition:space:abc"
	key := RoutingKey{Kind: "space", ID: spaceID}

	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-2", store, sink)
	consumer.ensureSubscribed(key)

	producer := NewChatReplyDelivery(
		&Identity{ID: "cognition-1", Type: NodeTypeCognition},
		NewSubstrate(store, time.Minute, nil, nil),
		events.NewBus(),
		false, // not bff: producer-only
		nil,
	)
	if producer == nil {
		t.Fatal("producer NewChatReplyDelivery returned nil")
	}

	// The SAME presence row id transitions through three states -- exactly what
	// the cognition handler writes across one agent turn.
	presenceTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptPresence)
	const presenceRowID = "v1:cognition:participant:presence:sofia"
	base := time.Now()
	for i, state := range []string{"idle", "thinking", "responding"} {
		producer.onLocalEvent(ctx, events.Event{
			Topic: presenceTopic,
			Kind:  events.KindNodeCreated,
			// Distinct emission instants, as the engine stamps via
			// events.NewEvent on each write (explicit here for determinism).
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Payload: map[string]any{
				"id":      presenceRowID,
				"spaceId": spaceID,
				"state":   state,
			},
		})
	}

	got := waitForEvents(t, sink, 3)
	for i, want := range []string{"idle", "thinking", "responding"} {
		if state, _ := got[i].Payload["state"].(string); state != want {
			t.Fatalf("delivery %d: got state %q want %q (all transitions of a stable row id must deliver, in order)", i, state, want)
		}
	}
}

// TestTextChunksStreamToWSOwningReplica is the regression proof for memql#1326
// ("chat streaming dead on the cluster"). The agent text-chat path streams
// reply tokens as v1:cognition:text:chunk graph inserts on the cognition node;
// before #1326 those were not chat-reply topics, rode only the star mesh, and
// structurally never reached a bff replica the producer had no dial to -- the
// browser saw nothing until the committed utterance landed. Chunks must ride
// the substrate like the utterance they precede, in order, with the committed
// utterance after the turn's last chunk on the same space key.
func TestTextChunksStreamToWSOwningReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	const spaceID = "v1:cognition:space:abc"
	key := RoutingKey{Kind: "space", ID: spaceID}

	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-2", store, sink)
	consumer.ensureSubscribed(key)

	producer := NewChatReplyDelivery(
		&Identity{ID: "cognition-1", Type: NodeTypeCognition},
		NewSubstrate(store, time.Minute, nil, nil),
		events.NewBus(),
		false, // not bff: producer-only
		nil,
	)
	if producer == nil {
		t.Fatal("producer NewChatReplyDelivery returned nil")
	}

	chunkTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptTextChunk)
	replyTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance)
	base := time.Now()
	for i, text := range []string{"Hello", " there"} {
		producer.onLocalEvent(ctx, events.Event{
			Topic:     chunkTopic,
			Kind:      events.KindNodeCreated,
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Payload: map[string]any{
				"id":      fmt.Sprintf("v1:cognition:text:chunk:text-chunk-%d", i),
				"spaceId": spaceID,
				"replyId": "utterance:reply-7",
				"text":    text,
				"index":   i,
			},
		})
	}
	producer.onLocalEvent(ctx, events.Event{
		Topic:     replyTopic,
		Kind:      events.KindNodeCreated,
		Timestamp: base.Add(5 * time.Millisecond),
		Payload: map[string]any{
			"id":      "utterance:reply-7",
			"spaceId": spaceID,
			"text":    "Hello there",
		},
	})

	got := waitForEvents(t, sink, 3)
	for i, want := range []string{"Hello", " there"} {
		if got[i].Topic != chunkTopic {
			t.Fatalf("delivery %d: got topic %q want chunk topic %q", i, got[i].Topic, chunkTopic)
		}
		if text, _ := got[i].Payload["text"].(string); text != want {
			t.Fatalf("delivery %d: got chunk text %q want %q", i, text, want)
		}
	}
	if got[2].Topic != replyTopic {
		t.Fatalf("committed utterance must deliver after the turn's last chunk; got topic %q", got[2].Topic)
	}
}

// TestClientToolRequestSurvivesDepeeredFastPath is the focused proof for
// memql#1846: the cross-node client-tool relay REQUEST leg (cognition -> browser)
// reaches the WS-owning bff replica EVEN WHEN the mesh fast-path cannot deliver
// it -- the de-peer window after a bff blue-green cutover.
//
// The scenario is the request-leg twin of TestChatReplyCrossReplicaDelivery:
// producer node (cognition) and consumer node (the bff owning the browser WS)
// share ONLY the durable outbox store -- there is NO mesh connection between
// them (modelling the de-peered/reaped peer whose Connection==nil, so
// forwardToPeers silently skips it). On the pre-#1846 path the request rode the
// mesh fast-path ONLY and the WS-owning replica never saw it, so the UI takeover
// silently dropped. With the request concept on the substrate, the WS-owning bff
// still pulls it durably, keyed by space, and fans it onto its local bus where
// the browser's space subscription dispatches the tool.
func TestClientToolRequestSurvivesDepeeredFastPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One durable store, shared across replicas. NO mesh link between producer
	// and consumer -- the de-peer window, where the substrate is the only path.
	store := newFakeOutboxStore()
	const spaceID = "v1:cognition:space:takeover"
	key := RoutingKey{Kind: "space", ID: spaceID}

	// Consumer: bff-2 owns the browser's WebSocket for this space (the freshly
	// cut-over replica that cognition has de-peered). It subscribes to the space
	// and fans the durable stream back onto its local bus.
	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-2", store, sink)
	consumer.ensureSubscribed(key)

	// Producer: cognition relays a client-tool REQUEST (uiRequestControl) by
	// emitting a v1:cognition:client:tool:request graph node keyed by the space.
	producer := NewChatReplyDelivery(
		&Identity{ID: "cognition-1", Type: NodeTypeCognition},
		NewSubstrate(store, time.Minute, nil, nil),
		events.NewBus(),
		false, // not bff: producer-only
		nil,
	)
	if producer == nil {
		t.Fatal("producer NewChatReplyDelivery returned nil")
	}

	requestTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptClientToolRequest)
	const callID = "call-takeover-1"
	// Drive it through onLocalEvent (not publishDeliverable directly) so the test
	// exercises the real producer path: the topic gate + space-key resolution +
	// per-occurrence EventID keying.
	producer.onLocalEvent(ctx, events.Event{
		Topic:     requestTopic,
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now(),
		Payload: map[string]any{
			"id":       callID, // request row id is the callId
			"callId":   callID,
			"toolName": "uiRequestControl",
			"spaceId":  spaceID,
		},
	})

	// The request must reach bff-2's local bus -- the browser dispatches it there.
	got := waitForEvents(t, sink, 1)
	if got[0].Topic != requestTopic {
		t.Fatalf("delivered wrong topic: got %q want %q", got[0].Topic, requestTopic)
	}
	if cid, _ := got[0].Payload["callId"].(string); cid != callID {
		t.Fatalf("delivered wrong callId: got %q want %q", cid, callID)
	}
	// Stamped remote so the local EventBridge will NOT loop it back onto the mesh.
	if !got[0].IsRemote() {
		t.Fatalf("re-published request must be marked remote, got %q", got[0].OriginNodeId)
	}
	// Cursor advanced past the delivered seq -- a reconnect would not replay it.
	if err := waitForCursor(t, store, key, "bff-2", 1); err != nil {
		t.Fatalf("cursor did not advance after delivery: %v", err)
	}
}

// TestClientToolRequestExactlyOnceAcrossPaths proves the browser never
// dispatches a relayed client-tool request twice when the mesh fast-path hint
// and the durable pull both carry the SAME request occurrence (the common case:
// the de-peer window is partial, not total). The producer keys the Deliverable
// EventID per occurrence (rowId@ts) on BOTH paths, so the substrate's
// per-subscription dedup collapses them to one delivery.
func TestClientToolRequestExactlyOnceAcrossPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	const spaceID = "v1:cognition:space:dup-takeover"
	key := RoutingKey{Kind: "space", ID: spaceID}

	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-1", store, sink)
	consumer.ensureSubscribed(key)

	dl := Deliverable{
		EventID:    "call-dup@1700000000000000000",
		Key:        key,
		Topic:      events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptClientToolRequest),
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "call-dup", "callId": "call-dup", "spaceId": spaceID},
		OriginNode: "cognition-1",
	}
	sub := NewSubstrate(store, time.Minute, nil, nil)
	// Same EventID published twice (mesh-hint copy + durable re-produce); the
	// outbox idempotently no-ops the second, so the browser sees it once.
	if _, err := sub.Publish(ctx, dl); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if _, err := sub.Publish(ctx, dl); err != nil {
		t.Fatalf("publish 2: %v", err)
	}

	got := waitForEvents(t, sink, 1)
	if len(got) != 1 {
		t.Fatalf("exactly-once violated: delivered %d copies", len(got))
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(sink.snapshot()); n != 1 {
		t.Fatalf("exactly-once violated: delivered %d copies", n)
	}
}

// waitForCursor polls until the stored cursor for (key, consumer) reaches at
// least want, or times out.
func waitForCursor(t *testing.T, store *fakeOutboxStore, key RoutingKey, consumer string, want int64) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cur, _ := store.LoadCursor(context.Background(), key, consumer); cur >= want {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	cur, _ := store.LoadCursor(context.Background(), key, consumer)
	return fmt.Errorf("cursor=%d want>=%d", cur, want)
}

// TestChatReplyExactlyOnceAcrossPaths proves the substrate's per-subscription
// dedup: the SAME reply arriving twice (e.g. a durable replay racing a
// re-produce) is delivered to the browser exactly once.
func TestChatReplyExactlyOnceAcrossPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	key := RoutingKey{Kind: "space", ID: "space:dup"}

	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-1", store, sink)
	consumer.ensureSubscribed(key)

	dl := Deliverable{
		EventID:    "utterance:once",
		Key:        key,
		Topic:      events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance),
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "utterance:once", "spaceId": "space:dup"},
		OriginNode: "agent-9",
	}
	sub := NewSubstrate(store, time.Minute, nil, nil)
	// Publish the same EventID twice; the outbox idempotently no-ops the second.
	if _, err := sub.Publish(ctx, dl); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if _, err := sub.Publish(ctx, dl); err != nil {
		t.Fatalf("publish 2: %v", err)
	}

	got := waitForEvents(t, sink, 1)
	if len(got) != 1 {
		t.Fatalf("exactly-once violated: delivered %d copies", len(got))
	}
	// Give a duplicate a chance to (wrongly) arrive before declaring success.
	time.Sleep(50 * time.Millisecond)
	if n := len(sink.snapshot()); n != 1 {
		t.Fatalf("exactly-once violated: delivered %d copies", n)
	}
}

// TestChatReplyPerSpaceOrdering proves per-space ordering: replies produced in
// sequence on a space arrive at the browser in produce order.
func TestChatReplyPerSpaceOrdering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	key := RoutingKey{Kind: "space", ID: "space:order"}
	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-1", store, sink)
	consumer.ensureSubscribed(key)

	sub := NewSubstrate(store, time.Minute, nil, nil)
	want := []string{"u1", "u2", "u3", "u4"}
	for _, id := range want {
		if _, err := sub.Publish(ctx, Deliverable{
			EventID: id,
			Key:     key,
			Topic:   events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance),
			Kind:    events.KindNodeCreated,
			Payload: map[string]any{"id": id, "spaceId": "space:order"},
		}); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}

	got := waitForEvents(t, sink, len(want))
	for i, e := range got {
		if id, _ := e.Payload["id"].(string); id != want[i] {
			t.Fatalf("order violated at %d: got %q want %q", i, id, want[i])
		}
	}
}

// TestChatReplySelfEchoSuppressed proves the bff does NOT re-publish a
// deliverable it PRODUCED itself. The original local-bus event already delivered
// that row to this replica's browsers (handleBusEvent does not dedup by id), so
// re-emitting the durable copy would double-deliver it. The cursor must still
// advance past the self-produced row so a reconnect does not replay it.
func TestChatReplySelfEchoSuppressed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	key := RoutingKey{Kind: "space", ID: "space:self"}
	sink := &busSink{}
	// The consumer bff is "bff-self": it both owns the WS and produced the row.
	consumer := newConsumerDelivery(t, ctx, "bff-self", store, sink)
	consumer.ensureSubscribed(key)

	sub := NewSubstrate(store, time.Minute, nil, nil)
	// A self-produced row (origin == the consumer node) followed by a remote one.
	if _, err := sub.Publish(ctx, Deliverable{
		EventID: "u-self", Key: key,
		Topic:      events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance),
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "u-self", "spaceId": "space:self"},
		OriginNode: "bff-self",
	}); err != nil {
		t.Fatalf("publish self: %v", err)
	}
	if _, err := sub.Publish(ctx, Deliverable{
		EventID: "u-remote", Key: key,
		Topic:      events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance),
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "u-remote", "spaceId": "space:self"},
		OriginNode: "cognition-7",
	}); err != nil {
		t.Fatalf("publish remote: %v", err)
	}

	// Only the remote row is re-published; the self row is suppressed.
	got := waitForEvents(t, sink, 1)
	time.Sleep(50 * time.Millisecond)
	all := sink.snapshot()
	if len(all) != 1 {
		ids := make([]string, 0, len(all))
		for _, e := range all {
			id, _ := e.Payload["id"].(string)
			ids = append(ids, id)
		}
		t.Fatalf("expected exactly 1 re-published event (remote only), got %d: %v", len(all), ids)
	}
	if id, _ := got[0].Payload["id"].(string); id != "u-remote" {
		t.Fatalf("re-published the wrong row: got %q want u-remote", id)
	}
	// Cursor advanced past BOTH rows (self row Acked even though not re-published).
	if err := waitForCursor(t, store, key, "bff-self", 2); err != nil {
		t.Fatalf("cursor must advance past the suppressed self row too: %v", err)
	}
}

// TestChatReplyTopicGate documents which topics the chat-reply migration owns:
// utterance / presence / canvasState (created+updated). Everything else stays
// on the mesh forward (scope boundary: RPC=#1265, streaming=#1266).
//
// The concept ids are pinned as LITERALS here on purpose: the constants held a
// wrong presence id (v1:cognition:presence, a concept that does not exist) from
// #1264 until the #1316 cluster probe caught it -- a symbolic comparison can
// never catch a constant that drifted from the DSL.
func TestChatReplyTopicGate(t *testing.T) {
	literals := map[string]string{
		"utterance":         "v1:cognition:utterance",
		"presence":          "v1:cognition:participant:presence",
		"textChunk":         "v1:cognition:text:chunk",
		"canvasState":       "v1:copresent:canvasState",
		"clientToolRequest": "v1:cognition:client:tool:request",
	}
	if conceptUtterance != literals["utterance"] ||
		conceptPresence != literals["presence"] ||
		conceptTextChunk != literals["textChunk"] ||
		conceptCanvasState != literals["canvasState"] ||
		conceptClientToolRequest != literals["clientToolRequest"] {
		t.Fatalf("chat-reply concept ids drifted from the DSL: got %q/%q/%q/%q/%q",
			conceptUtterance, conceptPresence, conceptTextChunk, conceptCanvasState, conceptClientToolRequest)
	}
	on := []string{
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance),
		events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptUtterance),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptPresence),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptTextChunk),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptCanvasState),
		events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptCanvasState),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptClientToolRequest),
		events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptClientToolRequest),
	}
	for _, topic := range on {
		if !isChatReplyTopic(topic) {
			t.Fatalf("topic %q must be on the substrate path", topic)
		}
	}
	off := []string{
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, "v1:cluster:node"),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, "v1:planner:plan"),
		events.BuildTopicWithConcept(events.TopicGraphNodeDeleted, conceptUtterance),
		"automation.completed",
	}
	for _, topic := range off {
		if isChatReplyTopic(topic) {
			t.Fatalf("topic %q must NOT be diverted to the substrate", topic)
		}
	}
}

// TestSpaceInterestTopicGate documents the consumer-side subscription triggers
// (memql#1316): participant / session created+updated signal space interest,
// and chat-reply topics are NOT interest topics (they have their own path).
func TestSpaceInterestTopicGate(t *testing.T) {
	on := []string{
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptParticipant),
		events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptParticipant),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptSession),
		events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptSession),
	}
	for _, topic := range on {
		if !isSpaceInterestTopic(topic) {
			t.Fatalf("topic %q must be a space-interest trigger", topic)
		}
		if isChatReplyTopic(topic) {
			t.Fatalf("topic %q must NOT be on the substrate publish path", topic)
		}
	}
	off := []string{
		events.BuildTopicWithConcept(events.TopicGraphNodeDeleted, conceptParticipant),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance),
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, "v1:cluster:node"),
		"automation.completed",
	}
	for _, topic := range off {
		if isSpaceInterestTopic(topic) {
			t.Fatalf("topic %q must NOT be a space-interest trigger", topic)
		}
	}
}

// TestSpaceInterestSubscribesWithoutPublishing is the focused proof for the
// memql#1316 passive-subscriber fix: a LOCAL session-created event (what every
// client writes on opening a space) subscribes the bff to the space's durable
// stream -- so a reply produced on ANOTHER node reaches this replica's browser
// -- while the interest event itself is never published to the substrate.
func TestSpaceInterestSubscribesWithoutPublishing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	const spaceID = "v1:cognition:space:interest"
	key := RoutingKey{Kind: "space", ID: spaceID}
	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-2", store, sink)

	// The client opens the space on bff-2: join + create-session execute on
	// bff-2's engine and land as LOCAL events. No chat-reply event exists yet.
	sessionTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptSession)
	consumer.onLocalEvent(ctx, events.Event{
		Topic:   sessionTopic,
		Kind:    events.KindNodeCreated,
		Payload: map[string]any{"id": "session:abc", "spaceId": spaceID},
	})

	// Interest events are consumer-side only: nothing was published durably.
	if rows, _ := store.ReadAfter(ctx, key, 0, 0); len(rows) != 0 {
		t.Fatalf("interest event must not be published to the substrate, found %d rows", len(rows))
	}

	// A reply produced on a different node now reaches bff-2's local bus.
	producer := NewSubstrate(store, time.Minute, nil, nil)
	replyTopic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptUtterance)
	if _, err := producer.Publish(ctx, Deliverable{
		EventID: "utterance:reply-9", Key: key,
		Topic:      replyTopic,
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "utterance:reply-9", "spaceId": spaceID},
		OriginNode: "cognition-1",
	}); err != nil {
		t.Fatalf("publish reply: %v", err)
	}
	got := waitForEvents(t, sink, 1)
	if id, _ := got[0].Payload["id"].(string); id != "utterance:reply-9" {
		t.Fatalf("delivered wrong payload id: got %q", id)
	}
}

// TestSpaceInterestIgnoresRemoteEvents pins the locality rule: a participant /
// session event that arrived FROM A PEER is evidence the space is active
// somewhere, not that this replica anchors a browser for it -- it must not
// subscribe (otherwise every replica converges on every active space).
func TestSpaceInterestIgnoresRemoteEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()
	sink := &busSink{}
	consumer := newConsumerDelivery(t, ctx, "bff-2", store, sink)

	consumer.onLocalEvent(ctx, events.Event{
		Topic:        events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptSession),
		Kind:         events.KindNodeCreated,
		Payload:      map[string]any{"id": "session:remote", "spaceId": "v1:cognition:space:elsewhere"},
		OriginNodeId: "bff-1", // remote: produced on a different replica
	})

	consumer.mu.Lock()
	subbed := len(consumer.subbed)
	consumer.mu.Unlock()
	if subbed != 0 {
		t.Fatalf("remote interest event must not subscribe, got %d subscriptions", subbed)
	}
}

// TestResolveSpaceKey covers the payload shapes the three chat-reply concepts
// carry: utterance/presence flatten spaceId; canvasState flattens space; both
// also work from the nested payload map.
func TestResolveSpaceKey(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
		ok      bool
	}{
		{"utterance spaceId", map[string]any{"spaceId": "s1"}, "space:s1", true},
		{"canvas space", map[string]any{"space": "s2"}, "space:s2", true},
		{"nested payload", map[string]any{"payload": map[string]any{"spaceId": "s3"}}, "space:s3", true},
		{"no space", map[string]any{"foo": "bar"}, "", false},
		{"empty string", map[string]any{"spaceId": ""}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := resolveSpaceKey(tc.payload)
			if ok != tc.ok {
				t.Fatalf("ok: got %v want %v", ok, tc.ok)
			}
			if ok && key.String() != tc.want {
				t.Fatalf("key: got %q want %q", key.String(), tc.want)
			}
		})
	}
}

// waitForEvents polls the sink until it has at least n events or times out.
func waitForEvents(t *testing.T, sink *busSink, n int) []events.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := sink.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events; got %d", n, len(sink.snapshot()))
	return nil
}
