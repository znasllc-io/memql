package memql

// badge_gate_request_id_test.go -- the coupling between the two badge lists
// (memql#3935).
//
// badgeGate decides WHICH payloads a live operator grant may not send.
// badgePayloadRequestId pulls the inner request_id off a payload so the
// refusal correlates to the caller's request rather than only to the envelope
// messageId. They are two switches over one set, in two functions, forty lines
// apart, and adding a payload to the first without adding it to the second is
// the natural mistake.
//
// IT IS ALSO THE WORSE FAILURE. A stream-keyed SDK dispatcher routes replies by
// request_id; a rejection carrying none reaches no waiting listener, so the
// caller HANGS. Not gating the payload at all would at least have returned
// something. So the invariant is: everything badgeGate restricts,
// badgePayloadRequestId can name -- for every payload that has a request_id to
// name.
//
// Enumerated from the protobuf descriptor rather than from a hand-written list,
// because a hand-written list is a third place to forget the payload.

import (
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

const badgeGateSentinelRequestId = "req-sentinel"

// TestEveryBadgeRestrictedPayloadCanNameItsRequestId walks every payload the
// client oneof carries, asks badgeGate about it, and requires
// badgePayloadRequestId to answer for the restricted ones.
func TestEveryBadgeRestrictedPayloadCanNameItsRequestId(t *testing.T) {
	// Stamp a LIVE grant directly: badgeStamped short-circuits the lazy claims
	// read, which a bare session's nil stream cannot satisfy.
	session := &streamSession{}
	session.badgeStamped = true
	session.badgeExpiresAt = time.Now().Add(time.Hour)

	fields := (&memqlv1.MemqlClientMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("payload").Fields()
	restricted := 0
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		envelope, hasRequestId := envelopeForPayload(fd)
		if envelope == nil {
			continue
		}
		if session.badgeGate(envelope) != badgeGateRestricted {
			continue
		}
		restricted++
		if !hasRequestId {
			// A restricted payload with no request_id field of its own has
			// nothing to name, and the envelope messageId is the whole of what
			// a client can correlate on. Nothing to assert.
			continue
		}
		if got := badgePayloadRequestId(envelope); got != badgeGateSentinelRequestId {
			t.Errorf("badgeGate restricts %s but badgePayloadRequestId returns %q -- a stream-keyed "+
				"client would never see the refusal and would hang instead. Add a case for it.",
				fd.Name(), got)
		}
	}

	// The gate is only meaningful if it found something. A refactor that
	// emptied the restricted set would otherwise pass silently.
	if restricted == 0 {
		t.Fatal("no payload was restricted; this gate examined nothing")
	}
}

// envelopeForPayload builds a client envelope carrying one oneof payload, with
// its request_id set to the sentinel when it has one.
func envelopeForPayload(fd protoreflect.FieldDescriptor) (*memqlv1.MemqlClientMessage, bool) {
	envelope := &memqlv1.MemqlClientMessage{MessageId: "m-1"}
	reflected := envelope.ProtoReflect()
	value := reflected.NewField(fd)
	body := value.Message()
	requestIdFd := body.Descriptor().Fields().ByName("request_id")
	hasRequestId := requestIdFd != nil && requestIdFd.Kind() == protoreflect.StringKind
	if hasRequestId {
		body.Set(requestIdFd, protoreflect.ValueOfString(badgeGateSentinelRequestId))
	}
	reflected.Set(fd, value)
	return envelope, hasRequestId
}
