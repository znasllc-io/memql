package memql

import (
	"testing"
)

// TestClientToolRPCKeyMatchesCognition pins the logical endpoint kind so the
// agent's Serve key and cognition's Call key (which uses the same literal
// "rpcClientTool" kind + requestId) can never silently drift apart.
func TestClientToolRPCKeyMatchesCognition(t *testing.T) {
	t.Parallel()
	key := ClientToolRPCKey("req-123")
	if key.Kind != "rpcClientTool" {
		t.Fatalf("endpoint kind = %q, want rpcClientTool (must match cognition's rpcClientToolKind)", key.Kind)
	}
	if key.ID != "req-123" {
		t.Fatalf("endpoint id = %q, want req-123", key.ID)
	}
}

// TestDecodeClientToolResult proves the request payload (the exact map shape
// cognition's relay marshals) decodes back into a ClientToolResult with content
// items intact. This is the cross-binary wire contract between
// integrations/cognition and this consumer.
func TestDecodeClientToolResult(t *testing.T) {
	t.Parallel()
	payload := map[string]any{
		clientToolRPCKeyCallID:   "call-7",
		clientToolRPCKeyIsError:  true,
		clientToolRPCKeyErrorMsg: "boom",
		clientToolRPCKeyContent: []any{
			map[string]any{"type": "text", "text": "hello", "mimeType": "text/plain"},
			map[string]any{"type": "resource", "uri": "mem://x", "data": "ZGF0YQ=="},
		},
	}
	result := decodeClientToolResult(payload)
	if result == nil {
		t.Fatal("decode returned nil")
	}
	if result.GetCallId() != "call-7" {
		t.Fatalf("callId = %q", result.GetCallId())
	}
	if !result.GetIsError() || result.GetErrorMessage() != "boom" {
		t.Fatalf("error fields not decoded: isError=%v msg=%q", result.GetIsError(), result.GetErrorMessage())
	}
	if len(result.GetContent()) != 2 {
		t.Fatalf("content len = %d, want 2", len(result.GetContent()))
	}
	if result.GetContent()[0].GetText() != "hello" || result.GetContent()[0].GetMimeType() != "text/plain" {
		t.Fatalf("content[0] mismatch: %+v", result.GetContent()[0])
	}
	if result.GetContent()[1].GetUri() != "mem://x" || result.GetContent()[1].GetData() != "ZGF0YQ==" {
		t.Fatalf("content[1] mismatch: %+v", result.GetContent()[1])
	}
}

// TestDecodeClientToolResultEmpty proves a nil/empty payload degrades to nil (or
// an empty-callId result) rather than panicking -- a malformed reply must not
// crash the agent's Serve loop.
func TestDecodeClientToolResultEmpty(t *testing.T) {
	t.Parallel()
	if decodeClientToolResult(nil) != nil {
		t.Fatal("nil payload should decode to nil")
	}
	r := decodeClientToolResult(map[string]any{})
	if r == nil || r.GetCallId() != "" {
		t.Fatalf("empty payload should yield empty-callId result, got %+v", r)
	}
}

// The full cross-replica RPC round-trip (a Call from one replica reaching a
// Serve handler on another over the durable store, reply routed back) is proven
// against the substrate's in-memory fake in component/node/substrate_rpc_test.go.
// This package owns the client-tool MAPPING onto that primitive: the endpoint
// key contract (above) and the payload <-> ClientToolResult decode (the two
// tests above), which together pin the cross-binary wire contract with the
// cognition relay.
