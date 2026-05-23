package client

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// tools_test.go -- round-trip tests for the memql#174 tool surface.
// Reuses the mockStream helper from dispatcher_test.go (same package).

// TestListTools_RoundTrip: ListTools sends a ListToolsMsg, the test
// injects a correlated ListToolsResult, and the SDK shape comes back
// with every ToolDefinition field populated.
func TestListTools_RoundTrip(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	// Run ListTools in a goroutine so we can answer it.
	type out struct {
		res *ListToolsResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := qc.ListTools(context.Background(), ListToolsArgs{Cursor: "page-2"})
		done <- out{res: r, err: err}
	}()

	// Read the outbound message, assert it carries ListToolsMsg with
	// the cursor we passed.
	sent := <-stream.sendCh
	listMsg := sent.GetListTools()
	if listMsg == nil {
		t.Fatalf("expected ListToolsMsg payload, got %+v", sent.GetPayload())
	}
	if listMsg.GetCursor() != "page-2" {
		t.Errorf("cursor: want %q, got %q", "page-2", listMsg.GetCursor())
	}

	// Inject the correlated response.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_ListToolsResult{
			ListToolsResult: &memqlv1.ListToolsResult{
				NextCursor: "page-3",
				Tools: []*memqlv1.ToolDefinition{
					{
						Name:            "uiClick",
						Description:     "click a DOM element",
						InputSchema:     `{"type":"object"}`,
						ClientExecution: true,
						Scopes:          []string{"interact"},
					},
					{
						Name:            "queryActiveSpaces",
						Description:     "server-side query",
						InputSchema:     `{"type":"object"}`,
						ClientExecution: false,
					},
				},
			},
		},
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ListTools: %v", got.err)
		}
		if got.res.NextCursor != "page-3" {
			t.Errorf("NextCursor: want page-3, got %q", got.res.NextCursor)
		}
		if len(got.res.Tools) != 2 {
			t.Fatalf("tools: want 2, got %d", len(got.res.Tools))
		}
		if got.res.Tools[0].Name != "uiClick" || !got.res.Tools[0].ClientExecution {
			t.Errorf("tool[0] mismatched: %+v", got.res.Tools[0])
		}
		if got.res.Tools[0].Scopes[0] != "interact" {
			t.Errorf("scopes not carried: %+v", got.res.Tools[0].Scopes)
		}
		if got.res.Tools[1].ClientExecution {
			t.Errorf("tool[1] should be server-executed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ListTools to return")
	}
}

// TestCallTool_RoundTrip: CallTool sends a CallToolMsg with the
// supplied arguments map; the test injects a correlated
// CallToolResult; the SDK shape comes back with content + is_error.
func TestCallTool_RoundTrip(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	type out struct {
		res *CallToolResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := qc.CallTool(context.Background(), CallToolArgs{
			Name:      "queryActiveSpaces",
			Arguments: map[string]any{"userId": "user-123"},
		})
		done <- out{res: r, err: err}
	}()

	sent := <-stream.sendCh
	callMsg := sent.GetCallTool()
	if callMsg == nil {
		t.Fatalf("expected CallToolMsg payload, got %+v", sent.GetPayload())
	}
	if callMsg.GetName() != "queryActiveSpaces" {
		t.Errorf("name: want queryActiveSpaces, got %q", callMsg.GetName())
	}
	if userId := callMsg.GetArguments().GetFields()["userId"].GetStringValue(); userId != "user-123" {
		t.Errorf("arguments.userId: want user-123, got %q", userId)
	}

	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_CallToolResult{
			CallToolResult: &memqlv1.CallToolResult{
				Content: []*memqlv1.ToolResultContent{
					{Type: "text", Text: "ok"},
				},
				IsError: false,
			},
		},
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("CallTool: %v", got.err)
		}
		if got.res.IsError {
			t.Error("CallToolResult.IsError should be false")
		}
		if len(got.res.Content) != 1 || got.res.Content[0].Text != "ok" {
			t.Errorf("content mismatched: %+v", got.res.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CallTool to return")
	}
}

// TestCallTool_ErrorResult: an is_error=true response flows through
// to the SDK result.
func TestCallTool_ErrorResult(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	done := make(chan *CallToolResult, 1)
	go func() {
		r, _ := qc.CallTool(context.Background(), CallToolArgs{Name: "broken"})
		done <- r
	}()
	sent := <-stream.sendCh
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_CallToolResult{
			CallToolResult: &memqlv1.CallToolResult{
				IsError: true,
				Content: []*memqlv1.ToolResultContent{{Type: "text", Text: "boom"}},
			},
		},
	}
	got := <-done
	if got == nil || !got.IsError || got.Content[0].Text != "boom" {
		t.Errorf("error result not surfaced: %+v", got)
	}
}

// TestRegisterClientToolHandler_RoundTrip: server pushes a
// ClientToolCall; the SDK invokes the registered handler; the
// handler's return ships back as a ClientToolResult correlated by
// call_id.
func TestRegisterClientToolHandler_RoundTrip(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	gotCall := make(chan *ClientToolCall, 1)
	unregister := d.RegisterClientToolHandler(func(ctx context.Context, call *ClientToolCall) *ClientToolResult {
		gotCall <- call
		return &ClientToolResult{
			Content: []ToolResultContent{
				{Type: "text", Text: "handled"},
			},
		}
	})
	defer unregister()

	// Push a ClientToolCall onto the stream.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{
				CallId:        "call-abc",
				TurnId:        "turn-1",
				AgentId:       "agent-x",
				ToolName:      "uiClick",
				ArgumentsJson: `{"opId":"foo"}`,
				TimeoutMs:     5000,
			},
		},
	}

	// Handler should fire.
	select {
	case call := <-gotCall:
		if call.CallId != "call-abc" {
			t.Errorf("call_id: want call-abc, got %q", call.CallId)
		}
		if call.ToolName != "uiClick" || call.AgentId != "agent-x" {
			t.Errorf("call fields wrong: %+v", call)
		}
		if call.ArgumentsJson != `{"opId":"foo"}` {
			t.Errorf("argumentsJson wrong: %q", call.ArgumentsJson)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not invoked within deadline")
	}

	// Result should ship back on the stream.
	select {
	case sent := <-stream.sendCh:
		res := sent.GetClientToolResult()
		if res == nil {
			t.Fatalf("expected ClientToolResult payload, got %+v", sent.GetPayload())
		}
		if res.GetCallId() != "call-abc" {
			t.Errorf("reply call_id: want call-abc, got %q", res.GetCallId())
		}
		if res.GetIsError() {
			t.Error("reply.is_error should be false")
		}
		if len(res.GetContent()) != 1 || res.GetContent()[0].GetText() != "handled" {
			t.Errorf("content mismatched: %+v", res.GetContent())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result shipped within deadline")
	}
}

// TestRegisterClientToolHandler_NilResultIsErrored: a handler that
// returns nil is treated as is_error=true so the agent's parked tool
// call always unblocks.
func TestRegisterClientToolHandler_NilResultIsErrored(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	d.RegisterClientToolHandler(func(ctx context.Context, call *ClientToolCall) *ClientToolResult {
		return nil
	})
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{
				CallId:   "call-nil",
				ToolName: "x",
			},
		},
	}
	select {
	case sent := <-stream.sendCh:
		res := sent.GetClientToolResult()
		if res == nil || !res.GetIsError() {
			t.Fatalf("nil handler return must surface as is_error=true; got %+v", res)
		}
		if res.GetCallId() != "call-nil" {
			t.Errorf("reply must echo call_id; got %q", res.GetCallId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result shipped within deadline")
	}
}

// TestRegisterClientToolHandler_NoHandlerDrops: a call that arrives
// with no registered handler is dropped silently (no panic, no
// outbound message). The server's per-call deadline handles the
// timeout on its side.
func TestRegisterClientToolHandler_NoHandlerDrops(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{
				CallId:   "orphan",
				ToolName: "x",
			},
		},
	}
	select {
	case sent := <-stream.sendCh:
		t.Fatalf("no handler -> no outbound message expected; got %+v", sent.GetPayload())
	case <-time.After(150 * time.Millisecond):
		// expected: nothing shipped.
	}
}

// TestRegisterClientToolHandler_UnregisterStopsDispatch: after the
// unregister func runs, subsequent calls go to the no-handler path.
func TestRegisterClientToolHandler_UnregisterStopsDispatch(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	var calls int32
	unregister := d.RegisterClientToolHandler(func(ctx context.Context, call *ClientToolCall) *ClientToolResult {
		atomic.AddInt32(&calls, 1)
		return &ClientToolResult{Content: []ToolResultContent{{Type: "text", Text: "ok"}}}
	})

	// First call: handler runs, result ships.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{CallId: "c1", ToolName: "x"},
		},
	}
	select {
	case <-stream.sendCh:
	case <-time.After(time.Second):
		t.Fatal("first call: no result shipped")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("first call: handler should have run; got calls=%d", atomic.LoadInt32(&calls))
	}

	unregister()

	// Second call: dropped silently.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{CallId: "c2", ToolName: "x"},
		},
	}
	select {
	case sent := <-stream.sendCh:
		t.Fatalf("after unregister: no result expected; got %+v", sent.GetPayload())
	case <-time.After(150 * time.Millisecond):
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("handler ran after unregister; calls=%d", atomic.LoadInt32(&calls))
	}
}

// TestRegisterClientToolHandler_ReplaceOnReRegister: a second
// Register replaces the first handler. The first call's unregister
// func is then a no-op (the second handler stays installed).
func TestRegisterClientToolHandler_ReplaceOnReRegister(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	var calls struct {
		mu sync.Mutex
		a  int
		b  int
	}

	unregisterA := d.RegisterClientToolHandler(func(ctx context.Context, call *ClientToolCall) *ClientToolResult {
		calls.mu.Lock()
		calls.a++
		calls.mu.Unlock()
		return &ClientToolResult{}
	})
	_ = d.RegisterClientToolHandler(func(ctx context.Context, call *ClientToolCall) *ClientToolResult {
		calls.mu.Lock()
		calls.b++
		calls.mu.Unlock()
		return &ClientToolResult{}
	})
	// Re-running unregisterA should NOT clobber handler B -- the
	// "second Register replaces" semantic means the first
	// unregister's bookkeeping was superseded.

	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{CallId: "c1", ToolName: "x"},
		},
	}
	<-stream.sendCh // drain the result

	unregisterA() // should be a no-op now (handler B is current).

	// Next call should still hit handler B.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: &memqlv1.ClientToolCall{CallId: "c2", ToolName: "x"},
		},
	}
	<-stream.sendCh

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.a != 0 {
		t.Errorf("handler A should never have fired after being replaced; calls.a=%d", calls.a)
	}
	if calls.b != 2 {
		t.Errorf("handler B should have fired twice; calls.b=%d", calls.b)
	}
}
