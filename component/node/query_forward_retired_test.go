package node

// query_forward_retired_test.go -- the cross-node query-forwarding machinery
// was removed in memql#2814, and these tests keep it removed on the WIRE.
//
// Deleting a Go handler is self-enforcing: the code is gone and nothing
// compiles against it. Deleting a proto field is not. Tags 60 (query_forward /
// query_response) were live on NodeClientMessage / NodeServerMessage, so a
// node built before the removal still puts a QueryForward on tag 60. If a
// later change assigns tag 60 to a different message, those two nodes parse
// the same bytes as different types during any rolling upgrade -- the classic
// wire-compat failure, and the reason the repo reserves retired numbers
// instead of just deleting them (see the #984 precedent in
// component/grpc/memql.proto).
//
// These assert the reservation itself, which is the thing a future edit can
// silently undo.

import (
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// retiredField is one (message, tag, name) triple that must stay retired.
type retiredField struct {
	desc protoreflect.MessageDescriptor
	tag  protoreflect.FieldNumber
	name protoreflect.Name
}

func retiredQueryForwardFields() []retiredField {
	return []retiredField{
		{
			desc: (&nodev1.NodeClientMessage{}).ProtoReflect().Descriptor(),
			tag:  60,
			name: "query_forward",
		},
		{
			desc: (&nodev1.NodeServerMessage{}).ProtoReflect().Descriptor(),
			tag:  60,
			name: "query_response",
		},
	}
}

// TestQueryForwardTagsStayReserved is the wire-compat guard: tag 60 must be
// reserved, not merely absent. Absent alone would let the next field added to
// either envelope be handed tag 60 by a well-meaning author.
func TestQueryForwardTagsStayReserved(t *testing.T) {
	for _, rf := range retiredQueryForwardFields() {
		t.Run(string(rf.desc.Name())+"/"+string(rf.name), func(t *testing.T) {
			if !rf.desc.ReservedRanges().Has(rf.tag) {
				t.Errorf("tag %d is not reserved on %s -- a future field could be assigned it, "+
					"and a pre-#2814 node still sends %s on that tag (memql#2814)",
					rf.tag, rf.desc.FullName(), rf.name)
			}
			if !rf.desc.ReservedNames().Has(rf.name) {
				t.Errorf("name %q is not reserved on %s (memql#2814)", rf.name, rf.desc.FullName())
			}
		})
	}
}

// TestQueryForwardFieldsAreGone pins the removal itself: no field may occupy
// the retired tag or name. Reserving a tag while a field still uses it is a
// state protoc rejects, but the NAME check also catches a re-add under a
// different tag, which protoc would happily accept.
func TestQueryForwardFieldsAreGone(t *testing.T) {
	for _, rf := range retiredQueryForwardFields() {
		t.Run(string(rf.desc.Name())+"/"+string(rf.name), func(t *testing.T) {
			fields := rf.desc.Fields()
			if f := fields.ByNumber(rf.tag); f != nil {
				t.Errorf("%s has a field on retired tag %d: %s (memql#2814)",
					rf.desc.FullName(), rf.tag, f.FullName())
			}
			if f := fields.ByName(rf.name); f != nil {
				t.Errorf("%s reintroduced %q on tag %d -- the query-forwarding path was removed "+
					"because propagating caller identity across it could not be done safely; "+
					"the mesh authority contract is being designed on memql#2876, and this path "+
					"must not come back without it",
					rf.desc.FullName(), rf.name, f.Number())
			}
		})
	}
}

// legacyQueryForwardEnvelope hand-encodes the bytes a PRE-#2814 node puts on
// the wire: a NodeClientMessage whose payload oneof carries QueryForward on
// tag 60. It is built with protowire rather than the generated types because
// the generated types no longer have the field -- which is the whole point.
func legacyQueryForwardEnvelope() []byte {
	// Inner QueryForward{request_id: "req-legacy", query: "query allNumbers"}.
	var inner []byte
	inner = protowire.AppendTag(inner, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, "req-legacy")
	inner = protowire.AppendTag(inner, 2, protowire.BytesType)
	inner = protowire.AppendString(inner, "query allNumbers")

	// Outer NodeClientMessage{message_id: "msg-legacy", query_forward: inner}.
	var outer []byte
	outer = protowire.AppendTag(outer, 1, protowire.BytesType)
	outer = protowire.AppendString(outer, "msg-legacy")
	outer = protowire.AppendTag(outer, 60, protowire.BytesType)
	outer = protowire.AppendBytes(outer, inner)
	return outer
}

// TestLegacyQueryForwardEnvelopeIsIgnoredNotFatal is the rolling-upgrade
// guard. During any upgrade a pre-#2814 node still sends tag 60 to a post-
// #2814 node. Removing a proto field does not stop those bytes arriving, so
// the receiving node must treat them as an unknown field and carry on -- not
// fail the unmarshal and not tear down the peer stream.
func TestLegacyQueryForwardEnvelopeIsIgnoredNotFatal(t *testing.T) {
	var msg nodev1.NodeClientMessage
	if err := proto.Unmarshal(legacyQueryForwardEnvelope(), &msg); err != nil {
		t.Fatalf("a legacy peer's tag-60 envelope failed to unmarshal: %v -- "+
			"reserving the tag must keep it parseable-and-ignored, otherwise every "+
			"pre-#2814 peer breaks the stream during a rolling upgrade", err)
	}

	// The retired field must not resolve to any payload: it is unknown now.
	if msg.Payload != nil {
		t.Fatalf("tag 60 resolved to a payload (%T); it must be an unknown field", msg.Payload)
	}
	// The rest of the envelope still decodes.
	if got := msg.GetMessageId(); got != "msg-legacy" {
		t.Errorf("message_id = %q, want %q -- unrelated fields must survive", got, "msg-legacy")
	}

	// And dispatch must be a no-op rather than a panic or a peer-visible error.
	svc := &nodeService{logger: testLogger(), identity: testIdentity()}
	stream := newFakeStream()
	svc.handleMessage("peer-legacy", &msg, stream)

	stream.mu.Lock()
	sent := len(stream.sent)
	stream.mu.Unlock()
	if sent != 0 {
		t.Errorf("handleMessage sent %d message(s) for a retired payload; expected silence "+
			"(the default branch logs at Debug and returns)", sent)
	}
}

// TestQueryForwardMessagesAreGone asserts the message types themselves are no
// longer in the file descriptor, so nothing can construct one.
func TestQueryForwardMessagesAreGone(t *testing.T) {
	file := (&nodev1.NodeClientMessage{}).ProtoReflect().Descriptor().ParentFile()
	msgs := file.Messages()
	for i := 0; i < msgs.Len(); i++ {
		switch name := msgs.Get(i).Name(); name {
		case "QueryForward", "QueryResponse":
			t.Errorf("message %s still exists in %s -- removed in memql#2814", name, file.Path())
		}
	}
}
