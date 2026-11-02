package node

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/proto"
)

// A ROW TOO LARGE TO BROADCAST (the 2026-11-01 production incident).
//
// A workflow step that bound a 308 KB vendor answer wrote it into its journal
// row twice (`result` and `value`); the row's event carries the row twice (flat
// and under `payload`); an update publishes that event twice (`created` and
// `updated`). One receipt was 2.5 MB of broadcast, and every replica of every
// node type heard about seven copies of each event and decoded every one of
// them. Three briefs in a few seconds took two nodes down. These tests hold the
// line that stops it: the writer's own bus keeps the whole row, and what crosses
// the mesh is the row without its largest fields.

// stepReceiptEvent is the event a step's receipt publishes, with the journal's
// shape: the bound value recorded twice, the row flat and under `payload`. The
// value is what Kraken's status endpoint answers: 850 components, 308 KB.
func stepReceiptEvent(topic string) events.Event {
	components := make([]any, 0, 850)
	for i := range 850 {
		components = append(components, map[string]any{
			"id":                    fmt.Sprintf("component-%d", i),
			"name":                  strings.Repeat("a market pair name ", 6),
			"status":                "operational",
			"created_at":            "2026-11-01T00:00:00.000Z",
			"updated_at":            "2026-11-01T00:00:00.000Z",
			"position":              float64(i),
			"description":           nil,
			"showcase":              false,
			"group_id":              "group",
			"page_id":               "page",
			"group":                 false,
			"only_show_if_degraded": false,
		})
	}
	value := map[string]any{"status": float64(200), "body": map[string]any{"components": components}}
	row := map[string]any{
		"ownerUserId": "v1:identity:user:owner",
		"runId":       "v1:work:run:run-1",
		"key":         "statusReply",
		"status":      "done",
		"durationMs":  float64(12),
		"result": map[string]any{
			"stepId": "statusReply", "status": "success",
			"result": value, "value": value, "valueRecorded": true,
		},
	}
	payload := map[string]any{
		"id":        "v1:work:step:step-1",
		"nodeId":    "v1:work:step:step-1",
		"concept":   "v1:work:step",
		"nodeType":  "node",
		"createdAt": "2026-11-01T21:33:44.000000Z",
		"actor":     "system",
		"payload":   row,
	}
	for k, v := range row {
		payload[k] = v
	}
	return events.Event{Topic: topic, Kind: events.KindNodeUpdated, Timestamp: time.Now(), Payload: payload}
}

const stepUpdatedTopic = "graph.node.updated.v1:work:step"

func onlyCopy(t *testing.T, s *inboundStream) *nodev1.EventForward {
	t.Helper()
	copies := drainOutbox(s)
	if len(copies) != 1 {
		t.Fatalf("expected one copy on the stream, got %d", len(copies))
	}
	return copies[0]
}

func TestARowTooLargeToBroadcastCrossesTheMeshWithoutItsLargestFields(t *testing.T) {
	eb := newBridgeFor(t, "origin", NodeTypeAgent)
	peer := attachAcceptedPeer(t, eb.peerManager, "peer", NodeTypeBFF)

	eb.onLocalEvent(stepReceiptEvent(stepUpdatedTopic))

	got := onlyCopy(t, peer)
	if size := proto.Size(got.GetPayload()); size > meshMaxEventBytes {
		t.Fatalf("the copy is %d bytes on the wire; a broadcast event may not exceed %d, "+
			"because every replica decodes about seven copies of it", size, meshMaxEventBytes)
	}

	fields := got.GetPayload().GetFields()
	nested := fields["payload"].GetStructValue().GetFields()
	if _, ok := fields["result"]; ok {
		t.Error("the oversized field still crossed the mesh on the flat row")
	}
	if _, ok := nested["result"]; ok {
		t.Error("the oversized field still crossed the mesh under payload")
	}
	// Run dispatch, the row gate and every other subscriber on the far side
	// decide on these, so they have to survive.
	for _, k := range []string{"id", "nodeId", "concept", "createdAt", "ownerUserId", "runId", "status"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("flat field %q did not survive", k)
		}
	}
	for _, k := range []string{"ownerUserId", "runId", "status", "key"} {
		if _, ok := nested[k]; !ok {
			t.Errorf("payload field %q did not survive", k)
		}
	}

	marker := fields[events.PayloadOmittedFieldsKey].GetListValue().GetValues()
	if len(marker) != 1 || marker[0].GetStringValue() != "result" {
		t.Errorf("the copy must name what it left behind, got %v", marker)
	}
}

// The event is shared: the writer's own subscribers read the same maps while the
// bridge runs. The live view on the writer's node and the automation that fires
// there depend on the whole row.
func TestTheWritersOwnEventKeepsTheWholeRow(t *testing.T) {
	eb := newBridgeFor(t, "origin", NodeTypeAgent)
	attachAcceptedPeer(t, eb.peerManager, "peer", NodeTypeBFF)

	ev := stepReceiptEvent(stepUpdatedTopic)
	want := stepReceiptEvent(stepUpdatedTopic)
	eb.onLocalEvent(ev)

	if !reflect.DeepEqual(ev.Payload, want.Payload) {
		t.Fatal("putting the event on the mesh changed the writer's own copy")
	}
	if ev.PayloadThinned() {
		t.Fatal("the writer's own copy reads as thinned")
	}
}

func TestARowThatFitsCrossesTheMeshWhole(t *testing.T) {
	eb := newBridgeFor(t, "origin", NodeTypeAgent)
	peer := attachAcceptedPeer(t, eb.peerManager, "peer", NodeTypeBFF)

	row := map[string]any{"status": "done", "result": map[string]any{"value": "small"}}
	eb.onLocalEvent(events.Event{Topic: stepUpdatedTopic, Kind: events.KindNodeUpdated, Timestamp: time.Now(),
		Payload: map[string]any{"id": "v1:work:step:s", "concept": "v1:work:step", "status": "done",
			"result": row["result"], "payload": row}})

	fields := onlyCopy(t, peer).GetPayload().GetFields()
	if _, ok := fields["result"]; !ok {
		t.Error("a row that fits lost a field")
	}
	if _, ok := fields[events.PayloadOmittedFieldsKey]; ok {
		t.Error("a row that fits was marked as thinned")
	}
}

// Thinning is for ROW events on a BROADCAST rule, the case that multiplies. A
// non-graph topic has no row a consumer can read again, and a targeted rule's
// consumer is one node type holding a contract with the whole event.
func TestOnlyABroadcastRowEventIsThinned(t *testing.T) {
	big := stepReceiptEvent(stepUpdatedTopic).Payload

	t.Run("a non-graph broadcast", func(t *testing.T) {
		eb := newBridgeFor(t, "origin", NodeTypeAgent)
		peer := attachAcceptedPeer(t, eb.peerManager, "peer", NodeTypeBFF)
		eb.onLocalEvent(events.Event{Topic: "providers.reload.all", Timestamp: time.Now(), Payload: big})
		fields := onlyCopy(t, peer).GetPayload().GetFields()
		if _, ok := fields["result"]; !ok {
			t.Error("a non-graph event lost a field it has no way to read again")
		}
	})

	t.Run("a targeted rule", func(t *testing.T) {
		eb := newBridgeFor(t, "origin", NodeTypeBFF)
		peer := attachAcceptedPeer(t, eb.peerManager, "agent-1", NodeTypeAgent)
		eb.onLocalEvent(events.Event{Topic: "graph.node.updated.v1:os:askConversation", Timestamp: time.Now(), Payload: big})
		fields := onlyCopy(t, peer).GetPayload().GetFields()
		if _, ok := fields["result"]; !ok {
			t.Error("an event on a targeted rule lost a field its one consumer may read")
		}
	})
}

// A node that hears a thinned copy publishes it and relays it thinned, and the
// marker survives the hop, because the far node's decisions turn on it.
func TestAThinnedCopyStaysThinnedAcrossARelay(t *testing.T) {
	origin := newBridgeFor(t, "origin", NodeTypeAgent)
	toRelay := attachAcceptedPeer(t, origin.peerManager, "relay", NodeTypeBFF)
	origin.onLocalEvent(stepReceiptEvent(stepUpdatedTopic))
	thin := onlyCopy(t, toRelay)

	relay := newBridgeFor(t, "relay", NodeTypeBFF)
	onward := attachAcceptedPeer(t, relay.peerManager, "far", NodeTypeMCP)
	heard := make(chan events.Event, 1)
	unsubscribe := relay.localBus.Subscribe(stepUpdatedTopic, func(e events.Event) { heard <- e })
	t.Cleanup(unsubscribe)

	relay.ReceiveForward(thin, "origin")

	select {
	case e := <-heard:
		if !e.PayloadThinned() {
			t.Error("the event a relay published does not read as thinned")
		}
		if !e.IsRemote() {
			t.Error("the event a relay published does not read as remote")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay published nothing on its bus")
	}

	fields := onlyCopy(t, onward).GetPayload().GetFields()
	if _, ok := fields[events.PayloadOmittedFieldsKey]; !ok {
		t.Error("the marker did not survive the relay")
	}
	if _, ok := fields["result"]; ok {
		t.Error("the relay carried the field the origin left behind")
	}
}

// A row with no field worth dropping, or every field already gone, must still
// leave: an event that vanishes leaves a far node's live view wrong with no sign
// of why. Its identity is what lets a reader fetch the rest.
func TestARowOfManySmallFieldsStillLeavesWithItsIdentity(t *testing.T) {
	eb := newBridgeFor(t, "origin", NodeTypeAgent)
	peer := attachAcceptedPeer(t, eb.peerManager, "peer", NodeTypeBFF)

	row := map[string]any{"status": "done"}
	for i := range 20000 {
		row[fmt.Sprintf("field%05d", i)] = "a small value"
	}
	payload := map[string]any{"id": "v1:work:step:s", "nodeId": "v1:work:step:s", "concept": "v1:work:step", "payload": row}
	for k, v := range row {
		payload[k] = v
	}
	eb.onLocalEvent(events.Event{Topic: stepUpdatedTopic, Kind: events.KindNodeUpdated, Timestamp: time.Now(), Payload: payload})

	got := onlyCopy(t, peer)
	if size := proto.Size(got.GetPayload()); size > meshMaxEventBytes {
		t.Fatalf("the copy is %d bytes, over the %d limit", size, meshMaxEventBytes)
	}
	fields := got.GetPayload().GetFields()
	for _, k := range []string{"id", "nodeId", "concept"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("identity field %q did not survive", k)
		}
	}
}

// OVER THE REAL TRANSPORT, IN THE CLOUD'S TOPOLOGY. The hop is the point: what
// the writer's bus holds and what a replica five links away holds are different
// events, and each must be the right one. Run the same thing with the thinning
// taken out and every replica hears the 1.3 MB event, about seven times.
func TestARowTooLargeToBroadcastReachesEveryNodeThinnedAndTheWriterWhole(t *testing.T) {
	m := newWireMesh(t)
	m.cloudTopology()
	m.start()
	m.awaitLinks()

	type sighting struct {
		thinned, hasResult, hasStatus bool
		copies                        int
	}
	var mu sync.Mutex
	heard := map[string]*sighting{}
	for _, id := range m.order {
		n := m.nodes[id]
		unsubscribe := n.bus.Subscribe(stepUpdatedTopic, func(e events.Event) {
			mu.Lock()
			defer mu.Unlock()
			s := heard[n.id]
			if s == nil {
				s = &sighting{}
				heard[n.id] = s
			}
			s.copies++
			s.thinned = e.PayloadThinned()
			_, s.hasResult = e.Payload["result"]
			_, s.hasStatus = e.Payload["status"]
		}, events.WithSubscriberName("test:oversized-row"))
		t.Cleanup(unsubscribe)
	}

	const writer = "workbench-a"
	m.nodes[writer].bus.Publish(stepReceiptEvent(stepUpdatedTopic))

	want := 0
	for _, id := range m.order {
		if participant(m.nodes[id]) {
			want++
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := len(heard)
		mu.Unlock()
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d participants heard the row: %v", got, want, heard)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a late duplicate must not count as a sighting

	mu.Lock()
	defer mu.Unlock()
	for id, s := range heard {
		if s.copies != 1 {
			t.Errorf("%s published the event %d times", id, s.copies)
		}
		if !s.hasStatus {
			t.Errorf("%s heard the row without the small fields its subscribers decide on", id)
		}
		if id == writer {
			if s.thinned || !s.hasResult {
				t.Errorf("the writer's own subscribers must get the whole row: %+v", *s)
			}
			continue
		}
		if !s.thinned || s.hasResult {
			t.Errorf("%s heard the whole row across the mesh: %+v", id, *s)
		}
	}
}
