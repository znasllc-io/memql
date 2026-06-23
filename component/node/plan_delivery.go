package node

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

// PlanDelivery routes the plan-lifecycle graph events
// (graph.node.created / graph.node.updated for v1:planner:plan) through the
// durable DeliverySubstrate (memql#1264, the #1263 outbox + cursor backbone),
// giving plan dispatch a real at-least-once delivery leg (memql#1495) instead
// of relying on the mesh fast-path plus the #1389 DB-poll recovery backstop.
//
// Why this exists -- the durability claim in eventbridge.go ("the mesh
// fast-path is purely a latency optimization; the durable substrate is the
// delivery guarantee") did NOT actually hold for plan graph events. Plan
// events ride the EventBridge broadcast ONLY (the graph.node.created.v1:planner:*
// routing rule). The planner node consumes them off its LOCAL bus
// (PlannerIntegration.Start subscribes PlannerAgentLoop.HandlePlanCreated /
// HandlePlanUpdated). When the broadcast is dropped while the planner is
// routeless -- the mesh peer-table decay incident (memql#1388, PR #1494) is the
// concrete trigger -- the plan-created event never reaches the planner, and the
// Plan strands in planning/queued. The #1389 stranded-plan watchdog recovers it
// by DB poll, but that is a recovery backstop measured in minutes, not a
// delivery guarantee.
//
// PlanDelivery mirrors ChatReplyDelivery exactly, with the parallel addressing
// inversion (physical "deliver to planner-pod-N" -> logical "deliver to whoever
// consumes plan lifecycle events"):
//
//   - Producer side (every mesh node that emits a plan graph event): on a
//     LOCAL, non-remote plan graph event, Publish a Deliverable to the
//     substrate keyed by the fixed logical key plan:lifecycle. The durable
//     outbox row is the guarantee; the EventBridge mesh broadcast stays as the
//     deduped low-latency fast-path. The plan-write site is the BFF
//     (createPlan) and the planner itself (status transitions), so both
//     run the producer side.
//   - Consumer side (planner only): a planner Subscribes ONCE to plan:lifecycle
//     on the substrate and re-publishes each deliverable onto its LOCAL bus --
//     where the existing PlannerAgentLoop subscriptions already consume it --
//     then Acks. The durable cursor means a planner whose mesh route was dead
//     when a plan event was produced catches up on cursor-pull replay (it
//     resumes from its cursor on every reconnect/poll), exactly the route-gap
//     the #1389 watchdog otherwise had to paper over.
//
// Idempotency vs the mesh copy -- DELIBERATELY DIFFERENT from chat-reply.
// ChatReplyDelivery SUPPRESSES the inbound mesh copy on the bff because the
// browser must receive each reply EXACTLY ONCE and the substrate is its single
// delivery path. Plan events have no such constraint: the planner's consumers
// are ALREADY idempotent -- HandlePlanCreated / RecoverStranded route through a
// per-plan once-guard (memql#1155) and the cross-replica ClusterExecutionGuard
// claims execution per planId@startedAt (memql#1363). So a plan event arriving
// on BOTH the mesh fast-path AND the durable replay is collapsed downstream;
// the mesh copy is NOT suppressed. PlanDelivery is purely an ADDITIONAL
// at-least-once path -- it can only ADD a delivery the planner would otherwise
// have missed, never cause a double-execution, because dedup lives in the
// consumer, not at the substrate boundary.
//
// Exactly-once for a single occurrence is still the substrate's per-subscription
// EventID dedup (the durable-pull copy and any mesh-hint copy of the same
// EventID collapse to one re-publish); per-key ordering is the substrate's
// per-key monotonic seq + cursor. The EventID is the plan row id suffixed with
// the bus event's emission timestamp (see publish), so a status transition that
// re-inserts under the same plan id (memQL is append-only; each write is a new
// row VERSION under the same logical id) still gets a distinct durable row
// rather than being swallowed by the outbox's EventID uniqueness -- the same
// per-occurrence keying the chat-reply presence path needed (memql#1324).
type PlanDelivery struct {
	*component.Component

	substrate DeliverySubstrate
	localBus  *events.Bus
	identity  *Identity
	isPlanner bool
	logger    *slog.Logger

	unsubscribe func()

	// republish re-emits a substrate deliverable onto the local bus. Defaulted
	// to localBus.Publish; overridable in tests to observe delivery.
	republish func(events.Event)

	mu        sync.Mutex
	cancelSub context.CancelFunc
	ctx       context.Context
	stopped   bool
}

const (
	// PlanDeliveryComponentName names the component in the lifecycle.
	PlanDeliveryComponentName = common.ComponentName("nodePlanDelivery")
	// planDeliveryOrder runs just after ChatReplyDelivery (47) so the local bus
	// and the substrate are both available; before NodeServer (48).
	planDeliveryOrder = 47

	// conceptPlan is the plan-lifecycle concept id whose graph.node.created /
	// graph.node.updated events drive the planner. Pinned as a literal +
	// asserted against the DSL by TestPlanDeliveryTopicGate, the same way the
	// chat-reply concept ids are -- a symbolic comparison can't catch a constant
	// that drifted from the DSL (cf. the presence-id drift, memql#1316).
	conceptPlan = "v1:planner:plan"
)

// planLifecycleKey is the single logical delivery key every plan graph event is
// published to. Unlike chat-reply's per-space keys, the planner is a small
// fixed consumer set (the planner-tagged replicas) that must hear EVERY plan
// event, so one shared key with one durable cursor per planner replica is the
// right shape: a planner that was routeless replays seq>cursor and catches up
// on all missed plan events in order. Deleted events are not part of the
// dispatch stream (the planner reacts to created + status transitions).
var planLifecycleKey = RoutingKey{Kind: "plan", ID: "lifecycle"}

// NewPlanDelivery builds the plan-delivery router over the given substrate.
// isPlanner gates the consumer (Subscribe + re-publish) side: only planner
// replicas consume plan lifecycle events off their local bus, so only they
// subscribe and fan the durable stream back. Every node runs the producer
// (Publish) side -- the BFF writes new Plans, the planner writes status
// transitions. Returns nil when the substrate or local bus is absent
// (single-node / non-mesh binaries) so the caller can skip wiring it without a
// nil-guard at every call site.
func NewPlanDelivery(identity *Identity, substrate DeliverySubstrate, localBus *events.Bus, isPlanner bool, logger *slog.Logger) *PlanDelivery {
	if substrate == nil || localBus == nil {
		return nil
	}
	comp, _ := component.New(PlanDeliveryComponentName)
	d := &PlanDelivery{
		Component: comp,
		substrate: substrate,
		localBus:  localBus,
		identity:  identity,
		isPlanner: isPlanner,
		logger:    logger,
	}
	d.republish = d.localBus.Publish
	d.ConfigureLifecycle(
		component.WithRunHook(d.run),
		component.WithOnStopHook(d.cleanup),
	)
	return d
}

// Order returns the startup order.
func (*PlanDelivery) Order() int { return planDeliveryOrder }

// run subscribes to the local bus for the plan-lifecycle topics (producer side)
// and, on a planner, opens the single durable consumer subscription for the
// plan lifecycle key (consumer side).
func (d *PlanDelivery) run(ctx context.Context, markStarted func()) error {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()

	d.unsubscribe = d.localBus.Subscribe("graph.node.#", func(event events.Event) {
		d.onLocalEvent(ctx, event)
	}, events.WithSubscriberName("nodePlanDelivery"))

	// Consumer side (planner): subscribe the plan-lifecycle key once. An
	// existing cursor row resumes exactly; a brand-new consumer (fresh planner
	// pod) starts at the key's current high watermark -- the same rollout-safe
	// default chat-reply uses (memql#1328). Catch-up after a route gap WHILE the
	// planner is up is the cursor resume on the durable poll; a fresh pod that
	// missed events produced before it ever subscribed is the #1389 watchdog's
	// remaining job (the substrate cannot retro-deliver to a consumer that did
	// not exist yet), so this is strictly additive to that backstop.
	if d.isPlanner {
		d.startConsumer(ctx)
	}

	markStarted()
	<-ctx.Done()
	return ctx.Err()
}

// onLocalEvent handles one local bus event. It Publishes locally-produced plan
// graph events to the substrate (producer side). The consumer side is a single
// fixed subscription opened in run(), so -- unlike chat-reply -- there is no
// per-event ensureSubscribed here.
func (d *PlanDelivery) onLocalEvent(ctx context.Context, event events.Event) {
	if !isPlanTopic(event.Topic) {
		return
	}
	// Producer side: only durably publish events PRODUCED here. A remote event
	// was already published to the outbox by its origin node; re-publishing it
	// would be an idempotent no-op (same EventID), so skip the round-trip. It
	// also means the planner does not re-publish its own replayed events back to
	// the substrate (the re-published deliverable is stamped remote).
	if event.IsRemote() {
		return
	}
	planId := stringField(event.Payload, "id")
	if planId == "" {
		// No stable plan id to dedup on -- never publish an unkeyed deliverable
		// (it would double-deliver against the mesh copy). Drop to the mesh-only
		// path for this (degenerate) event.
		d.warn("plan delivery: event has no stable plan id; not publishing to substrate",
			"topic", event.Topic)
		return
	}
	d.publish(ctx, planId, event)
}

// publish writes one plan graph event to the durable outbox under the shared
// plan-lifecycle key. The EventID is the plan row id suffixed with the bus
// event's nanosecond emission timestamp, so each status transition of the same
// plan id is its own durable row (the outbox enforces EventID uniqueness; a bare
// row id would swallow every transition after the first, the chat-reply presence
// bug memql#1324) and the durable copy dedups byte-identically against any mesh
// hint of the same occurrence.
func (d *PlanDelivery) publish(ctx context.Context, planId string, event events.Event) {
	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	d.publishDeliverable(ctx, Deliverable{
		EventID:    planId + "@" + strconv.FormatInt(ts.UnixNano(), 10),
		Key:        planLifecycleKey,
		Topic:      event.Topic,
		Kind:       event.Kind,
		Payload:    event.Payload,
		OriginNode: d.nodeID(),
	})
}

// publishDeliverable is the substrate Publish call, split out so the producer
// path is testable without a live local bus.
func (d *PlanDelivery) publishDeliverable(ctx context.Context, dl Deliverable) {
	if _, err := d.substrate.Publish(ctx, dl); err != nil {
		d.warn("plan delivery: substrate publish failed",
			"topic", dl.Topic, "event_id", dl.EventID, "error", err)
	}
}

// startConsumer opens the single durable subscription for the plan-lifecycle
// key and drains it in a goroutine. Idempotent: a second call while already
// subscribed is a no-op.
func (d *PlanDelivery) startConsumer(ctx context.Context) {
	d.mu.Lock()
	if d.stopped || d.cancelSub != nil {
		d.mu.Unlock()
		return
	}
	base := d.ctx
	if base == nil {
		base = ctx
	}
	subCtx, cancel := context.WithCancel(base)
	d.cancelSub = cancel
	d.mu.Unlock()

	ch, err := d.substrate.Subscribe(subCtx, planLifecycleKey, d.consumerID())
	if err != nil {
		d.mu.Lock()
		d.cancelSub = nil
		d.mu.Unlock()
		cancel()
		d.warn("plan delivery: substrate subscribe failed", "key", planLifecycleKey.String(), "error", err)
		return
	}
	if d.logger != nil {
		d.logger.Info("plan delivery: subscribed plan-lifecycle key to substrate",
			"key", planLifecycleKey.String(), "consumer", d.consumerID())
	}
	go d.consume(subCtx, ch)
}

// consume drains the plan-lifecycle subscription: re-publish each deliverable
// onto the local bus (so the PlannerAgentLoop subscribers fire) and Ack to
// advance the cursor. The re-published event is tagged with its OriginNode so
// the local EventBridge treats it as remote and does NOT re-forward it back over
// the mesh -- and so PlanDelivery's own producer side (onLocalEvent) skips it.
//
// A deliverable this node PRODUCED itself (origin == self) is Acked but NOT
// re-published: the original local-bus event already delivered it to this
// planner's consumers, so re-emitting the durable copy would feed a second
// (deduped, but wasteful) trigger. We must still advance the cursor past it (it
// is a real seq position) so a reconnect does not replay it. Cross-node plan
// events (origin != self) are the rows this subscription exists to deliver, and
// they are re-published exactly once per occurrence (the substrate's
// per-subscription EventID dedup collapses any mesh-hint duplicate).
func (d *PlanDelivery) consume(ctx context.Context, ch <-chan Deliverable) {
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
			if err := d.substrate.Ack(ctx, planLifecycleKey, d.consumerID(), dl.Seq); err != nil {
				d.warn("plan delivery: ack failed",
					"key", planLifecycleKey.String(), "seq", dl.Seq, "error", err)
			}
		}
	}
}

// cleanup tears down the local-bus subscription and the durable subscription.
func (d *PlanDelivery) cleanup() {
	if d.unsubscribe != nil {
		d.unsubscribe()
		d.unsubscribe = nil
	}
	d.mu.Lock()
	d.stopped = true
	if d.cancelSub != nil {
		d.cancelSub()
		d.cancelSub = nil
	}
	d.mu.Unlock()
}

// consumerID is the durable cursor identity for this node's subscription. The
// node id makes the cursor per-replica, so a different planner replica gets its
// own clean resume rather than inheriting another replica's acked position
// (ADR 4.2 / 4.4).
func (d *PlanDelivery) consumerID() string { return d.nodeID() }

func (d *PlanDelivery) nodeID() string {
	if d.identity != nil {
		return d.identity.ID
	}
	return ""
}

func (d *PlanDelivery) warn(msg string, args ...any) {
	if d.logger != nil {
		d.logger.Warn(msg, args...)
	}
}

// --- pure helpers (no receiver state; unit-testable directly) ----------------

// isPlanTopic reports whether a topic is a plan-lifecycle graph event
// (created / updated for v1:planner:plan). Deleted events are not part of the
// dispatch stream the planner reacts to.
func isPlanTopic(topic string) bool {
	return topic == events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptPlan) ||
		topic == events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptPlan)
}
