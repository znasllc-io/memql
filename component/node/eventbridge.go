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

	defaultTTL        = 3
	defaultDedupSize  = 8192
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
}

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
		seen:        newEventDedup(defaultDedupSize),
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
// A peer with Connection=nil (e.g. its outbound stream is mid-reconnect)
// is counted as "skipped" -- the message is NOT delivered to it. Callers
// should treat skipped peers as missed deliveries. If the originating
// concept has stricter delivery semantics than fire-and-forget (e.g.
// client-tool-request), it must layer its own retry/outbox on top.
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

	sent, skipped := 0, 0
	var skippedPeers []string
	for _, peer := range targets {
		if peer.Connection != nil {
			peer.Connection.Send(msg)
			sent++
		} else {
			skipped++
			skippedPeers = append(skippedPeers, peer.Info.GetNodeId())
		}
	}

	if len(targets) > 0 {
		if skipped > 0 {
			// Elevated to Warn so silent delivery failures show up
			// without needing DEBUG logs. Previously events to a peer
			// with Connection=nil were lost without any indication.
			eb.logger.Warn("event forwarded with skipped peers (nil Connection)",
				"topic", forward.Topic,
				"event_id", forward.EventId,
				"sent", sent,
				"skipped", skipped,
				"skipped_peers", skippedPeers,
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

	sent, skipped := 0, 0
	var skippedPeers []string
	for _, peer := range targets {
		if peer.Info.NodeId == excludeNodeId {
			continue // don't send back to the node that sent it
		}
		if peer.Connection != nil {
			peer.Connection.Send(msg)
			sent++
		} else {
			skipped++
			skippedPeers = append(skippedPeers, peer.Info.GetNodeId())
		}
	}

	if skipped > 0 {
		eb.logger.Warn("mesh relay forwarded with skipped peers (nil Connection)",
			"topic", evt.Topic,
			"event_id", evt.EventId,
			"ttl", evt.Ttl,
			"sent", sent,
			"skipped", skipped,
			"skipped_peers", skippedPeers,
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
