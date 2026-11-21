package calls

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

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

// TestABookedCallWhoseHostNeverCameWaitsForItUntilItsEnd: a booked call is its host's for
// the window its key holder booked. A guest who opens it -- early, on time or late -- waits
// behind its lock, and a host who never came has not left, so no countdown runs before the
// booked end: an interviewer six minutes late, a minute past where a countdown from the
// booked start would have run out, is let in by the host link and lets the candidate in.
// After the booked end the countdown runs from the end: a call a guest still waits in ends
// host_left a grace later, not empty, because somebody is still waiting to come in, and
// its booking is marked ended.
func TestABookedCallWhoseHostNeverCameWaitsForItUntilItsEnd(t *testing.T) {
	cw := newCallWorld(t, "nohost", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	grace := callrules.DefaultPolicy().HostLeftGrace

	// The candidate opens the interview early and waits behind its lock.
	interview := cw.booked(cw.host(), 5*time.Minute, 30, "Late interviewer")
	waits, candidate, err := cw.join(interview.GuestToken, "Candidate")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	roomID, _ := cw.bookingRoom(interview.ID)

	// The automation's passes, every thirty seconds, to the booked start plus six minutes:
	// none ends the call.
	for late := interview.Start.Add(grace + time.Minute); cw.clock.Now().Before(late); {
		cw.clock.Advance(30 * time.Second)
		cw.reconcile()
		require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a booked call whose host never came was ended inside its booked window")
	}
	require.Equal(t, callrules.BookingStarted, cw.row(scheduledCallConcept, interview.ID)["status"])

	// The interviewer arrives, by the host link, is let into the call and lets the
	// candidate in.
	in, interviewer, err := cw.joinBy(callrules.JoinBody{HostToken: interview.HostToken, Name: "Interviewer"})
	require.NoError(t, err, "the host link was refused inside the booked window")
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome, "a host arriving late inside the booked window was not let in")
	require.Equal(t, roomID, interviewer.RoomID, "the host was let into another room")
	require.NoError(t, cw.act(interviewer, callrules.HostAdmit, candidate.SeatID))
	admitted, err := cw.svc.Queue(ctx, candidate)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, admitted.Outcome, "the late host could not let the candidate in")

	// Nobody hosting it ever comes: the candidate waits through the booked window, and the
	// countdown runs from the booked end.
	unhosted := cw.booked(cw.host(), 5*time.Minute, 30, "No interviewer")
	cw.advanceTo(unhosted.Start)
	waits, _, err = cw.join(unhosted.GuestToken, "Candidate")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	unhostedRoom, _ := cw.bookingRoom(unhosted.ID)
	cw.advanceTo(unhosted.End)
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, unhostedRoom)["status"], "the call ended at its booked end with somebody still waiting")
	cw.advanceTo(unhosted.End.Add(grace - time.Second))
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, unhostedRoom)["status"], "the countdown from the booked end ran out early")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	room := cw.row(roomConcept, unhostedRoom)
	require.Equal(t, "ended", room["status"], "a booked call whose host never came stayed open past its booked end and the grace")
	require.Equal(t, callrules.EndReasonHostLeft, room["endReason"], "a call with a guest still waiting is not empty: the countdown ends it")
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, unhosted.ID)["status"], "the call ended and its booking did not")
}

// TestAHostWhoCameAndLeftABookedCallGetsTheCountdown: a host who was in a booked call and
// left it after its start is counted from that leave, as in any call: the call ends
// host_left the grace after it, its booked end still far off.
func TestAHostWhoCameAndLeftABookedCallGetsTheCountdown(t *testing.T) {
	cw := newCallWorld(t, "hostcamebooked", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	grace := callrules.DefaultPolicy().HostLeftGrace
	sc := cw.booked(cw.host(), 5*time.Minute, 60, "Host comes and goes")
	cw.advanceTo(sc.Start)
	in, host, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	roomID, _ := cw.bookingRoom(sc.ID)
	waits, guest, err := cw.joinBy(callrules.JoinBody{Token: sc.GuestToken, Name: "Guest"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	require.NoError(t, cw.act(host, callrules.HostAdmit, guest.SeatID))
	guestIn, err := cw.svc.Queue(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, guestIn.Outcome)
	cw.media.setPresent(roomID, person(guestIn.Identity))

	cw.clock.Advance(10 * time.Minute)
	left := cw.clock.Now()
	require.NoError(t, cw.svc.Leave(ctx, host))
	cw.advanceTo(left.Add(grace - time.Second))
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "the countdown ran out before the grace after the host left")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	ended := cw.row(roomConcept, roomID)
	require.Equal(t, "ended", ended["status"], "a booked call whose host left stayed open past the countdown")
	require.Equal(t, callrules.EndReasonHostLeft, ended["endReason"])
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, sc.ID)["status"])
}

// TestABookedCallWhoseQueuedHostGaveUpEnds: a host passes a booked call's lock, so a host
// who opens the call with its host link while the media server is full waits only for a
// seat, with a place in line. One who stops polling gives that place up after the queue
// entry's lifetime, and is not coming any more: a host who never got in never came. Before
// its booked end a booked call is its host's (host.go hostLeft) and one that empties is
// not ended empty (host.go endsEmpty), so no pass ends either call before then. After it,
// the call nobody waits in ends empty at its booked end, and the one a guest still waits
// in behind the lock ends host_left, the grace after its booked end. Each booking is
// marked ended, and each key starts a call again. No pass deletes the media room of a call
// it leaves open.
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
	x := cw.booked(alone, 5*time.Minute, 10, "Host alone")
	y := cw.booked(withGuest, 5*time.Minute, 10, "Host and a guest")
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
	// entry's lifetime and past the booked end plus the grace, and no pass may delete the
	// media room of a call it leaves open.
	p := callrules.DefaultPolicy()
	until := x.End.Add(p.HostLeftGrace)
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
	assert.Equal(t, callrules.EndReasonEmpty, ended["endReason"], "nobody was in it or waiting: it ends empty, at its booked end")
	assert.False(t, rowTime(ended, "endedAt").Before(x.End), "a booked call whose queued host gave up ended inside its booked window")
	assert.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, x.ID)["status"])
	ended = cw.row(roomConcept, roomY)
	assert.Equal(t, "ended", ended["status"], "a booked call with a guest waiting for a host who gave up stayed open")
	assert.Equal(t, callrules.EndReasonHostLeft, ended["endReason"], "a guest still waits in it: the countdown from its booked end ends it")
	assert.False(t, rowTime(ended, "endedAt").Before(y.End.Add(p.HostLeftGrace)), "the countdown from the booked end ran out early")
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
// in it leaves and nobody is waiting to come in; its media room is deleted and, for a
// booked call past its booked end, its booking marked ended. While somebody waits, the
// countdown governs instead. A waiter who stopped polling has given up its place and
// keeps no call open, and reconciliation ends a call left with nobody in it and nobody
// waiting. (A booked call that empties before its booked end waits for its host:
// TestABookedCallThatEmptiesBeforeItsEndWaitsForItsHost.)
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

	// A booked call that empties after its booked end has its booking marked ended.
	sess := cw.host()
	sc := cw.booked(sess, 5*time.Minute, 30, "Booked")
	hostIn, hostC, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host C"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, hostIn.Outcome)
	cw.advanceTo(sc.End)
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

// TestABookedCallThatEmptiesBeforeItsEndWaitsForItsHost: a booked call is its host's to
// keep until its booked end. A guest who opens it early -- to try the link -- and leaves
// does not end it, nor does a pass, and its host, arriving at the start, joins it. A
// booked call nobody comes back to stays open, through the booked start plus the grace,
// until its booked end, where the empty-room rule ends it, empty, its booking marked
// ended. After its booked end the empty-room rule applies as it does to any call: the
// last person leaving ends it, empty.
func TestABookedCallThatEmptiesBeforeItsEndWaitsForItsHost(t *testing.T) {
	cw := newCallWorld(t, "bookedempty", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	grace := callrules.DefaultPolicy().HostLeftGrace

	// The candidate tries the link early, waits behind the call's lock, and leaves.
	interview := cw.booked(cw.host(), 5*time.Minute, 30, "Interview")
	waits, early, err := cw.joinBy(callrules.JoinBody{Token: interview.GuestToken, Name: "Candidate"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	roomID, _ := cw.bookingRoom(interview.ID)
	require.NoError(t, cw.svc.Leave(ctx, early))
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a guest who opened a booked call early and left ended it")
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a pass ended a booked call that emptied before its booked end")
	require.Equal(t, callrules.BookingStarted, cw.row(scheduledCallConcept, interview.ID)["status"])

	// The interviewer arrives at the start, by the host link, and is let into the call.
	cw.advanceTo(interview.Start)
	cw.reconcile()
	in, host, err := cw.joinBy(callrules.JoinBody{HostToken: interview.HostToken, Name: "Interviewer"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome, "the host arriving at the start was not let into the call")
	require.Equal(t, roomID, host.RoomID, "the host was let into another room")
	cw.media.setPresent(roomID, person(in.Identity))

	// A booked call nobody comes back to stays its host's until its booked end, where it
	// ends empty: nobody is in it and nobody waits.
	abandoned := cw.booked(cw.host(), 5*time.Minute, 30, "Nobody comes")
	waits, gone, err := cw.joinBy(callrules.JoinBody{Token: abandoned.GuestToken, Name: "Early guest"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	abandonedRoom, _ := cw.bookingRoom(abandoned.ID)
	require.NoError(t, cw.svc.Leave(ctx, gone))
	cw.advanceTo(abandoned.Start.Add(grace + time.Minute))
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, abandonedRoom)["status"], "a pass ended an abandoned booked call inside its booked window")
	cw.advanceTo(abandoned.End)
	cw.reconcile()
	ended := cw.row(roomConcept, abandonedRoom)
	require.Equal(t, "ended", ended["status"], "an abandoned booked call stayed open past its booked end")
	require.Equal(t, callrules.EndReasonEmpty, ended["endReason"], "nobody was in it or waiting: it ends empty at its booked end")
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, abandoned.ID)["status"])

	// After its booked end, the last person leaving ends a booked call as any other. (The
	// abandoned call's booked end is past the interview's, so the clock already is.)
	require.False(t, cw.clock.Now().Before(interview.End))
	require.NoError(t, cw.svc.Leave(ctx, host))
	done := cw.row(roomConcept, roomID)
	require.Equal(t, "ended", done["status"], "the last person left a booked call after its end and it goes on")
	require.Equal(t, callrules.EndReasonEmpty, done["endReason"])
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, interview.ID)["status"])
}

// TestAHostWhoLeavesABookedCallEarlyKeepsItForItsStart: a booked call's host-left
// countdown runs from its booked start at the earliest, so a host who opens the call
// early to check it, and leaves, does not end it before it begins. Through every pass up
// to the booked start the call stays open, and a guest arriving then joins it, behind its
// lock, for a host to admit. With no host back by the booked start plus the grace, the
// pass ends it host_left and its booking is marked ended.
func TestAHostWhoLeavesABookedCallEarlyKeepsItForItsStart(t *testing.T) {
	cw := newCallWorld(t, "hostleftearly", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	grace := callrules.DefaultPolicy().HostLeftGrace
	// Nine minutes ahead: inside the early opening (calls.ScheduledCallOpensEarly), and far
	// enough ahead that a countdown from the leave alone would run out before the start.
	sc := cw.booked(cw.host(), 9*time.Minute, 30, "Checked early")
	in, host, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	roomID, _ := cw.bookingRoom(sc.ID)
	require.NoError(t, cw.svc.Leave(ctx, host))
	require.False(t, rowTime(cw.row(roomConcept, roomID), "hostLeftAt").IsZero(), "the host's leaving was not recorded")

	// The automation's passes, every thirty seconds, up to the booked start.
	for cw.clock.Now().Before(sc.Start) {
		cw.clock.Advance(30 * time.Second)
		cw.reconcile()
		require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "the countdown ended a booked call before its booked start")
	}
	waits, guest, err := cw.joinBy(callrules.JoinBody{Token: sc.GuestToken, Name: "Guest"})
	require.NoError(t, err, "a guest arriving at the booked start was refused")
	require.Equal(t, callrules.OutcomeWait, waits.Outcome, "a guest arriving at the booked start was not let into the call's waiting room")
	require.Equal(t, roomID, guest.RoomID, "the guest was let into another room")

	// Nobody hosting it comes back: the countdown runs from the booked start.
	cw.advanceTo(sc.Start.Add(grace - time.Second))
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "the countdown ended the call before the grace from its booked start")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	ended := cw.row(roomConcept, roomID)
	require.Equal(t, "ended", ended["status"], "a booked call nobody hosted stayed open past its countdown")
	require.Equal(t, callrules.EndReasonHostLeft, ended["endReason"])
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, sc.ID)["status"], "the call ended and its booking did not")
}

// TestAHostBackBeforeABookedCountdownRunsOutKeepsTheCall: a host who left a booked call
// early and comes back before its booked start plus the grace -- here two minutes after
// the start, past where a countdown from the leave alone would have run out -- is let in,
// the countdown is cleared, and the call goes on past the grace.
func TestAHostBackBeforeABookedCountdownRunsOutKeepsTheCall(t *testing.T) {
	cw := newCallWorld(t, "hostbackbooked", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	grace := callrules.DefaultPolicy().HostLeftGrace
	sc := cw.booked(cw.host(), 9*time.Minute, 30, "Host comes back")
	in, host, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	roomID, _ := cw.bookingRoom(sc.ID)
	require.NoError(t, cw.svc.Leave(ctx, host))

	for back := sc.Start.Add(2 * time.Minute); cw.clock.Now().Before(back); {
		cw.clock.Advance(30 * time.Second)
		cw.reconcile()
	}
	again, _, err := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err, "the host coming back before the countdown ran out was refused")
	require.Equal(t, callrules.OutcomeAdmit, again.Outcome)
	require.True(t, rowTime(cw.row(roomConcept, roomID), "hostLeftAt").IsZero(), "the host came back and the countdown still runs")
	cw.media.setPresent(roomID, person(again.Identity))

	for until := sc.Start.Add(grace + time.Minute); cw.clock.Now().Before(until); {
		cw.clock.Advance(30 * time.Second)
		cw.reconcile()
	}
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a booked call whose host came back was ended")
	require.Equal(t, callrules.BookingStarted, cw.row(scheduledCallConcept, sc.ID)["status"])
}

// ---------------------------------------------------------------------------
// a booking ends with its call
// ---------------------------------------------------------------------------

// TestEndingABookedCallMarksItsBookingEnded: however a booked call ends -- its host
// ends it, the room empties after its booked end, or the host-left countdown runs out --
// its booking is marked ended in the same write, so the key's bookings read and the check
// a new booking is held to no longer see it.
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
	// The call runs to its booked end, and its last person leaves: before the end, a
	// booked call that empties waits for its host (host.go endsEmpty).
	cw.advanceTo(emptied.End)
	require.NoError(t, cw.svc.Leave(ctx, host))
	require.Equal(t, callrules.BookingEnded, cw.row(scheduledCallConcept, emptied.ID)["status"])

	// The third, back to back with the second, opened by a guest at its start, whose host
	// never comes: the countdown runs from its booked end (host.go hostLeft).
	waits, _, err := cw.join(countedOut.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	cw.advanceTo(countedOut.End.Add(callrules.DefaultPolicy().HostLeftGrace))
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
// the media room before every pass
// ---------------------------------------------------------------------------

// passOpened checks that a pass was handed out only once its call's media room was opened:
// the media server was asked to open exactly one room since `opened` asks had been made,
// and it is the room the pass names, on the bootstrap server, with the call's limits.
func (cw *callWorld) passOpened(res callrules.JoinResult, opened int, who string) {
	cw.t.Helper()
	require.Equal(cw.t, callrules.OutcomeAdmit, res.Outcome, "%s was not let in: %+v", who, res)
	grants, _ := verifyPass(cw.t, res.Pass, testMediaConfig().APIKey, testAPISecret)
	limit := uint32(callrules.DefaultPolicy().MaxPeople + callrules.DefaultPolicy().MaxAgents + 1)
	rooms := cw.media.rooms()
	require.Len(cw.t, rooms, opened+1, "%s was handed a pass without its media room being opened first", who)
	require.Equal(cw.t, fakeMediaRoom{Server: BootstrapMediaServerID, Name: grants.Video.Room, Max: limit}, rooms[opened], who)
}

// TestEveryPassOpensItsMediaRoomFirst: a media server opens no room a connection names
// (room.auto_create false), and a room it opened can be gone by the time somebody
// connects -- closed for having had nobody in it, or forgotten in a restart. So before ANY
// pass is handed out, its call's media room is opened with the call's limits, and a media
// server answers that for a room it still holds with the room unchanged: the host's pass at
// Start, a guest's joining people already in the call, a waiter's on the poll that lets it
// in, and a rejoin's. A media server that cannot open the room hands out no pass.
func TestEveryPassOpensItsMediaRoomFirst(t *testing.T) {
	cw := newCallWorld(t, "reopen", 2)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostA, a := cw.started("Host A") // seat 1 of 2
	cw.passOpened(a.Join, 0, "the host at Start")
	opened := len(cw.media.rooms())
	in, guestA, err := cw.join(a.Call.GuestToken, "Guest A") // seat 2
	require.NoError(t, err)
	cw.passOpened(in, opened, "a guest joining people already in the call")

	// A full server queues B's host: Start opens B's media room and hands out no pass.
	bHost := cw.host()
	b, err := cw.svc.Start(ctx, bHost, callrules.StartBody{Name: "Host B", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, b.Join.Outcome)
	require.Empty(t, b.Join.Pass)
	bHost, err = cw.svc.Session(ctx, cw.ref, bHost.Token)
	require.NoError(t, err)

	// B's media room has had nobody in it since its start. A seat frees and B's host is
	// let in on its poll: the room is opened again before the pass.
	require.NoError(t, cw.svc.Leave(ctx, guestA))
	opened = len(cw.media.rooms())
	polled, err := cw.svc.Queue(ctx, bHost)
	require.NoError(t, err)
	cw.passOpened(polled, opened, "a waiter let in on its poll")
	require.Equal(t, b.Call.ID, cw.media.rooms()[opened].Name)

	// A rejoin is the same seat with a fresh pass, and its room is opened first too.
	opened = len(cw.media.rooms())
	again, _, err := cw.svc.Join(ctx, cw.ref, &hostA, callrules.JoinBody{Name: "Host A", Confirm: confirmed})
	require.NoError(t, err)
	cw.passOpened(again, opened, "a rejoin")
	require.Equal(t, a.Call.ID, cw.media.rooms()[opened].Name)

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

// TestSeatsBackAfterAMediaServerRestartGetARoomThatExists: a media server that restarts
// during a call forgets its rooms, and everybody in the call reconnects at once, each
// asking for a fresh pass -- here one by its page's poll and one by a rejoin, the two ways
// a page comes back. The server opens no room a connection names (room.auto_create false),
// so a pass for a room it no longer holds is a connection it refuses, for every seat in
// the call. Each of the two is handed a pass only once the room is open again with the
// call's limits, although neither is alone in the call; and the room is forgotten again
// between them, so the second pass cannot lean on the first one's opening.
func TestSeatsBackAfterAMediaServerRestartGetARoomThatExists(t *testing.T) {
	cw := newCallWorld(t, "restart", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	limit := uint32(callrules.DefaultPolicy().MaxPeople + callrules.DefaultPolicy().MaxAgents + 1)
	hostSess, started := cw.started("Host")
	in, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)

	// heldFor checks that the room a pass names is one the media server holds now, as the
	// call opens it.
	heldFor := func(res callrules.JoinResult, who string) {
		t.Helper()
		require.Equal(t, callrules.OutcomeAdmit, res.Outcome, "%s was not let back in: %+v", who, res)
		grants, _ := verifyPass(t, res.Pass, testMediaConfig().APIKey, testAPISecret)
		require.Equal(t, started.Call.ID, grants.Video.Room, "%s was handed a pass for another room", who)
		room, held := cw.media.holding(BootstrapMediaServerID, grants.Video.Room)
		require.True(t, held, "%s was handed a pass for a room the media server does not hold", who)
		require.Equal(t, limit, room.Max, "%s's room was opened without the call's limits", who)
	}

	cw.media.restart()
	polled, err := cw.svc.Queue(ctx, hostSess)
	require.NoError(t, err)
	heldFor(polled, "the host, back by a poll")

	cw.media.restart()
	rejoined, _, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	heldFor(rejoined, "the guest, back by a rejoin")
}

// ---------------------------------------------------------------------------
// the roster
// ---------------------------------------------------------------------------

// callsVersions is every stored version of every calls row of this test's site: what a
// read that writes nothing leaves exactly as it was.
func (cw *callWorld) callsVersions() int {
	cw.t.Helper()
	concepts := []string{sessionConcept, roomConcept, seatConcept, queueEntryConcept, scheduledCallConcept}
	return stateWhere(cw.t, cw.db, `concept IN (?) AND payload->>'siteId' = ?`, bun.In(concepts), siteConcept+":"+cw.site).Versions
}

// roster is the roster a session reads, which must not fail: what its own ask about its
// call (SessionCall) answers to a host seat in an open call, and nil to every other.
func (cw *callWorld) roster(sess callrules.Session) *callrules.Roster {
	cw.t.Helper()
	call, err := cw.svc.SessionCall(context.Background(), sess)
	require.NoError(cw.t, err)
	require.Equal(cw.t, call.Host, call.Roster != nil, "a seat was told it hosts the call with no roster, or given one without being told")
	return call.Roster
}

// rosterSeats is the ids a roster lists in the call, in order.
func rosterSeats(roster *callrules.Roster) []string {
	out := []string{}
	for _, seat := range roster.InCall {
		out = append(out, seat.Seat)
	}
	return out
}

// TestTheRosterNamesTheSeatsAHostActsOn: a host seat in an open call reads the call's
// roster -- the seats in it in the order they were let in, each with the media identity a
// client matches the media server's participants by, and the seats waiting to come in, in
// line order, each with when it began waiting -- and the ids it names are the ids the host
// controls take. Reading it writes nothing. A guest's seat, a host seat that has left, a
// session seated nowhere and an ended call read no roster at all.
func TestTheRosterNamesTheSeatsAHostActsOn(t *testing.T) {
	t.Run("a host's session lists a waiting seat behind the lock, and admit with that seat id lets it in", func(t *testing.T) {
		cw := newCallWorld(t, "rosteradmit", 50)
		if cw == nil {
			return
		}
		ctx := context.Background()
		hostSess, started := cw.started("Host")
		require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))
		cw.clock.Advance(time.Second)
		knocked := cw.clock.Now()
		res, waiter, err := cw.join(started.Call.GuestToken, "Knocking")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeWait, res.Outcome)
		cw.clock.Advance(30 * time.Second)

		before := cw.callsVersions()
		roster := cw.roster(hostSess)
		require.Equal(t, before, cw.callsVersions(), "reading the roster wrote a row")
		require.NotNil(t, roster, "a host seat in an open call read no roster")
		require.Equal(t, []callrules.RosterSeat{{
			Seat: hostSess.SeatID, DisplayName: "Host", Kind: "person", Host: true, Status: "admitted", Identity: started.Join.Identity,
		}}, roster.InCall)
		require.Equal(t, []callrules.RosterWaiter{{Seat: waiter.SeatID, DisplayName: "Knocking", Kind: "person", Since: knocked}}, roster.Waiting,
			"the waiter is not listed, or not as waiting since it knocked")

		require.NoError(t, cw.act(hostSess, callrules.HostAdmit, roster.Waiting[0].Seat))
		in, err := cw.svc.Queue(ctx, waiter)
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, in.Outcome, "the seat the roster named was not let in")

		roster = cw.roster(hostSess)
		require.Empty(t, roster.Waiting)
		require.Len(t, roster.InCall, 2)
		require.Equal(t, waiter.SeatID, roster.InCall[1].Seat, "the seat let in last is not listed last")
		require.Equal(t, in.Identity, roster.InCall[1].Identity)
		require.False(t, roster.InCall[1].Host)
	})

	t.Run("it lists an in-call seat that the host can then mute and remove by that id", func(t *testing.T) {
		cw := newCallWorld(t, "rostermute", 50)
		if cw == nil {
			return
		}
		hostSess, started := cw.started("Host")
		// Let in after the host, so admission order is the clock's and not the seat ids'.
		cw.clock.Advance(time.Second)
		in, guest, err := cw.join(started.Call.GuestToken, "Guest")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, in.Outcome)

		roster := cw.roster(hostSess)
		require.Len(t, roster.InCall, 2)
		listed := roster.InCall[1]
		require.Equal(t, callrules.RosterSeat{Seat: guest.SeatID, DisplayName: "Guest", Kind: "person", Status: "admitted", Identity: in.Identity}, listed)

		roomID := started.Call.ID
		cw.media.setPresent(roomID, person(listed.Identity, MediaTrack{SID: "TR_mic", Source: "microphone"}))
		require.NoError(t, cw.act(hostSess, callrules.HostMute, listed.Seat))
		require.Equal(t, []fakeMediaMute{{Server: BootstrapMediaServerID, Room: roomID, Identity: in.Identity, Track: "TR_mic"}}, cw.media.mutes(),
			"the mute did not reach the seat the roster named")
		require.NoError(t, cw.act(hostSess, callrules.HostRemove, listed.Seat))
		require.Equal(t, "removed", cw.row(seatConcept, guest.SeatID)["status"])
		require.Equal(t, []string{hostSess.SeatID}, rosterSeats(cw.roster(hostSess)), "a removed seat is still listed in the call")
	})

	// THE SEAT DECIDES, NOT THE SESSION'S KIND: a roster is the call of a host seat in it.
	t.Run("a guest's session, a session seated nowhere and a host who has left read no roster", func(t *testing.T) {
		cw := newCallWorld(t, "rosterguest", 50)
		if cw == nil {
			return
		}
		ctx := context.Background()
		hostSess, started := cw.started("Host")
		_, guest, err := cw.join(started.Call.GuestToken, "Guest")
		require.NoError(t, err)
		require.Nil(t, cw.roster(guest), "a guest's seat read the roster")
		require.Nil(t, cw.roster(cw.host()), "a session seated nowhere read a roster")

		// The guest keeps the call open, so the host is outside a call that still runs.
		require.NoError(t, cw.svc.Leave(ctx, hostSess))
		require.Nil(t, cw.roster(hostSess), "a host who left read the roster")

		_, err = cw.svc.SessionCall(ctx, callrules.Session{Token: strings.Repeat("a", 32), Site: cw.ref})
		require.ErrorIs(t, err, callrules.ErrNoSession, "a session that opens nothing is not a session with no roster")
	})

	t.Run("an ended call's host session reads no roster", func(t *testing.T) {
		cw := newCallWorld(t, "rosterended", 50)
		if cw == nil {
			return
		}
		hostSess, _ := cw.started("Host")
		require.NotNil(t, cw.roster(hostSess))
		require.NoError(t, cw.act(hostSess, callrules.HostEnd, ""))
		require.Nil(t, cw.roster(hostSess), "an ended call's host read its roster")
	})

	// A HOST IS OFFERED THE ONE LOCK CONTROL THAT CHANGES THE CALL, so the roster says whether
	// the call is locked, read fresh with the rest of it: a lock another host made shows on
	// this host's next poll, and so does the unlock.
	t.Run("it says whether the call is locked, whichever host locked it", func(t *testing.T) {
		cw := newCallWorld(t, "rosterlock", 50)
		if cw == nil {
			return
		}
		hostSess, started := cw.started("Host")
		require.False(t, cw.roster(hostSess).Locked, "a call nobody locked reads as locked")

		in, cohost, err := cw.joinBy(callrules.JoinBody{HostToken: started.Call.HostToken, Name: "Co-host"})
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
		require.NoError(t, cw.act(cohost, callrules.HostLock, ""))
		require.True(t, cw.roster(hostSess).Locked, "a call another host locked reads as unlocked")
		require.True(t, cw.roster(cohost).Locked, "a call this host locked reads as unlocked")

		require.NoError(t, cw.act(hostSess, callrules.HostUnlock, ""))
		require.False(t, cw.roster(cohost).Locked, "a call another host unlocked reads as locked")
	})

	// LINE ORDER: a seat with a place in the media server's line keeps it when the lock lifts,
	// while one held behind the lock from the start has none and asks at the back. So the one
	// in line comes first, although the other began waiting earlier.
	t.Run("the waiting seats are listed in line order, a place in line before one behind the lock", func(t *testing.T) {
		cw := newCallWorld(t, "rosterline", 50)
		if cw == nil {
			return
		}
		ctx := context.Background()
		hostSess, started := cw.started("Host")
		require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))
		cw.clock.Advance(time.Second)
		lockedAt := cw.clock.Now()
		res, behind, err := cw.join(started.Call.GuestToken, "Behind the lock")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeWait, res.Outcome)

		cut := testMediaConfig()
		cut.Seats = cw.admittedIn(started.Call.ID)
		_, err = cw.in.Bootstrap(ctx, cut)
		require.NoError(t, err)
		require.NoError(t, cw.act(hostSess, callrules.HostUnlock, ""))
		cw.clock.Advance(time.Second)
		queuedAt := cw.clock.Now()
		res, inLine, err := cw.join(started.Call.GuestToken, "In line")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeQueue, res.Outcome)
		require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))

		require.Equal(t, []callrules.RosterWaiter{
			{Seat: inLine.SeatID, DisplayName: "In line", Kind: "person", Since: queuedAt},
			{Seat: behind.SeatID, DisplayName: "Behind the lock", Kind: "person", Since: lockedAt},
		}, cw.roster(hostSess).Waiting)
	})

	// A SEAT THAT LEFT AND COMES BACK TO WAIT is waiting since it came back, not since the
	// first time it waited.
	t.Run("a seat that comes back to wait is waiting since it came back", func(t *testing.T) {
		cw := newCallWorld(t, "rosteragain", 50)
		if cw == nil {
			return
		}
		ctx := context.Background()
		hostSess, started := cw.started("Host")
		in, guest, err := cw.join(started.Call.GuestToken, "Guest")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
		require.NoError(t, cw.svc.Leave(ctx, guest))
		require.NoError(t, cw.act(hostSess, callrules.HostLock, ""))
		cw.clock.Advance(time.Minute)
		back := cw.clock.Now()
		res, again, err := cw.rejoin(guest, started.Call.GuestToken)
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeWait, res.Outcome)
		require.Equal(t, guest.SeatID, again.SeatID, "the rejoin took a second seat")

		require.Equal(t, []callrules.RosterWaiter{{Seat: guest.SeatID, DisplayName: "Again", Kind: "person", Since: back}}, cw.roster(hostSess).Waiting)
	})
}

// ---------------------------------------------------------------------------
// what a seat is told about its call
// ---------------------------------------------------------------------------

// sessionCall is what a session's own ask about itself says of its call, which must not
// fail.
func (cw *callWorld) sessionCall(sess callrules.Session) callrules.SeatCall {
	cw.t.Helper()
	call, err := cw.svc.SessionCall(context.Background(), sess)
	require.NoError(cw.t, err)
	return call
}

// TestASeatIsToldWhetherItHostsAndWhenTheCountdownEnds: every seat of an open call learns
// from its own session (GET /session) whether it hosts the call now -- the hand-over to a
// key holder included, which hands out no new pass -- and, while the host-left countdown
// runs, when it ends unless a host comes back: the deadline the countdown itself ends the
// call at (calls.HostLeft), the same instant for every seat. A host back in the call stops
// it, a booked call whose host never came has none before its booked end, and an ended call
// tells nobody anything. The host's roster comes with its host flag, and only then.
func TestASeatIsToldWhetherItHostsAndWhenTheCountdownEnds(t *testing.T) {
	t.Run("the host-left countdown, and a host back in the call", func(t *testing.T) {
		cw := newCallWorld(t, "seatcountdown", 50)
		if cw == nil {
			return
		}
		ctx := context.Background()
		grace := callrules.DefaultPolicy().HostLeftGrace
		hostSess, started := cw.started("Host")
		in, guest, err := cw.join(started.Call.GuestToken, "Guest")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
		cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(in.Identity))

		host := cw.sessionCall(hostSess)
		require.True(t, host.Host, "the host seat in the call is not told it hosts")
		require.NotNil(t, host.Roster, "a host is told it hosts but given no roster")
		require.True(t, host.CountdownEndsAt.IsZero(), "a countdown runs with the host in the call")
		opened := rowTime(cw.row(roomConcept, started.Call.ID), "openedAt")
		guestCall := cw.sessionCall(guest)
		require.NotEmpty(t, guestCall.LeaveToken)
		require.Equal(t, callrules.SeatCall{RoomID: started.Call.ID, Identity: in.Identity, LeaveToken: guestCall.LeaveToken, Title: callTitle("", opened), OpenedAt: opened}, guestCall, "a guest was given host controls or a countdown while the host is in the call")

		cw.clock.Advance(time.Minute)
		left := cw.clock.Now()
		require.NoError(t, cw.svc.Leave(ctx, hostSess))
		require.Equal(t, callrules.SeatCall{RoomID: started.Call.ID, Identity: in.Identity, LeaveToken: guestCall.LeaveToken, CountdownKind: "host_left", Title: callTitle("", opened), OpenedAt: opened, CountdownEndsAt: left.Add(grace)}, cw.sessionCall(guest),
			"a guest is not told when the call ends now that the host left")
		require.Equal(t, callrules.SeatCall{}, cw.sessionCall(hostSess), "a host who left is told about a call it is not in")

		back, _, err := cw.svc.Join(ctx, cw.ref, &hostSess, callrules.JoinBody{Name: "Host", Confirm: confirmed})
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
		require.True(t, cw.sessionCall(guest).CountdownEndsAt.IsZero(), "the host came back and the guest is still counted down")
		require.True(t, cw.sessionCall(hostSess).Host)

		// Run out and not yet ended by a pass: the deadline still stands, now past, so a page
		// can say the call is ending. The pass ends it, and then nobody is told anything.
		require.NoError(t, cw.svc.Leave(ctx, hostSess))
		again := cw.clock.Now()
		cw.advanceTo(again.Add(grace + time.Second))
		require.Equal(t, again.Add(grace), cw.sessionCall(guest).CountdownEndsAt, "a countdown that ran out is not told as one")
		cw.reconcile()
		require.Equal(t, "ended", cw.row(roomConcept, started.Call.ID)["status"])
		require.Equal(t, callrules.SeatCall{}, cw.sessionCall(guest), "a seat of an ended call is told about it")
	})

	t.Run("a key holder the call is handed to is told it hosts it", func(t *testing.T) {
		cw := newCallWorld(t, "seathandover", 50)
		if cw == nil {
			return
		}
		ctx := context.Background()
		hostSess, started := cw.started("Host")
		keyRes, keyHolder := cw.keyHolderJoin(started.Call.GuestToken, "Key holder")
		require.Equal(t, callrules.OutcomeAdmit, keyRes.Outcome)
		cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(keyRes.Identity))
		require.False(t, cw.sessionCall(keyHolder).Host, "a key holder is told it hosts before the call is handed to it")

		require.NoError(t, cw.svc.Leave(ctx, hostSess))
		cw.reconcile()
		handed := cw.sessionCall(keyHolder)
		require.True(t, handed.Host, "the key holder the call was handed to is not told it hosts")
		require.NotNil(t, handed.Roster)
		require.True(t, handed.CountdownEndsAt.IsZero(), "a call with a host in it still counts down")
	})

	t.Run("a booked call whose host never came counts down only after its booked end", func(t *testing.T) {
		cw := newCallWorld(t, "seatbooked", 50)
		if cw == nil {
			return
		}
		grace := callrules.DefaultPolicy().HostLeftGrace
		sc := cw.booked(cw.host(), 5*time.Minute, 30, "Unhosted")
		cw.advanceTo(sc.Start)
		waits, candidate, err := cw.join(sc.GuestToken, "Candidate")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeWait, waits.Outcome)
		cw.advanceTo(sc.Start.Add(grace + time.Minute))
		require.Equal(t, callrules.SeatCall{RoomID: candidate.RoomID, Title: "Unhosted"}, cw.sessionCall(candidate), "a countdown runs inside a booked call's window")
		cw.advanceTo(sc.End)
		require.Equal(t, callrules.SeatCall{RoomID: candidate.RoomID, Title: "Unhosted", CountdownKind: "host_left", CountdownEndsAt: sc.End.Add(grace)}, cw.sessionCall(candidate),
			"a seat waiting past the booked end is not told when the call ends")
	})

	t.Run("a session seated nowhere is told nothing, and one that opens nothing is no session", func(t *testing.T) {
		cw := newCallWorld(t, "seatnowhere", 50)
		if cw == nil {
			return
		}
		require.Equal(t, callrules.SeatCall{}, cw.sessionCall(cw.host()))
		_, err := cw.svc.SessionCall(context.Background(), callrules.Session{Token: strings.Repeat("a", 32), Site: cw.ref})
		require.ErrorIs(t, err, callrules.ErrNoSession)
	})
}
