package memql

// Structured graph subscription contract (memql#2460).
//
// Graph subscriptions are STRUCTURED: the client sends a concept type id +
// CDC action verbs and the SERVER composes the bus topic, so the
// `graph.node.<action>.<concept>` grammar never appears on the client wire.
// The legacy free-text `filter` field survives only for the non-graph
// kinds and is rejected for graph subscriptions.

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestComposeSubscriptionPatterns(t *testing.T) {
	const graph = memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS
	const telemetry = memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY
	created := memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED
	updated := memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UPDATED
	unspec := memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UNSPECIFIED

	t.Run("graph concept + action", func(t *testing.T) {
		got, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{
			Kind: graph, Concept: "v1:cognition:utterance",
			Actions: []memqlv1.GraphNodeAction{created},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"graph.node.created.v1:cognition:utterance"}, got)
	})

	t.Run("graph all concepts + all actions", func(t *testing.T) {
		got, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{Kind: graph})
		require.NoError(t, err)
		assert.Equal(t, []string{"graph.node.*.#"}, got)
	})

	t.Run("graph concept + multiple actions", func(t *testing.T) {
		got, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{
			Kind: graph, Concept: "v1:cognition:utterance",
			Actions: []memqlv1.GraphNodeAction{created, updated},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{
			"graph.node.created.v1:cognition:utterance",
			"graph.node.updated.v1:cognition:utterance",
		}, got)
	})

	t.Run("graph rejects legacy free-text filter", func(t *testing.T) {
		_, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{Kind: graph, Filter: "node.created.#"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "free-text")
	})

	t.Run("graph rejects concept with dots", func(t *testing.T) {
		_, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{Kind: graph, Concept: "graph.node.created"})
		require.Error(t, err)
	})

	t.Run("graph rejects wildcard concept", func(t *testing.T) {
		_, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{Kind: graph, Concept: "v1:cognition:*"})
		require.Error(t, err)
	})

	t.Run("graph rejects unspecified action in list", func(t *testing.T) {
		_, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{
			Kind: graph, Actions: []memqlv1.GraphNodeAction{unspec},
		})
		require.Error(t, err)
	})

	t.Run("non-graph keeps free-text filter", func(t *testing.T) {
		got, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{Kind: telemetry, Filter: "foo.#"})
		require.NoError(t, err)
		assert.Equal(t, []string{"telemetry.foo.#"}, got)
	})

	t.Run("non-graph rejects structured concept", func(t *testing.T) {
		_, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{Kind: telemetry, Concept: "v1:cognition:utterance"})
		require.Error(t, err)
	})

	t.Run("non-graph rejects structured actions", func(t *testing.T) {
		_, err := composeSubscriptionPatterns(&memqlv1.SubscribeMsg{
			Kind: telemetry, Actions: []memqlv1.GraphNodeAction{created},
		})
		require.Error(t, err)
	})
}

// newSubscribeTestSession builds a minimal streamSession with a real event
// bus + a capture stream -- enough to drive handleSubscribe / handleBusEvent
// without an engine or auth.
func newSubscribeTestSession(t *testing.T) (*streamSession, *captureStream, *events.Bus) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	svc := &service{logger: logger, eventBus: bus}
	cs := newCaptureStream(t)
	s := &streamSession{
		service:   svc,
		stream:    cs,
		logger:    logger,
		eventChan: make(chan events.Event, 16),
		closeChan: make(chan struct{}),
	}
	return s, cs, bus
}

func TestHandleSubscribe_StructuredRoundTrip(t *testing.T) {
	s, cs, bus := newSubscribeTestSession(t)

	env := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_Subscribe{
			Subscribe: &memqlv1.SubscribeMsg{
				SubscriptionId: "sub-1",
				Kind:           memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
				Concept:        "v1:cognition:utterance",
				Actions:        []memqlv1.GraphNodeAction{memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED},
			},
		},
	}
	require.NoError(t, s.handleSubscribe(env, env.GetSubscribe()))

	// The subscription-created ack echoes the server-composed patterns.
	ack := findSubscriptionAck(t, cs, "subscription-created")
	require.NotNil(t, ack, "expected a subscription-created ack")
	assert.Equal(t, "sub-1", ack.GetSubscriptionId())

	// The stored subscription carries the server-composed topic, not any
	// client-supplied string.
	v, ok := s.subscriptions.Load("sub-1")
	require.True(t, ok, "subscription must be stored")
	assert.Equal(t, []string{"graph.node.created.v1:cognition:utterance"}, v.(*subscriptionInfo).patterns)

	// A matching event published to the bus reaches the session's eventChan
	// -- proves the server-composed pattern matches the real CDC topic.
	bus.Publish(events.Event{
		Topic:     "graph.node.created.v1:cognition:utterance",
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]any{"id": "utt-1", "concept": "v1:cognition:utterance"},
	})
	// A wrong-action event must NOT be delivered (created-only subscription).
	bus.Publish(events.Event{
		Topic:     "graph.node.deleted.v1:cognition:utterance",
		Kind:      events.KindNodeDeleted,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]any{"id": "utt-2", "concept": "v1:cognition:utterance"},
	})

	ev := readBusEvent(t, s.eventChan, 2*time.Second)
	require.Equal(t, "graph.node.created.v1:cognition:utterance", ev.Topic)

	// Deliver it (what forwardEvents does) and assert the client notification
	// carries concept + eventKind as first-class fields -- clients match on
	// those, never on the topic string.
	before := len(cs.sent)
	s.handleBusEvent(ev)
	note := findEventAfter(t, cs, before)
	require.NotNil(t, note, "handleBusEvent must emit an Event notification")
	assert.Equal(t, "sub-1", note.GetEvent().GetSubscriptionId())
	payload := note.GetEvent().GetPayload().AsMap()
	assert.Equal(t, "v1:cognition:utterance", payload["concept"], "concept is a first-class event field")
	assert.Equal(t, "node_created", payload["eventKind"], "eventKind is a first-class event field")

	// The filtered-out deleted event never reached the channel.
	select {
	case extra := <-s.eventChan:
		t.Fatalf("unexpected extra event delivered: %s", extra.Topic)
	default:
	}
}

func TestHandleSubscribe_RejectsLegacyGraphFilter(t *testing.T) {
	s, cs, _ := newSubscribeTestSession(t)

	env := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_Subscribe{
			Subscribe: &memqlv1.SubscribeMsg{
				SubscriptionId: "sub-legacy",
				Kind:           memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
				Filter:         "node.created.v1:cognition:utterance",
			},
		},
	}
	require.NoError(t, s.handleSubscribe(env, env.GetSubscribe()))

	// Rejected loudly with a subscription-error ack and NO stored subscription.
	errAck := findSubscriptionAck(t, cs, "subscription-error")
	require.NotNil(t, errAck, "legacy graph filter must be rejected with a subscription-error")
	_, stored := s.subscriptions.Load("sub-legacy")
	assert.False(t, stored, "a rejected subscription must not be registered")
}

// findSubscriptionAck returns the first Event message whose payload carries
// the given eventType (e.g. "subscription-created" / "subscription-error").
func findSubscriptionAck(t *testing.T, cs *captureStream, eventType string) *memqlv1.EventNotification {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, m := range cs.sent {
		ev := m.GetEvent()
		if ev == nil {
			continue
		}
		if ev.GetPayload().AsMap()["eventType"] == eventType {
			return ev
		}
	}
	return nil
}

func readBusEvent(t *testing.T, ch <-chan events.Event, timeout time.Duration) events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for bus event after %s", timeout)
		return events.Event{}
	}
}
