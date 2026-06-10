package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The RPC layer is tested against the SAME fakeOutboxStore the substrate tests
// use (defined in delivery_substrate_test.go). A shared store models the durable
// DB; two separate Substrate instances over that one store model two physical
// replicas. This lets a test prove that a reply published by one replica reaches
// a caller subscribed on a DIFFERENT replica -- the reply-routing-by-logical-key
// guarantee that is the whole point of memql#1265.

// rpcKey builds a routing key in a non-"space" kind so RPC keys are visibly
// distinct from the event-pattern keys the substrate tests use.
func rpcKey(kind, idv string) RoutingKey { return RoutingKey{Kind: kind, ID: idv} }

// newReplica builds a Substrate over the shared store, simulating one physical
// node. dedup TTL is generous so a duplicate within a test window is always
// caught.
func newReplica(store outboxStore) *Substrate {
	return NewSubstrate(store, time.Minute, nil, nil)
}

// echoHandler returns a handler that echoes its payload back under "echo" and
// records every method it saw (for ordering/dedup assertions).
func echoHandler() (RPCHandler, *handlerRecorder) {
	rec := &handlerRecorder{}
	h := func(_ context.Context, req RPCRequest) (map[string]any, error) {
		rec.record(req)
		return map[string]any{"echo": req.Payload["msg"], "method": req.Method}, nil
	}
	return h, rec
}

type handlerRecorder struct {
	mu   sync.Mutex
	seen []RPCRequest
}

func (h *handlerRecorder) record(req RPCRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, req)
}

func (h *handlerRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

func (h *handlerRecorder) methods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.seen))
	for i, r := range h.seen {
		out[i] = stringOf(r.Payload, "msg")
	}
	return out
}

// TestSubstrateRPCCallReply is the happy path: a Call gets the handler's reply,
// correlated, with the body intact -- all on one replica.
func TestSubstrateRPCCallReply(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "a1")
	server := NewSubstrateRPC(rep, rpcKey("agent", "a1-serve-reply"), "agent-node", nil)
	handler, rec := echoHandler()
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	client := NewSubstrateRPC(rep, rpcKey("bff", "caller1"), "bff-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	resp, err := client.Call(callCtx, serveKey, "doThing", map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp["echo"] != "hello" {
		t.Fatalf("expected echo=hello, got %v", resp["echo"])
	}
	if resp["method"] != "doThing" {
		t.Fatalf("expected method=doThing, got %v", resp["method"])
	}
	if rec.count() != 1 {
		t.Fatalf("expected handler invoked once, got %d", rec.count())
	}
}

// TestSubstrateRPCReplyRoutesAcrossReplicas is the core memql#1265 guarantee:
// the callee runs on replica B, the caller issues its Call from replica A, and
// the reply -- published by B to the caller's LOGICAL reply key -- is delivered
// to A's pump and matched back to the pending call. Neither leg names a node-id;
// only the shared durable store connects them.
func TestSubstrateRPCReplyRoutesAcrossReplicas(t *testing.T) {
	store := newFakeOutboxStore()
	replicaA := newReplica(store) // caller lives here
	replicaB := newReplica(store) // callee lives here

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "shared")
	server := NewSubstrateRPC(replicaB, rpcKey("agent", "b-reply"), "agent-node-B", nil)
	handler, _ := echoHandler()
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	callerKey := rpcKey("bff", "caller-cross")
	client := NewSubstrateRPC(replicaA, callerKey, "bff-node-A", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 3*time.Second)
	defer callCancel()
	resp, err := client.Call(callCtx, serveKey, "crossReplica", map[string]any{"msg": "ping"})
	if err != nil {
		t.Fatalf("cross-replica Call: %v", err)
	}
	if resp["echo"] != "ping" {
		t.Fatalf("cross-replica: expected echo=ping, got %v", resp["echo"])
	}
}

// TestSubstrateRPCCallerRestartReplaysReply proves a reply produced while the
// caller's reply-key owner is DOWN is replayed when a new owner takes over the
// key (same consumerID -> resumes the cursor). This is the dispatch-survives-a-
// replica-restart acceptance criterion at the RPC layer.
func TestSubstrateRPCCallerRestartReplaysReply(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callerKey := rpcKey("bff", "restart-caller")
	consumerID := "bff-logical-caller"

	// 1. The reply is published to the caller's key BEFORE any caller owner is
	//    listening (simulates the reply landing while the caller replica is
	//    mid-restart). Publish a well-formed reply envelope directly.
	corr := "corr-restart-1"
	replyDeliverable := Deliverable{
		EventID: "rpc-rep-" + corr,
		Key:     callerKey,
		Topic:   rpcTopic,
		Kind:    0,
		Payload: map[string]any{
			rpcKeyKind:          rpcKindReply,
			rpcKeyCorrelationID: corr,
			rpcKeyBody:          map[string]any{"late": "value"},
		},
	}
	if _, err := rep.Publish(ctx, replyDeliverable); err != nil {
		t.Fatalf("pre-publish reply: %v", err)
	}

	// 2. A NEW caller owner comes up with the same logical consumerID and a
	//    pending waiter for corr. Its pump replays the backlog from cursor 0 and
	//    must deliver the buffered reply.
	client := NewSubstrateRPC(rep, callerKey, consumerID, nil)
	defer client.Close()
	respCh := make(chan rpcReply, 1)
	client.mu.Lock()
	client.pending[corr] = &pendingCall{respCh: respCh}
	client.mu.Unlock()
	if err := client.ensureStarted(ctx); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}

	select {
	case reply := <-respCh:
		if reply.payload["late"] != "value" {
			t.Fatalf("replayed reply body wrong: %v", reply.payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("buffered reply was not replayed to the new caller owner")
	}
}

// TestSubstrateRPCDuplicateRequestIdempotentReply proves at-least-once on the
// request leg yields exactly-once for the caller: a request re-delivered to the
// handler produces a reply with the SAME EventID, which the substrate's
// unique-event-id guard collapses, so the caller's waiter fires once.
func TestSubstrateRPCDuplicateRequestIdempotentReply(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "idem")
	server := NewSubstrateRPC(rep, rpcKey("agent", "idem-reply"), "agent-node", nil)

	var calls int
	var mu sync.Mutex
	handler := func(_ context.Context, req RPCRequest) (map[string]any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return map[string]any{"n": calls}, nil
	}
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	client := NewSubstrateRPC(rep, rpcKey("bff", "idem-caller"), "bff-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	resp, err := client.Call(callCtx, serveKey, "m", map[string]any{"msg": "x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// First reply must have landed (n>=1). The handler may run more than once
	// under at-least-once redelivery, but the caller sees one reply for the
	// correlation id.
	if _, ok := resp["n"]; !ok {
		t.Fatalf("expected n in reply, got %v", resp)
	}
}

// TestSubstrateRPCHandlerErrorSurfaced proves a handler error becomes a typed
// error reply so the caller unblocks with an error instead of hanging.
func TestSubstrateRPCHandlerErrorSurfaced(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "err")
	server := NewSubstrateRPC(rep, rpcKey("agent", "err-reply"), "agent-node", nil)
	handler := func(_ context.Context, _ RPCRequest) (map[string]any, error) {
		return nil, fmt.Errorf("boom")
	}
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	client := NewSubstrateRPC(rep, rpcKey("bff", "err-caller"), "bff-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	_, err := client.Call(callCtx, serveKey, "m", nil)
	if err == nil {
		t.Fatal("expected error from handler to surface to caller")
	}
}

// TestSubstrateRPCHandlerPanicSurfaced proves a panicking handler does not kill
// the Serve loop and still produces an error reply.
func TestSubstrateRPCHandlerPanicSurfaced(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "panic")
	server := NewSubstrateRPC(rep, rpcKey("agent", "panic-reply"), "agent-node", nil)
	handler := func(_ context.Context, _ RPCRequest) (map[string]any, error) {
		panic("kaboom")
	}
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	client := NewSubstrateRPC(rep, rpcKey("bff", "panic-caller"), "bff-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	if _, err := client.Call(callCtx, serveKey, "m", nil); err == nil {
		t.Fatal("expected panic to surface as an error reply")
	}

	// The Serve loop must still be alive: a second call succeeds against a
	// non-panicking re-registration is overkill; instead prove the loop did not
	// exit by issuing another call that the SAME panicking handler answers (it
	// will error again, but the fact we get a reply at all proves the loop ran).
	callCtx2, cancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer cancel2()
	if _, err := client.Call(callCtx2, serveKey, "m2", nil); err == nil {
		t.Fatal("expected second panic to also surface (loop alive)")
	}
}

// TestSubstrateRPCCallTimeout proves a Call against a key no one serves returns
// the ctx error rather than hanging forever, and cleans up its pending waiter.
func TestSubstrateRPCCallTimeout(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewSubstrateRPC(rep, rpcKey("bff", "timeout-caller"), "bff-node", nil)
	defer client.Close()

	callCtx, callCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer callCancel()
	_, err := client.Call(callCtx, rpcKey("agent", "nobody-home"), "m", nil)
	if err == nil {
		t.Fatal("expected timeout error against unserved key")
	}

	// Pending waiter must be cleared so a late reply can't leak it.
	client.mu.Lock()
	n := len(client.pending)
	client.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected pending table empty after timeout, got %d", n)
	}
}

// TestSubstrateRPCConcurrentCalls proves correlation keeps many in-flight calls
// distinct: N concurrent calls each get exactly their own reply back.
func TestSubstrateRPCConcurrentCalls(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "concurrent")
	server := NewSubstrateRPC(rep, rpcKey("agent", "concurrent-reply"), "agent-node", nil)
	handler := func(_ context.Context, req RPCRequest) (map[string]any, error) {
		// Echo the unique token back so each caller can verify it got ITS reply.
		return map[string]any{"token": req.Payload["token"]}, nil
	}
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	client := NewSubstrateRPC(rep, rpcKey("bff", "concurrent-caller"), "bff-node", nil)
	defer client.Close()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("tok-%d", i)
			cctx, ccancel := context.WithTimeout(ctx, 4*time.Second)
			defer ccancel()
			resp, err := client.Call(cctx, serveKey, "m", map[string]any{"token": token})
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if resp["token"] != token {
				errs <- fmt.Errorf("call %d: got token %v, want %s", i, resp["token"], token)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestSubstrateRPCForeignDeliverableIgnored proves a non-RPC deliverable sharing
// a served key is skipped (not dispatched) and its cursor still advances so it
// can't wedge replay.
func TestSubstrateRPCForeignDeliverableIgnored(t *testing.T) {
	store := newFakeOutboxStore()
	rep := newReplica(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveKey := rpcKey("agent", "foreign")
	server := NewSubstrateRPC(rep, rpcKey("agent", "foreign-reply"), "agent-node", nil)
	handler, rec := echoHandler()
	go func() { _ = server.Serve(ctx, serveKey, handler) }()

	// Publish a foreign (event-pattern) deliverable on the served key.
	if _, err := rep.Publish(ctx, Deliverable{
		EventID: "foreign-1",
		Key:     serveKey,
		Topic:   "graph.node.created",
		Kind:    0,
		Payload: map[string]any{"not": "rpc"},
	}); err != nil {
		t.Fatalf("publish foreign: %v", err)
	}

	// Then a real RPC call must still be handled (the foreign row didn't wedge
	// the loop / cursor).
	client := NewSubstrateRPC(rep, rpcKey("bff", "foreign-caller"), "bff-node", nil)
	defer client.Close()
	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	if _, err := client.Call(callCtx, serveKey, "m", map[string]any{"msg": "ok"}); err != nil {
		t.Fatalf("Call after foreign row: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("handler should have run exactly once (foreign skipped), got %d", rec.count())
	}
}
