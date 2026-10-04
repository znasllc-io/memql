package node

// forward_receive_loop_test.go -- a forwarded call never holds the peer
// stream's RECEIVE loop.
//
// handleMessage runs on the loop that reads a peer's stream. A handler called
// inline there holds it for as long as the call runs -- a model generation, a
// tool dispatch, an app session measured in minutes -- and for that long this
// node reads nothing else from the peer: no heartbeat (so the liveness checker
// marks a healthy peer offline), no event forward, no second request, and not
// the CANCEL for the very call that is blocking, because the cancel arrives on
// the same stream. The pull and the probe learned this first; these tests pin
// it for every forward that answers a request.
//
// TO CONFIRM IT IS LOAD-BEARING: call any of the three handlers inline in
// stream_handler.go and the matching subtest fails on its first assertion.

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// blockingForwardHandler parks every forwarded call until it is cancelled or
// released, and records the cancels it was handed.
type blockingForwardHandler struct {
	mu        sync.Mutex
	started   map[string]chan struct{}
	cancelled map[string]bool
	release   chan struct{}
}

func newBlockingForwardHandler() *blockingForwardHandler {
	return &blockingForwardHandler{
		started:   map[string]chan struct{}{},
		cancelled: map[string]bool{},
		release:   make(chan struct{}),
	}
}

func (h *blockingForwardHandler) startedCh(requestId string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.started[requestId]
	if !ok {
		ch = make(chan struct{})
		h.started[requestId] = ch
	}
	return ch
}

func (h *blockingForwardHandler) park(ctx context.Context, requestId string) {
	close(h.startedCh(requestId))
	select {
	case <-ctx.Done():
	case <-h.release:
	}
}

func (h *blockingForwardHandler) cancel(requestId string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelled[requestId] = true
}

func (h *blockingForwardHandler) wasCancelled(requestId string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelled[requestId]
}

func (h *blockingForwardHandler) HandleForwardedRequest(ctx context.Context, req *nodev1.WorkerForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.park(ctx, req.GetRequestId())
}
func (h *blockingForwardHandler) CancelForwardedRequest(_ context.Context, requestId string) {
	h.cancel(requestId)
}
func (h *blockingForwardHandler) HandleForwardedModelCall(ctx context.Context, req *nodev1.ModelForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.park(ctx, req.GetRequestId())
}
func (h *blockingForwardHandler) CancelForwardedModelCall(_ context.Context, requestId string) {
	h.cancel(requestId)
}
func (h *blockingForwardHandler) HandleForwardedModelPull(ctx context.Context, req *nodev1.ModelPullForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.park(ctx, req.GetRequestId())
}
func (h *blockingForwardHandler) CancelForwardedModelPull(_ context.Context, requestId string) {
	h.cancel(requestId)
}
func (h *blockingForwardHandler) HandleForwardedModelProbe(ctx context.Context, req *nodev1.ModelProbeForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.park(ctx, req.GetRequestId())
}
func (h *blockingForwardHandler) CancelForwardedModelProbe(_ context.Context, requestId string) {
	h.cancel(requestId)
}
func (h *blockingForwardHandler) HandleForwardedAppCall(ctx context.Context, req *nodev1.AppCallForwardRequest, _ func(*nodev1.NodeServerMessage) error) {
	h.park(ctx, req.GetRequestId())
}
func (h *blockingForwardHandler) CancelForwardedAppCall(_ context.Context, requestId string) {
	h.cancel(requestId)
}

// handledWithin runs one handleMessage and reports whether it returned inside
// the window -- i.e. whether the receive loop was free to read the next
// message.
func handledWithin(svc *nodeService, msg *nodev1.NodeClientMessage, stream nodev1.NodeService_StreamServer, window time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.handleMessage("peer-a", msg, stream)
	}()
	select {
	case <-done:
		return true
	case <-time.After(window):
		return false
	}
}

func TestAForwardedCallDoesNotHoldThePeerReceiveLoop(t *testing.T) {
	cases := []struct {
		name    string
		request *nodev1.NodeClientMessage
		cancel  *nodev1.NodeClientMessage
	}{
		{
			name: "model call",
			request: &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelForwardRequest{
				ModelForwardRequest: &nodev1.ModelForwardRequest{RequestId: "model-1"}}},
			cancel: &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_ModelForwardCancel{
				ModelForwardCancel: &nodev1.ModelForwardCancel{RequestId: "model-1"}}},
		},
		{
			name: "app call",
			request: &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_AppCallForwardRequest{
				AppCallForwardRequest: &nodev1.AppCallForwardRequest{RequestId: "app-1"}}},
			cancel: &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_AppCallForwardCancel{
				AppCallForwardCancel: &nodev1.AppCallForwardCancel{RequestId: "app-1"}}},
		},
		{
			name: "tool dispatch",
			request: &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_WorkerForwardRequest{
				WorkerForwardRequest: &nodev1.WorkerForwardRequest{RequestId: "tool-1"}}},
			cancel: &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_WorkerForwardCancel{
				WorkerForwardCancel: &nodev1.WorkerForwardCancel{RequestId: "tool-1"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newBlockingForwardHandler()
			defer close(handler.release)
			svc := &nodeService{logger: testLogger(), identity: testIdentity(), workerForwardHandler: handler}
			stream := newFakeStream()

			if !handledWithin(svc, tc.request, stream, time.Second) {
				t.Fatalf("the %s held the peer's receive loop: handleMessage did not return while the call ran, "+
					"so no heartbeat, event or cancel from that peer could be read until it finished", tc.name)
			}
			requestId := requestIdOf(tc.request)
			select {
			case <-handler.startedCh(requestId):
			case <-time.After(time.Second):
				t.Fatalf("the %s was never handed to the handler", tc.name)
			}

			// The cancel arrives on the SAME stream, so it is readable only
			// because the call above is not holding the loop.
			if !handledWithin(svc, tc.cancel, stream, time.Second) {
				t.Fatalf("the cancel for the %s could not be read", tc.name)
			}
			if !handler.wasCancelled(requestId) {
				t.Fatalf("the cancel for the in-flight %s never reached its handler", tc.name)
			}
		})
	}
}

func requestIdOf(msg *nodev1.NodeClientMessage) string {
	switch p := msg.GetPayload().(type) {
	case *nodev1.NodeClientMessage_ModelForwardRequest:
		return p.ModelForwardRequest.GetRequestId()
	case *nodev1.NodeClientMessage_WorkerForwardRequest:
		return p.WorkerForwardRequest.GetRequestId()
	case *nodev1.NodeClientMessage_AppCallForwardRequest:
		return p.AppCallForwardRequest.GetRequestId()
	}
	return ""
}

// A node with no forward handler answers an app call with a refusal that says
// nothing ran, rather than dropping it: the sender is parked on the reply, and
// "refused before start" is what lets its route move on to the next source.
func TestAnAppCallWithNoHandlerIsRefusedBeforeStart(t *testing.T) {
	svc := &nodeService{logger: testLogger(), identity: testIdentity()}
	stream := newFakeStream()
	svc.handleMessage("peer-a", &nodev1.NodeClientMessage{Payload: &nodev1.NodeClientMessage_AppCallForwardRequest{
		AppCallForwardRequest: &nodev1.AppCallForwardRequest{RequestId: "app-2"}}}, stream)

	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d messages, want exactly one answer", len(stream.sent))
	}
	resp := stream.sent[0].GetAppCallForwardResponse()
	if resp == nil || resp.GetRequestId() != "app-2" || !resp.GetRefusedBeforeStart() || resp.GetErrorCode() != "not_configured" {
		t.Fatalf("answer = %+v, want a not_configured refusal before start for app-2", resp)
	}
}
