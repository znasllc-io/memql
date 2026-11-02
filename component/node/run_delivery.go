package node

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

// RunDelivery also retains work.run.recovery scheduling hints. They are not
// graph writes: the consumer must re-read the exact owned stale run before
// claiming, and the producer observes a real claim rather than treating delivery
// as progress. This reuses the lifecycle cursor; it transfers no live session.
//
// RunDelivery routes the run-lifecycle graph events (graph.node.created /
// graph.node.updated for v1:work:run) through the durable DeliverySubstrate
// (memql#1264, the #1263 outbox + cursor backbone), giving run dispatch a real
// at-least-once delivery leg (memql#1495) instead of relying on the mesh
// fast-path alone.
//
// PORTED FROM THE PLAN LIFECYCLE (memql#5053). It was PlanDelivery, and the
// incident that produced it is the reason it was ported rather than deleted
// with the concept: the durability claim in eventbridge.go ("the mesh
// fast-path is purely a latency optimization; the durable substrate is the
// delivery guarantee") did NOT hold for graph events that ride the broadcast
// ONLY. When the broadcast was dropped while the consuming node was routeless
// -- the mesh peer-table decay incident (memql#1388, PR #1494) -- the event
// never arrived and the work stranded.
//
// The work spine has exactly that shape: v1:work:run events are broadcast
// (component/node/routing.go), and the RUN DISPATCHER (memql#5054) consumes
// them off its local bus to claim and execute a run. A dropped broadcast means
// a compiled run that nobody executes -- which the abandoned sweep then closes
// with a message about a node going away. So the guarantee moves with the
// mechanism.
//
// What did NOT move is the recovery backstop: the plan side had the #1389
// stranded-plan watchdog polling the database, and the run side has no
// equivalent. That makes this leg MORE load-bearing here than it was there,
// not less.
//
// RunDelivery mirrors ChatReplyDelivery exactly, with the parallel addressing
// inversion (physical "deliver to agent-pod-N" -> logical "deliver to whoever
// executes runs"):
//
//   - Producer side (every mesh node that emits a run graph event): on a
//     LOCAL, non-remote run graph event, Publish a Deliverable to the
//     substrate keyed by the fixed logical key run:lifecycle. The durable
//     outbox row is the guarantee; the EventBridge mesh broadcast stays as the
//     deduped low-latency fast-path. A run row is written by the node that
//     opened it and by the node executing it, so both run the producer side.
//   - Consumer side (executing replicas only): one Subscribes ONCE to
//     run:lifecycle on the substrate and re-publishes each deliverable onto
//     its LOCAL bus -- where the run dispatcher's subscription already
//     consumes it -- then Acks. The durable cursor means a replica whose mesh
//     route was dead when a run event was produced catches up on cursor-pull
//     replay, which is the route gap that would otherwise leave a compiled run
//     unexecuted until the abandoned sweep closed it.
//
// Idempotency vs the mesh copy -- DELIBERATELY DIFFERENT from chat-reply.
// ChatReplyDelivery SUPPRESSES the inbound mesh copy on the bff because the
// browser must receive each reply EXACTLY ONCE and the substrate is its single
// delivery path. Run events have no such constraint, and the reason is the
// dispatcher's own design: it CLAIMS a run before executing it, against a
// Postgres-backed guard keyed on the run id (memql#5054). So a run event
// arriving on BOTH the mesh fast-path and the durable replay is collapsed by
// that claim; the mesh copy is NOT suppressed. RunDelivery is purely an
// ADDITIONAL at-least-once path -- it can only ADD a delivery an executing
// replica would otherwise have missed, never cause a double-execution, because
// the dedup lives in the consumer rather than at the substrate boundary.
//
// Exactly-once for a single occurrence is still the substrate's per-subscription
// EventID dedup (the durable-pull copy and any mesh-hint copy of the same
// EventID collapse to one re-publish); per-key ordering is the substrate's
// per-key monotonic seq + cursor. The EventID is the run row id suffixed with
// the bus event's emission timestamp (see publish), so a status transition that
// re-inserts under the same run id (MemQL is append-only; each write is a new
// row VERSION under the same logical id) still gets a distinct durable row
// rather than being swallowed by the outbox's EventID uniqueness -- the same
// per-occurrence keying the chat-reply presence path needed (memql#1324).
type RunDelivery struct {
	*component.Component

	substrate DeliverySubstrate
	localBus  *events.Bus
	identity  *Identity
	runsSteps bool
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
	// RunDeliveryComponentName names the component in the lifecycle.
	RunDeliveryComponentName = common.ComponentName("nodeRunDelivery")
	// runDeliveryOrder runs just after ChatReplyDelivery (47) so the local bus
	// and the substrate are both available; before NodeServer (48).
	runDeliveryOrder = 47

	// conceptRun is the run-lifecycle concept id whose graph.node.created /
	// graph.node.updated events drive the run dispatcher. Pinned as a literal
	// + asserted against the DSL by the topic gate, the same way the
	// chat-reply concept ids are -- a symbolic comparison can't catch a
	// constant that drifted from the DSL (cf. the presence-id drift,
	// memql#1316).
	conceptRun = "v1:work:run"
)

// runLifecycleKey is the single logical delivery key every run graph event is
// published to. Unlike chat-reply's per-space keys, the consumer set is small
// and fixed (the agent-tagged replicas) and every one of them must hear EVERY
// run event, so one shared key with one durable cursor per replica is the
// right shape: a replica that was routeless replays seq>cursor and catches up
// on the runs it missed, in order. Deleted events are not part of the dispatch
// stream (the dispatcher reacts to created + status transitions).
var runLifecycleKey = RoutingKey{Kind: "run", ID: "lifecycle"}

// NewRunDelivery builds the run-delivery router over the given substrate.
// runsSteps gates the consumer (Subscribe + re-publish) side: replicas that
// EXECUTE runs (agent) or COMPILE them (planner) consume run lifecycle events
// off their local bus, so only they subscribe and fan the durable stream back.
// Every node runs the producer (Publish) side -- a run row is written by
// whichever node opened it (often the bff) and by whichever one is compiling
// or executing it. Returns nil when the substrate or local bus is absent
// (single-node / non-mesh binaries) so the caller can skip wiring it without a
// nil-guard at every call site.
func NewRunDelivery(identity *Identity, substrate DeliverySubstrate, localBus *events.Bus, runsSteps bool, logger *slog.Logger) *RunDelivery {
	if substrate == nil || localBus == nil {
		return nil
	}
	comp, _ := component.New(RunDeliveryComponentName)
	d := &RunDelivery{
		Component: comp,
		substrate: substrate,
		localBus:  localBus,
		identity:  identity,
		runsSteps: runsSteps,
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
func (*RunDelivery) Order() int { return runDeliveryOrder }

// run subscribes to the local bus for the run-lifecycle topics (producer side)
// and, on an executing replica, opens the single durable consumer subscription for
// the run lifecycle key (consumer side).
func (d *RunDelivery) run(ctx context.Context, markStarted func()) error {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()

	stopGraph := d.localBus.Subscribe("graph.node.#", func(event events.Event) {
		d.onLocalEvent(ctx, event)
	}, events.WithSubscriberName("nodeRunDelivery"))
	stopRecovery := d.localBus.Subscribe(events.TopicWorkRunRecovery, func(event events.Event) {
		d.onLocalEvent(ctx, event)
	}, events.WithSubscriberName("nodeRunRecoveryDelivery"))
	d.unsubscribe = func() { stopGraph(); stopRecovery() }

	// Consumer side (executing replica): subscribe the run-lifecycle key once. An
	// existing cursor row resumes exactly; a brand-new consumer (fresh replica
	// pod) starts at the key's current high watermark -- the same rollout-safe
	// default chat-reply uses (memql#1328). Catch-up after a route gap WHILE the
	// replica is up is the cursor resume on the durable poll; a fresh pod that
	// missed events produced before it ever subscribed is the #1389 watchdog's
	// remaining job (the substrate cannot retro-deliver to a consumer that did
	// not exist yet), so this is strictly additive to that backstop.
	if d.runsSteps {
		d.startConsumer(ctx)
	}

	markStarted()
	<-ctx.Done()
	return ctx.Err()
}

// onLocalEvent handles one local bus event. It Publishes locally-produced run
// graph events to the substrate (producer side). The consumer side is a single
// fixed subscription opened in run(), so -- unlike chat-reply -- there is no
// per-event ensureSubscribed here.
func (d *RunDelivery) onLocalEvent(ctx context.Context, event events.Event) {
	if !isRunTopic(event.Topic) {
		return
	}
	// Producer side: only durably publish events PRODUCED here. A remote event
	// was already published to the outbox by its origin node; re-publishing it
	// would be an idempotent no-op (same EventID), so skip the round-trip. It
	// also means an executing replica does not re-publish its own replayed events back to
	// the substrate (the re-published deliverable is stamped remote).
	if event.IsRemote() {
		return
	}
	runId := stringField(event.Payload, "id")
	if runId == "" {
		// No stable run id to dedup on -- never publish an unkeyed deliverable
		// (it would double-deliver against the mesh copy). Drop to the mesh-only
		// path for this (degenerate) event.
		d.warn("run delivery: event has no stable run id; not publishing to substrate",
			"topic", event.Topic)
		return
	}
	d.publish(ctx, runId, event)
}

// publish writes one run graph event to the durable outbox under the shared
// run-lifecycle key. The EventID is the run row id suffixed with the bus
// event's nanosecond emission timestamp, so each status transition of the same
// run id is its own durable row (the outbox enforces EventID uniqueness; a bare
// row id would swallow every transition after the first, the chat-reply presence
// bug memql#1324) and the durable copy dedups byte-identically against any mesh
// hint of the same occurrence.
func (d *RunDelivery) publish(ctx context.Context, runId string, event events.Event) {
	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	d.publishDeliverable(ctx, Deliverable{
		EventID:    runId + "@" + strconv.FormatInt(ts.UnixNano(), 10),
		Key:        runLifecycleKey,
		Topic:      event.Topic,
		Kind:       event.Kind,
		Payload:    event.Payload,
		OriginNode: d.nodeID(),
	})
}

// publishDeliverable is the substrate Publish call, split out so the producer
// path is testable without a live local bus.
func (d *RunDelivery) publishDeliverable(ctx context.Context, dl Deliverable) {
	if _, err := d.substrate.Publish(ctx, dl); err != nil {
		d.warn("run delivery: substrate publish failed",
			"topic", dl.Topic, "event_id", dl.EventID, "error", err)
	}
}

// startConsumer opens the single durable subscription for the run-lifecycle
// key and drains it in a goroutine. Idempotent: a second call while already
// subscribed is a no-op.
func (d *RunDelivery) startConsumer(ctx context.Context) {
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

	ch, err := d.substrate.Subscribe(subCtx, runLifecycleKey, d.consumerID())
	if err != nil {
		d.mu.Lock()
		d.cancelSub = nil
		d.mu.Unlock()
		cancel()
		d.warn("run delivery: substrate subscribe failed", "key", runLifecycleKey.String(), "error", err)
		return
	}
	if d.logger != nil {
		d.logger.Info("run delivery: subscribed run-lifecycle key to substrate",
			"key", runLifecycleKey.String(), "consumer", d.consumerID())
	}
	go d.consume(subCtx, ch)
}

// consume drains the run-lifecycle subscription: re-publish each deliverable
// onto the local bus (so the run dispatcher fires) and Ack to
// advance the cursor. The re-published event is tagged with its OriginNode so
// the local EventBridge treats it as remote and does NOT re-forward it back over
// the mesh -- and so RunDelivery's own producer side (onLocalEvent) skips it.
//
// A deliverable this node PRODUCED itself (origin == self) is Acked but NOT
// re-published: the original local-bus event already delivered it to this
// executing replica's consumers, so re-emitting the durable copy would feed a second
// (deduped, but wasteful) trigger. We must still advance the cursor past it (it
// is a real seq position) so a reconnect does not replay it. Cross-node run
// events (origin != self) are the rows this subscription exists to deliver, and
// they are re-published exactly once per occurrence (the substrate's
// per-subscription EventID dedup collapses any mesh-hint duplicate).
func (d *RunDelivery) consume(ctx context.Context, ch <-chan Deliverable) {
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
			if err := d.substrate.Ack(ctx, runLifecycleKey, d.consumerID(), dl.Seq); err != nil {
				d.warn("run delivery: ack failed",
					"key", runLifecycleKey.String(), "seq", dl.Seq, "error", err)
			}
		}
	}
}

// cleanup tears down the local-bus subscription and the durable subscription.
func (d *RunDelivery) cleanup() {
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
// node id makes the cursor per-replica, so a different executing replica replica gets its
// own clean resume rather than inheriting another replica's acked position
// (ADR 4.2 / 4.4).
func (d *RunDelivery) consumerID() string { return d.nodeID() }

func (d *RunDelivery) nodeID() string {
	if d.identity != nil {
		return d.identity.ID
	}
	return ""
}

func (d *RunDelivery) warn(msg string, args ...any) {
	if d.logger != nil {
		d.logger.Warn(msg, args...)
	}
}

// --- pure helpers (no receiver state; unit-testable directly) ----------------

// isRunTopic admits run lifecycle creation/updates and the explicitly typed
// recovery hint. Deleted events are not part of the
// dispatch stream the executing replica reacts to.
func isRunTopic(topic string) bool {
	return topic == events.TopicWorkRunRecovery ||
		topic == events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptRun) ||
		topic == events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, conceptRun)
}
