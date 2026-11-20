package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestSessionCallKeepsTheRoomStartAcrossLateJoinsAndReconnects(t *testing.T) {
	cw := newCallWorld(t, "session-room-start", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	opened := rowTime(cw.row(roomConcept, started.Call.ID), "openedAt")
	require.False(t, opened.IsZero())
	require.Equal(t, opened, cw.sessionCall(host).OpenedAt)
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity))
	cw.clock.Advance(2 * time.Minute)
	joined, guest, err := cw.join(started.Call.GuestToken, "Late guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	require.Equal(t, opened, cw.sessionCall(guest).OpenedAt)
	require.True(t, rowTime(cw.row(seatConcept, guest.SeatID), "joinedAt").After(opened))
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(joined.Identity))

	// A second service instance and a resumed media pass read the same stored
	// room start. Neither local service state nor the new join time defines it.
	peer := secondReplica(t, cw.eng).Service()
	peer.now, peer.media = cw.clock.Now, cw.media.backend
	cw.clock.Advance(30 * time.Second)
	back, resumed, err := peer.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Reconnected", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
	require.Equal(t, guest.SeatID, resumed.SeatID)
	view, err := peer.SessionCall(ctx, resumed)
	require.NoError(t, err)
	require.Equal(t, opened, view.OpenedAt)

	wrongSite := guest
	wrongSite.Site.SiteID = "another-site"
	view, err = peer.SessionCall(ctx, wrongSite)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	require.True(t, view.OpenedAt.IsZero())
	// Client-supplied seat/room ids cannot select a different room's start.
	forged := guest
	forged.SeatID, forged.RoomID = "another-seat", "another-room"
	view, err = peer.SessionCall(ctx, forged)
	require.NoError(t, err)
	require.Equal(t, opened, view.OpenedAt)

	require.NoError(t, cw.svc.Leave(ctx, guest))
	require.True(t, cw.sessionCall(guest).OpenedAt.IsZero())
	back, _, err = cw.rejoin(guest, "")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
	require.Equal(t, opened, cw.sessionCall(guest).OpenedAt)
	require.NoError(t, cw.act(host, callrules.HostLock, ""))
	waits, waiting, err := cw.join(started.Call.GuestToken, "Waiting guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	require.True(t, cw.sessionCall(waiting).OpenedAt.IsZero())
	require.NoError(t, cw.act(host, callrules.HostRemove, guest.SeatID))
	require.True(t, cw.sessionCall(guest).OpenedAt.IsZero())
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	require.True(t, cw.sessionCall(host).OpenedAt.IsZero())
	require.True(t, cw.sessionCall(waiting).OpenedAt.IsZero())
}
