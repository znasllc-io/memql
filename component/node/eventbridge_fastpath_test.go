package node

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// meshHintWire is a test transport that carries a fast-path hint from a producer
// EventBridge to a consumer EventBridge over the EXACT encode/decode path the
// real mesh uses -- encodeMeshHint -> EventForward -> HandleInbound ->
// decodeMeshHint -> fastPathSink -- but without standing up a gRPC peer
// connection. It is the producer node's meshFastPath: when the producer
// substrate Publishes, it calls PublishHint here, which builds the wire envelope
// exactly as EventBridge.PublishHint does and hands it to the consumer's inbound
// handler synchronously, simulating the broadcast reaching the owning replica.
type meshHintWire struct {
	consumer *EventBridge
}

func (w *meshHintWire) PublishHint(d Deliverable) {
	// Mirror EventBridge.PublishHint's envelope construction so the test
	// exercises encodeMeshHint + decodeMeshHint + the inbound topic routing.
	payload, err := structpb.NewStruct(encodeMeshHint(d))
	if err != nil {
		panic(err)
	}
	w.consumer.HandleInbound(&nodev1.EventForward{
		EventId:      "hint-envelope-" + d.EventID,
		Topic:        meshHintTopic,
		Kind:         int32(d.Kind),
		Ts:           timestamppb.Now(),
		Payload:      payload,
		OriginNodeId: d.OriginNode,
		Ttl:          1,
	})
}

// drainOne reads one deliverable from ch within timeout, failing if none arrives.
func drainOne(t *testing.T, ch <-chan Deliverable, timeout time.Duration, what string) Deliverable {
	t.Helper()
	select {
	case d, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed before a delivery", what)
		}
		return d
	case <-time.After(timeout):
		t.Fatalf("%s: no delivery within %s", what, timeout)
		return Deliverable{}
	}
}

// TestMeshFastPathWakesRemoteSubscriber is the focused proof for memql#1289: a
// deliverable produced on node A wakes a subscriber on node B via the MESH HINT,
// NOT the 250ms durable poll. Producer (A) and consumer (B) share the durable
// store; A's substrate has B's EventBridge wired as its mesh fast-path (through
// the real encode/decode wire). The hint must deliver to B WELL under the
// substratePollInterval (250ms) -- if it only arrived via the poll floor, the
// delivery would land no sooner than ~250ms.
func TestMeshFastPathWakesRemoteSubscriber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()

	// Consumer node B: substrate (durable-only fast-path -- it receives hints, it
	// does not emit them) + an EventBridge whose fast-path sink is B's substrate.
	subB := NewSubstrate(store, time.Minute, nil, testLogger())
	ebB := NewEventBridge(&Identity{ID: "bff-B", Type: NodeTypeBFF}, events.NewBus(), NewPeerManager(&Identity{ID: "bff-B"}, testLogger()), testLogger())
	ebB.SetFastPathSink(subB.HandleFastPath)

	// Producer node A: substrate whose mesh fast-path delivers hints to B.
	subA := NewSubstrate(store, time.Minute, &meshHintWire{consumer: ebB}, testLogger())

	key := testKey("space-1289")

	// B subscribes (it owns the user's WS for this space).
	ch, err := subB.Subscribe(ctx, key, "consumer-B")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Let B's tail goroutine register + finish its initial (empty) drain so it is
	// parked in the select, ready to be woken by a hint.
	time.Sleep(20 * time.Millisecond)

	// A produces a reply. This Append + PublishHint happens far faster than the
	// 250ms poll; if the hint works, B delivers almost immediately.
	start := time.Now()
	if _, err := subA.Publish(ctx, mkDeliverable(key, "evt-1", "graph.node.created.v1:cognition:utterance")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := drainOne(t, ch, time.Second, "mesh-hint delivery")
	elapsed := time.Since(start)

	if got.EventID != "evt-1" {
		t.Fatalf("delivered wrong event: got %q", got.EventID)
	}
	// The crux: it arrived via the hint, not the poll floor. Allow generous
	// scheduling slack but stay well under substratePollInterval (250ms).
	if elapsed >= substratePollInterval {
		t.Fatalf("delivery took %s (>= poll floor %s): hint did not wake the subscriber", elapsed, substratePollInterval)
	}
}

// TestMeshFastPathDedupCollapsesWithDurable proves the exactly-once invariant
// across the real wire (ADR 4.5): the mesh-hint copy and the durable-pull copy
// of the same EventID collapse to EXACTLY ONE delivery, and the cursor advances
// on the durable path so a reconnect does not replay the row.
func TestMeshFastPathDedupCollapsesWithDurable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()

	subB := NewSubstrate(store, time.Minute, nil, testLogger())
	ebB := NewEventBridge(&Identity{ID: "bff-B", Type: NodeTypeBFF}, events.NewBus(), NewPeerManager(&Identity{ID: "bff-B"}, testLogger()), testLogger())
	ebB.SetFastPathSink(subB.HandleFastPath)
	subA := NewSubstrate(store, time.Minute, &meshHintWire{consumer: ebB}, testLogger())

	key := testKey("space-dedup")

	ch, err := subB.Subscribe(ctx, key, "consumer-B")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// Produce three events on A. Each fires a hint to B (fast path) AND lands in
	// the durable store B polls/drains. The two copies of each EventID must
	// collapse to one delivery.
	for _, eid := range []string{"e1", "e2", "e3"} {
		if _, err := subA.Publish(ctx, mkDeliverable(key, eid, "t")); err != nil {
			t.Fatalf("publish %s: %v", eid, err)
		}
	}

	// Collect deliveries + Ack each (advancing the durable cursor).
	seen := map[string]int{}
	var lastSeq int64
	deadline := time.After(time.Second)
	for len(seen) < 3 {
		select {
		case d := <-ch:
			seen[d.EventID]++
			if d.Seq > lastSeq {
				lastSeq = d.Seq
			}
			if err := subB.Ack(ctx, key, "consumer-B", d.Seq); err != nil {
				t.Fatalf("ack: %v", err)
			}
		case <-deadline:
			t.Fatalf("did not receive all 3 events; got %v", seen)
		}
	}

	// Drain a settle window to catch any erroneous duplicate from the other path.
	settle := time.After(150 * time.Millisecond)
	for {
		select {
		case d := <-ch:
			seen[d.EventID]++
		case <-settle:
			for eid, n := range seen {
				if n != 1 {
					t.Fatalf("event %s delivered %d times; want exactly once (hint+durable must collapse)", eid, n)
				}
			}
			// Cursor advanced on the durable path: a fresh consumer replaying with
			// THIS consumer's acked cursor sees nothing after it.
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			replay, err := subB.Subscribe(ctx2, key, "consumer-B")
			if err != nil {
				t.Fatalf("replay subscribe: %v", err)
			}
			select {
			case d := <-replay:
				t.Fatalf("replay after acked cursor re-delivered seq %d (%s): cursor did not advance on durable path", d.Seq, d.EventID)
			case <-time.After(120 * time.Millisecond):
				// Correct: nothing past the acked cursor.
			}
			return
		}
	}
}

// TestMeshFastPathReplayAcrossReplica proves a DIFFERENT replica taking over the
// space (a new consumerID) still replays the full backlog from seq 0 -- the mesh
// fast-path is best-effort and never the source of truth, so a replica that
// never saw the live hints still catches up entirely from the durable outbox
// (ADR 4.4 + 4.5). This is the cross-replica-restart guarantee #1289 must keep.
func TestMeshFastPathReplayAcrossReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newFakeOutboxStore()

	// Producer A with a fast-path pointed at the OLD replica B (which we will not
	// even subscribe -- the hints fall on the floor, simulating the old replica
	// being gone). The durable rows still land in the shared store.
	ebDead := NewEventBridge(&Identity{ID: "bff-dead", Type: NodeTypeBFF}, events.NewBus(), NewPeerManager(&Identity{ID: "bff-dead"}, testLogger()), testLogger())
	// No sink set on ebDead: an inbound hint is a no-op, exactly like a replica
	// with no live subscription for the key.
	subA := NewSubstrate(store, time.Minute, &meshHintWire{consumer: ebDead}, testLogger())

	key := testKey("space-replay")
	for _, eid := range []string{"r1", "r2", "r3"} {
		if _, err := subA.Publish(ctx, mkDeliverable(key, eid, "t")); err != nil {
			t.Fatalf("publish %s: %v", eid, err)
		}
	}

	// A NEW replica C subscribes with a fresh consumerID -- it never saw the live
	// hints, so it must replay the entire backlog from the durable store.
	subC := NewSubstrate(store, time.Minute, nil, testLogger())
	ch, err := subC.Subscribe(ctx, key, "consumer-C")
	if err != nil {
		t.Fatalf("subscribe C: %v", err)
	}

	want := []string{"r1", "r2", "r3"}
	for i, eid := range want {
		got := drainOne(t, ch, time.Second, "replay row "+eid)
		if got.EventID != eid {
			t.Fatalf("replay order wrong at %d: got %q want %q", i, got.EventID, eid)
		}
		if got.Seq != int64(i+1) {
			t.Fatalf("replay seq wrong for %s: got %d want %d", eid, got.Seq, i+1)
		}
	}
}

// TestMeshHintEnvelopeRoundTrip is a direct unit check that encodeMeshHint /
// decodeMeshHint preserve every Deliverable field across the structpb wire
// (numbers survive the float64 round-trip; the routing key + EventID gate
// rejects a malformed hint).
func TestMeshHintEnvelopeRoundTrip(t *testing.T) {
	orig := Deliverable{
		EventID:    "evt-xyz",
		Key:        RoutingKey{Kind: "space", ID: "s-1"},
		Seq:        42,
		Topic:      "graph.node.created.v1:cognition:utterance",
		Kind:       events.KindNodeCreated,
		Payload:    map[string]any{"id": "evt-xyz", "text": "hi"},
		OriginNode: "node-A",
	}

	payload, err := structpb.NewStruct(encodeMeshHint(orig))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, ok := decodeMeshHint(&nodev1.EventForward{Topic: meshHintTopic, Payload: payload})
	if !ok {
		t.Fatal("decode returned ok=false for a well-formed hint")
	}
	if got.EventID != orig.EventID || got.Key != orig.Key || got.Seq != orig.Seq ||
		got.Topic != orig.Topic || got.Kind != orig.Kind || got.OriginNode != orig.OriginNode {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, orig)
	}
	if got.Payload["text"] != "hi" {
		t.Fatalf("payload not preserved: %+v", got.Payload)
	}

	// A malformed hint (missing routing key) is rejected so it is never fed as a
	// bogus delivery.
	bad, _ := structpb.NewStruct(map[string]any{hintKeyEventID: "x"})
	if _, ok := decodeMeshHint(&nodev1.EventForward{Topic: meshHintTopic, Payload: bad}); ok {
		t.Fatal("decode accepted a hint with no routing key")
	}
}
