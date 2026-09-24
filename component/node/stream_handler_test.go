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
	mu     sync.Mutex
	sent   []*nodev1.NodeServerMessage
	recvCh chan *nodev1.NodeClientMessage
	ctx    context.Context
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

func (f *fakeStream) SendMsg(m any) error             { return nil }
func (f *fakeStream) RecvMsg(m any) error             { return nil }
func (f *fakeStream) SetHeader(md metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(md metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(md metadata.MD)       {}
func (f *fakeStream) Context() context.Context        { return f.ctx }

// TestHandleEventForward_PublishesLocally is the regression guard for the
// shipped "a peer receives the graph event but the handler never runs" bug.
// Peer events arrived at nodeService.handleEventForward, were logged, and were
// never republished on the local bus -- so no local subscriber (integration
// handler, automation trigger, gRPC subscriber) ever saw them. The handler now
// hands every arrival to the PeerManager's event sink, which NewEventBridge
// installs; this test pins that the wiring cannot silently disappear again.
func TestHandleEventForward_PublishesLocally(t *testing.T) {
	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()

	pm := NewPeerManager(testIdentity(), testLogger())
	_ = NewEventBridge(testIdentity(), bus, pm, testLogger())

	svc := &nodeService{
		logger:      testLogger(),
		identity:    testIdentity(),
		peerManager: pm,
	}

	// Subscribe to the local bus. The handler should be invoked once the
	// peer event is bridged.
	received := make(chan events.Event, 1)
	bus.Subscribe("graph.node.created.v1:library:artifact", func(e events.Event) {
		received <- e
	})

	payload, _ := structpb.NewStruct(map[string]any{"partitionId": "space-1"})
	svc.handleEventForward("peer-bff", &nodev1.EventForward{
		EventId:      "evt-abc",
		Topic:        "graph.node.created.v1:library:artifact",
		Kind:         int32(events.KindNodeCreated),
		Ts:           timestamppb.New(time.Now()),
		Payload:      payload,
		OriginNodeId: "bff-local",
	})

	select {
	case e := <-received:
		if e.OriginNodeId != "bff-local" {
			t.Errorf("expected OriginNodeId bff-local, got %q", e.OriginNodeId)
		}
		if e.Payload["partitionId"] != "space-1" {
			t.Errorf("expected payload partitionId=space-1, got %v", e.Payload["partitionId"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for local bus publish -- peer event was not bridged")
	}
}

// TestHandleEventForward_NoBridgeDoesNotPanic guards a node whose peer table
// has no event bridge installed: an arriving event is dropped, not a crash.
func TestHandleEventForward_NoBridgeDoesNotPanic(t *testing.T) {
	svc := &nodeService{
		logger:      testLogger(),
		identity:    testIdentity(),
		peerManager: NewPeerManager(testIdentity(), testLogger()),
	}
	svc.handleEventForward("peer", &nodev1.EventForward{
		EventId: "evt-x",
		Topic:   "graph.node.created.v1:library:artifact",
	})
}
