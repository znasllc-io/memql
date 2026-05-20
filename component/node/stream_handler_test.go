package node

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeStream is the minimum NodeService_StreamServer implementation needed
// to exercise the unary-like handlers (handleEventForward, etc.). Most of
// the bidi surface is unused by these handlers; stubs return defaults.
type fakeStream struct {
	mu       sync.Mutex
	sent     []*nodev1.NodeServerMessage
	recvCh   chan *nodev1.NodeClientMessage
	ctx      context.Context
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		recvCh: make(chan *nodev1.NodeClientMessage, 8),
		ctx:    context.Background(),
	}
}

func (f *fakeStream) Send(msg *nodev1.NodeServerMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeStream) Recv() (*nodev1.NodeClientMessage, error) {
	msg, ok := <-f.recvCh
	if !ok {
		return nil, context.Canceled
	}
	return msg, nil
}

func (f *fakeStream) SendMsg(m any) error                  { return nil }
func (f *fakeStream) RecvMsg(m any) error                  { return nil }
func (f *fakeStream) SetHeader(md metadata.MD) error       { return nil }
func (f *fakeStream) SendHeader(md metadata.MD) error      { return nil }
func (f *fakeStream) SetTrailer(md metadata.MD)            {}
func (f *fakeStream) Context() context.Context             { return f.ctx }

// TestHandleEventForward_PublishesLocally is the regression guard for the
// shipped "cognition receives the utterance event but handleUtteranceFor
// Cognition never runs" bug. Peer events arrived at nodeService.handle
// EventForward, were logged and ACKed, and never republished on the local
// bus -- so no local subscriber (integration handler, automation trigger,
// gRPC subscriber) ever saw them. Fix: nodeService now invokes the
// EventInbound hook; this test pins the contract so the wiring can't
// silently disappear again.
func TestHandleEventForward_PublishesLocally(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	eventBridge := NewEventBridge(testIdentity(), bus, pm, testLogger())

	svc := &nodeService{
		logger:       testLogger(),
		identity:     testIdentity(),
		peerManager:  pm,
		eventInbound: eventBridge,
	}

	// Subscribe to the local bus. The handler should be invoked once the
	// peer event is bridged.
	received := make(chan events.Event, 1)
	bus.Subscribe("graph.node.created.v1:cognition:utterance", func(e events.Event) {
		received <- e
	})

	payload, _ := structpb.NewStruct(map[string]any{"spaceId": "space-1"})
	svc.handleEventForward("peer-bff", &nodev1.EventForward{
		EventId:      "evt-abc",
		Topic:        "graph.node.created.v1:cognition:utterance",
		Kind:         int32(events.KindNodeCreated),
		Ts:           timestamppb.New(time.Now()),
		Payload:      payload,
		OriginNodeId: "bff-local",
		Ttl:          3,
	}, newFakeStream())

	select {
	case e := <-received:
		if e.OriginNodeId != "bff-local" {
			t.Errorf("expected OriginNodeId bff-local, got %q", e.OriginNodeId)
		}
		if e.Payload["spaceId"] != "space-1" {
			t.Errorf("expected payload spaceId=space-1, got %v", e.Payload["spaceId"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for local bus publish -- peer event was not bridged")
	}
}

// TestHandleEventForward_NoInboundDoesNotPanic guards the nil-inbound
// path. NodeServer doesn't force SetEventInbound, so a misconfigured
// binary must still handle incoming events (they will just be dropped).
func TestHandleEventForward_NoInboundDoesNotPanic(t *testing.T) {
	svc := &nodeService{
		logger:       testLogger(),
		identity:     testIdentity(),
		eventInbound: nil, // not wired
	}

	stream := newFakeStream()
	svc.handleEventForward("peer", &nodev1.EventForward{
		EventId: "evt-x",
		Topic:   "graph.node.created.v1:cognition:utterance",
		Ttl:     3,
	}, stream)

	// ACK should still fire so the sender doesn't retry forever.
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 ACK sent, got %d", len(stream.sent))
	}
	if stream.sent[0].GetEventAck() == nil {
		t.Error("expected EventAck payload on sent message")
	}
}

// TestHandleEventForward_SendsAck pins the ACK contract for the happy
// path (sender relies on it for backpressure + retry decisions).
func TestHandleEventForward_SendsAck(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	pm := NewPeerManager(testIdentity(), testLogger())
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	svc := &nodeService{
		logger:       testLogger(),
		identity:     testIdentity(),
		peerManager:  pm,
		eventInbound: eb,
	}

	stream := newFakeStream()
	svc.handleEventForward("peer", &nodev1.EventForward{
		EventId: "evt-ack",
		Topic:   "graph.node.created.v1:cognition:utterance",
		Ttl:     3,
	}, stream)

	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 ACK, got %d", len(stream.sent))
	}
	ack := stream.sent[0].GetEventAck()
	if ack == nil {
		t.Fatal("expected EventAck payload")
	}
	if ack.EventId != "evt-ack" {
		t.Errorf("expected EventAck EventId=evt-ack, got %q", ack.EventId)
	}
}
