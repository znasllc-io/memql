package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/bus"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		n        int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc..."},
	}

	for _, tt := range tests {
		got := truncate(tt.input, tt.n)
		if got != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.expected)
		}
	}
}

func TestMarshalExecuteResult(t *testing.T) {
	// Test nil result
	val, err := marshalExecuteResult(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val == nil {
		t.Error("expected non-nil value for nil result")
	}
}

func TestHandleBusRequestUnknownType(t *testing.T) {
	cfg := bus.DefaultChannelConfig()
	w := bus.NewWiring(cfg)

	e := &MemQLEngine{wiring: w}

	msg := bus.NewMessage()
	msg.Payload = &busv1.InternalMessage_DbQuery{
		DbQuery: &busv1.DbQueryRequest{Query: "SELECT 1"},
	}
	req := bus.NewRequest(msg)

	// Should handle unknown type gracefully by sending error response
	e.handleBusRequest(nil, req)

	// Should get an error response (non-blocking since ReplyTo is buffered)
	select {
	case resp := <-req.ReplyTo:
		execResp := resp.GetEngineExecuteResponse()
		if execResp == nil {
			t.Fatal("expected EngineExecuteResponse for unknown type")
		}
		if execResp.Success {
			t.Error("expected success=false for unknown type")
		}
		if execResp.Error != "unknown engine request type" {
			t.Errorf("expected 'unknown engine request type', got %q", execResp.Error)
		}
	default:
		t.Error("expected a reply for unknown request type")
	}
}
