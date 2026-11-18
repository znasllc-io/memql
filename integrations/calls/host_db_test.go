package calls

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/core/id"
)

// host_db_test.go -- the host controls, the host-left countdown and the empty-room rule
// (host.go), against a real engine and database. For service_db_test.go's reason the
// media server is the one fake, and here it also shows who is connected to each media
// room (fakeMedia.setPresent), as a media server's participant list would.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the skip
// into a failure.

// act is one host control made with a session.
func (cw *callWorld) act(sess callrules.Session, action, seat string) error {
	cw.t.Helper()
	return cw.svc.Host(context.Background(), sess, callrules.HostActionRequest{Action: action, Seat: seat})
}

// keyHolderJoin is a person holding a key of their own who joins a call by its guest
// link with the session that key opened: a key holder in the call, who may take over
// as its host.
func (cw *callWorld) keyHolderJoin(link, name string) (callrules.JoinResult, callrules.Session) {
	cw.t.Helper()
	sess := cw.host()
	res, out, err := cw.svc.Join(context.Background(), cw.ref, &sess, callrules.JoinBody{Token: link, Name: name, Confirm: confirmed})
	require.NoError(cw.t, err)
	return res, out
}

// entryOf is a seat's newest queue entry, by when it was enqueued.
func (cw *callWorld) entryOf(seatID string) map[string]any {
	cw.t.Helper()
	var newest map[string]any
	for _, entry := range cw.rowsOf(queueEntryConcept) {
		if bare(entry["seatId"]) != seatID {
			continue
		}
		if newest == nil || rowTime(entry, "enqueuedAt").After(rowTime(newest, "enqueuedAt")) {
			newest = entry
		}
	}
	require.NotNil(cw.t, newest, "the seat %s has no queue entry", seatID)
	return newest
}

// reconcile is one reconciliation pass, which must not fail.
func (cw *callWorld) reconcile() {
	cw.t.Helper()
	require.NoError(cw.t, cw.svc.Reconcile(context.Background()))
}

// ---------------------------------------------------------------------------
// who may act
// ---------------------------------------------------------------------------

// TestOnlyTheHostSeatMayAct: a host control is made by a host seat that is in the call
// now -- the key holder who started it, or anyone holding its host link -- and by
// nobody else: a guest, a session seated nowhere and a host who has left are refused
// and change nothing. A seat of another call is not found, an unknown control or one
// that names no seat is invalid, and an ended call takes no control at all. The track
// controls reach exactly the matching sources of the seat's tracks on the media server,
// and a seat with none of them is no failure.
func TestOnlyTheHostSeatMayAct(t *testing.T) {
	cw := newCallWorld(t, "hostonly", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	in, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)

	// A guest is no host: every control is refused, and nothing moved.
	for _, req := range []callrules.HostActionRequest{
		{Action: callrules.HostMute, Seat: hostSess.SeatID}, {Action: callrules.HostCameraOff, Seat: hostSess.SeatID},
		{Action: callrules.HostStopShare, Seat: hostSess.SeatID}, {Action: callrules.HostRemove, Seat: hostSess.SeatID},
		{Action: callrules.HostAdmit, Seat: guest.SeatID}, {Action: callrules.HostLock}, {Action: callrules.HostUnlock},
		{Action: callrules.HostEnd},
	} {
		requireReason(t, cw.svc.Host(ctx, guest, req), callrules.ErrForbidden, callrules.ReasonNotHost)
	}
	room := cw.row(roomConcept, roomID)
	require.Equal(t, "open", room["status"])
	require.NotEqual(t, true, room["locked"])
	require.Equal(t, "admitted", cw.row(seatConcept, hostSess.SeatID)["status"])
	require.Empty(t, cw.media.mutes(), "a guest's control reached the media server")
	require.Empty(t, cw.media.deletions())
	require.Empty(t, cw.media.removals())

	// A key holder's session seated nowhere hosts nothing, and no session is none.
	requireReason(t, cw.act(cw.host(), callrules.HostLock, ""), callrules.ErrForbidden, callrules.ReasonNotHost)
	require.ErrorIs(t, cw.act(callrules.Session{Token: "not-a-token", Site: cw.ref}, callrules.HostLock, ""), callrules.ErrNoSession)

	// The host's malformed requests, and seats that are not in the host's call.
	require.ErrorIs(t, cw.act(hostSess, "kick", guest.SeatID), callrules.ErrInvalid)
	require.ErrorIs(t, cw.act(hostSess, callrules.HostMute, ""), callrules.ErrInvalid, "a control done to a seat named none")
	_, other := cw.started("Other host")
	_, otherGuest, err := cw.join(other.Call.GuestToken, "Other guest")
	require.NoError(t, err)
	require.ErrorIs(t, cw.act(hostSess, callrules.HostRemove, otherGuest.SeatID), callrules.ErrNotFound)
	require.ErrorIs(t, cw.act(hostSess, callrules.HostMute, id.NewShortId()), callrules.ErrNotFound)
	require.Equal(t, "admitted", cw.row(seatConcept, otherGuest.SeatID)["status"], "a host acted on a seat of another call")

	// The track controls reach the matching sources and nothing else; a track already
	// muted is left alone.
	cw.media.setPresent(roomID, person(in.Identity,
		MediaTrack{SID: "TR_mic", Source: "microphone"}, MediaTrack{SID: "TR_cam", Source: "camera"},
		MediaTrack{SID: "TR_share", Source: "screen_share"}, MediaTrack{SID: "TR_share_audio", Source: "screen_share_audio"},
		MediaTrack{SID: "TR_mic_muted", Source: "microphone", Muted: true}))
	require.NoError(t, cw.act(hostSess, callrules.HostMute, guest.SeatID))
	require.Equal(t, []fakeMediaMute{{Server: BootstrapMediaServerID, Room: roomID, Identity: in.Identity, Track: "TR_mic"}}, cw.media.mutes())
	require.NoError(t, cw.act(hostSess, callrules.HostCameraOff, guest.SeatID))
	require.NoError(t, cw.act(hostSess, callrules.HostStopShare, guest.SeatID))
	var tracks []string
	for _, m := range cw.media.mutes() {
		tracks = append(tracks, m.Track)
	}
	require.Equal(t, []string{"TR_mic", "TR_cam", "TR_share", "TR_share_audio"}, tracks)
	cw.media.setPresent(roomID) // nobody connected: nothing to mute, and no failure
	require.NoError(t, cw.act(hostSess, callrules.HostMute, guest.SeatID))
	require.Len(t, cw.media.mutes(), 4)

	// Lock and unlock, and whoever holds the host link is a host too.
	require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))
	require.Equal(t, true, cw.row(roomConcept, roomID)["locked"])
	_, cohost, err := cw.joinBy(callrules.JoinBody{HostToken: started.Call.HostToken, Name: "Co-host"})
	require.NoError(t, err)
	require.NoError(t, cw.act(cohost, callrules.HostUnlock, ""))
	require.Equal(t, false, cw.row(roomConcept, roomID)["locked"])
	versions := cw.versions(roomConcept, roomID)
	require.NoError(t, cw.act(cohost, callrules.HostUnlock, ""))
	require.Equal(t, versions, cw.versions(roomConcept, roomID), "unlocking an unlocked call wrote the room")

	// A host who left hosts nothing any more.
	require.NoError(t, cw.svc.Leave(ctx, cohost))
	requireReason(t, cw.act(cohost, callrules.HostLock, ""), callrules.ErrForbidden, callrules.ReasonNotHost)

	// An ended call takes no control.
	require.NoError(t, cw.act(hostSess, callrules.HostEnd, ""))
	requireReason(t, cw.act(hostSess, callrules.HostLock, ""), callrules.ErrEnded, callrules.ReasonCallEnded)
	require.Equal(t, "open", cw.row(roomConcept, other.Call.ID)["status"], "ending one call ended another")
}

// ---------------------------------------------------------------------------
// removal and admission
// ---------------------------------------------------------------------------

// TestRemoveBarsRejoin: a host removes a seat. The seat is removed and its identity
// disconnected, and its session never comes back in, by rejoining, by the call's link or
// by polling. A pass cannot be revoked, so when the identity shows up again on a pass it
// still holds, reconciliation disconnects it again. A waiter removed from the line loses
// its place, and a host does not remove their own seat. KNOWN LIMITATION, pinned: a
// person who clears their session and comes back through the link is somebody the call
// has never seen, and takes a new seat (the keyed-fingerprint bans come later).
func TestRemoveBarsRejoin(t *testing.T) {
	cw := newCallWorld(t, "remove", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	in, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	cw.media.setPresent(roomID, person(started.Join.Identity), person(in.Identity))

	require.NoError(t, cw.act(hostSess, callrules.HostRemove, guest.SeatID))
	seat := cw.row(seatConcept, guest.SeatID)
	require.Equal(t, "removed", seat["status"])
	require.Equal(t, cw.clock.Now(), rowTime(seat, "leftAt"), "a removal does not say when the seat left the call")
	removal := fakeMediaRemoval{Server: BootstrapMediaServerID, Room: roomID, Identity: in.Identity}
	require.Contains(t, cw.media.removals(), removal, "the removed seat's identity was not disconnected")

	// The session never comes back in.
	shut, _, err := cw.rejoin(guest, "")
	requireRefused(t, shut, err, callrules.ErrForbidden, callrules.ReasonRemoved)
	shut, _, err = cw.rejoin(guest, started.Call.GuestToken)
	requireRefused(t, shut, err, callrules.ErrForbidden, callrules.ReasonRemoved)
	polled, err := cw.svc.Queue(ctx, guest)
	requireRefused(t, polled, err, callrules.ErrForbidden, callrules.ReasonRemoved)
	require.Equal(t, "removed", cw.row(seatConcept, guest.SeatID)["status"])

	// The identity shows up again on the pass it still holds: reconciliation disconnects
	// it again, and does not give the seat back.
	before := len(cw.media.removals())
	cw.media.setPresent(roomID, person(started.Join.Identity), person(in.Identity))
	cw.reconcile()
	require.Len(t, cw.media.removals(), before+1)
	require.Equal(t, removal, cw.media.removals()[before], "the identity that came back was not disconnected again")
	require.Equal(t, "removed", cw.row(seatConcept, guest.SeatID)["status"], "reconciliation gave a removed seat back")

	// Removing it again writes nothing and disconnects again.
	versions := cw.versions(seatConcept, guest.SeatID)
	require.NoError(t, cw.act(hostSess, callrules.HostRemove, guest.SeatID))
	require.Equal(t, versions, cw.versions(seatConcept, guest.SeatID), "removing a removed seat wrote it")
	require.Len(t, cw.media.removals(), before+2)

	// A host does not remove their own seat: they leave.
	require.ErrorIs(t, cw.act(hostSess, callrules.HostRemove, hostSess.SeatID), callrules.ErrInvalid)
	require.Equal(t, "in_call", cw.row(seatConcept, hostSess.SeatID)["status"], "the host's own seat moved") // seen connected by the pass above

	// KNOWN LIMITATION: with no session the link decides, so a person who cleared
	// theirs is a newcomer with a new seat. Part 1 has no identity to bar.
	back, fresh, err := cw.join(started.Call.GuestToken, "Guest again")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
	require.NotEqual(t, guest.SeatID, fresh.SeatID)

	// A waiter removed from the line loses its place: the server is cut to the people it
	// carries, the next joiner queues, and the host removes them.
	cut := testMediaConfig()
	cut.Seats = cw.admittedIn(roomID)
	_, err = cw.in.Bootstrap(ctx, cut)
	require.NoError(t, err)
	queued, waiter, err := cw.join(started.Call.GuestToken, "Waiter")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, queued.Outcome)
	require.NoError(t, cw.act(hostSess, callrules.HostRemove, waiter.SeatID))
	require.Equal(t, "removed", cw.row(seatConcept, waiter.SeatID)["status"])
	require.Equal(t, "expired", cw.entryOf(waiter.SeatID)["status"], "a removed waiter kept its place in line")
	polled, err = cw.svc.Queue(ctx, waiter)
	requireRefused(t, polled, err, callrules.ErrForbidden, callrules.ReasonRemoved)
}

// TestAdmitLetsOneWaiterPastTheLock: in a locked call a host admits one waiter. The seat
// records when it was let in, and on its own next poll it is decided as any new ask of
// a running call: admitted with a pass when the media server has a seat for it after
// the line ahead, queued otherwise. Nobody else passes the lock, admission hands out no
// pass of its own, and a seat that is not waiting cannot be admitted. A waiter queued
// before the call was locked is held behind the lock; once admitted its entry is back in
// line at its own place.
func TestAdmitLetsOneWaiterPastTheLock(t *testing.T) {
	cw := newCallWorld(t, "admitlock", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))
	w1Res, w1, err := cw.join(started.Call.GuestToken, "First waiter")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, w1Res.Outcome)
	_, w2, err := cw.join(started.Call.GuestToken, "Second waiter")
	require.NoError(t, err)

	require.NoError(t, cw.act(hostSess, callrules.HostAdmit, w1.SeatID))
	seat := cw.row(seatConcept, w1.SeatID)
	require.Equal(t, "waiting", seat["status"], "admission let the waiter in without their own request")
	require.Equal(t, cw.clock.Now(), rowTime(seat, "letInAt"))

	in, err := cw.svc.Queue(ctx, w1)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome, "the admitted waiter was not let in on its poll")
	require.NotEmpty(t, in.Pass)
	require.False(t, in.Host, "an admitted waiter became a host")
	still, err := cw.svc.Queue(ctx, w2)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, still.Outcome, "a waiter nobody admitted passed the lock")
	require.Equal(t, "locked", still.Reason)

	// A seat that is in already, or gone, is not waiting to be admitted.
	require.NoError(t, cw.act(hostSess, callrules.HostAdmit, w1.SeatID), "admitting a seat already in is an error")
	require.NoError(t, cw.svc.Leave(ctx, w2))
	requireReason(t, cw.act(hostSess, callrules.HostAdmit, w2.SeatID), callrules.ErrConflict, callrules.ReasonNotWaiting)

	// The media server full: an admitted waiter is queued as anyone joining the call is,
	// and a waiter held behind the lock goes back to its place in line once admitted.
	cut := testMediaConfig()
	cut.Seats = cw.admittedIn(roomID)
	_, err = cw.in.Bootstrap(ctx, cut)
	require.NoError(t, err)
	require.NoError(t, cw.act(hostSess, callrules.HostUnlock, ""))
	q1, held, err := cw.join(started.Call.GuestToken, "Queued before the lock")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, q1.Outcome)
	require.Equal(t, 1, q1.Position)
	require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))
	q2, later, err := cw.join(started.Call.GuestToken, "Locked out")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, q2.Outcome)
	heldPoll, err := cw.svc.Queue(ctx, held)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, heldPoll.Outcome, "a queued waiter was not held behind the lock")

	require.NoError(t, cw.act(hostSess, callrules.HostAdmit, held.SeatID))
	back, err := cw.svc.Queue(ctx, held)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, back.Outcome, "an admitted waiter jumped a full media server")
	require.Equal(t, 1, back.Position, "an admitted waiter lost its place in line")
	require.NoError(t, cw.act(hostSess, callrules.HostAdmit, later.SeatID))
	behind, err := cw.svc.Queue(ctx, later)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, behind.Outcome)
	require.Equal(t, 2, behind.Position, "a waiter admitted from behind the lock went ahead of one queued before it")

	// A seat frees: the first in line is let in on its own poll.
	require.NoError(t, cw.svc.Leave(ctx, w1))
	early, err := cw.svc.Queue(ctx, later)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, early.Outcome)
	first, err := cw.svc.Queue(ctx, held)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, first.Outcome)
}

// ---------------------------------------------------------------------------
// the host leaving, and the call ending
// ---------------------------------------------------------------------------

// TestHostLeftCountdownEndsOrHandsOver: when the last host seat leaves, the room records
// when, and five minutes later reconciliation ends the call -- host_left, the media room
// deleted, every seat out -- unless a host came back meanwhile, which clears the
// countdown. When a key holder is in the call, they take over as host at once and the
// call never ends for want of one. A host who never got in, queued at Start, never left,
// so no countdown runs for them.
func TestHostLeftCountdownEndsOrHandsOver(t *testing.T) {
	cw := newCallWorld(t, "hostleft", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	grace := callrules.DefaultPolicy().HostLeftGrace

	// The countdown runs out.
	hostA, a := cw.started("Host A")
	inA, guestA, err := cw.join(a.Call.GuestToken, "Guest A")
	require.NoError(t, err)
	cw.media.setPresent(a.Call.ID, person(inA.Identity))
	require.NoError(t, cw.svc.Leave(ctx, hostA))
	leftAt := cw.clock.Now()
	require.Equal(t, leftAt, rowTime(cw.row(roomConcept, a.Call.ID), "hostLeftAt"), "the host's leaving did not start the countdown")
	cw.clock.Advance(grace - time.Second)
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, a.Call.ID)["status"], "the countdown ended the call early")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	ended := cw.row(roomConcept, a.Call.ID)
	require.Equal(t, "ended", ended["status"], "the countdown ran out and the call goes on")
	require.Equal(t, callrules.EndReasonHostLeft, ended["endReason"])
	require.Equal(t, cw.clock.Now(), rowTime(ended, "endedAt"))
	require.Contains(t, cw.media.deletions(), fakeMediaRef{Server: BootstrapMediaServerID, Room: a.Call.ID})
	require.Equal(t, "left", cw.row(seatConcept, guestA.SeatID)["status"])
	res, _, err := cw.rejoin(guestA, "")
	requireRefused(t, res, err, callrules.ErrEnded, callrules.ReasonCallEnded)

	// The host comes back in time, and the countdown is cleared.
	hostB, b := cw.started("Host B")
	inB, _, err := cw.join(b.Call.GuestToken, "Guest B")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Leave(ctx, hostB))
	require.False(t, rowTime(cw.row(roomConcept, b.Call.ID), "hostLeftAt").IsZero())
	backB, _, err := cw.svc.Join(ctx, cw.ref, &hostB, callrules.JoinBody{Name: "Host B", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, backB.Outcome)
	require.True(t, rowTime(cw.row(roomConcept, b.Call.ID), "hostLeftAt").IsZero(), "the host came back and the countdown still runs")
	cw.media.setPresent(b.Call.ID, person(backB.Identity), person(inB.Identity))

	// A key holder in the call takes over at once.
	hostC, c := cw.started("Host C")
	keyRes, keyHolder := cw.keyHolderJoin(c.Call.GuestToken, "Key holder")
	require.Equal(t, callrules.OutcomeAdmit, keyRes.Outcome)
	require.False(t, keyRes.Host)
	require.Equal(t, keyHolder.KeyID, bare(cw.row(seatConcept, keyHolder.SeatID)["keyId"]), "the seat does not record the key its holder unlocked")
	inC, _, err := cw.join(c.Call.GuestToken, "Guest C")
	require.NoError(t, err)
	cw.media.setPresent(c.Call.ID, person(keyRes.Identity), person(inC.Identity))
	require.NoError(t, cw.svc.Leave(ctx, hostC))
	cw.reconcile()
	require.Equal(t, true, cw.row(seatConcept, keyHolder.SeatID)["host"], "the key holder did not take over")
	roomC := cw.row(roomConcept, c.Call.ID)
	require.Equal(t, keyHolder.SeatID, bare(roomC["hostSeatId"]))
	require.True(t, rowTime(roomC, "hostLeftAt").IsZero(), "a call with a host still counts down")
	require.NoError(t, cw.act(keyHolder, callrules.HostLock, ""), "the new host cannot act")

	// A host who never got in never left: the server is cut to the people it carries,
	// and a new call's host is queued at Start.
	cut := testMediaConfig()
	cut.Seats = cw.admittedIn(b.Call.ID) + cw.admittedIn(c.Call.ID)
	_, err = cw.in.Bootstrap(ctx, cut)
	require.NoError(t, err)
	queuedHost := cw.host()
	d, err := cw.svc.Start(ctx, queuedHost, callrules.StartBody{Name: "Host D", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, d.Join.Outcome)

	cw.clock.Advance(grace + time.Minute)
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, b.Call.ID)["status"], "a call whose host came back was ended")
	require.Equal(t, "open", cw.row(roomConcept, c.Call.ID)["status"], "a call a key holder took over was ended")
	roomD := cw.row(roomConcept, d.Call.ID)
	require.Equal(t, "open", roomD["status"], "the countdown ran for a host still waiting to get in")
	require.True(t, rowTime(roomD, "hostLeftAt").IsZero())
}

// TestABookedCallWhoseHostNeverCameCountsFromItsStart: a booked call opened by a guest
// waits for its host, who may never come. The host-left countdown runs from the booked
// start -- not from the early opening, which would cut the host's own minutes -- and when
// it runs out the call ends host_left and its booking is marked ended. A call opened
// after its start counts from its opening, so a guest who is late does not open a call
// that is already over.
func TestABookedCallWhoseHostNeverCameCountsFromItsStart(t *testing.T) {
	cw := newCallWorld(t, "nohost", 50)
	if cw == nil {
		return
	}
	grace := callrules.DefaultPolicy().HostLeftGrace
	sess := cw.host()
	early := cw.booked(sess, 5*time.Minute, 30, "Opened early")
	opened, _, err := cw.join(early.GuestToken, "Early guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, opened.Outcome)
	roomID, _ := cw.bookingRoom(early.ID)

	cw.clock.Advance(early.Start.Sub(cw.clock.Now()) + grace - time.Second)
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "the countdown ran from the early opening")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	room := cw.row(roomConcept, roomID)
	require.Equal(t, "ended", room["status"], "a booked call whose host never came stayed open past the countdown")
	require.Equal(t, callrules.EndReasonHostLeft, room["endReason"])
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, early.ID)["status"], "the call ended and its booking did not")

	// Opened late, the call counts from its opening.
	late := cw.booked(sess, 10*time.Minute, 60, "Opened late")
	cw.clock.Advance(late.Start.Sub(cw.clock.Now()) + 20*time.Minute)
	lateIn, _, err := cw.join(late.GuestToken, "Late guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, lateIn.Outcome)
	lateRoom, _ := cw.bookingRoom(late.ID)
	cw.clock.Advance(grace - time.Second)
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, lateRoom)["status"], "a call opened late was ended before its own countdown ran")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	require.Equal(t, "ended", cw.row(roomConcept, lateRoom)["status"])
}

// TestABookedCallWhoseQueuedHostGaveUpEnds: a host passes a booked call's lock, so a host
// who opens the call with its host link while the media server is full waits only for a
// seat, with a place in line. One who stops polling gives that place up after the queue
// entry's lifetime, and is not coming any more. The automation's passes then end the call
// -- empty when nobody else waits, host_left by the countdown from its booked start when a
// guest waits behind the lock -- its booking is marked ended, and its key starts a call
// again. No pass deletes the media room of a call it leaves open.
func TestABookedCallWhoseQueuedHostGaveUpEnds(t *testing.T) {
	cw := newCallWorld(t, "hostgaveup", 1)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostA, a := cw.started("Host A") // the server's one seat
	cw.media.setPresent(a.Call.ID, person(a.Join.Identity))

	// Two booked calls on two keys, each opened by its host link and queued behind A; the
	// second has a guest waiting behind its lock too.
	alone, withGuest := cw.host(), cw.host()
	x := cw.booked(alone, 5*time.Minute, 30, "Host alone")
	y := cw.booked(withGuest, 5*time.Minute, 30, "Host and a guest")
	for _, sc := range []callrules.ScheduledCall{x, y} {
		queued, _, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host " + sc.Title})
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeQueue, queued.Outcome, "the host link was not queued on the full server")
	}
	waits, _, err := cw.joinBy(callrules.JoinBody{Token: y.GuestToken, Name: "Guest"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	roomX, _ := cw.bookingRoom(x.ID)
	roomY, _ := cw.bookingRoom(y.ID)
	require.Equal(t, true, cw.row(roomConcept, roomX)["locked"])

	// Nobody polls again. The automation passes every thirty seconds, past the queue
	// entry's lifetime and past the booked start plus the grace, and no pass may delete
	// the media room of a call it leaves open.
	p := callrules.DefaultPolicy()
	until := x.Start.Add(p.HostLeftGrace)
	if expiry := cw.clock.Now().Add(p.QueueEntryTTL); expiry.After(until) {
		until = expiry
	}
	until = until.Add(2 * time.Minute)
	deletedOpen := map[string]bool{}
	for cw.clock.Now().Before(until) {
		cw.clock.Advance(30 * time.Second)
		cw.reconcile()
		for _, deleted := range cw.media.deletions() {
			if cw.row(roomConcept, deleted.Room)["status"] != "ended" {
				deletedOpen[deleted.Room] = true
			}
		}
	}
	assert.Empty(t, deletedOpen, "a pass deleted the media room of a call it left open")
	ended := cw.row(roomConcept, roomX)
	assert.Equal(t, "ended", ended["status"], "a booked call whose queued host gave up stayed open")
	assert.Equal(t, callrules.EndReasonEmpty, ended["endReason"])
	assert.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, x.ID)["status"])
	ended = cw.row(roomConcept, roomY)
	assert.Equal(t, "ended", ended["status"], "a booked call with a guest waiting for a host who gave up stayed open")
	assert.Equal(t, callrules.EndReasonHostLeft, ended["endReason"])
	assert.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, y.ID)["status"])

	// Both keys start calls again once A frees the server's seat: the first is let in and
	// the second queues behind it, and neither is refused for a call still open.
	require.NoError(t, cw.svc.Leave(ctx, hostA))
	restart, err := cw.svc.Start(ctx, alone, callrules.StartBody{Name: "Host X", Confirm: confirmed})
	assert.NoError(t, err, "the key of a booked call whose host gave up starts no call")
	assert.Equal(t, callrules.OutcomeAdmit, restart.Join.Outcome)
	behind, err := cw.svc.Start(ctx, withGuest, callrules.StartBody{Name: "Host Y", Confirm: confirmed})
	assert.NoError(t, err, "the key of a booked call whose host gave up starts no call")
	assert.Equal(t, callrules.OutcomeQueue, behind.Join.Outcome)
}

// TestTheLastPersonLeavingEndsTheCall: a call ends at once, empty, when the last person
// in it leaves and nobody is waiting to come in; its media room is deleted and its
// booking marked ended. While somebody waits, the countdown governs instead. A waiter who
// stopped polling has given up its place and keeps no call open, and reconciliation ends
// a call left with nobody in it and nobody waiting.
func TestTheLastPersonLeavingEndsTheCall(t *testing.T) {
	cw := newCallWorld(t, "empty", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()

	hostA, a := cw.started("Host A")
	_, guestA, err := cw.join(a.Call.GuestToken, "Guest A")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Leave(ctx, guestA))
	require.Equal(t, "open", cw.row(roomConcept, a.Call.ID)["status"], "a call ended with its host still in it")
	require.NoError(t, cw.svc.Leave(ctx, hostA))
	room := cw.row(roomConcept, a.Call.ID)
	require.Equal(t, "ended", room["status"], "the last person left and the call goes on")
	require.Equal(t, callrules.EndReasonEmpty, room["endReason"])
	require.Equal(t, cw.clock.Now(), rowTime(room, "endedAt"))
	require.Contains(t, cw.media.deletions(), fakeMediaRef{Server: BootstrapMediaServerID, Room: a.Call.ID})

	// Somebody waiting keeps the call open, and the countdown governs.
	hostB, b := cw.started("Host B")
	require.NoError(t, cw.act(hostB, callrules.HostLock, ""))
	_, waiterB, err := cw.join(b.Call.GuestToken, "Waiter B")
	require.NoError(t, err)
	require.NoError(t, cw.svc.Leave(ctx, hostB))
	roomB := cw.row(roomConcept, b.Call.ID)
	require.Equal(t, "open", roomB["status"], "a call ended with somebody waiting to come in")
	require.Equal(t, cw.clock.Now(), rowTime(roomB, "hostLeftAt"))
	require.NoError(t, cw.svc.Leave(ctx, waiterB))
	require.Equal(t, callrules.EndReasonEmpty, cw.row(roomConcept, b.Call.ID)["endReason"], "the waiter gave up and the call stayed open")

	// A booked call that empties has its booking marked ended.
	sess := cw.host()
	sc := cw.booked(sess, 5*time.Minute, 30, "Booked")
	hostIn, hostC, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host C"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, hostIn.Outcome)
	require.NoError(t, cw.svc.Leave(ctx, hostC))
	roomC, _ := cw.bookingRoom(sc.ID)
	require.Equal(t, callrules.EndReasonEmpty, cw.row(roomConcept, roomC)["endReason"])
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, sc.ID)["status"])

	// A call whose only seat gave up its place in line is ended by reconciliation: the
	// server is cut to the one seat another call's host holds, a new call's host queues
	// and never polls again.
	_, e := cw.started("Host E")
	cw.media.setPresent(e.Call.ID, person(e.Join.Identity))
	cut := testMediaConfig()
	cut.Seats = 1
	_, err = cw.in.Bootstrap(ctx, cut)
	require.NoError(t, err)
	d, err := cw.svc.Start(ctx, cw.host(), callrules.StartBody{Name: "Host D", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, d.Join.Outcome)
	cw.clock.Advance(callrules.DefaultPolicy().QueueEntryTTL - time.Second)
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, d.Call.ID)["status"], "a call was ended while its host still held a place in line")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	require.Equal(t, callrules.EndReasonEmpty, cw.row(roomConcept, d.Call.ID)["endReason"], "a call nobody is in or waiting for stayed open")
}

// ---------------------------------------------------------------------------
// a booking ends with its call
// ---------------------------------------------------------------------------

// TestEndingABookedCallMarksItsBookingEnded: however a booked call ends -- its host
// ends it, the room empties, or the host-left countdown runs out -- its booking is marked
// ended in the same write, so the key's bookings read and the check a new booking is
// held to no longer see it.
func TestEndingABookedCallMarksItsBookingEnded(t *testing.T) {
	cw := newCallWorld(t, "endbooked", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	byHost := cw.booked(sess, 5*time.Minute, 30, "Ended by its host") // 0:05 to 0:35
	emptied := cw.booked(sess, 35*time.Minute, 30, "Emptied")         // 0:35 to 1:05
	countedOut := cw.booked(sess, 65*time.Minute, 30, "Counted out")  // 1:05 to 1:35

	idsOf := func() []string {
		read, err := cw.calls.store.bookingsForKey(ctx, cw.owner, sess.KeyID)
		require.NoError(t, err)
		var out []string
		for _, b := range read {
			out = append(out, b.ID)
		}
		return out
	}

	in, host, err := cw.joinBy(callrules.JoinBody{HostToken: byHost.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	require.Contains(t, idsOf(), byHost.ID, "a started booking is not read")
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, byHost.ID)["status"])
	room, _ := cw.bookingRoom(byHost.ID)
	require.Equal(t, callrules.EndReasonEnded, cw.row(roomConcept, room)["endReason"])

	cw.clock.Advance(26 * time.Minute) // nine minutes before the second's start
	in, host, err = cw.joinBy(callrules.JoinBody{HostToken: emptied.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	require.NoError(t, cw.svc.Leave(ctx, host))
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, emptied.ID)["status"])

	cw.clock.Advance(30 * time.Minute) // nine minutes before the third's start
	waits, _, err := cw.join(countedOut.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	cw.clock.Advance(countedOut.Start.Sub(cw.clock.Now()) + callrules.DefaultPolicy().HostLeftGrace)
	cw.reconcile()
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, countedOut.ID)["status"])

	require.Empty(t, idsOf(), "the key's bookings read still returns bookings whose calls ended")
	cw.capUpcoming(1)
	cw.booked(sess, time.Hour, 30, "A slot nobody else holds")
}

// TestAKeyThatOpenedAndEndedManyBookedCallsStillBooks: a key that has opened and ended
// more booked calls than the policy lets it have upcoming still books: an ended call
// holds no slot and is not read.
func TestAKeyThatOpenedAndEndedManyBookedCallsStillBooks(t *testing.T) {
	cw := newCallWorld(t, "manybooked", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	cw.capUpcoming(2)
	for i := 0; i < 3; i++ {
		sc := cw.booked(sess, 5*time.Minute, 30, fmt.Sprintf("Call %d", i))
		in, host, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host"})
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
		require.NoError(t, cw.act(host, callrules.HostEnd, ""))
		cw.clock.Advance(time.Minute)
	}
	read, err := cw.calls.store.bookingsForKey(ctx, cw.owner, sess.KeyID)
	require.NoError(t, err)
	require.Empty(t, read, "the key's bookings read grew with the calls it ended")
	cw.booked(sess, time.Hour, 30, "First of two")
	cw.booked(sess, 2*time.Hour, 30, "Second of two")
	_, err = cw.book(sess, 3*time.Hour, 30, "One past the cap")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonTooManyUpcoming)
}

// ---------------------------------------------------------------------------
// the media room before the first pass
// ---------------------------------------------------------------------------

// TestTheFirstPassIntoAnEmptyRoomOpensItsMediaRoomAgain: a media server closes a room
// nobody has been in for a while, and a participant who then connects would find it
// gone, or opened again without the call's limits. So before a pass is handed to the
// only person in a call, the media room is opened again with its limits (a media server
// answers that for a room it has, so nothing changes when it is open). A pass for
// somebody joining people already in the call opens nothing.
func TestTheFirstPassIntoAnEmptyRoomOpensItsMediaRoomAgain(t *testing.T) {
	cw := newCallWorld(t, "reopen", 2)
	if cw == nil {
		return
	}
	ctx := context.Background()
	limit := uint32(callrules.DefaultPolicy().MaxPeople + callrules.DefaultPolicy().MaxAgents + 1)
	hostA, a := cw.started("Host A") // seat 1 of 2
	_, guestA, err := cw.join(a.Call.GuestToken, "Guest A")
	require.NoError(t, err) // seat 2: joining people already in, it opened nothing
	require.Len(t, cw.media.rooms(), 1)

	bHost := cw.host()
	b, err := cw.svc.Start(ctx, bHost, callrules.StartBody{Name: "Host B", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, b.Join.Outcome)
	bHost, err = cw.svc.Session(ctx, cw.ref, bHost.Token)
	require.NoError(t, err)
	require.Len(t, cw.media.rooms(), 2, "Start did not open the call's media room")

	// B's media room has had nobody in it since its start. A seat frees and B's host is
	// let in: the room is opened again before the pass.
	require.NoError(t, cw.svc.Leave(ctx, guestA))
	in, err := cw.svc.Queue(ctx, bHost)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	rooms := cw.media.rooms()
	require.Len(t, rooms, 3, "the first pass into an empty room did not open its media room")
	require.Equal(t, fakeMediaRoom{Server: BootstrapMediaServerID, Name: b.Call.ID, Max: limit}, rooms[2])

	// A host alone in their call who asks again is the call's only person too.
	again, _, err := cw.svc.Join(ctx, cw.ref, &hostA, callrules.JoinBody{Name: "Host A", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, again.Outcome)
	rooms = cw.media.rooms()
	require.Len(t, rooms, 4)
	require.Equal(t, fakeMediaRoom{Server: BootstrapMediaServerID, Name: a.Call.ID, Max: limit}, rooms[3])

	// A media server that cannot open the room hands out no pass, and the seat keeps its
	// place for a later ask.
	cw.media.mu.Lock()
	cw.media.failCreate = fmt.Errorf("the media server is down")
	cw.media.mu.Unlock()
	refused, err := cw.svc.Queue(ctx, bHost)
	require.Error(t, err)
	require.Empty(t, refused.Pass)
	cw.media.mu.Lock()
	cw.media.failCreate = nil
	cw.media.mu.Unlock()
	require.Equal(t, "admitted", cw.row(seatConcept, bHost.SeatID)["status"])
}
