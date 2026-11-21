package memql

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/bus"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	"github.com/znasllc-io/memql/core/component"
)

func TestExecuteFailureDoesNotLogPrivateQueryPrefix(t *testing.T) {
	var log bytes.Buffer
	e := &MemQLEngine{Component: &component.Component{Logger: slog.New(slog.NewTextHandler(&log, nil))}}
	msg := bus.NewMessage()
	msg.Payload = &busv1.InternalMessage_EngineExecute{EngineExecute: &busv1.EngineExecuteRequest{Query: `builtin privateInput(event: {text: "private-spoken-content"})`}}
	req := bus.NewRequest(msg)
	e.handleExecuteRequest(context.Background(), req, "test-correlation")
	require.NotContains(t, log.String(), "private-spoken-content")
	require.NotContains(t, log.String(), "privateInput")
	require.Contains(t, log.String(), "test-correlation")
	require.Contains(t, log.String(), "query_bytes=")
	require.Contains(t, log.String(), "error_type=")
	reply := <-req.ReplyTo
	require.False(t, reply.GetEngineExecuteResponse().Success)
}

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
