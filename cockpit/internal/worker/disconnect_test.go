package worker

import (
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStreamEndCodeReasonStableShapes(t *testing.T) {
	code, reason := streamEndCodeReason(nil)
	if code != "ok" || reason != "session_end" {
		t.Fatalf("nil: %s/%s", code, reason)
	}
	code, reason = streamEndCodeReason(io.EOF)
	if code != "eof" || reason != "client_eof" {
		t.Fatalf("EOF: %s/%s", code, reason)
	}
	code, reason = streamEndCodeReason(errServerDrain)
	if code != "canceled" || reason != DisconnectReasonServerDrain {
		t.Fatalf("drain: %s/%s", code, reason)
	}
	st := status.Error(codes.Unavailable, "transport is closing")
	code, reason = streamEndCodeReason(st)
	if code != codes.Unavailable.String() || reason != "transport is closing" {
		t.Fatalf("status: %s/%s", code, reason)
	}
	code, reason = streamEndCodeReason(errors.New("server requested drain"))
	if reason != DisconnectReasonServerDrain {
		t.Fatalf("legacy drain string: %s/%s", code, reason)
	}
}
