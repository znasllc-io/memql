package calls

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func spotlightAsk(t *testing.T, svc *Service, host callrules.Session, targetSeat, targetIdentity, nonce string) callrules.HostActionRequest {
	t.Helper()
	current, err := svc.SessionCall(context.Background(), host)
	require.NoError(t, err)
	require.NotNil(t, current.Spotlight)
	return callrules.HostActionRequest{Action: "spotlight", Spotlight: &callrules.SpotlightRequest{
		RoomID: current.RoomID, ActorIdentity: current.Identity, Revision: current.Spotlight.Revision,
		RequestID: nonce, Seat: targetSeat, Identity: targetIdentity,
	}}
}

func TestSpotlightHostAuthoritySharedProjectionAndIdempotency(t *testing.T) {
	cw := newCallWorld(t, "spotlight-authority", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	joined, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	req := spotlightAsk(t, cw.svc, host, guest.SeatID, joined.Identity, "spotlight-first-0001")
	require.ErrorContains(t, cw.svc.Host(ctx, guest, req), "not_host")
	wrong := *req.Spotlight
	wrong.ActorIdentity = "previous-host-admission"
	require.ErrorContains(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: "spotlight", Spotlight: &wrong}), "spotlight_admission_changed")
	_, other := cw.started("Other host")
	otherJoined, otherGuest, err := cw.join(other.Call.GuestToken, "Other guest")
	require.NoError(t, err)
	wrong = *req.Spotlight
	wrong.Seat, wrong.Identity = otherGuest.SeatID, otherJoined.Identity
	require.ErrorIs(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: "spotlight", Spotlight: &wrong}), callrules.ErrNotFound)
	require.NoError(t, cw.svc.Host(ctx, host, req))
	peer := allowanceReplica(t, cw)
	before := mustProcessingRoom(t, cw, started.Call.ID).Version
	for range 10 {
		require.NoError(t, peer.Host(ctx, host, req))
	}
	require.Equal(t, before, mustProcessingRoom(t, cw, started.Call.ID).Version, "replayed clicks must not append room versions")
	for _, actor := range []callrules.Session{host, guest} {
		view, e := peer.SessionCall(ctx, actor)
		require.NoError(t, e)
		require.Equal(t, &callrules.SpotlightParticipant{Seat: guest.SeatID, Identity: joined.Identity, DisplayName: "Guest"}, view.Spotlight.Participant)
	}
	same := spotlightAsk(t, peer, host, guest.SeatID, joined.Identity, "spotlight-same-selection")
	require.NoError(t, peer.Host(ctx, host, same))
	require.Equal(t, before, mustProcessingRoom(t, cw, started.Call.ID).Version)
	wrong = *req.Spotlight
	wrong.Identity = "different-target"
	require.ErrorContains(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: "spotlight", Spotlight: &wrong}), "idempotency_conflict")
	clear := spotlightAsk(t, peer, host, "", "", "spotlight-clear-0001")
	require.NoError(t, peer.Host(ctx, host, clear))
	require.ErrorContains(t, cw.svc.Host(ctx, host, req), "spotlight_changed", "old successful operation cannot undo the later clear")
	view, err := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Nil(t, view.Spotlight.Participant)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostLock}))
	wait, waiting, err := cw.join(started.Call.GuestToken, "Waiting guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, wait.Outcome)
	waitingView, err := cw.svc.SessionCall(ctx, waiting)
	require.NoError(t, err)
	require.Nil(t, waitingView.Spotlight, "waiting seats receive no shared participant projection")
	invalidTarget := spotlightAsk(t, peer, host, waiting.SeatID, "not-admitted", "spotlight-waiting-target")
	require.ErrorContains(t, peer.Host(ctx, host, invalidTarget), "spotlight_target_changed")
	require.Empty(t, cw.media.mutes())
	require.Empty(t, cw.media.removals())
	require.Empty(t, cw.media.deletions(), "spotlight must not alter provider permissions or membership")
}

func TestSpotlightDoesNotFollowTargetOrHostAcrossReentry(t *testing.T) {
	cw := newCallWorld(t, "spotlight-reentry", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	joined, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	req := spotlightAsk(t, cw.svc, host, guest.SeatID, joined.Identity, "spotlight-before-reentry")
	require.NoError(t, cw.svc.Host(ctx, host, req))
	require.NoError(t, cw.svc.Leave(ctx, guest))
	view, err := cw.svc.SessionCall(ctx, host)
	require.NoError(t, err)
	require.Nil(t, view.Spotlight.Participant)
	left, err := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Nil(t, left.Spotlight)
	back, guest, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: started.Call.GuestToken, Name: "Returned guest", Confirm: confirmed})
	require.NoError(t, err)
	require.NotEqual(t, joined.Identity, back.Identity)
	require.NoError(t, cw.svc.Host(ctx, host, req), "retry confirms its old effect without recreating it")
	view, err = cw.svc.SessionCall(ctx, host)
	require.NoError(t, err)
	require.Nil(t, view.Spotlight.Participant)
	stale := spotlightAsk(t, cw.svc, host, guest.SeatID, joined.Identity, "spotlight-stale-target")
	require.ErrorContains(t, cw.svc.Host(ctx, host, stale), "spotlight_target_changed")
	fresh := spotlightAsk(t, cw.svc, host, guest.SeatID, back.Identity, "spotlight-current-target")
	require.NoError(t, cw.svc.Host(ctx, host, fresh))
	oldHost := spotlightAsk(t, cw.svc, host, "", "", "spotlight-old-host-action")
	require.NoError(t, cw.svc.Leave(ctx, host))
	_, host, err = cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: started.Call.HostToken, Name: "Host returned", Confirm: confirmed})
	require.NoError(t, err)
	require.ErrorContains(t, cw.svc.Host(ctx, host, oldHost), "spotlight_admission_changed")
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostRemove, Seat: guest.SeatID}))
	view, err = cw.svc.SessionCall(ctx, host)
	require.NoError(t, err)
	require.Nil(t, view.Spotlight.Participant)
}

func TestSpotlightConcurrentEditsAndSharedMutationBudget(t *testing.T) {
	cw := newCallWorld(t, "spotlight-budget", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	joined, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	services := []*Service{cw.svc, allowanceReplica(t, cw)}
	requests := []callrules.HostActionRequest{
		spotlightAsk(t, cw.svc, host, guest.SeatID, joined.Identity, "spotlight-race-guest"),
		spotlightAsk(t, cw.svc, host, host.SeatID, started.Join.Identity, "spotlight-race-host"),
	}
	var errors [2]error
	var running sync.WaitGroup
	ready := make(chan struct{})
	for n, service := range services {
		running.Add(1)
		go func(n int, service *Service) {
			defer running.Done()
			<-ready
			errors[n] = service.Host(ctx, host, requests[n])
		}(n, service)
	}
	close(ready)
	running.Wait()
	wins := 0
	for _, err := range errors {
		if err == nil {
			wins++
		} else {
			require.ErrorContains(t, err, "spotlight_changed")
		}
	}
	require.Equal(t, 1, wins)
	for n := 1; n < spotlightChangesPerMinute; n++ {
		view, e := cw.svc.SessionCall(ctx, host)
		require.NoError(t, e)
		seat, identity := guest.SeatID, joined.Identity
		if view.Spotlight.Participant != nil && view.Spotlight.Participant.Seat == seat {
			seat, identity = host.SeatID, started.Join.Identity
		}
		req := spotlightAsk(t, cw.svc, host, seat, identity, fmt.Sprintf("spotlight-budget-%03d", n))
		require.NoError(t, services[n%2].Host(ctx, host, req))
	}
	req := spotlightAsk(t, cw.svc, host, "", "", "spotlight-budget-overflow")
	before := mustProcessingRoom(t, cw, started.Call.ID).Version
	require.ErrorContains(t, services[1].Host(ctx, host, req), "spotlight_rate_limited")
	require.Equal(t, before, mustProcessingRoom(t, cw, started.Call.ID).Version)
	cw.clock.Advance(time.Minute)
	require.NoError(t, services[0].Host(ctx, host, req))
}

func TestSpotlightAgentHostUsesSameProjectionAndControlScope(t *testing.T) {
	cw, key, _ := commerceFixture(t, "spotlight-agent")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, key)
	ctx := context.Background()
	token, grantID := delegationTestGrant(t, cw, key, delegationTestRequest(cw))
	quote, err := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, err)
	raw, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, token, callrules.AgentRequest{Action: "start", QuoteID: quote.(*callrules.CallQuote).QuoteID, Name: "Agent host", IdempotencyKey: "spotlight-agent-start"})
	require.NoError(t, err)
	start := raw.(callrules.StartResult)
	joined, guest, err := cw.join(start.Call.GuestToken, "Human guest")
	require.NoError(t, err)
	raw, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "session"})
	require.NoError(t, err)
	view := raw.(callrules.SeatCall)
	req := callrules.AgentRequest{Action: "host", Control: "spotlight", Spotlight: &callrules.SpotlightRequest{RoomID: view.RoomID, ActorIdentity: view.Identity, Revision: view.Spotlight.Revision, RequestID: "spotlight-agent-select", Seat: guest.SeatID, Identity: joined.Identity}}
	_, err = cw.svc.Agent(ctx, cw.ref, token, req)
	require.NoError(t, err)
	human, err := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, joined.Identity, human.Spotlight.Participant.Identity)
	raw, err = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "session"})
	require.NoError(t, err)
	require.Equal(t, human.Spotlight, raw.(callrules.SeatCall).Spotlight)
	d, err := cw.svc.store.delegation(ctx, cw.owner, grantID)
	require.NoError(t, err)
	d.State.Request.Scopes = []string{"read", "host"}
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
	_, err = cw.svc.Agent(ctx, cw.ref, token, req)
	require.ErrorContains(t, err, "delegation_scope")
}

func TestSpotlightLostCommittedResponseReplaysWithoutAnotherWrite(t *testing.T) {
	cw := newCallWorld(t, "spotlight-response-loss", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	joined, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	req := spotlightAsk(t, cw.svc, host, guest.SeatID, joined.Identity, "spotlight-lost-response")
	broken, fault := qualificationAfterCommit(cw, "updateCallsRoom(")
	require.Error(t, broken.Host(ctx, host, req))
	require.True(t, fault.failed)
	committed := mustProcessingRoom(t, cw, started.Call.ID).Version
	require.NoError(t, allowanceReplica(t, cw).Host(ctx, host, req))
	require.Equal(t, committed, mustProcessingRoom(t, cw, started.Call.ID).Version)
	view, err := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, joined.Identity, view.Spotlight.Participant.Identity)
}
