package calls

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func recoverInvitation(t *testing.T, service *Service, session callrules.Session, identity string) string {
	t.Helper()
	result, err := service.InvitationLink(context.Background(), session, callrules.InvitationLinkRequest{Identity: identity})
	require.NoError(t, err)
	require.True(t, callrules.TokenWellFormed(result.GuestToken))
	return result.GuestToken
}

func TestInvitationLinkConcurrentReplicasRetainOnePrivateAliasAndOriginalLinks(t *testing.T) {
	cw := newCallWorld(t, "link-replicas", 20)
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	peer := secondReplica(t, cw.eng).Service()
	peer.now, peer.media = cw.clock.Now, cw.media.backend
	beforeRoom := cw.row(roomConcept, start.Call.ID)
	const count = 8
	results := make([]callrules.InvitationLinkResult, count)
	errs := make([]error, count)
	var group sync.WaitGroup
	for i := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			service := cw.svc
			if i%2 == 1 {
				service = peer
			}
			results[i], errs[i] = service.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: start.Join.Identity})
		}()
	}
	group.Wait()
	token := results[0].GuestToken
	require.True(t, callrules.TokenWellFormed(token))
	for i := range count {
		require.NoError(t, errs[i])
		require.Equal(t, token, results[i].GuestToken)
	}
	require.NotEqual(t, start.Call.GuestToken, token)
	require.NotEqual(t, start.Call.HostToken, token)
	digest := callrules.Digest(cw.pepper(), []byte(token))
	require.Len(t, cw.rowsOf(invitationLinkConcept), 1)
	require.Equal(t, 1, cw.versions(invitationLinkConcept, digest), "recovery must not write on every click or replica")
	require.Equal(t, beforeRoom, cw.row(roomConcept, start.Call.ID), "recovery changed a room or its original links")
	require.Zero(t, cw.rowsContaining(token), "a row, journal or provenance stored the plaintext alias")
	joined, _, err := peer.Join(context.Background(), cw.ref, nil, callrules.JoinBody{Token: token, Name: "Recovered guest", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	require.False(t, joined.Host)
	original, _, err := cw.join(start.Call.GuestToken, "Original guest")
	require.NoError(t, err)
	require.False(t, original.Host)
	cohost, session, err := cw.joinBy(callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Cohost"})
	require.NoError(t, err)
	require.True(t, cohost.Host)
	require.Equal(t, token, recoverInvitation(t, peer, session, cohost.Identity))
	_, _, err = cw.joinBy(callrules.JoinBody{HostToken: token, Name: "Forged host"})
	require.ErrorIs(t, err, callrules.ErrNotFound)
}

func TestInvitationLinkRefusesEveryNonCurrentHostAndStalePage(t *testing.T) {
	cw := newCallWorld(t, "link-host-check", 20)
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	guestJoin, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	forged := guest
	forged.Kind, forged.SeatID, forged.RoomID = callrules.SessionHost, host.SeatID, host.RoomID
	otherSite := cw.ref
	otherSite.SiteID = "other-site"
	wrongSite := host
	wrongSite.Site = otherSite
	for _, test := range []struct {
		session  callrules.Session
		identity string
		want     error
	}{
		{guest, guestJoin.Identity, callrules.ErrForbidden},
		{forged, start.Join.Identity, callrules.ErrForbidden},
		{host, guestJoin.Identity, callrules.ErrForbidden},
		{host, strings.Repeat("z", 16), callrules.ErrForbidden},
		{wrongSite, start.Join.Identity, callrules.ErrNoSession},
		{cw.host(), start.Join.Identity, callrules.ErrForbidden},
	} {
		result, err := cw.svc.InvitationLink(context.Background(), test.session, callrules.InvitationLinkRequest{Identity: test.identity})
		require.ErrorIs(t, err, test.want)
		require.Empty(t, result.GuestToken)
	}
	require.Empty(t, cw.rowsOf(invitationLinkConcept))
	require.NoError(t, cw.svc.Host(context.Background(), host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	_, err = cw.svc.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: start.Join.Identity})
	require.ErrorIs(t, err, callrules.ErrEnded)
	// The same cookie starts another call. A request from its old page must not
	// recover that newer room even though its session is again a valid host.
	next, err := cw.svc.Start(context.Background(), host, callrules.StartBody{Name: "Next", Confirm: confirmed})
	require.NoError(t, err)
	_, err = cw.svc.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: start.Join.Identity})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	require.Empty(t, cw.rowsOf(invitationLinkConcept))
	recoverInvitation(t, cw.svc, host, next.Join.Identity)
}

func TestInvitationLinkRefusesWaitingRemovedLeftAndExpiredHosts(t *testing.T) {
	for _, state := range []string{"waiting", "removed", "left", "expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			cw := newCallWorld(t, "link-"+state, 20)
			if cw == nil {
				return
			}
			host, start := cw.started("Host")
			want := callrules.ErrForbidden
			switch state {
			case "expired":
				cw.clock.Advance(callrules.SessionLifetime + time.Second)
				want = callrules.ErrNoSession
			case "revoked":
				row, _, err := cw.svc.session(context.Background(), cw.ref, host.Token, cw.clock.Now())
				require.NoError(t, err)
				cw.asOwnerMutate(sessionConcept, row.ID, "updateCallsSession", map[string]any{"sessionId": row.ID, "endedAt": cw.clock.Now()})
				want = callrules.ErrNoSession
			default:
				cw.asOwnerMutate(seatConcept, host.SeatID, "updateCallsSeat", map[string]any{"seatId": host.SeatID, "status": state})
			}
			result, err := cw.svc.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: start.Join.Identity})
			require.ErrorIs(t, err, want)
			require.Empty(t, result.GuestToken)
			require.Empty(t, cw.rowsOf(invitationLinkConcept))
		})
	}
}

func TestInvitationLinkRechecksSessionAfterWaitingForRoomGate(t *testing.T) {
	cw := newCallWorld(t, "link-expire-gated", 20)
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	held, err := memql.AcquireWriteGates(context.Background(), sharedBoot.gates, roomGate(start.Call.ID))
	require.NoError(t, err)
	release := sync.OnceFunc(held)
	t.Cleanup(release)
	entered := make(chan struct{})
	service := *cw.svc
	service.gateDB = func() *sql.DB { close(entered); return sharedBoot.gates }
	done := make(chan error, 1)
	go func() {
		_, err := service.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: start.Join.Identity})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not reach the room gate")
	}
	cw.clock.Advance(callrules.SessionLifetime + time.Second)
	release()
	select {
	case err := <-done:
		require.ErrorIs(t, err, callrules.ErrNoSession)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not finish")
	}
	require.Empty(t, cw.rowsOf(invitationLinkConcept))
}

func TestInvitationLinkFollowsHostHandoverAndNeverCrossesSites(t *testing.T) {
	cw := newCallWorld(t, "link-handover", 20)
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	token := recoverInvitation(t, cw.svc, host, start.Join.Identity)
	joined, holder := cw.keyHolderJoin(start.Call.GuestToken, "Next host")
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(joined.Identity))
	require.NoError(t, cw.svc.Leave(context.Background(), host))
	cw.reconcile()
	require.True(t, cw.sessionCall(holder).Host)
	require.Equal(t, token, recoverInvitation(t, cw.svc, holder, joined.Identity))
	_, err := cw.svc.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: start.Join.Identity})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	other := cw.ref
	other.SiteID = "another-site"
	_, _, err = cw.svc.Join(context.Background(), other, nil, callrules.JoinBody{Token: token, Name: "Wrong site", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrNotFound)
	other = cw.ref
	other.OwnerUserID = "another-owner"
	_, _, err = cw.svc.Join(context.Background(), other, nil, callrules.JoinBody{Token: token, Name: "Wrong owner", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrNotFound)
}

func TestInvitationLinkForScheduledRoomEndsWithTheRoom(t *testing.T) {
	cw := newCallWorld(t, "link-scheduled", 20)
	if cw == nil {
		return
	}
	booking := cw.booked(cw.host(), time.Minute, 30, "Booked call")
	cw.advanceTo(booking.Start)
	joined, host, err := cw.joinBy(callrules.JoinBody{HostToken: booking.HostToken, Name: "Booked host"})
	require.NoError(t, err)
	token := recoverInvitation(t, cw.svc, host, joined.Identity)
	guest, _, err := cw.join(token, "Alias guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, guest.Outcome, "recovery must not bypass a booked call's lock")
	_, _, err = cw.join(booking.GuestToken, "Original guest")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Host(context.Background(), host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	_, _, err = cw.join(token, "Too late")
	require.ErrorIs(t, err, callrules.ErrEnded)
	_, err = cw.svc.InvitationLink(context.Background(), host, callrules.InvitationLinkRequest{Identity: joined.Identity})
	require.ErrorIs(t, err, callrules.ErrEnded)
}

func TestInvitationLinkUsesTheSameEmailReceiptAndTrustedTemplate(t *testing.T) {
	cw, service, sender := invitationWorld(t, "link-email")
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	token := recoverInvitation(t, service, host, start.Join.Identity)
	first, err := service.Invite(context.Background(), host, callrules.InviteRequest{Email: "same@example.test", Token: start.Call.GuestToken})
	require.NoError(t, err)
	require.Equal(t, "sent", first.Status)
	duplicate, err := service.Invite(context.Background(), host, callrules.InviteRequest{Email: "same@example.test", Token: token})
	require.NoError(t, err)
	require.True(t, duplicate.Duplicate)
	require.Equal(t, 1, sender.count())
	_, err = service.Invite(context.Background(), host, callrules.InviteRequest{Email: "other@example.test", Token: token})
	require.NoError(t, err)
	require.Equal(t, 2, sender.count())
	require.Contains(t, sender.messages[1].TextBody, "#j="+token)
	require.NotContains(t, sender.messages[1].TextBody+sender.messages[1].HTMLBody, start.Call.HostToken)
	require.Zero(t, cw.rowsContaining(token))
}
