package memql

// Tests for the send-size guard on MemqlService.Stream.
//
// grpc-go does not fail a call whose message is over the send limit: it ends
// the stream. That is the whole reason the server measures an answer before it
// writes it. Without the guard, one oversize query answer is not "that request
// failed", it is "the connection every browser tab rides on is closed", and
// every request in flight goes with it. These tests pin the three things the
// guard must do: refuse exactly at the limit and not before it, answer the
// request that asked with a sentence, and leave the stream usable.

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/grpclimits"
	"github.com/znasllc-io/memql/core/id"
)

func limitTestSession(t *testing.T) (*streamSession, *captureStream) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cs := newCaptureStream(t)
	return &streamSession{service: &service{logger: logger}, stream: cs, logger: logger}, cs
}

// answerWith returns a result carrying one string of n bytes. The string is a
// slice of body, so a test that builds several sizes pays for one allocation.
func answerWith(body string, n int) *memqlv1.Result {
	return &memqlv1.Result{Data: []*structpb.Value{structpb.NewStringValue(body[:n])}}
}

// stampedSize is the size of the message sendServerMessage will actually hand
// to the stream for a query answer: the same payload with the message id and
// correlation it stamps on. The id is a fixed-width uuid, so a placeholder of
// the same width measures the same.
func stampedSize(requestID, correlate string, result *memqlv1.Result) int {
	return proto.Size(&memqlv1.MemqlServerMessage{
		MessageId:   strings.Repeat("m", len(id.NewShortId())),
		CorrelateTo: correlate,
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{RequestId: requestID, Result: result, Done: true},
		},
	})
}

// answerThatStampsTo returns the result whose stamped message is exactly
// target bytes. The enclosing length prefixes grow in steps, so the string
// length is found by correcting for the difference until it lands.
func answerThatStampsTo(t *testing.T, body, requestID, correlate string, target int) *memqlv1.Result {
	t.Helper()
	n := target - 64
	for i := 0; i < 8; i++ {
		got := stampedSize(requestID, correlate, answerWith(body, n))
		if got == target {
			return answerWith(body, n)
		}
		n += target - got
	}
	t.Fatalf("no payload size stamps to exactly %d bytes", target)
	return nil
}

func queryResultFor(cs *captureStream, requestID string) *memqlv1.QueryResultChunk {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, m := range cs.sent {
		if qr := m.GetQueryResult(); qr != nil && qr.GetRequestId() == requestID {
			return qr
		}
	}
	return nil
}

// TestAnAnswerTooLargeToSendFailsOnlyItsOwnRequest is the contract: an answer
// over the limit is never written to the stream, the request that asked gets a
// sentence it can show, and the next answer on the same stream goes out.
func TestAnAnswerTooLargeToSendFailsOnlyItsOwnRequest(t *testing.T) {
	s, cs := limitTestSession(t)
	limit := grpclimits.MaxMessageBytes
	body := strings.Repeat("x", limit+1)

	// One byte past the limit: the smallest answer the guard must refuse.
	over := answerThatStampsTo(t, body, "req-over", "msg-over", limit+1)
	if err := s.sendQueryResult("req-over", "msg-over", over); err != nil {
		t.Fatalf("sendQueryResult returned %v; the request was answered, with an error, so the stream is fine", err)
	}

	if queryResultFor(cs, "req-over") != nil {
		t.Fatal("an answer over the limit was written to the stream")
	}
	qe := queryErrorFor(cs, "req-over")
	if qe == nil {
		t.Fatal("the request that asked got no answer at all")
	}
	if qe.GetCode() != codes.ResourceExhausted.String() {
		t.Errorf("code = %q, want %q", qe.GetCode(), codes.ResourceExhausted.String())
	}
	const want = "This answer is too large to send: it is 32.1 MiB and one answer can carry at most 32 MiB. " +
		"Ask for fewer rows or fewer fields and try again."
	if qe.GetMessage() != want {
		t.Errorf("message =\n%q\nwant\n%q", qe.GetMessage(), want)
	}
	md := qe.GetMetadata()
	if md["code"] != "RESULT_TOO_LARGE" || md["limitBytes"] != "33554432" || md["sizeBytes"] != "33554433" {
		t.Errorf("metadata = %v, want code RESULT_TOO_LARGE with sizeBytes 33554433 and limitBytes 33554432", md)
	}

	// The stream is still there: the next answer is delivered.
	if err := s.sendQueryResult("req-small", "msg-small", answerWith(body, 16)); err != nil {
		t.Fatalf("the stream did not survive the refusal: %v", err)
	}
	if queryResultFor(cs, "req-small") == nil {
		t.Fatal("the answer after the refused one was not delivered")
	}

	// Nothing the guard let through is over the limit, whatever it said.
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, m := range cs.sent {
		if size := proto.Size(m); size > limit {
			t.Fatalf("a %d-byte message reached the stream; the limit is %d", size, limit)
		}
	}
}

// TestAnAnswerExactlyAtTheLimitIsSent pins the other side of the boundary. The
// receiver refuses a message STRICTLY larger than its limit (grpc-go compares
// length > max), so refusing at the limit would turn away an answer both ends
// would have carried.
func TestAnAnswerExactlyAtTheLimitIsSent(t *testing.T) {
	s, cs := limitTestSession(t)
	limit := grpclimits.MaxMessageBytes
	body := strings.Repeat("x", limit+1)

	exact := answerThatStampsTo(t, body, "req-exact", "msg-exact", limit)
	if err := s.sendQueryResult("req-exact", "msg-exact", exact); err != nil {
		t.Fatalf("sendQueryResult: %v", err)
	}
	qr := queryResultFor(cs, "req-exact")
	if qr == nil {
		t.Fatalf("an answer of exactly %d bytes was refused; only larger ones are", limit)
	}
	if queryErrorFor(cs, "req-exact") != nil {
		t.Fatal("an answer at the limit was both sent and refused")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if size := proto.Size(cs.sent[0]); size != limit {
		t.Fatalf("the sent message is %d bytes; the test meant to send exactly %d", size, limit)
	}
}

// TestAnEventTooLargeToSendIsDroppedAndFlagged covers the messages no request
// is waiting on. A live-feed event over the limit would end the stream the
// same way an answer does. It cannot fail "its own request", so it is dropped
// and the next notification that reaches the wire says so (gap_before), which
// is the signal a client already answers by re-reading. The number it would
// have carried is given back: seq is the count of notifications ON THE WIRE,
// so a number spent on one that never left would be a gap the client could
// not tell from a real loss.
func TestAnEventTooLargeToSendIsDroppedAndFlagged(t *testing.T) {
	s, cs := limitTestSession(t)
	limit := grpclimits.MaxMessageBytes
	body := strings.Repeat("x", limit+1)

	huge := &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_Event{
		Event: &memqlv1.EventNotification{
			SubscriptionId: "sub-1",
			Payload:        &structpb.Struct{Fields: map[string]*structpb.Value{"row": structpb.NewStringValue(body)}},
		},
	}}
	err := s.sendServerMessage("", huge)
	var tooLarge *messageTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("an oversize event returned %v, want a messageTooLargeError", err)
	}
	if tooLarge.size <= limit || tooLarge.limit != limit {
		t.Errorf("error reports size %d / limit %d for a message over %d", tooLarge.size, tooLarge.limit, limit)
	}
	cs.mu.Lock()
	sent := len(cs.sent)
	cs.mu.Unlock()
	if sent != 0 {
		t.Fatalf("%d messages reached the stream; the oversize event must not", sent)
	}

	if err := s.sendEventNotification("", "sub-1", "after", nil); err != nil {
		t.Fatalf("the stream did not survive the dropped event: %v", err)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.sent) != 1 {
		t.Fatalf("%d messages on the stream, want the one that followed", len(cs.sent))
	}
	ev := cs.sent[0].GetEvent()
	if ev.GetSeq() != 1 {
		t.Errorf("seq = %d, want 1: the dropped event never reached the wire, so it spent no number", ev.GetSeq())
	}
	if !ev.GetGapBefore() {
		t.Error("the notification after a dropped event must say gap_before, or the client never learns it missed one")
	}
}

// TestEveryQueryAnswerIsSentThroughSendQueryResult keeps the guard's two halves
// together. sendServerMessage refuses an oversize message wherever it comes
// from, which is what protects the stream. But a refusal only becomes a
// sentence for the person who asked when the sender is sendQueryResult, because
// that is the one place that knows which request a result answers. A second
// site that builds a QueryResult message itself would still be safe for the
// stream and would silently leave its request without any answer.
func TestEveryQueryAnswerIsSentThroughSendQueryResult(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, ".pb.go") || f == "message_limit.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "MemqlServerMessage_QueryResult{") {
			t.Errorf("%s builds a QueryResult message itself; send it with sendQueryResult so an answer over the limit "+
				"fails its own request with a sentence", f)
		}
	}
}
