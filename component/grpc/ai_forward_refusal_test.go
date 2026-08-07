package memql

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// ai_forward_refusal_test.go -- memql#3205.
//
// The receiver refuses a forward it cannot prove the authority of. The SHAPE of
// that refusal is the load-bearing part, and it differs by payload class:
//
//   - an OPENER is answered terminally, so the caller gets an error rather
//     than a hang;
//   - a CONTINUATION is DROPPED, because it shares the PARENT turn's
//     request_id and any response on that id can close the parent's channel.

func envelopeBytes(t *testing.T, payload any) []byte {
	t.Helper()
	env := &memqlv1.MemqlClientMessage{MessageId: "env-1"}
	switch p := payload.(type) {
	case *memqlv1.MemqlClientMessage_AiChat:
		env.Payload = p
	case *memqlv1.MemqlClientMessage_AgentPreemptTurn:
		env.Payload = p
	case *memqlv1.MemqlClientMessage_ClientToolResult:
		env.Payload = p
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamChunk:
		env.Payload = p
	default:
		t.Fatalf("unhandled payload %T", payload)
	}
	b, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// collectSends returns a send func plus a pointer to what it captured.
func collectSends() (func(*nodev1.NodeServerMessage) error, *[]*nodev1.NodeServerMessage) {
	var got []*nodev1.NodeServerMessage
	return func(m *nodev1.NodeServerMessage) error {
		got = append(got, m)
		return nil
	}, &got
}

// An OPENER whose authority cannot be proven is refused terminally: exactly one
// response, done=true, carrying a QueryError the caller can surface.
func TestHandleForwardedRequestRefusesAnUnprovableOpenerTerminally(t *testing.T) {
	svc := &service{logger: testLogger()}
	send, got := collectSends()

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId:     "req-opener",
		MemqlEnvelope: envelopeBytes(t, &memqlv1.MemqlClientMessage_AiChat{AiChat: &memqlv1.AiChatMsg{}}),
		// No Authority at all -- the pre-contract shape, and the shape an
		// old producer sends during a rolling deploy.
	}, send)

	if len(*got) != 1 {
		t.Fatalf("got %d responses, want exactly 1 terminal refusal", len(*got))
	}
	resp := (*got)[0].GetAiForwardResponse()
	if resp == nil || !resp.GetDone() {
		t.Fatalf("refusal was not a terminal AiForwardResponse: %+v", (*got)[0])
	}

	var server memqlv1.MemqlServerMessage
	if err := proto.Unmarshal(resp.GetMemqlServerMsg(), &server); err != nil {
		t.Fatalf("unmarshal refusal body: %v", err)
	}
	qe := server.GetQueryError()
	if qe == nil || !strings.Contains(qe.GetError().GetMessage(), "forwarded_authority_refused") {
		t.Errorf("refusal did not name the forwarded-authority gate: %+v", server.GetPayload())
	}
}

// THE ONE THAT MATTERS MOST. A refused CONTINUATION must send NOTHING.
//
// sendForwardError hardcodes done=true; AiForwardRouter.Dispatch calls
// cleanupInflight on done; cleanupInflight closes the response channel. A
// continuation carries the PARENT turn's request_id, so answering one at all
// ends the parent turn while the agent keeps running.
func TestHandleForwardedRequestDropsARefusedContinuation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload any
	}{
		{"AgentPreemptTurn", &memqlv1.MemqlClientMessage_AgentPreemptTurn{AgentPreemptTurn: &memqlv1.AgentPreemptTurnMsg{RequestId: "req-parent"}}},
		{"ClientToolResult", &memqlv1.MemqlClientMessage_ClientToolResult{ClientToolResult: &memqlv1.ClientToolResult{CallId: "c1"}}},
		{"AiTranscribeStreamChunk", &memqlv1.MemqlClientMessage_AiTranscribeStreamChunk{AiTranscribeStreamChunk: &memqlv1.AiTranscribeStreamChunk{RequestId: "req-parent"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &service{logger: testLogger()}
			send, got := collectSends()

			svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
				RequestId:     "req-parent",
				MemqlEnvelope: envelopeBytes(t, tc.payload),
				Continuation:  true,
			}, send)

			if len(*got) != 0 {
				t.Errorf("a refused continuation sent %d response(s); it must send NONE.\n\n"+
					"A continuation shares the PARENT turn's request_id. Any AiForwardResponse on "+
					"that id reaches the producer's Dispatch -- done=true closes the parent's "+
					"channel outright, and even done=false delivers a QueryError both consumers "+
					"treat as fatal. Dropping is the only shape that leaves the parent alive.",
					len(*got))
			}
		})
	}
}

// The refusal shape is decided by THIS node's payload-class table, not by the
// producer's `continuation` bool. A producer that mislabels a continuation as
// an opener must not be able to talk this node into killing a parent turn.
func TestRefusalShapeIgnoresAMislabelledContinuationFlag(t *testing.T) {
	svc := &service{logger: testLogger()}
	send, got := collectSends()

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId: "req-parent",
		MemqlEnvelope: envelopeBytes(t, &memqlv1.MemqlClientMessage_AgentPreemptTurn{
			AgentPreemptTurn: &memqlv1.AgentPreemptTurnMsg{RequestId: "req-parent"},
		}),
		Continuation: false, // WRONG: this payload is a continuation
	}, send)

	if len(*got) != 0 {
		t.Errorf("a mislabelled continuation was answered (%d response(s)). The receiver must "+
			"classify by the payload type it reads, using the wire flag only when the envelope "+
			"cannot be parsed -- otherwise a producer bug becomes a parent-turn kill.", len(*got))
	}
}

// THE POSITIVE CONTROL, and it has to drive the RECEIVER to be one.
//
// Every other test in this file asserts a refusal, and a receiver that refused
// unconditionally would satisfy all of them. So this sends a well-formed SYSTEM
// assertion -- exactly what the planner's AgentPreemptTurn and the cognition
// client-tool relay now send -- through HandleForwardedRequest and requires
// that the gate does NOT refuse it.
//
// AgentPreemptTurn is chosen deliberately: on a service with no agent turn
// registry the handler is a no-op that sends nothing, so "no forwarded_authority
// refusal was emitted" is a clean signal about the GATE rather than about
// whatever the handler would go on to do.
func TestHandleForwardedRequestAcceptsAWellFormedSystemAssertion(t *testing.T) {
	authority, err := auth.ForwardedAuthorityForSystem("system:planner-integration", time.Now())
	if err != nil {
		t.Fatalf("building a system authority: %v", err)
	}

	svc := &service{logger: testLogger()}
	send, got := collectSends()

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId: "req-accepted",
		MemqlEnvelope: envelopeBytes(t, &memqlv1.MemqlClientMessage_AgentPreemptTurn{
			AgentPreemptTurn: &memqlv1.AgentPreemptTurnMsg{RequestId: "req-accepted"},
		}),
		Continuation: true,
		Auth:         authority.Principal().Claims,
		Authority:    forwardedAuthorityToProto(authority, "planner-1", "planner"),
	}, send)

	for _, m := range *got {
		resp := m.GetAiForwardResponse()
		if resp == nil {
			continue
		}
		var server memqlv1.MemqlServerMessage
		if err := proto.Unmarshal(resp.GetMemqlServerMsg(), &server); err != nil {
			continue
		}
		if qe := server.GetQueryError(); qe != nil &&
			strings.Contains(qe.GetError().GetMessage(), "forwarded_authority_refused") {
			t.Fatalf("the gate REFUSED a well-formed system assertion: %s.\n\n"+
				"Both deliberate nil-claims producers send exactly this shape. If it is refused, "+
				"pausing a Plan and resuming a client tool both stop working in cluster mode -- "+
				"and because a continuation refusal is DROPPED rather than answered, they would "+
				"stop working silently.", qe.GetError().GetMessage())
		}
	}
}

// The same shape, one field short: the claims map naming a different subject
// than the assertion. Pins the cross-check that stops the two carriers drifting.
func TestHandleForwardedRequestRefusesWhenClaimsDisagreeWithTheAssertion(t *testing.T) {
	authority, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{UserId: "v1:identity:user:real", Role: auth.RoleWriter},
		auth.ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("building a user authority: %v", err)
	}

	svc := &service{logger: testLogger()}
	send, got := collectSends()

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId:     "req-mismatch",
		MemqlEnvelope: envelopeBytes(t, &memqlv1.MemqlClientMessage_AiChat{AiChat: &memqlv1.AiChatMsg{}}),
		Auth:          map[string]string{"sub": "v1:identity:user:someone-else"},
		Authority:     forwardedAuthorityToProto(authority, "bff-1", "bff"),
	}, send)

	if len(*got) != 1 {
		t.Fatalf("got %d responses, want 1 terminal refusal for a principal mismatch", len(*got))
	}
}

// A drift gate, and it PASSES TODAY by design: it exists so that adding a
// payload to the receiver's dispatch switch without adding it to the class
// table is a test failure rather than a silently mis-shaped refusal.
func TestForwardPayloadClassCoversEveryDispatchedPayload(t *testing.T) {
	dispatched := []any{
		&memqlv1.MemqlClientMessage_AiTranscribe{},
		&memqlv1.MemqlClientMessage_AiTranscribeStreamStart{},
		&memqlv1.MemqlClientMessage_AiTranscribeStreamChunk{},
		&memqlv1.MemqlClientMessage_AiTranscribeStreamEnd{},
		&memqlv1.MemqlClientMessage_AiSpeech{},
		&memqlv1.MemqlClientMessage_AiChat{},
		&memqlv1.MemqlClientMessage_AiSuggest{},
		&memqlv1.MemqlClientMessage_ListTools{},
		&memqlv1.MemqlClientMessage_CallTool{},
		&memqlv1.MemqlClientMessage_AgentGenerateTurn{},
		&memqlv1.MemqlClientMessage_ClientToolResult{},
		&memqlv1.MemqlClientMessage_AgentPreemptTurn{},
	}
	for _, p := range dispatched {
		if _, ok := forwardPayloadClass(p); !ok {
			t.Errorf("%T is dispatched by HandleForwardedRequest but has no entry in "+
				"forwardPayloadClass, so a refusal for it would be shaped by the producer's wire "+
				"flag alone -- and getting that wrong on a continuation kills a parent turn", p)
		}
	}
}
