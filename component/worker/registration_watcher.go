package worker

// The registration watcher (epic memql#5327, design D1).
//
// ===========================================================================
// ONE MECHANISM, TWO DECISIONS, NO NEW RPC
// ===========================================================================
// A machine's stream is held by exactly one agent replica, which is usually
// not the replica a revoke or a re-registration arrives on. Two findings from
// the 2026-09-13 audit are the same shape:
//
//   H-4  revoking a registration excluded it from ROUTING and left the stream
//        open, so a stolen laptop kept receiving dispatches.
//   M-4  a second stream for one machine on a second pod left both rows
//        flapping, with no supersede.
//
// Both are answered by reading the registration row itself. v1:worker:
// registration already carries broadcast routing rules (component/node/
// routing.go), so every agent replica already sees every write to it; this
// watcher is the consumer that asks two questions of each one and drains the
// stream when it holds it.
//
// WHY NOT A TARGETED RPC. A supersede message has to name the replica to send
// to -- which is precisely the field that is wrong when a pod has died. A
// broadcast is addressed to whoever is holding the stream, which is the only
// correct address for this.
//
// WHAT IT DOES NOT CLOSE. A replica that has lost its mesh connection sees no
// event (memql#5338). That is why the heartbeat's own credential re-check
// (session_liveness.go, design D3) is an INDEPENDENT backstop and not
// belt-and-braces: this half is fast and needs the mesh, that half is slower
// and needs nothing but the database the stream is already writing to.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

// RegistrationConcept is the concept whose graph events this watcher reads.
const RegistrationConcept = "v1:worker:registration"

// RegistrationEvent is the narrow projection of a graph.node event this
// watcher acts on.
//
// It is a STRUCT rather than the raw event because the two fields below are
// the whole of what the decision needs, and a watcher that took the event
// would be a watcher that could read the rest of the row -- which is how a
// mechanism acquires a second job.
type RegistrationEvent struct {
	RegistrationId  string
	RevokedAt       time.Time
	ConnectedNodeId string
}

// RegistrationWatcher ends the streams this replica holds when the graph says
// it should not be holding them.
type RegistrationWatcher struct {
	registry *Registry
	selfNode string
	logger   *slog.Logger
}

// NewRegistrationWatcher builds the watcher for one replica.
//
// selfNode is this replica's MEMQL_NODE_ID, threaded rather than read per
// event for the reason the server threads it: a process that answered "which
// node am I" differently at register and at supersede would drain the stream
// it had just accepted.
func NewRegistrationWatcher(registry *Registry, selfNode string, logger *slog.Logger) *RegistrationWatcher {
	return &RegistrationWatcher{
		registry: registry,
		selfNode: strings.TrimSpace(selfNode),
		logger:   logger,
	}
}

// Observe applies one registration event and reports the reason a stream was
// ended, or "" when nothing was.
//
// EVERY REPLICA RUNS THIS FOR EVERY EVENT, so the common answer is "" and the
// common cost is two string comparisons and one map lookup. Only the replica
// actually holding the named machine's stream does anything.
//
// The context is taken and unused on purpose: this is a subscriber callback
// and the seam should not have to change when a future ruling needs to read
// something. Nothing here blocks.
func (w *RegistrationWatcher) Observe(_ context.Context, ev RegistrationEvent) string {
	if w == nil || w.registry == nil {
		return ""
	}
	id := strings.TrimSpace(ev.RegistrationId)
	if id == "" {
		return ""
	}

	reason := w.verdict(ev)
	if reason == "" {
		return ""
	}
	if !w.registry.Terminate(id, reason) {
		// The ordinary answer on every replica but one.
		return ""
	}
	if w.logger != nil {
		w.logger.Info("worker: ended a stream on a registration event",
			"registration_id", id,
			"reason", reason,
			"self_node_id", w.selfNode,
			"connected_node_id", strings.TrimSpace(ev.ConnectedNodeId),
		)
	}
	return reason
}

// verdict is the whole decision, separated from the acting so it can be tested
// without a registry and read without following a call.
//
// ORDER MATTERS. Revocation is checked first because a revoked row may ALSO
// name another node -- a machine that reconnected elsewhere and was then
// revoked -- and "revoked" is the reason a reader needs; reporting that stream
// as merely superseded would file a security decision as a routine handover.
func (w *RegistrationWatcher) verdict(ev RegistrationEvent) string {
	if !ev.RevokedAt.IsZero() {
		return DisconnectReasonRevoked
	}
	holder := strings.TrimSpace(ev.ConnectedNodeId)
	switch {
	case holder == "":
		// Nobody claims the stream. This is what the disconnect path and the
		// stale-hold sweep both write, and neither is a reason to end a
		// stream: our own clear is the most likely author of this very event.
		return ""
	case w.selfNode == "":
		// A replica that does not know its own id cannot tell "somebody else
		// holds this" from "I do". Refusing to act is the only safe reading --
		// the alternative drains every stream on the node on the first event.
		return ""
	case holder == w.selfNode:
		return ""
	default:
		return DisconnectReasonSuperseded
	}
}

// ---------------------------------------------------------------------------
// The bus bridge
// ---------------------------------------------------------------------------

// RegistrationWatcherComponent is the bus-side ComponentName.
const RegistrationWatcherComponent = common.ComponentName("worker.registrationWatcher")

// registrationPattern matches graph.node.created AND graph.node.updated for
// v1:worker:registration in one subscription: "*" matches exactly one segment
// and a concept id carries colons rather than dots, so it is one segment
// either way.
//
// CREATED matters as much as UPDATED, and that is not forward-compatibility.
// A machine whose row was never written before -- a first pair while an old
// stream is somehow still open -- publishes created, and the supersede check
// is exactly as right there. `deleted` is covered too and fires for nothing
// today: there is no delete mutation for this concept, and a stream held
// against a row that has gone would be the same bug this file exists for.
const registrationPattern = "graph.node.*.v1:worker:registration"

// RegistrationEventSubscriber wires the bus to the watcher.
//
// It is a Dependency so app/ holds it beside the worker service it drains,
// the same shape edge.SiteInvalidationSubscriber and
// observe.CodeProfileSubscriber use.
type RegistrationEventSubscriber struct {
	logger      *slog.Logger
	bus         *events.Bus
	watcher     *RegistrationWatcher
	unsubscribe func()
	doneCh      chan struct{}
	readyCh     chan struct{}
	running     atomic.Bool
	startOnce   sync.Once
	stopOnce    sync.Once
}

// NewRegistrationEventSubscriber constructs the bridge. Subscribe happens in
// Start, so a bus that is not ready is a logged startup no-op rather than a
// constructor panic.
func NewRegistrationEventSubscriber(logger *slog.Logger, bus *events.Bus, watcher *RegistrationWatcher) *RegistrationEventSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	return &RegistrationEventSubscriber{
		logger:  logger.With("component", "worker.registrationWatcher"),
		bus:     bus,
		watcher: watcher,
		doneCh:  make(chan struct{}),
		readyCh: make(chan struct{}),
	}
}

// Start subscribes to the registration event pattern. Idempotent.
func (s *RegistrationEventSubscriber) Start(_ context.Context) {
	s.startOnce.Do(func() {
		defer close(s.readyCh)
		if s.bus == nil || s.watcher == nil {
			s.logger.Warn("no event bus or watcher available -- a revoked machine's stream will end on the heartbeat re-check instead")
			close(s.doneCh)
			return
		}
		s.running.Store(true)
		s.unsubscribe = s.bus.Subscribe(
			registrationPattern,
			s.handle,
			events.WithSubscriberName("worker.registrationWatcher"),
		)
		close(s.doneCh)
		s.logger.Info("worker registration watcher active", "pattern", registrationPattern)
	})
}

// Stop closes the subscription.
func (s *RegistrationEventSubscriber) Stop(_ context.Context) {
	s.stopOnce.Do(func() {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		s.running.Store(false)
	})
}

// Standard Dependency surface.
func (s *RegistrationEventSubscriber) IsRunning() bool { return s.running.Load() }
func (s *RegistrationEventSubscriber) Order() int      { return 10 }
func (s *RegistrationEventSubscriber) ComponentName() common.ComponentName {
	return RegistrationWatcherComponent
}
func (s *RegistrationEventSubscriber) Ready() <-chan struct{} { return s.readyCh }

// handle projects one graph event onto RegistrationEvent and hands it to the
// watcher.
//
// The payload's fields are FLATTENED onto the envelope as well as nested under
// "payload" (component/memql/executor_mutation.go), and this reads the nested
// object when it is there and the envelope otherwise -- the flattened copy is
// the one an .updated event is documented to carry, and the nested one is what
// both verbs agree on.
//
// A MALFORMED EVENT IS DROPPED rather than acted on. The two decisions this
// feeds both END a live connection, and the only safe reading of an event that
// does not say what it should is that it says nothing.
func (s *RegistrationEventSubscriber) handle(ev events.Event) {
	fields := ev.Payload
	if nested, ok := ev.Payload["payload"].(map[string]any); ok && nested != nil {
		fields = nested
	}
	id := strings.TrimSpace(eventString(ev.Payload, "id"))
	if id == "" {
		id = strings.TrimSpace(eventString(fields, "id"))
	}
	if id == "" {
		s.logger.Debug("registration event names no row", "topic", ev.Topic)
		return
	}
	reason := s.watcher.Observe(context.Background(), RegistrationEvent{
		RegistrationId:  id,
		RevokedAt:       eventTime(fields, "revokedAt"),
		ConnectedNodeId: eventString(fields, "connectedNodeId"),
	})
	if reason != "" {
		s.logger.Info("worker stream ended by a registration event",
			"registration_id", id,
			"reason", reason,
			"topic", ev.Topic,
			"originNode", ev.OriginNodeId,
		)
	}
}

func eventString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return strings.TrimSpace(str)
	}
	return ""
}

// eventTime parses an RFC3339 timestamp off an event field.
//
// AN UNPARSEABLE VALUE READS AS ZERO -- "not revoked" -- and that is the
// fail-open direction on purpose. This decides whether to end somebody's live
// connection, and a malformed timestamp is not evidence that a revoke
// happened. The heartbeat re-check (design D3) reaches the same row within one
// interval and decides from the credential itself, where the answer is a
// boolean nothing has to parse.
func eventTime(m map[string]any, key string) time.Time {
	raw := eventString(m, key)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}
