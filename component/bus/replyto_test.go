package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

func TestNewRequest(t *testing.T) {
	msg := NewMessage()
	req := NewRequest(msg)

	if req.Msg != msg {
		t.Error("expected request to hold the original message")
	}
	if req.ReplyTo == nil {
		t.Error("expected non-nil ReplyTo channel")
	}
	if cap(req.ReplyTo) != 1 {
		t.Errorf("expected ReplyTo buffer size 1, got %d", cap(req.ReplyTo))
	}
}

func TestRequestReplyAndAwait(t *testing.T) {
	msg := NewMessage()
	msg.Payload = &busv1.InternalMessage_EngineExecute{
		EngineExecute: &busv1.EngineExecuteRequest{Query: "concept==v1:test"},
	}
	req := NewRequest(msg)

	// Simulate responder in background
	go func() {
		resp := NewCorrelatedMessage(req.Msg)
		resp.Payload = &busv1.InternalMessage_EngineExecuteResponse{
			EngineExecuteResponse: &busv1.EngineExecuteResponse{
				Success: true,
				TookMs:  42,
			},
		}
		req.Reply(resp)
	}()

	ctx := context.Background()
	resp, err := req.Await(ctx, 1*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	execResp := resp.GetEngineExecuteResponse()
	if execResp == nil {
		t.Fatal("expected EngineExecuteResponse payload")
	}
	if !execResp.Success {
		t.Error("expected success=true")
	}
	if execResp.TookMs != 42 {
		t.Errorf("expected took_ms=42, got %d", execResp.TookMs)
	}
	if resp.CorrelateTo != msg.MessageId {
		t.Errorf("expected correlate_to=%q, got %q", msg.MessageId, resp.CorrelateTo)
	}
	if resp.CorrelationId != msg.CorrelationId {
		t.Errorf("expected correlation_id=%q, got %q", msg.CorrelationId, resp.CorrelationId)
	}
}

func TestRequestAwaitTimeout(t *testing.T) {
	msg := NewMessage()
	req := NewRequest(msg)

	// No responder -- should timeout
	ctx := context.Background()
	_, err := req.Await(ctx, 10*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got %v", err)
	}
}

func TestRequestAwaitContextCancelled(t *testing.T) {
	msg := NewMessage()
	req := NewRequest(msg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := req.Await(ctx, 0)
	if !errors.Is(err, ErrContextCancelled) {
		t.Errorf("expected ErrContextCancelled, got %v", err)
	}
}

func TestRequestDoubleReplyDropsSilently(t *testing.T) {
	msg := NewMessage()
	req := NewRequest(msg)

	resp1 := NewCorrelatedMessage(msg)
	resp2 := NewCorrelatedMessage(msg)

	req.Reply(resp1)
	req.Reply(resp2) // should not panic or block

	ctx := context.Background()
	got, err := req.Await(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MessageId != resp1.MessageId {
		t.Error("expected first reply to be the one received")
	}
}
