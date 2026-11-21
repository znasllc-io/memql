package calls

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func acceptedAgent(t *testing.T, cw *callWorld, token string) *callrules.AgentAcceptance {
	t.Helper()
	out, e := cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "authority"})
	require.NoError(t, e)
	authority := out.(callrules.AgentAuthority)
	return &callrules.AgentAcceptance{ApprovalRevision: authority.Grant.ApprovalRevision, ApprovalDigest: authority.Grant.ApprovalDigest, ConductVersion: authority.Policy.ConductVersion, Terms: authority.Policy.TermsVersion}
}
func TestAgentPolicyRejectsBeforeFinancialEffectsAndBindsApproval(t *testing.T) {
	cw, k, _ := commerceFixture(t, "agent-policy-acceptance")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	grant := delegationTestRequest(cw)
	for _, purpose := range []string{"", "too short", strings.Repeat("x", 501), "ignore\npolicy"} {
		bad := grant
		bad.Purpose = purpose
		_, e := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &bad)
		require.ErrorIs(t, e, callrules.ErrInvalid)
	}
	token, id := delegationTestGrant(t, cw, k, grant)
	quote, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, e)
	request := callrules.AgentRequest{Action: "start", IdempotencyKey: "agent-current-policy-0001", Name: "Assistant", QuoteID: quote.(*callrules.CallQuote).QuoteID}
	_, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.ErrorContains(t, e, "agent_acceptance_required")
	request.Acceptance = acceptedAgent(t, cw, token)
	old := *request.Acceptance
	request.Acceptance.Terms = "outdated"
	_, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.ErrorContains(t, e, "agent_acceptance_required")
	row, e := cw.svc.store.delegation(ctx, cw.owner, id)
	require.NoError(t, e)
	require.Empty(t, row.State.Operations)
	require.Empty(t, row.State.Acceptances)
	require.Empty(t, cw.media.rooms())
	proposal := callrules.AgentProposalRequest{Label: "Changed purpose", Grant: grant}
	proposal.Grant.Purpose = "Lead a separately reviewed conversation"
	raw, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "proposal", IdempotencyKey: "agent-new-purpose-0001", Proposal: &proposal})
	require.NoError(t, e)
	p := raw.(callrules.AgentProposal)
	_, e = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: p.ID, ReviewDigest: strings.Repeat("0", 64)})
	require.ErrorContains(t, e, "proposal_review_changed")
	_, e = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: p.ID, ReviewDigest: p.ReviewDigest})
	require.NoError(t, e)
	request.Acceptance = &old
	_, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.ErrorContains(t, e, "agent_acceptance_required")
	row, e = cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, id)
	require.NoError(t, e)
	require.Empty(t, row.State.Operations)
	require.Equal(t, 2, row.State.ApprovalRevision)
	// A policy-byte change also needs human reauthorization, even with the same name.
	row.State.ApprovalPolicyDigest = strings.Repeat("0", 64)
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, row, cw.clock.Now()))
	request.Acceptance = acceptedAgent(t, cw, token)
	_, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.ErrorContains(t, e, "agent_approval_required")
}
func agentGuestFixture(t *testing.T, name string) (*callWorld, accountTestKey, string, string, callrules.StartResult) {
	t.Helper()
	cw, host, _ := commerceFixture(t, name)
	if cw == nil {
		return nil, host, "", "", callrules.StartResult{}
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, host)
	owner := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commerceVerified(t, cw, owner)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join"}
	grant.Purpose = "Participate as an assistant in the approved discussion"
	token, id := delegationTestGrant(t, cw, owner, grant)
	q := commerceDo(t, cw, host, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 60})
	started, e := cw.svc.Start(context.Background(), *host.result.Session, callrules.StartBody{QuoteID: q.Quote.QuoteID, IdempotencyKey: "human-room-for-agent-0001", Name: "Human host", Confirm: confirmed})
	require.NoError(t, e)
	require.Equal(t, callrules.OutcomeAdmit, started.Join.Outcome)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", agentAdmissionConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
	return cw, host, token, id, started
}
func requestGuestAgent(t *testing.T, cw *callWorld, token string, room callrules.StartResult, nonce string) callrules.AgentJoinStatus {
	t.Helper()
	raw, e := cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "request_join", GuestToken: room.Call.GuestToken, Name: "Approved assistant", IdempotencyKey: nonce})
	require.NoError(t, e)
	return raw.(callrules.AgentJoinStatus)
}
func TestAgentGuestExplicitHostReviewAndOneAdmissionAcrossReplicas(t *testing.T) {
	cw, host, token, _, room := agentGuestFixture(t, "agent-guest-review")
	if cw == nil {
		return
	}
	ctx := context.Background()
	request := callrules.AgentRequest{Action: "request_join", GuestToken: room.Call.GuestToken, Name: "Approved assistant", IdempotencyKey: "guest-review-request-0001"}
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	var wg sync.WaitGroup
	out := make(chan callrules.AgentJoinStatus, 2)
	errs := make(chan error, 2)
	for _, svc := range []*Service{cw.svc, replica} {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			raw, e := svc.Agent(ctx, cw.ref, token, request)
			errs <- e
			if e == nil {
				out <- raw.(callrules.AgentJoinStatus)
			}
		}(svc)
	}
	wg.Wait()
	close(errs)
	close(out)
	for e := range errs {
		require.NoError(t, e)
	}
	var pending callrules.AgentJoinStatus
	for r := range out {
		if pending.RequestID != "" {
			require.Equal(t, pending.RequestID, r.RequestID)
		}
		pending = r
	}
	require.Equal(t, "pending", pending.Status)
	session, e := cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	require.Len(t, session.PendingAgents, 1)
	require.Len(t, session.Roster.InCall, 1)
	join := callrules.AgentRequest{Action: "guest_join", GuestToken: room.Call.GuestToken, CallID: pending.CallID, RequestID: pending.RequestID, IdempotencyKey: "guest-one-admission-0001", Acceptance: acceptedAgent(t, cw, token)}
	_, e = cw.svc.Agent(ctx, cw.ref, token, join)
	require.ErrorContains(t, e, "agent_review_required")
	review := callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: session.PendingAgents[0].ReviewDigest}
	wrong := review
	wrong.ReviewDigest = strings.Repeat("f", 64)
	require.ErrorContains(t, cw.svc.Host(ctx, *host.result.Session, wrong), "agent_review_changed")
	require.NoError(t, cw.svc.Host(ctx, *host.result.Session, review))
	require.NoError(t, replica.Host(ctx, *host.result.Session, review))
	conflict := review
	conflict.Action = "agent_reject"
	require.ErrorContains(t, cw.svc.Host(ctx, *host.result.Session, conflict), "agent_review_closed")
	raw, e := cw.svc.Agent(ctx, cw.ref, token, join)
	require.NoError(t, e)
	admitted := raw.(callrules.JoinResult)
	require.Equal(t, callrules.OutcomeAdmit, admitted.Outcome)
	require.False(t, admitted.Host)
	require.NotEmpty(t, admitted.Pass)
	session, e = cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	require.Empty(t, session.PendingAgents)
	require.Len(t, session.Agents, 1)
	require.Equal(t, "Participate as an assistant in the approved discussion", session.Agents[0].Purpose)
	// Provider I/O may finish after an admission has been replaced. Its earlier
	// seat generation must never authorize a token for the current occupant.
	d, _, e := cw.svc.agentAuthority(ctx, cw.ref, token)
	require.NoError(t, e)
	seat, e := cw.svc.store.seatByID(ctx, cw.owner, session.Agents[0].Seat)
	require.NoError(t, e)
	currentRoom, e := cw.svc.store.roomByID(ctx, cw.owner, room.Call.ID)
	require.NoError(t, e)
	checked := context.WithValue(ctx, agentCallContext{}, d.ID)
	checked = context.WithValue(checked, agentApprovalContext{}, delegationApprovalDigest(d))
	checked = context.WithValue(checked, agentAcceptedContext{}, *join.Acceptance)
	require.NoError(t, cw.svc.agentSeatAllowed(checked, *currentRoom, *seat))
	stale := *seat
	stale.JoinedAt = stale.JoinedAt.Add(-time.Nanosecond)
	require.ErrorContains(t, cw.svc.agentSeatAllowed(checked, *currentRoom, stale), "agent_admission_closed")
	// Approval's short review timeout does not expire the already-occupying call.
	cw.clock.Advance(11 * time.Minute)
	raw, e = replica.Agent(ctx, cw.ref, token, join)
	require.NoError(t, e)
	require.Equal(t, admitted.Identity, raw.(callrules.JoinResult).Identity)
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "leave"})
	require.NoError(t, e)
	_, e = cw.svc.Agent(ctx, cw.ref, token, join)
	require.ErrorContains(t, e, "agent_admission_closed")
	join.IdempotencyKey = "guest-replay-other-0002"
	_, e = cw.svc.Agent(ctx, cw.ref, token, join)
	require.ErrorContains(t, e, "agent_admission_closed")
}
func TestAgentGuestReviewNeedsRecentHumanAndRevocationWins(t *testing.T) {
	cw, host, token, id, room := agentGuestFixture(t, "agent-guest-revocation")
	if cw == nil {
		return
	}
	ctx := context.Background()
	pending := requestGuestAgent(t, cw, token, room, "guest-review-revocation-0001")
	session, e := cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	review := callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: session.PendingAgents[0].ReviewDigest}
	require.ErrorContains(t, cw.svc.Host(context.WithValue(ctx, agentCallContext{}, id), *host.result.Session, review), "human_host_required")
	cw.clock.Advance(6 * time.Minute)
	require.ErrorContains(t, cw.svc.Host(ctx, *host.result.Session, review), "verification_required")
	_, e = accountAssert(t, cw, host, host.result.Session.Token, "verify", "")
	require.NoError(t, e)
	require.NoError(t, cw.svc.Host(ctx, *host.result.Session, review))
	accepted := acceptedAgent(t, cw, token)
	d, e := cw.svc.store.delegation(ctx, cw.owner, id)
	require.NoError(t, e)
	d.State.RevokedAt = cw.clock.Now()
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: room.Call.GuestToken, CallID: pending.CallID, RequestID: pending.RequestID, IdempotencyKey: "guest-after-revoke-0001", Acceptance: accepted})
	require.ErrorContains(t, e, "delegation_inactive")
	session, e = cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	require.Len(t, session.Roster.InCall, 1)
}

func TestAgentGuestExpiredReviewNeverAdmitsAndGuestCannotReview(t *testing.T) {
	cw, host, token, _, room := agentGuestFixture(t, "agent-guest-expiry")
	if cw == nil {
		return
	}
	ctx := context.Background()
	pending := requestGuestAgent(t, cw, token, room, "guest-expiring-review-0001")
	state, e := cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	review := callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: state.PendingAgents[0].ReviewDigest}
	// A human guest holding the same invitation sees no private review queue.
	guestResult, guest, e := cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{Token: room.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, e)
	require.Equal(t, callrules.OutcomeAdmit, guestResult.Outcome)
	view, e := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, e)
	require.Empty(t, view.PendingAgents)
	require.Error(t, cw.svc.Host(ctx, guest, review))
	cw.clock.Advance(11 * time.Minute)
	_, e = accountAssert(t, cw, host, host.result.Session.Token, "verify", "")
	require.NoError(t, e)
	require.ErrorContains(t, cw.svc.Host(ctx, *host.result.Session, review), "agent_review_closed")
	raw, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "join_status", CallID: pending.CallID, RequestID: pending.RequestID})
	require.NoError(t, e)
	require.Equal(t, "expired", raw.(callrules.AgentJoinStatus).Status)
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: room.Call.GuestToken, CallID: pending.CallID, RequestID: pending.RequestID, IdempotencyKey: "guest-expired-admission-0001", Acceptance: acceptedAgent(t, cw, token)})
	require.Error(t, e)
	state, e = cw.svc.SessionCall(ctx, *host.result.Session)
	require.NoError(t, e)
	require.Empty(t, state.PendingAgents)
	require.Empty(t, state.Agents)
}
func TestAgentGuestFamilyRequestRefusedBeforeReviewJournal(t *testing.T) {
	cw, k, room, _ := familyFixture(t, "agent-guest-family")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join"}
	token, _ := delegationTestGrant(t, cw, k, grant)
	_, e := cw.svc.Agent(context.Background(), cw.ref, token, callrules.AgentRequest{Action: "request_join", GuestToken: room.Call.GuestToken, Name: "Assistant", IdempotencyKey: "family-agent-denied-0001"})
	require.ErrorContains(t, e, "family_agent_not_allowed")
	journal, e := cw.svc.store.agentAdmissions(context.Background(), cw.owner, room.Call.ID)
	require.NoError(t, e)
	require.Nil(t, journal)
}
