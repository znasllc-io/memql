package node

import (
	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EventBridge is the mesh's meshFastPath (memql#1289): the best-effort,
// low-latency hint surface that lets a cross-node live delivery wake the owning
// replica's subscriber INSTANTLY instead of falling back to the substrate's
// 250ms durable-poll floor. #1263 built the substrate's fast-path API
// (PublishHint / HandleFastPath) and #1264 wired the substrate durable-ONLY (nil
// fast-path); this is the missing low-latency half that #1266 streaming depends
// on.
//
// How it rides the existing mesh -- WHY there is no new wire message:
//
//   - PublishHint(d) (producer side) encodes the Deliverable into an ordinary
//     EventForward under the reserved meshHintTopic and broadcasts it over the
//     SAME NodeService transport EventBridge already uses for events. No proto
//     change, no new broker, the post-#1271 buffer-or-send forward path
//     verbatim (a hint to a Connection==nil peer buffers; a dropped hint is
//     harmless because the durable pull is the guarantee).
//   - HandleInbound (consumer side) recognises meshHintTopic, decodes it back to
//     a Deliverable, and hands it to the substrate's HandleFastPath via the
//     fastPathSink wired in app/cluster.go. HandleFastPath feeds the hint into
//     the per-subscription dedup window and wakes the local subscriber.
//
// The exactly-once + ordering + cursor invariants are the substrate's, NOT this
// transport's (proven in delivery_substrate_test.go):
//
//   - The mesh-hint copy and the durable-pull copy of the same EventID collapse
//     to EXACTLY ONE delivery via the per-subscription eventDedup (first path
//     wins). The hint carries the durable row's EventID byte-identically, so the
//     two paths dedup against each other.
//   - The cursor advances on the DURABLE path ONLY. A hint NEVER advances a
//     cursor, so a dropped/duplicated/re-circulated hint can neither lose a row
//     nor double-advance past one -- replay still re-reads everything after the
//     durable cursor.
//   - The hint is addressed by LOGICAL KEY: it is broadcast and every node that
//     owns a subscription for d.Key feeds it to that subscription; a node with
//     no live subscriber for the key simply discards it (HandleFastPath is a
//     no-op there). No physical node-id is ever named.
//
// The producer-side hop sits inside Substrate.Publish, which calls
// fastPath.PublishHint(d) AFTER the durable Append succeeds and only for a
// freshly-appended (non-duplicate) row -- so the hint never races ahead of its
// own durable row existing, and an idempotent re-produce emits no second hint.

const (
	// meshHintTopic tags a fast-path hint EventForward so the inbound handler
	// routes it to the substrate (HandleFastPath) instead of the local bus. It
	// is a transport tag only, never a routing/addressing key. A dedicated
	// broadcast forward rule (defaultRoutingRules) carries it to every peer.
	meshHintTopic = "mesh.hint.v1"

	// Reserved payload keys carrying the encoded Deliverable inside the hint's
	// EventForward.Payload. Namespaced so they can never collide with an
	// application event payload (mirrors substrate_rpc.go's __rpc_* envelope).
	hintKeyEventID    = "__mesh_hint_event_id"
	hintKeyKeyKind    = "__mesh_hint_key_kind"
	hintKeyKeyID      = "__mesh_hint_key_id"
	hintKeySeq        = "__mesh_hint_seq"
	hintKeyTopic      = "__mesh_hint_topic"
	hintKeyKind       = "__mesh_hint_kind"
	hintKeyOriginNode = "__mesh_hint_origin"
	hintKeyBody       = "__mesh_hint_body"
)

// PublishHint emits the deliverable on the mesh as a best-effort latency
// optimization (the meshFastPath contract, ADR 4.5). It is fire-and-forget: it
// MUST NOT block the durable path and the substrate's correctness MUST NOT
// depend on it arriving. The hint is broadcast to every peer; whichever node
// owns a live subscription for d.Key feeds it to that subscription via
// HandleFastPath, and every other node discards it.
//
// A hint for a node with no peers (single-node, or a worker before its mesh is
// up) simply forwards to zero targets -- harmless, the durable pull delivers.
func (eb *EventBridge) PublishHint(d Deliverable) {
	if eb == nil || eb.peerManager == nil {
		return
	}

	payload, err := structpb.NewStruct(encodeMeshHint(d))
	if err != nil {
		eb.logger.Warn("mesh fast-path: failed to encode hint payload",
			"key", d.Key.String(), "event_id", d.EventID, "error", err)
		return
	}

	forward := &nodev1.EventForward{
		// EventId is the mesh ENVELOPE id (distinct from d.EventID, the durable
		// dedup key). A fresh id per hint, so a hint never collides with the
		// dedup window of an ordinary event forward.
		EventId:      id.NewShortId(),
		Topic:        meshHintTopic,
		Kind:         int32(d.Kind),
		Ts:           timestamppb.Now(),
		Payload:      payload,
		OriginNodeId: d.OriginNode,
		Ttl:          1, // single hop: broadcast reaches every owner directly; no relay
	}

	// Broadcast to all peers -- logical addressing means we do not know (or care)
	// which physical replica owns the key's consumer; every owner picks it up,
	// every non-owner discards it. Reuses the post-#1271 buffer-or-send path.
	eb.forwardToPeers(forward, routingDecision{Forward: true, Broadcast: true})
}

// encodeMeshHint flattens a Deliverable into the reserved-key envelope carried
// inside the hint EventForward's payload.
func encodeMeshHint(d Deliverable) map[string]any {
	return map[string]any{
		hintKeyEventID:    d.EventID,
		hintKeyKeyKind:    d.Key.Kind,
		hintKeyKeyID:      d.Key.ID,
		hintKeySeq:        float64(d.Seq), // structpb numbers are float64
		hintKeyTopic:      d.Topic,
		hintKeyKind:       float64(int(d.Kind)),
		hintKeyOriginNode: d.OriginNode,
		hintKeyBody:       d.Payload,
	}
}

// decodeMeshHint reconstructs the Deliverable from an inbound hint EventForward.
// It returns ok=false when the envelope is malformed or missing the dedup key /
// routing key, so a corrupt hint is dropped rather than fed as a bogus delivery
// (the durable pull still delivers the real row).
func decodeMeshHint(evt *nodev1.EventForward) (Deliverable, bool) {
	if evt == nil || evt.Payload == nil {
		return Deliverable{}, false
	}
	m := evt.Payload.AsMap()

	eventID := hintString(m, hintKeyEventID)
	key := RoutingKey{Kind: hintString(m, hintKeyKeyKind), ID: hintString(m, hintKeyKeyID)}
	if eventID == "" || !key.Valid() {
		return Deliverable{}, false
	}

	var body map[string]any
	if v, ok := m[hintKeyBody].(map[string]any); ok {
		body = v
	}

	return Deliverable{
		EventID:    eventID,
		Key:        key,
		Seq:        int64(hintFloat(m, hintKeySeq)),
		Topic:      hintString(m, hintKeyTopic),
		Kind:       events.Kind(int(hintFloat(m, hintKeyKind))),
		Payload:    body,
		OriginNode: hintString(m, hintKeyOriginNode),
	}, true
}

func hintString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func hintFloat(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// Ensure EventBridge satisfies the substrate's meshFastPath seam.
var _ meshFastPath = (*EventBridge)(nil)
