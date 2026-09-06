package client

import (
	"context"
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
