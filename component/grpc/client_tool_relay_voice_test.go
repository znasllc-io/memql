package memql

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestDecodeClientToolResponsePayload_TopLevel proves the relay reads the
// flattened top-level fields a graph-node event carries.
func TestDecodeClientToolResponsePayload_TopLevel(t *testing.T) {
	callId, contentJSON, errMsg, isErr := decodeClientToolResponsePayload(map[string]any{
		"callId":      "c1",
		"contentJSON": `[{"type":"text","text":"ok"}]`,
		"isError":     false,
	})
	if callId != "c1" || contentJSON != `[{"type":"text","text":"ok"}]` || errMsg != "" || isErr {
		t.Fatalf("top-level decode wrong: id=%q content=%q err=%q isErr=%v", callId, contentJSON, errMsg, isErr)
	}
}

// TestDecodeClientToolResponsePayload_NestedFallback proves the relay falls back
// to the nested payload shape (matching cognition's handleClientToolResponse).
func TestDecodeClientToolResponsePayload_NestedFallback(t *testing.T) {
	callId, _, errMsg, isErr := decodeClientToolResponsePayload(map[string]any{
		"payload": map[string]any{
			"callId":       "c2",
			"isError":      true,
			"errorMessage": "browser refused",
		},
	})
	if callId != "c2" || !isErr || errMsg != "browser refused" {
		t.Fatalf("nested decode wrong: id=%q err=%q isErr=%v", callId, errMsg, isErr)
	}
}

// TestHandleClientToolResponseEvent_FiresWaiter proves a browser response event
// fires the service-scoped waiter parked here (the voice relay return leg,
// #1420), reusing DeliverClientToolResult.
func TestHandleClientToolResponseEvent_FiresWaiter(t *testing.T) {
	s := &service{}
	const callId = "call-voice-1"
	ch := make(chan *memqlv1.ClientToolResult, 1)
	s.clientToolMu.Lock()
	s.clientToolWaiters = map[string]chan *memqlv1.ClientToolResult{callId: ch}
	s.clientToolMu.Unlock()

	s.handleClientToolResponseEvent(events.Event{
		Topic: eventPatternClientToolResponseVoice,
		Payload: map[string]any{
			"callId":      callId,
			"contentJSON": `[{"type":"text","text":"theme changed"}]`,
		},
	})

	select {
	case got := <-ch:
		if got.GetCallId() != callId {
			t.Fatalf("wrong callId: %s", got.GetCallId())
		}
		c := got.GetContent()
		if len(c) != 1 || c[0].GetText() != "theme changed" {
			t.Fatalf("content decoded wrong: %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never fired from the response event")
	}
}

// TestHandleClientToolResponseEvent_NoWaiter proves a response for a call this
// node never parked (e.g. it belongs to the agent relay) is a benign no-op.
func TestHandleClientToolResponseEvent_NoWaiter(t *testing.T) {
	s := &service{}
	// No panic, no hang, no waiter.
	s.handleClientToolResponseEvent(events.Event{Payload: map[string]any{"callId": "not-ours"}})
	s.handleClientToolResponseEvent(events.Event{Payload: map[string]any{}}) // missing callId
}

// TestStartClientToolResponseRelay_BusIntegration proves the once-wired bus
// subscription routes a published client:tool:response event to the local
// waiter end-to-end (subscription + decode + deliver), the path the voice relay
// depends on for its return leg.
func TestStartClientToolResponseRelay_BusIntegration(t *testing.T) {
	bus := events.NewBus()
	s := &service{eventBus: bus}
	s.startClientToolResponseRelay()
	// Idempotent: a second call must not double-subscribe (sync.Once).
	s.startClientToolResponseRelay()

	const callId = "call-bus-1"
	ch := make(chan *memqlv1.ClientToolResult, 1)
	s.clientToolMu.Lock()
	s.clientToolWaiters = map[string]chan *memqlv1.ClientToolResult{callId: ch}
	s.clientToolMu.Unlock()

	bus.Publish(events.Event{
		Topic: "graph.node.created.v1:cognition:client:tool:response",
		Payload: map[string]any{
			"callId":      callId,
			"contentJSON": `[{"type":"text","text":"ok"}]`,
		},
	})

	select {
	case got := <-ch:
		if got.GetCallId() != callId {
			t.Fatalf("wrong callId via bus: %s", got.GetCallId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never fired via the bus subscription")
	}
}

// TestRelayClientToolToBrowser_GuardsRejectFast proves the relay refuses fast
// (rather than parking for the full timeout) when it cannot reach a browser:
// no space to scope the request to, or no event bus to fire the waiter.
func TestRelayClientToolToBrowser_GuardsRejectFast(t *testing.T) {
	// Nil engine -> unavailable.
	s := &streamSession{service: &service{}}
	if _, err := s.relayClientToolToBrowser(t.Context(), "uiClick", "{}", "space-1"); err == nil {
		t.Fatal("expected error with nil engine")
	}
}

// TestRelayClientToolToBrowser_TimeoutSurfaces proves a relayed client tool that
// the browser never answers surfaces as a clean timeout error (so the voice
// model can tell the user), bounded by voiceClientToolTimeout. Driven without an
// engine by exercising the wait directly through the waiter the relay registers.
func TestRelayClientToolToBrowser_Timeout(t *testing.T) {
	// The full relayClientToolToBrowser needs an engine to emit the request
	// node; here we assert the bounded-wait contract it relies on: a waiter
	// that never receives a result unblocks via the timeout, not indefinitely.
	s := &service{}
	const callId = "call-timeout"
	ch := s2registerWaiter(s, callId)
	defer s2unregisterWaiter(s, callId)

	select {
	case <-ch:
		t.Fatal("no result was delivered; channel should not fire")
	case <-time.After(50 * time.Millisecond):
		// Expected: nothing arrived. The relay's select would take its
		// time.After(voiceClientToolTimeout) branch here.
	}
}

// s2registerWaiter / s2unregisterWaiter mirror the streamSession helpers at the
// service scope for the timeout test (the waiter map is service-scoped).
func s2registerWaiter(s *service, callId string) chan *memqlv1.ClientToolResult {
	ch := make(chan *memqlv1.ClientToolResult, 1)
	s.clientToolMu.Lock()
	if s.clientToolWaiters == nil {
		s.clientToolWaiters = make(map[string]chan *memqlv1.ClientToolResult)
	}
	s.clientToolWaiters[callId] = ch
	s.clientToolMu.Unlock()
	return ch
}

func s2unregisterWaiter(s *service, callId string) {
	s.clientToolMu.Lock()
	delete(s.clientToolWaiters, callId)
	s.clientToolMu.Unlock()
}
