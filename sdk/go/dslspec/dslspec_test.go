package dslspec

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// mockStream implements memqlv1.MemqlService_StreamClient for testing the SDK
// method against a scripted server. Mirrors sdk/go/pack/pack_test.go.
type mockStream struct {
	sendCh chan *memqlv1.MemqlClientMessage
	recvCh chan *memqlv1.MemqlServerMessage
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

func (m *mockStream) Header() (metadata.MD, error) { return nil, nil }
func (m *mockStream) Trailer() metadata.MD         { return nil }
func (m *mockStream) CloseSend() error             { close(m.recvCh); return nil }
func (m *mockStream) Context() context.Context     { return context.Background() }
func (m *mockStream) SendMsg(any) error            { return nil }
func (m *mockStream) RecvMsg(any) error            { return nil }

// reply drains the next client message and injects a correlated server reply
// built from the client's message_id.
func reply(t *testing.T, stream *mockStream, fn func(*memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage) {
	t.Helper()
	go func() {
		sent := <-stream.sendCh
		resp := fn(sent)
		resp.CorrelateTo = sent.GetMessageId()
		stream.recvCh <- resp
	}()
}

func TestFetch(t *testing.T) {
	stream := newMockStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	c := NewClient(d)

	const wantJSON = `{"version":"1.0.0","constructs":[{"keyword":"concept"}]}`
	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		if req.GetDslSpec() == nil {
			t.Errorf("expected DslSpecMsg, got %T", req.GetPayload())
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_DslSpecResult{
				DslSpecResult: &memqlv1.DslSpecResult{
					SpecJson: wantJSON,
					Version:  "1.0.0",
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	spec, err := c.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if spec.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %q", spec.Version)
	}
	if spec.JSON != wantJSON {
		t.Errorf("spec JSON mismatch: %q", spec.JSON)
	}
}
