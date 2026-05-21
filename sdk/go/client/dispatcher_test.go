package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc/metadata"
)

// mockStream implements the minimal MemqlService_StreamClient interface for testing.
type mockStream struct {
	sendCh chan *memqlv1.MemqlClientMessage
	recvCh chan *memqlv1.MemqlServerMessage
	closed bool
}

func newMockStream() *mockStream {
	return &mockStream{
		sendCh: make(chan *memqlv1.MemqlClientMessage, 10),
		recvCh: make(chan *memqlv1.MemqlServerMessage, 10),
	}
}

func (m *mockStream) Send(msg *memqlv1.MemqlClientMessage) error {
	m.sendCh <- msg
	return nil
}

func (m *mockStream) Recv() (*memqlv1.MemqlServerMessage, error) {
	msg, ok := <-m.recvCh
	if !ok {
		return nil, context.Canceled
	}
	return msg, nil
}

// Implement remaining grpc.ClientStream methods as no-ops.
func (m *mockStream) Header() (metadata.MD, error) { return nil, nil }
func (m *mockStream) Trailer() metadata.MD         { return nil }
func (m *mockStream) CloseSend() error             { close(m.recvCh); return nil }
func (m *mockStream) Context() context.Context     { return context.Background() }
func (m *mockStream) SendMsg(any) error            { return nil }
func (m *mockStream) RecvMsg(any) error            { return nil }

func TestDispatcherCorrelation(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	// Send a message and simulate a correlated response.
	msg := &memqlv1.MemqlClientMessage{
		MessageId: "test-123",
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{ClientId: "test"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start SendAndWait in a goroutine.
	respCh := make(chan *memqlv1.MemqlServerMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := d.SendAndWait(ctx, msg)
		respCh <- resp
		errCh <- err
	}()

	// Read the sent message.
	sent := <-stream.sendCh
	if sent.GetMessageId() != "test-123" {
		t.Fatalf("expected message_id 'test-123', got %q", sent.GetMessageId())
	}

	// Inject a correlated response.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: "test-123",
		Payload: &memqlv1.MemqlServerMessage_ServerHello{
			ServerHello: &memqlv1.ServerHello{
				NodeId:  "node-1",
				Version: "1.0",
			},
		},
	}

	// Check the response.
	resp := <-respCh
	err := <-errCh
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetServerHello().GetNodeId() != "node-1" {
		t.Errorf("expected node_id 'node-1', got %q", resp.GetServerHello().GetNodeId())
	}
}

func TestDispatcherUncorrelatedEvents(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	// Inject an uncorrelated event.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_Heartbeat{
			Heartbeat: &memqlv1.HeartbeatMsg{},
		},
	}

	// Should appear on the events channel.
	select {
	case msg := <-d.Events():
		if msg.GetHeartbeat() == nil {
			t.Error("expected heartbeat event")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// TestRotateAuth_Success drives the happy path: client sends
// RotateAuthMsg with the freshly-rolled token; server replies
// RotateAuthResult{ok: true}; RotateAuth returns nil.
//
// This is the contract the cockpit's background refresher relies on
// to keep an in-flight stream's identity aligned with the on-disk
// credential. A regression here means admin-side revocation would
// stay invisible to live sessions until the next reconnect.
func TestRotateAuth_Success(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh <- d.RotateAuth(ctx, "freshly-rolled-jwt")
	}()

	sent := <-stream.sendCh
	rotateMsg := sent.GetRotateAuth()
	if rotateMsg == nil {
		t.Fatalf("expected RotateAuthMsg payload; got %T", sent.GetPayload())
	}
	if rotateMsg.GetAccessToken() != "freshly-rolled-jwt" {
		t.Errorf("rotate carried wrong token: %q", rotateMsg.GetAccessToken())
	}

	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_RotateAuthResult{
			RotateAuthResult: &memqlv1.RotateAuthResult{Ok: true},
		},
	}

	if err := <-errCh; err != nil {
		t.Fatalf("RotateAuth: %v", err)
	}
}

// TestRotateAuth_ServerRejection covers the case where the server
// answers RotateAuthResult{ok: false}. RotateAuth must return an
// error wrapping ErrRotateAuthRejected, so the caller can branch on
// the sentinel and distinguish a hard reject from a transport blip.
func TestRotateAuth_ServerRejection(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh <- d.RotateAuth(ctx, "bad-jwt")
	}()

	sent := <-stream.sendCh
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_RotateAuthResult{
			RotateAuthResult: &memqlv1.RotateAuthResult{
				Ok:               false,
				Error:            "invalid_token",
				ErrorDescription: "kid not in JWKS cache",
			},
		},
	}

	err := <-errCh
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRotateAuthRejected) {
		t.Fatalf("expected errors.Is(err, ErrRotateAuthRejected); err = %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_token") {
		t.Errorf("error message should surface server's error code; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "kid not in JWKS cache") {
		t.Errorf("error message should surface server's error description; got %q", err.Error())
	}
}

// TestRotateAuth_EmptyTokenRejected exercises the precondition guard.
// A blank token must be refused without ever hitting the wire -- the
// server would respond with invalid_token anyway, but firing the
// roundtrip wastes a network call and produces a misleading "server
// rejected" log entry. Refusing locally keeps the diagnosis clean.
func TestRotateAuth_EmptyTokenRejected(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := d.RotateAuth(ctx, "  ")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if errors.Is(err, ErrRotateAuthRejected) {
		t.Errorf("empty-input error must NOT carry ErrRotateAuthRejected (no server interaction); got %v", err)
	}
	select {
	case unexpected := <-stream.sendCh:
		t.Errorf("RotateAuth sent a message despite the empty-token guard: %+v", unexpected)
	default:
		// good -- nothing sent
	}
}

// TestRotateAuth_MalformedReply guards against silent acceptance of a
// reply that's correlated but doesn't carry the expected payload
// shape. Without this check a future server bug that sent (say) a
// QueryError on the rotate's message_id would be treated as success
// and the client would believe its identity was rotated when it
// wasn't.
func TestRotateAuth_MalformedReply(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh <- d.RotateAuth(ctx, "any-token")
	}()

	sent := <-stream.sendCh
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_Heartbeat{
			Heartbeat: &memqlv1.HeartbeatMsg{},
		},
	}

	err := <-errCh
	if err == nil {
		t.Fatal("expected error for malformed reply, got nil")
	}
	if !strings.Contains(err.Error(), "missing RotateAuthResult") {
		t.Errorf("error should call out the missing payload; got %q", err.Error())
	}
}

func TestDispatcherSendAssignsId(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)

	msg := &memqlv1.MemqlClientMessage{}
	id, err := d.Send(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty message_id")
	}

	sent := <-stream.sendCh
	if sent.GetMessageId() != id {
		t.Errorf("sent message_id %q does not match returned id %q", sent.GetMessageId(), id)
	}
}
