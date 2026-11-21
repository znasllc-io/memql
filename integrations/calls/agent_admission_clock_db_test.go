package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestAgentGuestAdmissionUsesPersistedTimePrecision(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "waiting"}[waiting], func(t *testing.T) {
			cw, host, token, _, room := agentGuestFixture(t, "agent-clock-precision")
			if cw == nil {
				return
			}
			ctx := context.Background()
			cw.clock.Advance(123 * time.Nanosecond)
			pending := requestGuestAgent(t, cw, token, room, "clock-review-request-0001")
			join := callrules.AgentRequest{Action: "guest_join", GuestToken: room.Call.GuestToken, CallID: room.Call.ID, RequestID: pending.RequestID, IdempotencyKey: "clock-agent-admission-0001", Acceptance: acceptedAgent(t, cw, token)}
			_, e := cw.svc.Agent(ctx, cw.ref, token, join)
			require.ErrorContains(t, e, "agent_review_required")
			view, e := cw.svc.SessionCall(ctx, *host.result.Session)
			require.NoError(t, e)
			require.NoError(t, cw.svc.Host(ctx, *host.result.Session, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: view.PendingAgents[0].ReviewDigest}))
			bad := join
			wrong := *join.Acceptance
			wrong.Terms = "outdated"
			bad.Acceptance = &wrong
			bad.IdempotencyKey = "clock-agent-wrong-assent-0001"
			_, e = cw.svc.Agent(ctx, cw.ref, token, bad)
			require.ErrorContains(t, e, "agent_acceptance_required")
			if waiting {
				require.NoError(t, cw.svc.Host(ctx, *host.result.Session, callrules.HostActionRequest{Action: callrules.HostLock}))
			}
			raw, e := cw.svc.Agent(ctx, cw.ref, token, join)
			require.NoError(t, e)
			admitted := raw.(callrules.JoinResult)
			if waiting {
				require.Equal(t, callrules.OutcomeWait, admitted.Outcome)
				view, e = cw.svc.SessionCall(ctx, *host.result.Session)
				require.NoError(t, e)
				require.Len(t, view.Roster.Waiting, 1)
				require.NoError(t, cw.svc.Host(ctx, *host.result.Session, callrules.HostActionRequest{Action: callrules.HostAdmit, Seat: view.Roster.Waiting[0].Seat}))
				cw.clock.Advance(321 * time.Nanosecond)
				raw, e = cw.svc.Agent(ctx, cw.ref, token, join)
				require.NoError(t, e)
				admitted = raw.(callrules.JoinResult)
			}
			require.Equal(t, callrules.OutcomeAdmit, admitted.Outcome)
			require.NotEmpty(t, admitted.Pass)
			refreshed, e := cw.svc.Agent(ctx, cw.ref, token, join)
			require.NoError(t, e)
			require.Equal(t, admitted.Identity, refreshed.(callrules.JoinResult).Identity)
		})
	}
}
