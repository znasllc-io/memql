package node

// forward_cancel_order_test.go -- a CANCEL always finds its forwarded call.
//
// A forwarded call runs on its own goroutine, so it never holds the peer's
// receive loop (forward_receive_loop_test.go). That opened a window: the cancel
// arrives on the same stream right behind its request, and it used to be
// matched only against a table the HANDLER fills from that goroutine, once the
// goroutine got round to running. A cancel read first found nothing, and the
// call then ran to its ceiling -- for an app call, a Claude Code session opened
// on somebody's laptop for a caller that had already given up.
//
// The receive loop now registers each call before it spawns it and hands it a
// context the cancel ends. These tests use a handler that learns about a cancel
// ONLY through that context (its own Cancel* methods do nothing), which is
// exactly the position a real handler is in when the cancel beats its
// goroutine.
//
// TO CONFIRM THEY ARE LOAD-BEARING: hand the handler stream.Context() (or its
// WithoutCancel) instead of the registered context and the first test fails for
// every family; drop the remembered cancels and the second one does.

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// contextOnlyHandler records the context each forwarded call was handed and
// never hears a Cancel* call: it has not registered the request yet, the way a
// handler whose goroutine has not run has not.
type contextOnlyHandler struct {
	mu   sync.Mutex
	ctxs map[string]context.Context
	ran  chan string
}

func newContextOnlyHandler() *contextOnlyHandler {
	return &contextOnlyHandler{ctxs: map[string]context.Context{}, ran: make(chan string, 16)}
}

func (h *contextOnlyHandler) record(ctx context.Context, requestId string) {
	h.mu.Lock()
	h.ctxs[requestId] = ctx
	h.mu.Unlock()
	h.ran <- requestId
	// Park like a real call would, until the context ends or the test does.
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
}

func (h *contextOnlyHandler) ctxFor(requestId string) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ctxs[requestId]
}

func (h *contextOnlyHandler) HandleForwardedRequest(ctx context.Context, req *nodev1.WorkerForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.record(ctx, req.GetRequestId())
}
func (h *contextOnlyHandler) CancelForwardedRequest(context.Context, string) {}
func (h *contextOnlyHandler) HandleForwardedModelCall(ctx context.Context, req *nodev1.ModelForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.record(ctx, req.GetRequestId())
}
func (h *contextOnlyHandler) CancelForwardedModelCall(context.Context, string) {}
func (h *contextOnlyHandler) HandleForwardedModelPull(ctx context.Context, req *nodev1.ModelPullForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.record(ctx, req.GetRequestId())
}
func (h *contextOnlyHandler) CancelForwardedModelPull(context.Context, string) {}
func (h *contextOnlyHandler) HandleForwardedModelProbe(ctx context.Context, req *nodev1.ModelProbeForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.record(ctx, req.GetRequestId())
}
func (h *contextOnlyHandler) CancelForwardedModelProbe(context.Context, string) {}
func (h *contextOnlyHandler) HandleForwardedAppCall(ctx context.Context, req *nodev1.AppCallForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.record(ctx, req.GetRequestId())
}
func (h *contextOnlyHandler) CancelForwardedAppCall(context.Context, string) {}

type forwardPair struct {
	name    string
	request *nodev1.NodeClientMessage
	cancel  *nodev1.NodeClientMessage
}

func forwardPairs(requestId string) []forwardPair {
	return []forwardPair{
		{"tool dispatch",
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_WorkerForwardRequest{
				WorkerForwardRequest: &nodev1.WorkerForwardRequest{RequestId: requestId}}},
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_WorkerForwardCancel{
				WorkerForwardCancel: &nodev1.WorkerForwardCancel{RequestId: requestId}}}},
		{"model call",
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelForwardRequest{
				ModelForwardRequest: &nodev1.ModelForwardRequest{RequestId: requestId}}},
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelForwardCancel{
				ModelForwardCancel: &nodev1.ModelForwardCancel{RequestId: requestId}}}},
		{"model pull",
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelPullForwardRequest{
				ModelPullForwardRequest: &nodev1.ModelPullForwardRequest{RequestId: requestId}}},
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelPullForwardCancel{
				ModelPullForwardCancel: &nodev1.ModelPullForwardCancel{RequestId: requestId}}}},
		{"model probe",
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelProbeForwardRequest{
				ModelProbeForwardRequest: &nodev1.ModelProbeForwardRequest{RequestId: requestId}}},
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelProbeForwardCancel{
				ModelProbeForwardCancel: &nodev1.ModelProbeForwardCancel{RequestId: requestId}}}},
		{"app call",
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_AppCallForwardRequest{
				AppCallForwardRequest: &nodev1.AppCallForwardRequest{RequestId: requestId}}},
			&nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_AppCallForwardCancel{
				AppCallForwardCancel: &nodev1.AppCallForwardCancel{RequestId: requestId}}}},
	}
}

// A cancel read right behind its request -- before the call's goroutine has
// had any chance to register it -- still ends the call.
func TestACancelRightBehindItsRequestEndsTheCall(t *testing.T) {
	for _, pair := range forwardPairs("behind-1") {
		t.Run(pair.name, func(t *testing.T) {
			handler := newContextOnlyHandler()
			svc := &nodeService{logger: testLogger(), identity: testIdentity(), workerForwardHandler: handler}
			stream := newFakeStream()

			// Back to back, on the one goroutine that reads the stream.
			svc.handleMessage("peer-a", pair.request, stream)
			svc.handleMessage("peer-a", pair.cancel, stream)

			select {
			case <-handler.ran:
			case <-time.After(time.Second):
				t.Fatalf("the %s was never handed to its handler", pair.name)
			}
			ctx := handler.ctxFor("behind-1")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatalf("the cancel for the %s was read and the call's context never ended: "+
					"the call would run to its ceiling for a caller that gave up", pair.name)
			}
		})
	}
}

// A cancel that OVERTAKES its request -- cancels travel on the peer's general
// outbox, which can pass a request on the stream's own queue -- keeps the
// request from running at all.
func TestACancelThatOvertakesItsRequestKeepsItFromRunning(t *testing.T) {
	for _, pair := range forwardPairs("ahead-1") {
		t.Run(pair.name, func(t *testing.T) {
			handler := newContextOnlyHandler()
			svc := &nodeService{logger: testLogger(), identity: testIdentity(), workerForwardHandler: handler}
			stream := newFakeStream()

			svc.handleMessage("peer-a", pair.cancel, stream)
			svc.handleMessage("peer-a", pair.request, stream)

			select {
			case id := <-handler.ran:
				t.Fatalf("the %s %q ran although its cancel had already arrived", pair.name, id)
			case <-time.After(100 * time.Millisecond):
			}

			// The memory is per request: the next call with a fresh id runs.
			fresh := forwardPairs("ahead-2")
			for _, p := range fresh {
				if p.name == pair.name {
					svc.handleMessage("peer-a", p.request, stream)
				}
			}
			select {
			case <-handler.ran:
			case <-time.After(time.Second):
				t.Fatalf("a %s with no cancel against it did not run", pair.name)
			}
		})
	}
}

// A cancel names a request FROM ITS OWN PEER. Another peer that happens to send
// the same id cannot end, or pre-empt, somebody else's call.
func TestACancelFromAnotherPeerDoesNotTouchTheCall(t *testing.T) {
	handler := newContextOnlyHandler()
	svc := &nodeService{logger: testLogger(), identity: testIdentity(), workerForwardHandler: handler}
	stream := newFakeStream()
	pair := forwardPairs("shared-id")[4] // app call

	svc.handleMessage("peer-a", pair.request, stream)
	svc.handleMessage("peer-b", pair.cancel, stream)
	<-handler.ran
	select {
	case <-handler.ctxFor("shared-id").Done():
		t.Fatal("peer-b's cancel ended peer-a's call")
	case <-time.After(100 * time.Millisecond):
	}
}

// The remembered cancels are bounded in time and in number: a cancel for a
// request that never arrives is forgotten, and a flood of them cannot grow the
// table without limit.
func TestRememberedCancelsAreBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	calls := &forwardCalls{now: func() time.Time { return now }}

	calls.cancel(forwardFamilyApp, "peer-a", "late")
	if _, _, ok := calls.begin(context.Background(), forwardFamilyApp, "peer-a", "late"); ok {
		t.Fatal("a request whose cancel arrived first was admitted")
	}
	// Consumed: the same id arriving again is a new request.
	ctx, end, ok := calls.begin(context.Background(), forwardFamilyApp, "peer-a", "late")
	if !ok || ctx.Err() != nil {
		t.Fatal("a remembered cancel refused a second request, not only the one it named")
	}
	end()

	calls.cancel(forwardFamilyApp, "peer-a", "never-arrives")
	now = now.Add(forwardCancelMemory + time.Second)
	if _, end, ok := calls.begin(context.Background(), forwardFamilyApp, "peer-a", "never-arrives"); !ok {
		t.Fatal("a cancel was remembered past its window")
	} else {
		end()
	}

	for i := 0; i < forwardCancelMemoryMax+50; i++ {
		calls.cancel(forwardFamilyApp, "peer-a", "flood-"+time.Duration(i).String())
	}
	calls.mu.Lock()
	remembered := len(calls.cancelled)
	calls.mu.Unlock()
	if remembered > forwardCancelMemoryMax {
		t.Fatalf("remembered %d cancels, want at most %d", remembered, forwardCancelMemoryMax)
	}
}
