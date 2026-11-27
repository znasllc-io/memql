package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// A managed publisher uses a machine seat for admission accounting, but has no
// human-reviewed agent purpose. Its exact durable lease classifies that seat;
// stopping or expiring the lease must not change the session wire type.
func TestResourceSessionRestoresWithoutInventingAgentApproval(t *testing.T) {
	cw, owner, started, _ := resourceFixture(t, "resource-session-restore")
	if cw == nil {
		return
	}
	ctx := context.Background()
	joined, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	source := registerTestResource(t, cw, owner, "resource-session-registration")
	publication := workflowResourcePublish(t, cw, owner, started.Call.ID, source, "resource-session-publication")
	room, err := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, err)
	require.NotNil(t, room)
	d, err := cw.svc.store.resourceState(ctx, cw.owner, cw.site, room.KeyID)
	require.NoError(t, err)
	require.Len(t, d.State.Leases, 1)
	lease := d.State.Leases[0]
	require.Equal(t, publication.Identity, lease.Identity)
	replica := allowanceReplica(t, cw)
	assertRestorable := func(t *testing.T) {
		t.Helper()
		for _, sess := range []callrules.Session{*owner.result.Session, guest} {
			restored, e := replica.Session(ctx, cw.ref, sess.Token)
			require.NoError(t, e)
			require.NotEmpty(t, restored.SeatID)
			view, e := replica.SessionCall(ctx, restored)
			require.NoError(t, e)
			require.Equal(t, room.ID, view.RoomID)
			require.NotEmpty(t, view.Identity)
			require.NotEmpty(t, view.LeaveToken)
			require.Empty(t, view.Agents, "a resource must never claim missing agent approval metadata")
			if sess.Token == owner.result.Session.Token {
				require.True(t, view.Host)
				require.NotNil(t, view.Roster)
				require.Len(t, view.Roster.InCall, 3, "resource remains an admitted roster seat")
			} else {
				require.False(t, view.Host)
				require.Nil(t, view.Roster)
			}
		}
	}
	assertRestorable(t)
	// Queue remains the ordinary fresh-pass path after the session has restored.
	queued, err := replica.Queue(ctx, *owner.result.Session)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, queued.Outcome)
	require.Equal(t, started.Join.Identity, queued.Identity)
	require.NotEmpty(t, queued.Pass)
	for _, status := range []string{"preparing", "awaiting_source", "publishing", "revoking", "stopped"} {
		t.Run(status, func(t *testing.T) {
			d.State.Leases[0].Status = status
			require.NoError(t, cw.svc.store.saveResourceState(ctx, d, cw.clock.Now()))
			assertRestorable(t)
		})
	}
	cw.clock.Advance(callrules.ResourceLeaseLifetime + time.Second)
	assertRestorable(t)
}

func TestResourceSessionRequiresExactLeaseAndPreservesReviewedAgent(t *testing.T) {
	cw, owner, started, _ := resourceFixture(t, "resource-session-classification")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", agentAdmissionConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join"}
	token, _ := delegationTestGrant(t, cw, owner, grant)
	pending := requestGuestAgent(t, cw, token, started, "resource-session-agent-request")
	before, err := cw.svc.SessionCall(ctx, *owner.result.Session)
	require.NoError(t, err)
	require.Len(t, before.PendingAgents, 1)
	require.NoError(t, cw.svc.Host(ctx, *owner.result.Session, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: before.PendingAgents[0].ReviewDigest}))
	raw, err := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: started.Call.GuestToken, CallID: pending.CallID, RequestID: pending.RequestID, IdempotencyKey: "resource-session-agent-join", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, err)
	agent := raw.(callrules.JoinResult)
	require.Equal(t, callrules.OutcomeAdmit, agent.Outcome)
	before, err = cw.svc.SessionCall(ctx, *owner.result.Session)
	require.NoError(t, err)
	require.Len(t, before.Agents, 1)
	require.Equal(t, grant.Purpose, before.Agents[0].Purpose)
	require.Positive(t, before.Agents[0].ApprovalRevision)
	require.Equal(t, callrules.AgentConductVersion, before.Agents[0].ConductVersion)

	source := registerTestResource(t, cw, owner, "resource-session-classify-registration")
	publication := workflowResourcePublish(t, cw, owner, started.Call.ID, source, "resource-session-classify-publication")
	room, err := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, err)
	d, err := cw.svc.store.resourceState(ctx, cw.owner, cw.site, room.KeyID)
	require.NoError(t, err)
	require.Len(t, d.State.Leases, 1)
	lease := d.State.Leases[0]
	view, err := cw.svc.SessionCall(ctx, *owner.result.Session)
	require.NoError(t, err)
	require.Equal(t, before.Agents, view.Agents, "a real approved agent retains its complete metadata beside a resource")

	for _, field := range []string{"room", "seat", "identity"} {
		t.Run(field, func(t *testing.T) {
			d.State.Leases[0] = lease
			switch field {
			case "room":
				d.State.Leases[0].RoomID = "unrelated-room"
			case "seat":
				d.State.Leases[0].SeatID = "unrelated-seat"
			case "identity":
				d.State.Leases[0].Identity = "unrelated-identity"
			}
			require.NoError(t, cw.svc.store.saveResourceState(ctx, d, cw.clock.Now()))
			view, e := cw.svc.SessionCall(ctx, *owner.result.Session)
			require.NoError(t, e)
			require.Len(t, view.Agents, 2, "unmatched machine seats must not be silently excused from agent metadata validation")
			var unreviewed *callrules.AgentParticipant
			for i := range view.Agents {
				if view.Agents[i].Identity == publication.Identity {
					unreviewed = &view.Agents[i]
				}
			}
			require.NotNil(t, unreviewed)
			require.Empty(t, unreviewed.Purpose)
			require.Zero(t, unreviewed.ApprovalRevision)
		})
	}
	d.State.Leases[0] = lease
	require.NoError(t, cw.svc.store.saveResourceState(ctx, d, cw.clock.Now()))
	resourceSeat, err := cw.svc.store.seatByID(ctx, cw.owner, lease.SeatID)
	require.NoError(t, err)
	require.NotNil(t, resourceSeat)
	for _, field := range []string{"key", "room"} {
		t.Run("seat_"+field, func(t *testing.T) {
			// These immutable seat fields cannot be reassigned by updateCallsSeat.
			// Exercise classification with a malformed read independently of writes.
			candidate := *resourceSeat
			switch field {
			case "key":
				candidate.KeyID = "unrelated-owner-key"
			case "room":
				candidate.RoomID = "unrelated-room"
			}
			classified, e := cw.svc.sessionResourceSeats(ctx, *room, []seatRow{candidate})
			require.NoError(t, e)
			require.Empty(t, classified, "an unrelated seat cannot borrow an owner's lease classification")
		})
	}
}

func TestResourceSessionDelegatedPublisherIsNotAnotherReviewedAgent(t *testing.T) {
	cw, owner, _ := commerceFixture(t, "resource-session-delegated")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", resourceConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(ctx)
	})
	commerceVerified(t, cw, owner)
	provider := &resourceProviderFake{values: map[string]*ResourceIngress{}}
	installResourceFake(cw.svc, provider)
	source := registerTestResource(t, cw, owner, "resource-delegated-registration")
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "host", "resources_publish"}
	grant.ResourceIDs = []string{source.ID}
	token, _ := delegationTestGrant(t, cw, owner, grant)
	raw, err := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "quote", ParticipantLimit: 4, DurationMinutes: 30, Resources: true})
	require.NoError(t, err)
	quote := raw.(*callrules.CallQuote)
	raw, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, token, callrules.AgentRequest{Action: "start", QuoteID: quote.QuoteID, IdempotencyKey: "resource-delegated-room", Name: "Reviewed host assistant"})
	require.NoError(t, err)
	started := raw.(callrules.StartResult)
	require.Equal(t, callrules.OutcomeAdmit, started.Join.Outcome)
	raw, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "resource", Resource: &callrules.ResourceRequest{Action: "start", ResourceID: source.ID, RoomID: started.Call.ID, ExpectedRevision: source.Revision, RequestID: "resource-delegated-publication"}})
	require.NoError(t, err)
	publication := raw.(callrules.ResourceResult).Publication
	require.NotNil(t, publication)
	room, err := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, err)
	d, err := cw.svc.store.resourceState(ctx, cw.owner, cw.site, room.KeyID)
	require.NoError(t, err)
	require.Len(t, d.State.Leases, 1)
	lease := d.State.Leases[0]
	require.NotEmpty(t, lease.GrantID)
	resourceSeat, err := cw.svc.store.seatByID(ctx, cw.owner, lease.SeatID)
	require.NoError(t, err)
	require.NotNil(t, resourceSeat)
	require.NotEmpty(t, resourceSeat.AgentData, "resource creation inherits the delegated publisher's reviewed context")
	for _, status := range []string{"awaiting_source", "publishing", "revoking", "stopped"} {
		d.State.Leases[0].Status = status
		require.NoError(t, cw.svc.store.saveResourceState(ctx, d, cw.clock.Now()))
		raw, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "session"})
		require.NoError(t, err)
		view := raw.(callrules.SeatCall)
		require.True(t, view.Host)
		require.Len(t, view.Agents, 1, "delegated source %s is not a second reviewed agent", status)
		require.Equal(t, started.Join.Identity, view.Agents[0].Identity)
		require.Equal(t, grant.Purpose, view.Agents[0].Purpose)
		require.Positive(t, view.Agents[0].ApprovalRevision)
		require.Equal(t, callrules.AgentConductVersion, view.Agents[0].ConductVersion)
		require.Len(t, view.Roster.InCall, 2)
	}
}
