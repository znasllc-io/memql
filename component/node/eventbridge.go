package node

import (
	"context"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	EventBridgeComponentName = common.ComponentName("nodeEventBridge")
	eventBridgeOrder         = 46 // after PeerManager (45), before NodeServer (48)

	defaultTTL = 3
	// defaultDedupTTL is how long a forwarded event id is remembered so a
	// re-delivered copy is suppressed (memql#1155). Time-windowed (not a
	// fixed count) so high event volume can't evict an id and re-admit it
	// mid-storm. The mesh re-circulation window is sub-second; 2 minutes is
	// generous headroom and bounds memory to ~rate*ttl.
	defaultDedupTTL = 2 * time.Minute
)

// EventBridge connects the local events.Bus to the distributed NodeService
// mesh. It subscribes to all local events and forwards matching ones to
// connected peers. Inbound events from peers are published locally after
// dedup and TTL checks.
type EventBridge struct {
	*component.Component

	localBus    *events.Bus
	peerManager *PeerManager
	identity    *Identity
	rules       []RoutingRule
	seen        *eventDedup
	logger      *slog.Logger
	unsubscribe func() // local bus subscription cleanup
	wiring      *bus.Wiring

	// suppressInboundChatReply, when true, drops the INBOUND mesh copy of the
	// chat-reply topics (utterance / presence / canvasState) before it is
	// published onto THIS node's local bus. Set only on the bff (via
	// SuppressInboundChatReply from app/cluster.go) once ChatReplyDelivery is
	// wired: on the bff the browser must receive those topics EXACTLY ONCE, and
	// the durable substrate (memql#1264) is their single delivery path, so a
	// second copy arriving over the mesh would double-deliver to the browser.
	//
	// This is deliberately asymmetric and inbound-only:
	//   - The OUTBOUND forward (onLocalEvent -> peers) is UNTOUCHED, so a human
	//     utterance written on the bff still reaches the cognition/agent worker
	//     that subscribes graph.node.created.v1:cognition:utterance to produce
	//     the reply. The chat TRIGGER path must keep flowing over the mesh.
	//   - On a WORKER (non-bff) the inbound publish is UNTOUCHED, so cognition
	//     still sees the human utterance on its local bus.
	// Only the bff's inbound-to-local-bus copy of these reply topics is dropped,
	// because only the bff fans them to a browser and only the bff subscribes
	// the substrate.
	suppressInboundChatReply bool

	// fastPathSink receives a decoded mesh fast-path hint inbound from a peer
	// (memql#1289). It is the local DeliverySubstrate's HandleFastPath, set on
	// every mesh node via SetFastPathSink from app/cluster.go. A nil sink means
	// the substrate is not wired (single-node / durable-only mode): an inbound
	// hint is then a harmless no-op, because the durable pull remains the
	// delivery guarantee. The hint NEVER touches the local bus and NEVER
	// advances a cursor -- it only feeds the per-subscription dedup window so a
	// live local subscriber wakes instantly instead of waiting for the durable
	// poll floor (ADR 4.5; see PublishHint).
	fastPathSink func(Deliverable)
}

// SuppressInboundChatReply drops the inbound mesh copy of the chat-reply topics
// on this node's local bus so the durable substrate is their single delivery
// path to the browser (memql#1264). Called from app/cluster.go on the bff only.
func (eb *EventBridge) SuppressInboundChatReply(on bool) { eb.suppressInboundChatReply = on }

// SetFastPathSink wires the local DeliverySubstrate's HandleFastPath as the
// receiver for inbound mesh fast-path hints (memql#1289). Called from
// app/cluster.go on every mesh node when the substrate is constructed with this
// EventBridge as its meshFastPath. With a sink set, an inbound hint EventForward
// is decoded back to a Deliverable and handed to the substrate, which feeds it
// into the per-subscription dedup window and wakes a live local subscriber
// instantly -- the low-latency cross-node path. Without a sink (durable-only),
// inbound hints are ignored: correctness never depends on them (ADR 4.5 rule 3).
func (eb *EventBridge) SetFastPathSink(sink func(Deliverable)) { eb.fastPathSink = sink }

// NewEventBridge creates an EventBridge that bridges events between the
// local bus and connected peers.
func NewEventBridge(identity *Identity, localBus *events.Bus, peerManager *PeerManager, logger *slog.Logger) *EventBridge {
	comp, _ := component.New(EventBridgeComponentName)

	eb := &EventBridge{
		Component:   comp,
		localBus:    localBus,
		peerManager: peerManager,
		identity:    identity,
		rules:       defaultRoutingRules(),
		seen:        newEventDedup(defaultDedupTTL),
		logger:      logger,
	}

	eb.ConfigureLifecycle(
		component.WithRunHook(eb.run),
		component.WithOnStopHook(eb.cleanup),
	)

	return eb
}

// Order returns the startup order.
func (*EventBridge) Order() int {
	return eventBridgeOrder
}

// HandleInbound processes an EventForward message received from a peer.
// It checks dedup and TTL, then publishes the event on the local bus.
func (eb *EventBridge) HandleInbound(evt *nodev1.EventForward) {
	if evt == nil {
		return
	}

	// Mesh fast-path hint (memql#1289). A hint rides the EventForward transport
	// under a reserved topic carrying an encoded Deliverable. It is NOT an
	// ordinary bus event: decode it and hand it to the substrate's
	// HandleFastPath, which feeds the per-subscription dedup window and wakes a
	// live local subscriber for LATENCY. It never touches the local bus and
	// never advances a cursor (ADR 4.5). A duplicate/re-circulated hint is
	// harmless -- the substrate's dedup and the durable cursor are the guards --
	// so it is deliberately NOT run through eb.seen (which would otherwise
	// suppress a legitimately-distinct deliverable that reused a coincidental
	// mesh envelope id). It is also NOT re-forwarded to other peers: the hint is
	// broadcast by the producer, so every owner already receives it directly.
	if evt.Topic == meshHintTopic {
		if eb.fastPathSink != nil {
			if d, ok := decodeMeshHint(evt); ok {
				eb.fastPathSink(d)
			}
		}
		return
	}

	// Chat-reply path is on the durable substrate (memql#1264). On the bff the
	// browser receives these topics ONLY via the substrate republish, so drop
	// the inbound mesh copy here to keep the browser stream exactly-once. The
	// mesh still carried this event to other peers (e.g. cognition's trigger);
	// we are only declining to ALSO publish it onto this bff's local bus. The
	// durable path is the guarantee, so dropping the best-effort mesh copy never
	// loses the event. Non-bff nodes never set this flag.
	if eb.suppressInboundChatReply && isChatReplyTopic(evt.Topic) {
		// Still record the id in the dedup window so a later mesh re-circulation
		// of the same event is recognised as already-handled.
		eb.seen.Check(evt.EventId)
		return
	}

	// Dedup check
	if eb.seen.Check(evt.EventId) {
		return
	}

	// TTL check
	if evt.Ttl <= 0 {
		eb.logger.Debug("dropping event with expired TTL",
			"event_id", evt.EventId,
			"topic", evt.Topic,
		)
		return
	}

	// Convert proto payload to map[string]any
	var payload map[string]any
	if evt.Payload != nil {
		payload = evt.Payload.AsMap()
	}
	if payload == nil {
		payload = make(map[string]any)
	}

	// Publish locally with origin tracking
	localEvent := events.Event{
		Topic:        evt.Topic,
		Kind:         events.Kind(evt.Kind),
		Timestamp:    evt.Ts.AsTime(),
		Payload:      payload,
		Metadata:     make(map[string]string),
		OriginNodeId: evt.OriginNodeId,
	}

	eb.publishViaBus(localEvent)
}

// run is the lifecycle loop. It subscribes to the local bus and watches
// for events to forward.
func (eb *EventBridge) run(ctx context.Context, markStarted func()) error {
	// Subscribe to all local events
	eb.unsubscribe = eb.localBus.Subscribe("#", func(event events.Event) {
		eb.onLocalEvent(event)
	}, events.WithSubscriberName("nodeEventBridge"))

	markStarted()

	<-ctx.Done()
	return ctx.Err()
}

// onLocalEvent is called for every local event. It evaluates routing rules
// and forwards the event to peers if appropriate.
func (eb *EventBridge) onLocalEvent(event events.Event) {
	// Never forward events that originated from another node (prevent loops)
	if event.IsRemote() {
		return
	}

	decision := evaluateRouting(eb.rules, event.Topic)
	if !decision.Forward {
		return
	}

	// Build the proto EventForward
	eventId := id.NewShortId()

	// Mark as seen so we don't re-process our own forward
	eb.seen.Check(eventId)

	payloadStruct, err := structpb.NewStruct(event.Payload)
	if err != nil {
		eb.logger.Warn("failed to convert event payload to struct",
			"topic", event.Topic,
			"error", err,
		)
		payloadStruct = &structpb.Struct{}
	}

	forward := &nodev1.EventForward{
		EventId:      eventId,
		Topic:        event.Topic,
		Kind:         int32(event.Kind),
		Ts:           timestamppb.New(event.Timestamp),
		Payload:      payloadStruct,
		OriginNodeId: eb.identity.ID,
		Ttl:          defaultTTL,
	}

	eb.forwardToPeers(forward, decision)
}

// forwardToPeers sends an EventForward to the appropriate peers based on the routing decision.
//
// A peer with Connection=nil (e.g. its outbound stream has not yet been
// attached after a blue-green cutover, or is mid-reconnect) does NOT lose
// the event: it is enqueued into that peer's bounded, TTL'd outbox
// (PeerManager.EnqueuePending) and flushed in order when AttachConnection
// finally binds the connection. The receiver dedups by EventId
// (component/node/dedup.go), so a buffered copy that also reaches the peer
// another way is harmless. (memql#1232)
//
// The earlier dead-for-delivery "skip" (memql#1245) is reverted (memql#1271):
// it silently DROPPED the fast-path copy to live non-parent replicas (a peer
// learned only via gossip is Connection==nil on this node even when it is up
// and owns the user's WebSocket), which broke cross-replica chat delivery on
// the star topology. The mesh is now only a best-effort fast-path over the
// durable delivery substrate (memql#1264), so the substrate -- not the mesh --
// is the delivery guarantee; the fast-path therefore never silently skips,
// it buffers (best effort) and lets the substrate backstop durability.
func (eb *EventBridge) forwardToPeers(forward *nodev1.EventForward, decision routingDecision) {
	msg := &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_EventForward{
			EventForward: forward,
		},
	}

	var targets []*PeerEntry
	if decision.Broadcast {
		targets = eb.peerManager.AllPeers()
	} else {
		targets = eb.peerManager.ByType(decision.TargetType)
	}

	sent, buffered := 0, 0
	var bufferedPeers []string
	for _, peer := range targets {
		conn, disp := eb.peerManager.forwardTarget(peer)
		switch disp {
		case dispositionSend:
			conn.Send(msg)
			sent++
		default: // dispositionBuffer (Connection==nil)
			eb.peerManager.EnqueuePending(peer.Info.GetNodeId(), msg)
			buffered++
			bufferedPeers = append(bufferedPeers, peer.Info.GetNodeId())
		}
	}

	if len(targets) > 0 {
		if buffered > 0 {
			// Buffered (not lost): the peer's outbox holds the event until
			// AttachConnection flushes it. Logged at Info so a churning peer
			// is still visible without implying a delivery failure.
			eb.logger.Info("event buffered for peers pending connection",
				"topic", forward.Topic,
				"event_id", forward.EventId,
				"sent", sent,
				"buffered", buffered,
				"buffered_peers", bufferedPeers,
				"broadcast", decision.Broadcast,
			)
		} else {
			eb.logger.Debug("event forwarded to peers",
				"topic", forward.Topic,
				"event_id", forward.EventId,
				"peer_count", sent,
				"broadcast", decision.Broadcast,
			)
		}
	}
}

// ForwardInboundToPeers re-forwards an inbound event to other peers with
// decremented TTL. Used for mesh propagation.
func (eb *EventBridge) ForwardInboundToPeers(evt *nodev1.EventForward, excludeNodeId string) {
	if evt.Ttl <= 1 {
		return // would be 0 after decrement, don't forward
	}

	decision := evaluateRouting(eb.rules, evt.Topic)
	if !decision.Forward {
		return
	}

	// Clone with decremented TTL
	forwarded := &nodev1.EventForward{
		EventId:      evt.EventId,
		Topic:        evt.Topic,
		Kind:         evt.Kind,
		Ts:           evt.Ts,
		Payload:      evt.Payload,
		OriginNodeId: evt.OriginNodeId,
		Ttl:          evt.Ttl - 1,
	}

	msg := &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_EventForward{
			EventForward: forwarded,
		},
	}

	var targets []*PeerEntry
	if decision.Broadcast {
		targets = eb.peerManager.AllPeers()
	} else {
		targets = eb.peerManager.ByType(decision.TargetType)
	}

	sent, buffered := 0, 0
	var bufferedPeers []string
	for _, peer := range targets {
		if peer.Info.NodeId == excludeNodeId {
			continue // don't send back to the node that sent it
		}
		conn, disp := eb.peerManager.forwardTarget(peer)
		switch disp {
		case dispositionSend:
			conn.Send(msg)
			sent++
		default: // dispositionBuffer (Connection==nil)
			// Buffer for a not-yet-attached peer (same reliability backstop
			// as forwardToPeers). The relayed copy keeps the original
			// EventId, so the receiver's dedup window suppresses any
			// duplicate that also arrives via a direct hop. (memql#1232)
			// The memql#1245 dead-peer skip is reverted here (memql#1271):
			// the durable substrate is the delivery guarantee, so the mesh
			// fast-path never silently drops to a Connection==nil peer.
			eb.peerManager.EnqueuePending(peer.Info.GetNodeId(), msg)
			buffered++
			bufferedPeers = append(bufferedPeers, peer.Info.GetNodeId())
		}
	}

	if buffered > 0 {
		eb.logger.Info("mesh relay buffered for peers pending connection",
			"topic", evt.Topic,
			"event_id", evt.EventId,
			"ttl", evt.Ttl,
			"sent", sent,
			"buffered", buffered,
			"buffered_peers", bufferedPeers,
		)
	}
}

// cleanup removes the local bus subscription.
func (eb *EventBridge) cleanup() {
	if eb.unsubscribe != nil {
		eb.unsubscribe()
		eb.unsubscribe = nil
	}
	eb.logger.Info("event bridge cleaned up")
}

// eventTimestamp returns a time.Time from a proto timestamp, or now if nil.
func eventTimestamp(ts *timestamppb.Timestamp) time.Time {
	if ts != nil {
		return ts.AsTime()
	}
	return time.Now().UTC()
}
