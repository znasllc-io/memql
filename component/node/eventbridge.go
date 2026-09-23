package node

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	EventBridgeComponentName = common.ComponentName("nodeEventBridge")
	eventBridgeOrder         = 46 // after PeerManager (45), before NodeServer (48)

	// meshMaxHops is how many links a copy may travel (memql#5338, D4): every
	// node within 16 links of an event's origin hears it, and nothing relays a
	// copy that has already travelled 16. It replaced a TTL of 3, which is
	// shorter than paths that exist in the cloud's own topology once every
	// stream carries events both ways (an edge on one bff to an edge on the
	// other is four links; an mcp parented on an edge, five).
	//
	// Sixteen is not tuned to a topology. A node relays a first sighting once,
	// so every first-sighting path is a simple path and, in a mesh of
	// seventeen nodes or fewer, no copy can run out of budget before every
	// node has heard it; the shorter-route relay in ReceiveForward covers a
	// bigger mesh. The budget is a hard stop for a mesh that has gone wrong,
	// not a knob.
	meshMaxHops = 16

	// defaultDedupTTL is how long a forwarded event id is remembered so a
	// re-delivered copy is suppressed (memql#1155). Time-windowed (not a
	// fixed count) so high event volume can't evict an id and re-admit it
	// mid-storm. The mesh re-circulation window is sub-second; 2 minutes is
	// generous headroom and bounds memory to ~rate*ttl.
	defaultDedupTTL = 2 * time.Minute

	// meshDropLogEvery rate-limits the "copies are being dropped" warning to
	// one line per interval, carrying the count since the last line.
	meshDropLogEvery = 10 * time.Second
)

// EventBridge connects the local events.Bus to the distributed NodeService
// mesh. It subscribes to all local events and forwards the ones a routing
// rule names to its peers; events arriving from peers are published locally
// ONCE and relayed ONCE (ReceiveForward).
//
// EVERY STREAM CARRIES EVENTS BOTH WAYS (memql#5338). A forward goes to every
// peer this node holds a stream to, whichever node opened it: a dialed
// connection, or the push half of a stream the peer opened to this node
// (inbound_stream.go). Before that epic events went only along OUTBOUND dials,
// and a node nobody dialed -- the edge, a product bff, mcp -- heard nothing.
// Design record: docs/superpowers/specs/2026-09-22-mesh-event-delivery-design.md.
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

	stats meshStats
}

// meshStats is what this node has done with broadcast events since it
// started: the counts behind its delivery report (memql#5338, D7). The same
// increments feed the process's Prometheus counters, so the two can never
// disagree about what they are counting.
type meshStats struct {
	since time.Time

	originated atomic.Int64 // events published here and put on the mesh
	heard      atomic.Int64 // events a peer delivered and this node published
	relayed    atomic.Int64 // events this node passed on
	duplicates atomic.Int64 // copies of events already heard, dropped
	hopLimited atomic.Int64 // copies not relayed at the hop limit
	dropped    atomic.Int64 // copies a transport could not take

	lastHeard atomic.Int64 // unix nanos of the last first sighting; 0 = never

	lastDropLog atomic.Int64 // unix nanos; rate-limits the drop warning
	dropsSince  atomic.Int64 // drops since the last warning
}

// SetFastPathSink wires the local DeliverySubstrate's HandleFastPath as the
// receiver for inbound mesh fast-path hints (memql#1289). Called from
// app/cluster.go on every mesh node when the substrate is constructed with this
// EventBridge as its meshFastPath. With a sink set, an inbound hint EventForward
// is decoded back to a Deliverable and handed to the substrate, which feeds it
// into the per-subscription dedup window and wakes a live local subscriber
// instantly -- the low-latency cross-node path. Without a sink (durable-only),
// inbound hints are ignored: correctness never depends on them (ADR 4.5 rule 3).
func (eb *EventBridge) SetFastPathSink(sink func(Deliverable)) { eb.fastPathSink = sink }

// NewEventBridge creates an EventBridge that bridges events between the local
// bus and the mesh, and installs it as peerManager's event sink: every stream
// this node reads -- accepted, parent, dialed -- delivers into it.
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
	eb.stats.since = time.Now().UTC()
	peerManager.setEventSink(eb)

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

// ReceiveForward is the ONE arrival path for an event a peer sent, whichever
// direction of whichever stream it came down (memql#5338, D3, D6). It is
// flooding with duplicate suppression:
//
//   - a FIRST sighting is published on the local bus and relayed;
//   - a repeat that travelled FEWER hops than every copy before it is relayed
//     again and not republished, so a copy that won the race the long way
//     round cannot starve the far side of the mesh of hop budget;
//   - any other repeat is dropped.
//
// So each node publishes an event once and relays it a bounded number of
// times -- in practice once -- and there is no loop to prevent: a node that
// already relayed a copy at least as short does nothing with this one.
func (eb *EventBridge) ReceiveForward(evt *nodev1.EventForward, fromNodeId string) {
	if evt == nil {
		return
	}

	// Mesh fast-path hint (memql#1289). A hint rides the EventForward transport
	// under a reserved topic carrying an encoded Deliverable. It is NOT an
	// ordinary bus event: decode it and hand it to the substrate's
	// HandleFastPath, which feeds the per-subscription dedup window and wakes a
	// live local subscriber for LATENCY. It never touches the local bus and
	// never advances a cursor (ADR 4.5). A duplicate hint is harmless -- the
	// substrate's dedup and the durable cursor are the guards -- so it is
	// deliberately NOT run through eb.seen (which would otherwise suppress a
	// legitimately-distinct deliverable that reused a coincidental mesh
	// envelope id). It is also never relayed: the producer sends it to every
	// peer it holds a stream to, and an owner it misses catches up on the
	// durable pull.
	if evt.Topic == meshHintTopic {
		if eb.fastPathSink != nil {
			if d, ok := decodeMeshHint(evt); ok {
				eb.fastPathSink(d)
			}
		}
		return
	}

	// A copy on the wire has travelled at least the link it arrived on. A
	// pre-epic sender sets no hops at all; reading its copy as 0 would make it
	// look shorter than any real route and set off a needless relay.
	hops := evt.GetHops()
	if hops < 1 {
		hops = 1
	}
	first, shorter := eb.seen.observe(evt.GetEventId(), hops)
	if !first {
		eb.stats.duplicates.Add(1)
		metrics.MeshEvent(metrics.MeshDuplicate, 1)
		if shorter {
			eb.relay(evt, hops, fromNodeId)
		}
		return
	}

	eb.stats.heard.Add(1)
	eb.stats.lastHeard.Store(time.Now().UnixNano())
	metrics.MeshEvent(metrics.MeshHeard, 1)

	var payload map[string]any
	if evt.Payload != nil {
		payload = evt.Payload.AsMap()
	}
	if payload == nil {
		payload = make(map[string]any)
	}
	eb.publishViaBus(events.Event{
		Topic:        evt.Topic,
		Kind:         events.Kind(evt.Kind),
		Timestamp:    evt.Ts.AsTime(),
		Payload:      payload,
		Metadata:     make(map[string]string),
		OriginNodeId: evt.OriginNodeId,
		Cause:        causeFromProto(evt.Cause),
	})

	eb.relay(evt, hops, fromNodeId)
}

// relay passes an arrived event on to this node's other peers, one link
// further along: every event participant except the peer it came from and
// the node it originated on. hops is how far the arriving copy travelled; a
// copy that has already travelled meshMaxHops goes no further.
func (eb *EventBridge) relay(evt *nodev1.EventForward, hops int32, fromNodeId string) {
	if hops >= meshMaxHops {
		eb.stats.hopLimited.Add(1)
		metrics.MeshEvent(metrics.MeshHopLimited, 1)
		return
	}
	// This node's own rules decide, not the origin's: a product pack can
	// register rules on one binary and not another, and a relay must not carry
	// a topic this binary was never told to.
	decision := evaluateRouting(eb.rules, evt.Topic)
	if !decision.Forward {
		return
	}
	relayed := &nodev1.EventForward{
		EventId:      evt.EventId,
		Topic:        evt.Topic,
		Kind:         evt.Kind,
		Ts:           evt.Ts,
		Payload:      evt.Payload,
		OriginNodeId: evt.OriginNodeId,
		Cause:        evt.Cause,
		Hops:         hops + 1,
	}
	eb.sendToPeers(relayed, decision, fromNodeId, evt.OriginNodeId)
	eb.stats.relayed.Add(1)
	metrics.MeshEvent(metrics.MeshRelayed, 1)
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
// and puts the event on the mesh if one names it.
func (eb *EventBridge) onLocalEvent(event events.Event) {
	// An event a peer delivered is relayed by ReceiveForward, never forwarded
	// again from here as though this node had originated it.
	if event.IsRemote() {
		return
	}

	decision := evaluateRouting(eb.rules, event.Topic)
	if !decision.Forward {
		return
	}

	eventId := id.NewShortId()
	// Recorded at distance 0, so no copy that comes back round can ever read
	// as new here, nor as a shorter route.
	eb.seen.observe(eventId, 0)

	payloadStruct, err := structpb.NewStruct(event.Payload)
	if err != nil {
		eb.logger.Warn("failed to convert event payload to struct",
			"topic", event.Topic,
			"error", err,
		)
		payloadStruct = &structpb.Struct{}
	}

	eb.sendToPeers(&nodev1.EventForward{
		EventId:      eventId,
		Topic:        event.Topic,
		Kind:         int32(event.Kind),
		Ts:           timestamppb.New(event.Timestamp),
		Payload:      payloadStruct,
		OriginNodeId: eb.identity.ID,
		Cause:        causeToProto(event.Cause),
		Hops:         1, // the link it is about to travel
	}, decision)
	eb.stats.originated.Add(1)
	metrics.MeshEvent(metrics.MeshOriginated, 1)
}

// sendToPeers puts one copy of forward on ONE transport to every target peer
// -- a dialed connection or the push half of a stream the peer opened
// (PeerManager.eventTarget) -- skipping the peers named in exclude, and
// returns how many copies were queued. It never blocks: a transport that
// cannot take the copy drops it, and the drop is counted.
//
// A peer with no transport at all (a sibling learned only from gossip) is
// skipped: a relay reaches it. What is NOT here is a buffer for a peer that
// is absent -- the retired #1232 outbox. For the traffic the durable
// substrate carries (memql#1264/#1265/#1266) that substrate is the guarantee;
// for an ordinary bus event a node with no live stream at all misses what is
// broadcast meanwhile, and a consumer that must converge anyway has its own
// floor (the readiness safety net, the edge cache TTL, the dialer ticker).
func (eb *EventBridge) sendToPeers(forward *nodev1.EventForward, decision routingDecision, exclude ...string) int {
	var targets []*PeerEntry
	if decision.Broadcast {
		targets = meshEventParticipants(eb.peerManager.AllPeers())
	} else {
		targets = eb.peerManager.ByType(decision.TargetType)
	}

	sent := 0
	for _, peer := range targets {
		if peer == nil || peer.Info == nil || excluded(peer.Info.NodeId, exclude) {
			continue
		}
		send, transport, ok := eb.peerManager.eventTarget(peer)
		if !ok {
			continue
		}
		if send(forward) {
			sent++
			metrics.MeshCopy(transport, metrics.MeshCopySent)
			continue
		}
		eb.noteDroppedCopy(peer.Info.NodeId, transport)
	}

	if len(targets) > 0 {
		eb.logger.Debug("mesh event sent to peers",
			"topic", forward.Topic,
			"event_id", forward.EventId,
			"hops", forward.Hops,
			"sent", sent,
			"targets", len(targets),
			"broadcast", decision.Broadcast,
		)
	}
	return sent
}

func excluded(nodeId string, exclude []string) bool {
	for _, x := range exclude {
		if x != "" && x == nodeId {
			return true
		}
	}
	return false
}

// noteDroppedCopy counts a copy a transport could not take, and says so at
// most once per meshDropLogEvery with the count since the last line. A peer
// that stops reading drops every copy; one warning per copy would bury the
// log.
func (eb *EventBridge) noteDroppedCopy(peerId, transport string) {
	eb.stats.dropped.Add(1)
	metrics.MeshCopy(transport, metrics.MeshCopyDropped)
	n := eb.stats.dropsSince.Add(1)
	now := time.Now().UnixNano()
	last := eb.stats.lastDropLog.Load()
	if now-last < int64(meshDropLogEvery) || !eb.stats.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	eb.stats.dropsSince.Add(-n)
	eb.logger.Warn("mesh: event copies dropped; a peer's stream could not take them",
		"peer_id", peerId, "transport", transport, "dropped", n)
}

// cleanup removes the local bus subscription.
func (eb *EventBridge) cleanup() {
	if eb.unsubscribe != nil {
		eb.unsubscribe()
		eb.unsubscribe = nil
	}
	eb.logger.Info("event bridge cleaned up")
}

// MeshReport is this node's account of what it hears (memql#5338, D7): its
// links, and what it has done with broadcast events since it started. The
// node's own v1:cluster:node row carries it, refreshed by the self heartbeat,
// and MemQL OS draws it in Cluster > Mesh.
type MeshReport struct {
	// Receives is false on a node that takes no mesh events by design --
	// identity (meshEventParticipants) -- so a zero `heard` there is the
	// design and not an island.
	Receives bool
	// Since is when this process started counting.
	Since time.Time
	// Links is every peer this node holds a stream to, in either direction.
	Links []MeshLink

	Heard      int64
	Duplicates int64
	Originated int64
	Relayed    int64
	Dropped    int64
	HopLimited int64

	// LastHeardAt is the last first sighting; zero until the first one.
	LastHeardAt time.Time
}

// MeshReport snapshots this node's delivery report.
func (eb *EventBridge) MeshReport() MeshReport {
	r := MeshReport{
		Receives:   takesMeshEvents(eb.identity.Type),
		Since:      eb.stats.since,
		Links:      eb.peerManager.meshLinks(),
		Heard:      eb.stats.heard.Load(),
		Duplicates: eb.stats.duplicates.Load(),
		Originated: eb.stats.originated.Load(),
		Relayed:    eb.stats.relayed.Load(),
		Dropped:    eb.stats.dropped.Load(),
		HopLimited: eb.stats.hopLimited.Load(),
	}
	if ns := eb.stats.lastHeard.Load(); ns != 0 {
		r.LastHeardAt = time.Unix(0, ns).UTC()
	}
	return r
}

// Wire is the report as the `mesh` object on a v1:cluster:node row. Field
// names are the concept's (dsl/cluster/concepts.memql); lastHeardAt is ABSENT
// until the first event, because a zero time would read as "heard at the epoch"
// rather than as "never".
func (r MeshReport) Wire() map[string]any {
	links := make([]any, 0, len(r.Links))
	for _, l := range r.Links {
		links = append(links, map[string]any{"node": l.Node, "type": l.Type, "via": l.Via})
	}
	out := map[string]any{
		"receives":   r.Receives,
		"since":      r.Since.UTC().Format(time.RFC3339),
		"links":      links,
		"heard":      r.Heard,
		"duplicates": r.Duplicates,
		"originated": r.Originated,
		"relayed":    r.Relayed,
		"dropped":    r.Dropped,
		"hopLimited": r.HopLimited,
	}
	if !r.LastHeardAt.IsZero() {
		out["lastHeardAt"] = r.LastHeardAt.UTC().Format(time.RFC3339)
	}
	return out
}

// causeToProto converts a Cause to the EventForward wire form (node.proto's
// EventCause, epic memql#5380). The zero cause -- a root event, the common
// case -- converts to nil, so a root event costs nothing extra on the mesh
// hop; causeFromProto(nil) reads back as the zero cause.
func causeToProto(c events.Cause) *nodev1.EventCause {
	if c.IsZero() {
		return nil
	}
	chain := make([]*nodev1.EventCauseLink, len(c.Chain))
	for i, l := range c.Chain {
		chain[i] = &nodev1.EventCauseLink{Automation: l.Automation, RunId: l.RunId}
	}
	return &nodev1.EventCause{
		CausationId:   c.CausationId,
		CorrelationId: c.CorrelationId,
		Depth:         int32(c.Depth),
		Chain:         chain,
	}
}

// causeFromProto reverses causeToProto. A nil proto -- an old node in a
// mixed-version rollout, or a root event -- gives the zero cause.
func causeFromProto(p *nodev1.EventCause) events.Cause {
	if p == nil {
		return events.Cause{}
	}
	chain := make([]events.Link, len(p.Chain))
	for i, l := range p.Chain {
		chain[i] = events.Link{Automation: l.Automation, RunId: l.RunId}
	}
	return events.Cause{
		CausationId:   p.CausationId,
		CorrelationId: p.CorrelationId,
		Depth:         int(p.Depth),
		Chain:         chain,
	}
}

// takesMeshEvents reports whether a node of this type RECEIVES mesh events.
// Every node type does but identity -- the one predicate meshEventParticipants
// and the delivery report both read, so the report cannot call identity an
// island while the bridge keeps it out on purpose.
func takesMeshEvents(t NodeType) bool {
	return NodeType(strings.ToLower(strings.TrimSpace(string(t)))) != NodeTypeIdentity
}

// meshEventParticipants filters a broadcast target list down to the nodes that
// take part in mesh event distribution (memql#3380).
//
// It exists because reachability and event membership stopped being the same
// thing. The bff dials the identity node so a deploy-control call can reach
// the one node that HAS a DeployControlService (isDialableType), which puts an
// identity PeerEntry in the bff's peer table -- and a broadcast routing rule
// (TargetType "") targets AllPeers. Without this filter, opening that route
// would also start fanning every broadcast topic -- graph.node.*.v1:cluster:*,
// cache.invalidate.*, authoring.promote.*, the planner lifecycle -- at the auth
// service, which republishes them on its local bus and would fire its
// subscribers and automations on events it has never seen before. That is a
// large, unrelated behaviour change to smuggle in behind a deploy-console fix.
//
// IT FILTERS WHO RECEIVES, NOT WHO SENDS (memql#5338, D5). Identity pushes its
// OWN events down the streams the bffs opened to it -- which is what the
// v1:identity:user / invitation / account / auditEvent / group rules in
// routing.go were written for, and what no route carried until every stream
// carried events both ways.
//
// The predicate is deliberately an exclusion of the ONE known non-participant
// rather than an allow-list of participants: peer types arrive from a
// NodeWelcome / PeerIntroduction over the wire and a gossiped sibling can carry
// an empty NodeType, so an allow-list would silently stop forwarding to peers
// it merely failed to recognise. Failing open for an unknown type preserves
// today's behaviour exactly; only "identity" is dropped.
//
// Targeted (non-broadcast) rules need no filter: ByType is never asked for
// identity, because no routing rule names it.
func meshEventParticipants(peers []*PeerEntry) []*PeerEntry {
	out := peers[:0:0]
	for _, p := range peers {
		if p != nil && p.Info != nil && !takesMeshEvents(NodeType(p.Info.NodeType)) {
			continue
		}
		out = append(out, p)
	}
	return out
}
