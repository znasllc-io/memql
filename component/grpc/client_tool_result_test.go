package memql

import (
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
)

// TestDeliverClientToolResultFiresWaiter proves the agent-side firer
// (memql#1265) hands a substrate-RPC-delivered ClientToolResult to the
// service-scoped waiter registered by callId, decoding the JSON content array,
// and reports found=true. This is the agent edge of the client-tool RPC cutover
// -- the same dispatch the legacy ForwardContinuation landing used, but reached
// over the durable substrate instead of a cached peer connection.
func TestDeliverClientToolResultFiresWaiter(t *testing.T) {
	s := &service{}
	const callId = "call-xyz"

	ch := make(chan *memqlv1.ClientToolResult, 1)
	s.clientToolMu.Lock()
	s.clientToolWaiters = map[string]chan *memqlv1.ClientToolResult{callId: ch}
	s.clientToolMu.Unlock()

	found := s.DeliverClientToolResult(node.ClientToolResultPayload{
		CallID:      callId,
		ContentJSON: `[{"type":"text","text":"done"}]`,
	})
	if !found {
		t.Fatal("expected found=true for a registered waiter")
	}

	select {
	case got := <-ch:
		if got.GetCallId() != callId {
			t.Fatalf("wrong callId: %s", got.GetCallId())
		}
		content := got.GetContent()
		if len(content) != 1 || content[0].GetType() != "text" || content[0].GetText() != "done" {
			t.Fatalf("content decoded wrong: %+v", content)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter channel never received the result")
	}

	// Waiter is consumed (deleted) -- a second delivery finds nothing.
	if s.DeliverClientToolResult(node.ClientToolResultPayload{CallID: callId}) {
		t.Fatal("expected found=false after the waiter was consumed")
	}
}

// TestDeliverClientToolResultNoWaiter proves a result for an unknown call is a
// benign found=false (no panic, no hang).
func TestDeliverClientToolResultNoWaiter(t *testing.T) {
	s := &service{}
	if s.DeliverClientToolResult(node.ClientToolResultPayload{CallID: "nope"}) {
		t.Fatal("expected found=false with no waiters registered")
	}
	if s.DeliverClientToolResult(node.ClientToolResultPayload{}) {
		t.Fatal("expected found=false for empty callId")
	}
}

// TestDeliverClientToolResultError proves an error result is carried through
// with IsError + ErrorMessage intact.
func TestDeliverClientToolResultError(t *testing.T) {
	s := &service{}
	const callId = "call-err"
	ch := make(chan *memqlv1.ClientToolResult, 1)
	s.clientToolMu.Lock()
	s.clientToolWaiters = map[string]chan *memqlv1.ClientToolResult{callId: ch}
	s.clientToolMu.Unlock()

	if !s.DeliverClientToolResult(node.ClientToolResultPayload{
		CallID:       callId,
		IsError:      true,
		ErrorMessage: "browser refused",
	}) {
		t.Fatal("expected found=true")
	}
	got := <-ch
	if !got.GetIsError() || got.GetErrorMessage() != "browser refused" {
		t.Fatalf("error fields lost: isErr=%v msg=%q", got.GetIsError(), got.GetErrorMessage())
	}
}
