package memql

import (
	"context"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// ai_forward_parent_stream_test.go -- memql#3205, acceptance criterion 6.
//
// "a refusal does NOT call cleanupInflight on a parent requestId -- a test
// pauses a Plan in cluster mode and asserts the parent turn's stream survives".
//
// This drives the whole chain the parent turn actually dies on, in one process:
//
//	ForwardContinuation  ->  HandleForwardedRequest (refuses)
//	                     ->  AiForwardRouter.Dispatch  ->  cleanupInflight
//
// Only the peer transport is elided (Dispatch is wired directly to the
// receiver's `send`), because the hop is a proto round-trip that cannot change
// the property under test. The producer, the receiver's refusal path, the
// router's inflight table and its cleanup rule are all the real ones.
//
// The planner-side half -- that handlePlanPreempt supplies a principal the
// receiver will accept, so the steady state is not a refusal at all -- is
// asserted in integrations/planner/preempt_test.go.

// parentStreamHarness seeds an inflight entry for a parent turn and returns a
// `send` func that feeds whatever the receiver produces straight back into the
// router, exactly as the peer connection's inbound hook does.
func parentStreamHarness(t *testing.T) (*AiForwardRouter, chan *memqlv1.MemqlServerMessage, func(*nodev1.NodeServerMessage) error) {
	t.Helper()
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	router := NewAiForwardRouter(node.NewPeerManager(identity, testLogger()), testLogger())

	ch := make(chan *memqlv1.MemqlServerMessage, 4)
	router.mu.Lock()
	router.inflight["req-parent"] = &inflightEntry{respCh: ch}
	router.mu.Unlock()

	send := func(m *nodev1.NodeServerMessage) error {
		if resp := m.GetAiForwardResponse(); resp != nil {
			router.Dispatch(resp)
		}
		return nil
	}
	return router, ch, send
}

func parentStreamAlive(t *testing.T, router *AiForwardRouter, ch chan *memqlv1.MemqlServerMessage) {
	t.Helper()
	if !router.HasInflight("req-parent") {
		t.Error("the PARENT turn's inflight entry is gone after a refused continuation.\n\n" +
			"sendForwardError hardcodes done=true, Dispatch calls cleanupInflight on done, and " +
			"a continuation shares the parent's request_id -- so answering one ends the parent " +
			"turn while the agent keeps running. That is the live breakage: pausing a Plan in " +
			"cluster mode would kill the turn it was pausing.")
	}
	select {
	case _, open := <-ch:
		if !open {
			t.Error("the parent turn's response channel was CLOSED by a refused continuation")
		} else {
			t.Error("a refused continuation delivered a message on the parent's channel. Even " +
				"with done=false a QueryError there is treated as fatal for the turn by both " +
				"consumers, so the parent dies either way")
		}
	default:
	}
}

// Pausing a Plan: the exact payload handlePlanPreempt sends, refused because
// its authority cannot be proven. The parent turn must survive.
func TestARefusedPreemptContinuationLeavesTheParentTurnAlive(t *testing.T) {
	router, ch, send := parentStreamHarness(t)
	svc := &service{logger: testLogger()}

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId: "req-parent",
		MemqlEnvelope: envelopeBytes(t, &memqlv1.MemqlClientMessage_AgentPreemptTurn{
			AgentPreemptTurn: &memqlv1.AgentPreemptTurnMsg{RequestId: "req-parent"},
		}),
		Continuation: true,
		// No Authority: the rolling-deploy shape, and the one a producer that
		// forgets the contract would send.
	}, send)

	parentStreamAlive(t, router, ch)
}

// The same, with an assertion that is PRESENT but unprovable -- an empty
// credential class. A receiver that special-cased "no authority at all" and
// answered every other refusal would pass the test above and fail this one.
func TestARefusedContinuationWithABadAuthorityLeavesTheParentTurnAlive(t *testing.T) {
	router, ch, send := parentStreamHarness(t)
	svc := &service{logger: testLogger()}

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId: "req-parent",
		MemqlEnvelope: envelopeBytes(t, &memqlv1.MemqlClientMessage_ClientToolResult{
			ClientToolResult: &memqlv1.ClientToolResult{CallId: "call-1"},
		}),
		Continuation: true,
		Authority: &nodev1.ForwardedAuthority{
			ContractVersion: "v1",
			PrincipalKind:   nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_USER,
			Subject:         "v1:identity:user:u-1",
			Role:            "owner",
			CredentialClass: "", // the rule: absence is never read as "no badge"
		},
	}, send)

	parentStreamAlive(t, router, ch)
}

// A malformed ENVELOPE on a continuation is the case where the payload class
// cannot be read at all, so the wire flag is the only signal left. It must
// still not answer.
func TestAMalformedContinuationEnvelopeLeavesTheParentTurnAlive(t *testing.T) {
	router, ch, send := parentStreamHarness(t)
	svc := &service{logger: testLogger()}

	svc.HandleForwardedRequest(context.Background(), &nodev1.AiForwardRequest{
		RequestId:     "req-parent",
		MemqlEnvelope: []byte{0xff, 0xff, 0xff, 0xff},
		Continuation:  true,
	}, send)

	parentStreamAlive(t, router, ch)
}
