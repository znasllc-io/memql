package calls

import (
	"context"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
	"testing"
	"time"
)

func TestDelegationTaskScopeDatabasePinsScheduleAndRecipientBeforeEffects(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-task-scope")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	grant := delegationTestRequest(cw)
	grant.Scopes = append(grant.Scopes, "invite")
	grant.InvitationRecipients = []string{"guest@example.test"}
	grant.ScheduleStart = cw.clock.Now().Add(10 * time.Minute)
	grant.MaxCalls = 1
	credential, grantID := delegationTestGrant(t, cw, k, grant)
	quoted, err := cw.svc.Agent(ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, err)
	quote := quoted.(*callrules.CallQuote)
	request := callrules.AgentRequest{Action: "schedule", IdempotencyKey: "task-approved-booking", QuoteID: quote.QuoteID, Start: grant.ScheduleStart.Add(time.Minute)}
	_, err = cw.svc.Agent(ctx, cw.ref, credential, request)
	require.ErrorContains(t, err, "delegation_schedule_scope")
	request.Action = "start"
	_, err = cw.svc.Agent(ctx, cw.ref, credential, request)
	require.ErrorContains(t, err, "delegation_schedule_scope")
	require.Empty(t, cw.media.rooms())
	request.Action, request.Start = "schedule", grant.ScheduleStart
	result, err := cw.svc.Agent(ctx, cw.ref, credential, request)
	require.NoError(t, err)
	booking := result.(callrules.ScheduledCall)
	sender := &invitationMail{}
	svc := cw.svc.WithInvitationSender(func() email.Sender { return sender })
	invite := callrules.AgentRequest{Action: "invite", Invitation: &callrules.InviteRequest{Email: "other@example.test", Token: booking.GuestToken, BookingID: booking.ID}}
	_, err = svc.Agent(ctx, cw.ref, credential, invite)
	require.ErrorContains(t, err, "delegation_recipient_scope")
	require.Equal(t, 0, sender.count())
	invite.Invitation.Email = "GUEST@example.test"
	_, err = svc.Agent(ctx, cw.ref, credential, invite)
	require.NoError(t, err)
	require.Equal(t, 1, sender.count())
	// A caller retaining an authenticated internal session cannot bypass the late
	// native booking guard by avoiding the outer Agent dispatch/reservation path.
	d, pepper, err := cw.svc.agentAuthority(ctx, cw.ref, credential)
	require.NoError(t, err)
	sess, err := cw.svc.delegationSessionReady(ctx, cw.ref, d, pepper)
	require.NoError(t, err)
	late := context.WithValue(ctx, agentCallContext{}, grantID)
	_, err = cw.svc.Reschedule(late, sess, booking.ID, callrules.ScheduleRequest{Start: grant.ScheduleStart.Add(time.Hour), Minutes: 60, QuoteID: quote.QuoteID, IdempotencyKey: "task-late-moved-booking"})
	require.ErrorContains(t, err, "delegation_schedule_scope")
	stored, err := cw.svc.store.bookingByID(memql.ContextWithFreshRead(ctx), cw.owner, booking.ID)
	require.NoError(t, err)
	require.True(t, stored.Start.Equal(grant.ScheduleStart.Truncate(time.Second)))
	// Narrowing an approval is as effective as revocation for a request that
	// retained its native session before waiting for a gate.
	d, err = cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
	require.NoError(t, err)
	d.State.Request.ScheduleStart = time.Time{}
	d.State.Request.Scopes = []string{"read"}
	d.State.Request.InvitationRecipients = nil
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
	require.ErrorContains(t, cw.svc.Cancel(late, sess, booking.ID), "delegation_scope")
}

func TestDelegationTaskScopeDatabaseNarrowedHostAuthorityStopsRetainedSession(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-host-scope")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	credential, grantID := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	quoted, err := cw.svc.Agent(ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, err)
	_, err = cw.svc.Agent(ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", Name: "Agent", QuoteID: quoted.(*callrules.CallQuote).QuoteID, IdempotencyKey: "narrowed-host-room"})
	require.NoError(t, err)
	d, pepper, err := cw.svc.agentAuthority(ctx, cw.ref, credential)
	require.NoError(t, err)
	sess, err := cw.svc.delegationSessionReady(ctx, cw.ref, d, pepper)
	require.NoError(t, err)
	late := context.WithValue(ctx, agentCallContext{}, grantID)
	late = context.WithValue(late, agentScopeContext{}, "control")
	host, err := cw.svc.hostOf(late, sess)
	require.NoError(t, err)
	d.State.Request.Scopes = []string{"read"}
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
	require.ErrorContains(t, cw.svc.setLock(late, host, true), "delegation_scope")
	require.ErrorContains(t, cw.svc.Host(late, sess, callrules.HostActionRequest{Action: callrules.HostEnd}), "delegation_scope")
	room, err := cw.svc.store.roomByID(memql.ContextWithFreshRead(ctx), cw.owner, host.room.ID)
	require.NoError(t, err)
	require.True(t, room.open())
	require.False(t, room.Locked)
}

func TestProposalRejectsDecorativeScheduleAndUnapprovedParticipantMailbox(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	grant := callrules.DelegationRequest{Action: "create", Label: "Task", Terms: "test", Participation: "adults_only", Scopes: []string{"schedule", "invite"}, InvitationRecipients: []string{"guest@example.test"}, MaxCallCents: 1000, PeriodLimitCents: 1000, LifetimeLimitCents: 1000, MaxParticipants: 2, MaxDurationMinutes: 60, ExpiresAt: now.Add(24 * time.Hour), MaxCalls: 1}
	p := callrules.AgentProposalRequest{Label: "Interview", Grant: grant, Start: now.Add(time.Hour)}
	require.False(t, validProposal(p, now))
	p.Grant.ScheduleStart = p.Start
	p.Participants = []callrules.ProposalParticipant{{Label: "Guest", RoleID: "guest", Email: "other@example.test"}}
	require.False(t, validProposal(p, now))
	p.Participants[0].Email = "guest@example.test"
	require.True(t, validProposal(p, now))
}
