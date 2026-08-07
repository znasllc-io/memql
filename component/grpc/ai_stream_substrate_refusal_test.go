package memql

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc/codes"
)

// ai_stream_substrate_refusal_test.go -- memql#3205.
//
// The contract promises that a refused OPENER is "answered terminally, so the
// caller gets an error rather than a hang". proxyAIStream drains the forward
// response channel into a goroutine, and until this change it DISCARDED
// everything -- so on the streaming-chat path the promise did not hold: the
// refusal vanished, consumeTokenStream blocked forever on a substrate stream
// the worker never opened, and the client hung with no error.
//
// That path has no self-healing, unlike its transcribe sibling: streaming chat
// sends no continuation, so nothing later trips the HasInflight check.

// The wire carries the gRPC code as its String() form, and codes.Code has no
// exported parser -- UnmarshalJSON rejects this spelling. So the mapping is
// explicit, and this pins it: flattening PermissionDenied to Internal would
// tell an operator "the server broke" when the truth is "authority refused",
// which is the wrong page of the runbook during a rolling deploy.
func TestForwardErrorCodeRoundTripsWhatSendForwardErrorWrites(t *testing.T) {
	for _, want := range []codes.Code{
		codes.PermissionDenied,
		codes.Unauthenticated,
		codes.InvalidArgument,
		codes.Unimplemented,
		codes.Unavailable,
		codes.FailedPrecondition,
	} {
		if got := forwardErrorCode(want.String()); got != want {
			t.Errorf("forwardErrorCode(%q) = %v, want %v -- sendForwardError writes exactly "+
				"code.String() onto the wire, so a gap here silently reclassifies a refusal",
				want.String(), got, want)
		}
	}
	if got := forwardErrorCode("NotAGrpcCode"); got != codes.Internal {
		t.Errorf("forwardErrorCode of an unknown string = %v, want Internal (fail closed rather "+
			"than guess)", got)
	}
}

// The refusal must reach the client as a QueryError rather than being dropped.
func TestSubstrateForwardRefusalReachesTheClient(t *testing.T) {
	sess, cs := newSessionForRotate(&service{logger: testLogger()})

	// The exact message a refused opener produces: a QueryError delivered on
	// the forward response channel.
	respCh := make(chan *memqlv1.MemqlServerMessage, 1)
	respCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: &memqlv1.QueryErrorMsg{
				RequestId: "req-1",
				Error: &memqlv1.QueryError{
					Code:    codes.PermissionDenied.String(),
					Message: "forwarded_authority_refused: forwarded authority is absent",
				},
			},
		},
	}
	close(respCh)

	// The drain loop as proxyAIStream runs it.
	for msg := range respCh {
		qe := msg.GetQueryError()
		if qe == nil {
			continue
		}
		_ = sess.sendQueryError("req-1", "correlate-1",
			forwardErrorCode(qe.GetError().GetCode()), qe.GetError().GetMessage())
	}

	sent := cs.lastSent()
	if sent == nil {
		t.Fatal("the refusal was dropped: nothing reached the client.\n\n" +
			"consumeTokenStream then blocks on a substrate stream the worker never opened, with " +
			"no timeout, until the client's gRPC stream dies -- a hang where the contract " +
			"promises a typed error, and streaming chat has no continuation to recover on.")
	}
	qe := sent.GetQueryError()
	if qe == nil {
		t.Fatalf("client received %T, want a QueryError", sent.GetPayload())
	}
	if qe.GetError().GetCode() != codes.PermissionDenied.String() {
		t.Errorf("client saw code %q, want %q -- the receiver's classification must survive the "+
			"relay", qe.GetError().GetCode(), codes.PermissionDenied.String())
	}
}
