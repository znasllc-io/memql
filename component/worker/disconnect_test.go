package worker

import (
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDisconnectCodeReasonStableShapes(t *testing.T) {
	code, reason := disconnectCodeReason(nil, "")
	if code != "ok" || reason != "session_end" {
		t.Fatalf("nil cause: got %s/%s", code, reason)
	}
	code, reason = disconnectCodeReason(io.EOF, "")
	if code != "eof" || reason != "client_eof" {
		t.Fatalf("EOF: got %s/%s", code, reason)
	}
	st := status.Error(codes.Unavailable, "transport is closing")
	code, reason = disconnectCodeReason(st, "")
	if code != codes.Unavailable.String() || reason != "transport is closing" {
		t.Fatalf("status: got %s/%s", code, reason)
	}
	code, reason = disconnectCodeReason(errors.New("boom"), DisconnectReasonServerDrain)
	if code != "canceled" || reason != DisconnectReasonServerDrain {
		t.Fatalf("drain reason must win: got %s/%s", code, reason)
	}
}
