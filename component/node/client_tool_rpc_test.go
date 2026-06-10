package node

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeFirer records DeliverClientToolResult calls and answers found per a
// pre-seeded set of "parked" callIds, modelling the agent's clientToolWaiters.
type fakeFirer struct {
	mu       sync.Mutex
	parked   map[string]bool // callId -> still parked
	received []ClientToolResultPayload
}

func newFakeFirer(parked ...string) *fakeFirer {
	f := &fakeFirer{parked: map[string]bool{}}
	for _, c := range parked {
		f.parked[c] = true
	}
	return f
}

func (f *fakeFirer) DeliverClientToolResult(p ClientToolResultPayload) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, p)
	if f.parked[p.CallID] {
		delete(f.parked, p.CallID) // fired once
		return true
	}
	return false
}

func (f *fakeFirer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func (f *fakeFirer) last() ClientToolResultPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return ClientToolResultPayload{}
	}
	return f.received[len(f.received)-1]
}

// TestClientToolResultRPCDeliversToParkedWaiter is the happy path on one
// replica: cognition Delivers a result to the agent turn; the agent's parked
// waiter fires and the reply reports found=true with the body intact.
func TestClientToolResultRPCDeliversToParkedWaiter(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const requestId = "turn-1"
	const callId = "call-abc"

	firer := newFakeFirer(callId)
	server := NewClientToolResultServer(rep, firer, "agent-node", nil)
	if server == nil {
		t.Fatal("server should be constructed")
	}
	server.BeginTurn(ctx, requestId)
	defer server.EndTurn(requestId)

	client := NewClientToolResultClient(rep, "cognition-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	found, err := client.Deliver(callCtx, requestId, ClientToolResultPayload{
		CallID:      callId,
		ContentJSON: `[{"type":"text","text":"hi"}]`,
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a parked waiter")
	}
	if firer.count() != 1 {
		t.Fatalf("expected firer invoked once, got %d", firer.count())
	}
	if got := firer.last(); got.CallID != callId || got.ContentJSON != `[{"type":"text","text":"hi"}]` {
		t.Fatalf("firer got wrong payload: %+v", got)
	}
}

// TestClientToolResultRPCRoutesAcrossReplicas is the memql#1265 guarantee for
// this flow: the agent runs on replica B, cognition Delivers from replica A, and
// the result reaches B over the shared durable store with NO live A<->B
// connection -- the #1245 churn case fixed by construction.
func TestClientToolResultRPCRoutesAcrossReplicas(t *testing.T) {
	store := newFakeOutboxStore()
	replicaA := newReplica(store) // cognition
	replicaB := newReplica(store) // agent

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const requestId = "turn-cross"
	const callId = "call-cross"

	firer := newFakeFirer(callId)
	server := NewClientToolResultServer(replicaB, firer, "agent-node-B", nil)
	server.BeginTurn(ctx, requestId)
	defer server.EndTurn(requestId)

	client := NewClientToolResultClient(replicaA, "cognition-node-A", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 3*time.Second)
	defer callCancel()
	found, err := client.Deliver(callCtx, requestId, ClientToolResultPayload{CallID: callId, ContentJSON: "[]"})
	if err != nil {
		t.Fatalf("cross-replica Deliver: %v", err)
	}
	if !found {
		t.Fatal("cross-replica: expected the parked waiter on replica B to fire")
	}
}

// TestClientToolResultRPCNoWaiterFoundFalse proves a result delivered to a turn
// whose waiter already moved on returns found=false with no transport error --
// a benign, observable outcome (not a hang, not a failure).
func TestClientToolResultRPCNoWaiterFoundFalse(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const requestId = "turn-empty"
	firer := newFakeFirer() // nothing parked
	server := NewClientToolResultServer(rep, firer, "agent-node", nil)
	server.BeginTurn(ctx, requestId)
	defer server.EndTurn(requestId)

	client := NewClientToolResultClient(rep, "cognition-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	found, err := client.Deliver(callCtx, requestId, ClientToolResultPayload{CallID: "missing"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if found {
		t.Fatal("expected found=false for a turn with no parked waiter")
	}
}

// TestClientToolResultRPCEndTurnStopsServing proves EndTurn tears the serve loop
// down: after EndTurn, a Deliver to that turn times out (no server) rather than
// silently succeeding.
func TestClientToolResultRPCEndTurnStopsServing(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const requestId = "turn-ended"
	const callId = "call-ended"
	firer := newFakeFirer(callId)
	server := NewClientToolResultServer(rep, firer, "agent-node", nil)
	server.BeginTurn(ctx, requestId)
	server.EndTurn(requestId)

	client := NewClientToolResultClient(rep, "cognition-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer callCancel()
	_, err := client.Deliver(callCtx, requestId, ClientToolResultPayload{CallID: callId})
	if err == nil {
		t.Fatal("expected timeout delivering to an ended turn (no server)")
	}
	if firer.count() != 0 {
		t.Fatalf("firer must not be invoked after EndTurn, got %d", firer.count())
	}
}

// TestClientToolResultServerNilWhenUnconfigured proves the constructors return
// nil (skip-wiring contract) when their deps are absent.
func TestClientToolResultServerNilWhenUnconfigured(t *testing.T) {
	if NewClientToolResultServer(nil, newFakeFirer(), "n", nil) != nil {
		t.Fatal("server should be nil with no substrate")
	}
	store := newFakeOutboxStore()
	if NewClientToolResultServer(newReplica(store), nil, "n", nil) != nil {
		t.Fatal("server should be nil with no firer")
	}
	if NewClientToolResultClient(nil, "n", nil) != nil {
		t.Fatal("client should be nil with no substrate")
	}
}
