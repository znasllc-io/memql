package node

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

// ChatReplyDelivery routes the chat-reply delivery path (utterance / presence /
// text-chunk / canvasState events that must reach the bff replica owning a
// user's WebSocket)
// through the durable DeliverySubstrate (memql#1264, Phase 1 of epic
// memql#1259). It is the first path cut over to the substrate; the generic
// ad-hoc mesh forwards (#1232 outbox, #1245 skip) stay in place for the
// not-yet-migrated paths (RPC dispatch = #1265, streaming = #1266; wholesale
// retirement = #1267).
//
// Why this exists -- the live "Sofia not responding" bug. A browser opens
// MemqlService.Stream (via /memql/ws) on ONE bff replica and subscribes that
// socket to the bff's LOCAL events.Bus for the chat-reply topics. A reply is
// produced on a worker (cognition/agent), lands on THAT node's local bus, and
// is pushed over the mesh by EventBridge.forwardToPeers. The mesh is a star and
// #1245 skips live non-parent replicas, so the copy can miss the bff replica
// that actually owns the browser's WS -- the silent cross-replica drop.
//
// The fix inverts the addressing from physical ("deliver to bff-pod-3") to
// logical ("deliver to whoever owns space X"):
//
//   - Producer side (every node that emits a chat-reply event): on a LOCAL,
//     non-remote chat-reply event we Publish a Deliverable to the substrate
//     keyed by space:<partitionId>. The durable outbox row is the guarantee; the
//     EventBridge mesh forward stays as the deduped low-latency fast-path.
//   - Consumer side (bff only): a bff Subscribes to space:<partitionId> on the
//     substrate and re-publishes each deliverable onto its LOCAL bus -- where
//     the existing browser gRPC subscription already consumes it -- then Acks.
//     A bff lazily subscribes to a space the moment it observes a local
//     chat-reply event for it (the human utterance the browser just wrote, a
//     presence/canvas change it made) OR a local space-interest event
//     (participant join / session create+touch -- what every client writes on
//     opening a space, memql#1316). Both are the signal that THIS replica
//     anchors a socket that cares about the space, so the reply reaches it even
//     though it is not the producer's mesh parent and even when the client only
//     READS the space.
//
// Exactly-once for the browser stream is the substrate's per-subscription
// EventID dedup (the mesh-hint copy and the durable-pull copy of the same
// EventID collapse to one delivery); per-space ordering is the substrate's
// per-key monotonic seq + cursor. The EventID we publish is the graph row id
// suffixed with the bus event's emission timestamp (see publish), carried
// byte-identical on both paths so they can never double-deliver.
type ChatReplyDelivery struct {
	*component.Component

	substrate   DeliverySubstrate
	localBus    *events.Bus
	identity    *Identity
	isBFF       bool
	logger      *slog.Logger
	unsubscribe func()

	// republish re-emits a substrate deliverable onto the local bus. Defaulted
	// to localBus.Publish; overridable in tests to observe delivery.
	republish func(events.Event)

	mu      sync.Mutex
	subbed  map[string]context.CancelFunc // routing-key string -> subscription canceller
	ctx     context.Context
	stopped bool
}

const (
	// ChatReplyDeliveryComponentName names the component in the lifecycle.
	ChatReplyDeliveryComponentName = common.ComponentName("nodeChatReplyDelivery")
	// chatReplyDeliveryOrder runs just after EventBridge (46) so the local bus
	// and the substrate are both available; before NodeServer (48).
	chatReplyDeliveryOrder = 47
)

// Chat-reply concept ids whose graph.node.created/updated events must reach the
// WS-owning bff replica. Confirmed against the code: utterance + presence +
// textChunk carry a partitionId field; canvasState carries a space field (see
// resolveSpaceKey).
//
// NOTE the presence id: the concept is declared under
// @namespace("cognition:participant") (dsl/cognition/concepts.memql), so its
// id is v1:cognition:participant:presence -- NOT v1:cognition:presence, which
// this constant carried from #1264 until the #1316 cluster probe caught that
// presence events never matched the topic gate (mesh-only delivery, no durable
// publish, no consumer subscription trigger). Matches
// database/memory-nodes.ConceptCognitionParticipantPresence (not imported here
// to keep the node package off the database package).
//
// textChunk (memql#1326): the agent TEXT-chat path streams reply tokens as
// v1:cognition:text:chunk graph inserts (cognition's emitTextChunk),
// NOT over the #1266 gRPC stream channel -- so the chunks are chat-reply
// payload exactly like the committed utterance they precede. Left off this set
// at #1264, they rode only the mesh star: cognition's single parent dial lands
// on ONE bff replica and bff replicas hold no connections to each other, so the
// WS-owning replica structurally never saw them (live trace: 40 chunks on the
// parent-dialed bff, 0 on the sibling owning the browser) and streaming was
// dead -- the reply popped in atomically when the utterance landed via the
// substrate. Per-key seq also guarantees the committed utterance sorts after
// the turn's last chunk on the same space key.
//
// clientToolRequest (memql#1846): the cross-node client-tool relay REQUEST leg
// (cognition/voice -> browser). Cognition emits a v1:cognition:client:tool:request
// graph node carrying a required partitionId; the browser picks it up off its space
// subscription on the WS-owning bff replica and dispatches the UI-takeover tool.
// Before #1846 that request rode ONLY the best-effort mesh fast-path
// (EventBridge.forwardToPeers -> sendTarget), which silently skips any peer with
// Connection==nil. After a bff blue-green cutover the WS-owning replica is
// de-peered/reaped on cognition (it is the gRPC client, registered Monitored
// with no AttachConnection, so its entry is Connection==nil essentially always),
// so the request silently dropped and the UI takeover never appeared -- the
// request-leg twin of the #1835 response-leg drop fixed by #1843. Routing it
// through the substrate (keyed by space, exactly like the utterance it
// accompanies) makes the WS-owning bff replica consume it durably regardless of
// mesh de-peering. The fast-path stays as the low-latency hint; the substrate's
// per-(key,consumer) EventID dedup collapses the two copies so the browser never
// dispatches the tool twice. The request row id is the callId (deterministic),
// and the producer keys the Deliverable EventID per occurrence (rowId@ts) like
// every other chat-reply topic, so a re-emit is its own durable row.
const (
	conceptUtterance         = "v1:cognition:utterance"
	conceptPresence          = "v1:cognition:participant:presence"
	conceptTextChunk         = "v1:cognition:text:chunk"
	conceptCanvasState       = "v1:copresent:canvasState"
	conceptClientToolRequest = "v1:cognition:client:tool:request"
)

// Space-interest concept ids (memql#1316): graph events that signal "a client
// on THIS replica opened space X" without being chat-reply payload themselves.
// On every space open the CoPresent client joins the space (participant row,
// first open only -- idempotent content-addressed id) and creates a session row
// (EVERY open, fresh sessionId), and heartbeats the session while the space
// stays open (touchSession). Both concepts carry a required partitionId.
// These are consumer-side subscription triggers ONLY -- they are never
// published to the substrate (the producer set stays the chat-reply topics;
// participant/session events keep riding the mesh fast-path).
const (
	conceptParticipant = "v1:cognition:participant"
	conceptSession     = "v1:cognition:session"
)

// NewChatReplyDelivery builds the chat-reply delivery router over the given
// substrate. isBFF gates the consumer (Subscribe + re-publish) side: only bff
// replicas own browser WebSockets, so only they subscribe and fan back to the
// local bus. Every node runs the producer (Publish) side. Returns nil when the
// substrate is absent (single-node / non-mesh binaries) so the caller can skip
// wiring it without a nil-guard at every call site.
func NewChatReplyDelivery(identity *Identity, substrate DeliverySubstrate, localBus *events.Bus, isBFF bool, logger *slog.Logger) *ChatReplyDelivery {
	if substrate == nil || localBus == nil {
		return nil
	}
	comp, _ := component.New(ChatReplyDeliveryComponentName)
	d := &ChatReplyDelivery{
		Component: comp,
		substrate: substrate,
		localBus:  localBus,
		identity:  identity,
		isBFF:     isBFF,
		logger:    logger,
		subbed:    make(map[string]context.CancelFunc),
	}
	d.republish = d.localBus.Publish
	d.ConfigureLifecycle(
		component.WithRunHook(d.run),
		component.WithOnStopHook(d.cleanup),
	)
	return d
}

// Order returns the startup order.
func (*ChatReplyDelivery) Order() int { return chatReplyDeliveryOrder }

// run subscribes to the local bus for the chat-reply topics and drives both
// sides off each local event: Publish to the substrate (producer) and, on a
// bff, ensure a substrate subscription for the event's space (consumer).
func (d *ChatReplyDelivery) run(ctx context.Context, markStarted func()) error {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()

	d.unsubscribe = d.localBus.Subscribe("graph.node.#", func(event events.Event) {
		d.onLocalEvent(ctx, event)
	}, events.WithSubscriberName("nodeChatReplyDelivery"))

	markStarted()
	<-ctx.Done()
	return ctx.Err()
}

// onLocalEvent handles one local bus event. It Publishes locally-produced
// chat-reply events to the substrate, and -- on a bff -- ensures the space is
// subscribed so the durable stream fans back to this replica's browsers.
func (d *ChatReplyDelivery) onLocalEvent(ctx context.Context, event events.Event) {
	if !isChatReplyTopic(event.Topic) {
		// Space-interest topics (memql#1316): a LOCALLY-produced participant /
		// session event is the engine-side trace of a client on THIS replica
		// opening the space (join + create-session fire on every space open),
		// so subscribe the space even though the client has produced no
		// chat-reply event yet. This closes the #1259 passive-subscriber drop:
		// a replica whose browser only READS a space still pulls the durable
		// stream. Local only -- a REMOTE participant/session event is evidence
		// the space is active somewhere, not that this replica anchors a
		// browser for it.
		if d.isBFF && !event.IsRemote() && isSpaceInterestTopic(event.Topic) {
			if key, ok := resolveSpaceKey(event.Payload); ok {
				d.ensureSubscribed(key)
			}
		}
		return
	}
	key, ok := resolveSpaceKey(event.Payload)
	if !ok {
		return
	}

	// Consumer side (bff): a local chat-reply event for this space means a
	// browser on THIS replica is involved with it, so ensure we're subscribed
	// to the durable stream for the space. Done for events we originate AND for
	// events that arrived from a peer (mesh fast-path / inbound bridge) -- both
	// are evidence this replica cares about the space.
	if d.isBFF {
		d.ensureSubscribed(key)
	}

	// Producer side: only durably publish events PRODUCED here. A remote event
	// was already published to the outbox by its origin node; re-publishing it
	// would just be an idempotent no-op (same EventID), so skip the round-trip.
	if event.IsRemote() {
		return
	}
	d.publish(ctx, key, event)
}

// publish writes one chat-reply event to the durable outbox keyed by its space.
// The EventID is the graph row id suffixed with the bus event's nanosecond
// emission timestamp, so the durable copy and the EventBridge mesh-hint copy
// dedup against each other on the consumer.
//
// The suffix exists because the row id alone is NOT unique per occurrence for
// upsert-style concepts: presence keeps ONE deterministic row id per
// participant across every state transition (memQL is append-only -- each
// write is a new row VERSION under the same logical id), and update() re-emits
// its row id too. The outbox enforces uniqueness on EventID, so a bare row id
// silently swallowed every write after the first -- the "assistant stuck at
// Idle" cluster bug (memql#1324). The Deliverable is built exactly once per
// occurrence on the producing node, so both paths still carry the same bytes
// and genuine re-deliveries of one occurrence still collapse.
func (d *ChatReplyDelivery) publish(ctx context.Context, key RoutingKey, event events.Event) {
	rowID := stringField(event.Payload, "id")
	if rowID == "" {
		rowID = stringField(event.Payload, "nodeId")
	}
	if rowID == "" {
		// No stable row id to dedup on -- never publish an unkeyed deliverable
		// (it would double-deliver against the mesh copy). Drop to the mesh-only
		// path for this (degenerate) event.
		d.warn("chat-reply delivery: event has no stable id; not publishing to substrate",
			"topic", event.Topic, "key", key.String())
		return
	}
	ts := event.Timestamp
	if ts.IsZero() {
		// Defensive: every engine emission stamps Timestamp (events.NewEvent),
		// but a hand-built event must still get a per-occurrence id.
		ts = time.Now()
	}

	d.publishDeliverable(ctx, Deliverable{
		EventID:    rowID + "@" + strconv.FormatInt(ts.UnixNano(), 10),
		Key:        key,
		Topic:      event.Topic,
		Kind:       event.Kind,
		Payload:    event.Payload,
		OriginNode: d.nodeID(),
	})
}

// publishDeliverable is the substrate Publish call, split out so the producer
// path is testable without a live local bus.
func (d *ChatReplyDelivery) publishDeliverable(ctx context.Context, dl Deliverable) {
	if _, err := d.substrate.Publish(ctx, dl); err != nil {
		d.warn("chat-reply delivery: substrate publish failed",
			"topic", dl.Topic, "key", dl.Key.String(), "event_id", dl.EventID, "error", err)
	}
}

// ensureSubscribed starts a single durable subscription per space-key
// (idempotent per key). An EXISTING consumer replays from its cursor, then
// tails live, re-publishing each deliverable onto the local bus and Acking as
// it goes. A BRAND-NEW consumer starts at the space key's current high
// watermark (the memql#1328 default): consumer ids are per-pod, so every
// rollout mints brand-new consumers, and the pre-#1328 backlog replay
// re-delivered up to a full retention window of chat history to every fresh
// pod. Live chat is a tail concern -- the graph rows are the source of truth
// for history -- so the tip is the correct starting position here; the
// browser's own (re)connect fetches history from the graph, not the outbox.
func (d *ChatReplyDelivery) ensureSubscribed(key RoutingKey) {
	d.mu.Lock()
	if d.stopped || d.ctx == nil {
		d.mu.Unlock()
		return
	}
	if _, ok := d.subbed[key.String()]; ok {
		d.mu.Unlock()
		return
	}
	subCtx, cancel := context.WithCancel(d.ctx)
	d.subbed[key.String()] = cancel
	d.mu.Unlock()

	ch, err := d.substrate.Subscribe(subCtx, key, d.consumerID())
	if err != nil {
		d.mu.Lock()
		delete(d.subbed, key.String())
		d.mu.Unlock()
		cancel()
		d.warn("chat-reply delivery: substrate subscribe failed", "key", key.String(), "error", err)
		return
	}
	if d.logger != nil {
		d.logger.Info("chat-reply delivery: subscribed space to substrate",
			"key", key.String(), "consumer", d.consumerID())
	}
	go d.consume(subCtx, key, ch)
}

// consume drains one space subscription: re-publish each deliverable onto the
// local bus (so the browser's gRPC subscription delivers it) and Ack to advance
// the cursor. The re-published event is tagged with its OriginNode so the local
// EventBridge treats it as remote and does NOT re-forward it back over the mesh.
//
// A deliverable this node PRODUCED itself is Acked but NOT re-published: the
// original local-bus event already delivered it to this replica's browsers
// (handleBusEvent does not dedup by row id), so re-emitting the durable copy
// would deliver the SAME row to the browser twice. We must still advance the
// cursor past it (it is a real seq position) so a reconnect does not replay it.
// Cross-node replies (origin != self) are the rows this subscription exists to
// deliver, and they are re-published exactly once.
func (d *ChatReplyDelivery) consume(ctx context.Context, key RoutingKey, ch <-chan Deliverable) {
	self := d.nodeID()
	for {
		select {
		case <-ctx.Done():
			return
		case dl, ok := <-ch:
			if !ok {
				return
			}
			if dl.OriginNode == "" || dl.OriginNode != self {
				d.republish(events.Event{
					Topic:        dl.Topic,
					Kind:         dl.Kind,
					Payload:      dl.Payload,
					Metadata:     map[string]string{},
					OriginNodeId: deliverableOrigin(dl, self),
				})
			}
			if err := d.substrate.Ack(ctx, key, d.consumerID(), dl.Seq); err != nil {
				d.warn("chat-reply delivery: ack failed",
					"key", key.String(), "seq", dl.Seq, "error", err)
			}
		}
	}
}

// cleanup tears down the local-bus subscription and every space subscription.
func (d *ChatReplyDelivery) cleanup() {
	if d.unsubscribe != nil {
		d.unsubscribe()
		d.unsubscribe = nil
	}
	d.mu.Lock()
	d.stopped = true
	for k, cancel := range d.subbed {
		cancel()
		delete(d.subbed, k)
	}
	d.mu.Unlock()
}

// consumerID is the durable cursor identity for this node's subscription. The
// node id makes the cursor per-replica, so a different bff replica taking over a
// space gets its own clean replay (ADR 4.2 / 4.4) rather than inheriting our
// acked position.
func (d *ChatReplyDelivery) consumerID() string { return d.nodeID() }

func (d *ChatReplyDelivery) nodeID() string {
	if d.identity != nil {
		return d.identity.ID
	}
	return ""
}

func (d *ChatReplyDelivery) warn(msg string, args ...any) {
	if d.logger != nil {
		d.logger.Warn(msg, args...)
	}
}

// --- pure helpers (no receiver state; unit-testable directly) ----------------

// isChatReplyTopic reports whether a topic is a graph event that must reach the
// bff replica owning a user's WebSocket: the chat-reply stream (created/updated
// for utterance / presence / textChunk / canvasState) plus the cross-node
// client-tool relay REQUEST leg (created/updated for clientToolRequest, #1846).
// Deleted events are not part of the stream the browser renders/dispatches.
func isChatReplyTopic(topic string) bool {
	for _, concept := range []string{conceptUtterance, conceptPresence, conceptTextChunk, conceptCanvasState, conceptClientToolRequest} {
		if topic == events.BuildTopicWithConcept(events.TopicGraphNodeCreated, concept) ||
			topic == events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, concept) {
			return true
		}
	}
	return false
}

// isSpaceInterestTopic reports whether a topic is a space-interest graph event
// (created/updated for participant / session, memql#1316) -- the consumer-side
// subscription trigger for clients that open a space without producing any
// chat-reply event. Never a producer-side topic.
func isSpaceInterestTopic(topic string) bool {
	for _, concept := range []string{conceptParticipant, conceptSession} {
		if topic == events.BuildTopicWithConcept(events.TopicGraphNodeCreated, concept) ||
			topic == events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, concept) {
			return true
		}
	}
	return false
}

// resolveSpaceKey extracts the logical delivery key (space:<partitionId>) from a
// chat-reply event payload. The graph emitter flattens the node's fields onto
// the event payload, so the space id is a top-level field: utterance + presence
// use "partitionId", canvasState uses "space". We also fall back to the nested
// "payload" map for robustness.
func resolveSpaceKey(payload map[string]any) (RoutingKey, bool) {
	for _, field := range []string{"partitionId", "space"} {
		if v := stringField(payload, field); v != "" {
			return RoutingKey{Kind: "space", ID: v}, true
		}
	}
	if nested, ok := payload["payload"].(map[string]any); ok {
		for _, field := range []string{"partitionId", "space"} {
			if v := stringField(nested, field); v != "" {
				return RoutingKey{Kind: "space", ID: v}, true
			}
		}
	}
	return RoutingKey{}, false
}

// stringField reads a non-empty string field from a payload map.
func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// deliverableOrigin returns the origin node id to stamp on the re-published
// event so the local EventBridge treats it as remote (IsRemote()==true) and
// does not loop it back onto the mesh. Falls back to this node's id when the
// deliverable carries none.
func deliverableOrigin(dl Deliverable, self string) string {
	if dl.OriginNode != "" {
		return dl.OriginNode
	}
	return self
}
