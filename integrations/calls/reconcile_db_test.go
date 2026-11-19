package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"

	"github.com/znasllc-io/memql/component/auth"
)

// reconcile_db_test.go -- reconciliation (reconcile.go) against a real engine and
// database: every open call's seats held to what its media server shows, the host-left
// countdown applied, and the queue's expired entries marked. The media server is the
// fake of service_db_test.go, and fakeMedia.setPresent is its participant list.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the skip
// into a failure.

// TestReconcileRepairsMissedAndReorderedEvents: reconciliation holds a call's seats to
// the media server's participant list, whatever the events said. A seat connected is in
// the call and seen now; seen again within two minutes, nothing is written. A seat stuck
// in the call with nobody connected is closed at its last seen time, or at its admission
// when it was never seen, once it has been missing for a pass's lifetime, counted from the
// first pass that missed it -- before that it may still be connecting, or coming back. A
// participant the call never let in is disconnected, and so is one whose seat left while
// its connection lingered.
func TestReconcileRepairsMissedAndReorderedEvents(t *testing.T) {
	cw := newCallWorld(t, "reconcileseats", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	link := started.Call.GuestToken
	in1, g1, err := cw.join(link, "First")
	require.NoError(t, err)
	in2, g2, err := cw.join(link, "Second")
	require.NoError(t, err)
	_, g3, err := cw.join(link, "Never connects")
	require.NoError(t, err)
	const stranger = "stranger-identity"
	cw.media.setPresent(roomID, person(started.Join.Identity), person(in1.Identity), person(in2.Identity), person(stranger))
	t0 := cw.clock.Now()
	joined := rowTime(cw.row(seatConcept, g3.SeatID), "joinedAt")

	// Missed joins: whoever is connected is in the call, seen now, and the stranger is
	// disconnected. The seat that has not connected yet keeps its place: the pass records
	// that it found it missing, and closes nothing.
	cw.reconcile()
	for _, seatID := range []string{hostSess.SeatID, g1.SeatID, g2.SeatID} {
		seat := cw.row(seatConcept, seatID)
		require.Equal(t, "in_call", seat["status"], "a connected seat is not in the call")
		require.Equal(t, t0, rowTime(seat, "lastSeenAt"))
		require.True(t, rowTime(seat, "absentSince").IsZero(), "a connected seat was recorded missing")
	}
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: BootstrapMediaServerID, Room: roomID, Identity: stranger},
		"a participant the call never let in was left connected")
	require.Equal(t, "admitted", cw.row(seatConcept, g3.SeatID)["status"], "a seat still connecting on its pass was closed")
	require.Equal(t, t0, rowTime(cw.row(seatConcept, g3.SeatID), "absentSince"), "the first pass to miss a seat did not record it")

	// Seen again inside two minutes, nothing is written.
	versions := cw.versions(seatConcept, g1.SeatID)
	cw.clock.Advance(90 * time.Second)
	cw.reconcile()
	require.Equal(t, versions, cw.versions(seatConcept, g1.SeatID), "a seat seen again within two minutes was written")
	require.Equal(t, "admitted", cw.row(seatConcept, g3.SeatID)["status"])

	// A missed leave: the first drops off without a word. At two minutes the others are
	// seen again, and the seat that never connected, missing since the first pass, is
	// closed at its admission. The one that dropped off was last seen two minutes ago --
	// the age a present seat's stamp routinely is -- so this pass, the first to miss it,
	// only records that, and it is still in the call.
	cw.clock.Advance(30 * time.Second)
	cw.media.setPresent(roomID, person(started.Join.Identity), person(in2.Identity))
	cw.reconcile()
	never := cw.row(seatConcept, g3.SeatID)
	require.Equal(t, "left", never["status"])
	require.Equal(t, joined, rowTime(never, "leftAt"), "a seat never seen was not closed at its admission")
	missed := cw.row(seatConcept, g1.SeatID)
	require.Equal(t, "in_call", missed["status"], "one pass that missed a seat closed it")
	require.Equal(t, cw.clock.Now(), rowTime(missed, "absentSince"))
	require.Equal(t, cw.clock.Now(), rowTime(cw.row(seatConcept, g2.SeatID), "lastSeenAt"), "a seat seen two minutes on was not refreshed")
	require.Equal(t, "in_call", cw.row(seatConcept, hostSess.SeatID)["status"])

	// It stays gone for a pass's lifetime after that: closed at its last seen time.
	cw.clock.Advance(callrules.DefaultPolicy().PassTTL)
	cw.reconcile()
	gone := cw.row(seatConcept, g1.SeatID)
	require.Equal(t, "left", gone["status"], "a seat stuck in the call with nobody connected was not closed")
	require.Equal(t, t0, rowTime(gone, "leftAt"), "a vanished seat was not closed at its last seen time")

	// A crossed leave: the second left, but its connection lingers on the pass it still
	// holds. Reconciliation disconnects it and leaves the seat as it left.
	require.NoError(t, cw.svc.Leave(ctx, g2))
	cw.media.setPresent(roomID, person(started.Join.Identity), person(in2.Identity))
	before := len(cw.media.removals())
	cw.reconcile()
	require.Len(t, cw.media.removals(), before+1)
	require.Equal(t, fakeMediaRemoval{Server: BootstrapMediaServerID, Room: roomID, Identity: in2.Identity}, cw.media.removals()[before])
	require.Equal(t, "left", cw.row(seatConcept, g2.SeatID)["status"], "reconciliation took a seat that left back into the call")
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a call with its host in it was ended")
}

// TestAPassThatMissesSeatsClosesNothingUntilTheyStayGone: a seat closes only once it has
// been missing for a pass's lifetime, counted from the first pass that found it missing,
// never from the age of its last stamp (ruling final I2). Every seat of a call is refreshed
// in the same pass, so every stamp in it is two minutes old at once; when the media server
// restarts, the pass that lands before anybody is back finds every seat missing, and must
// close none of them, or the call ends empty with everybody about to return. The seats
// that come back within the grace keep their places, and their records of going missing
// are cleared; a seat that stays gone for the grace is closed at its last sign of life.
func TestAPassThatMissesSeatsClosesNothingUntilTheyStayGone(t *testing.T) {
	cw := newCallWorld(t, "reconcilemissed", 50)
	if cw == nil {
		return
	}
	grace := callrules.DefaultPolicy().PassTTL
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	in, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	everybody := []MediaParticipant{person(started.Join.Identity), person(in.Identity)}
	cw.media.setPresent(roomID, everybody...)
	cw.reconcile()
	seen := cw.clock.Now()

	// The media server restarts with both stamps two minutes old, and a pass lands before
	// anybody is back: it closes nothing.
	cw.clock.Advance(2 * time.Minute)
	cw.media.setPresent(roomID)
	cw.reconcile()
	missedAt := cw.clock.Now()
	for _, seatID := range []string{hostSess.SeatID, guest.SeatID} {
		seat := cw.row(seatConcept, seatID)
		require.Equal(t, "in_call", seat["status"], "one pass that missed a seat closed it")
		require.Equal(t, missedAt, rowTime(seat, "absentSince"), "the pass did not record when it found the seat missing")
	}
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a pass during a media server restart ended the call")
	require.True(t, rowTime(cw.row(roomConcept, roomID), "hostLeftAt").IsZero(), "a host only missing started the countdown")

	// Everybody is back inside the grace: the seats stand, and nothing records them missing.
	cw.clock.Advance(grace - time.Second)
	cw.media.setPresent(roomID, everybody...)
	cw.reconcile()
	for _, seatID := range []string{hostSess.SeatID, guest.SeatID} {
		seat := cw.row(seatConcept, seatID)
		require.Equal(t, "in_call", seat["status"], "a seat back within the grace was closed")
		require.True(t, rowTime(seat, "absentSince").IsZero(), "a seat seen again is still recorded missing")
	}

	// The guest drops off for good: missed, still missing inside the grace, then closed at
	// its last sign of life once it has been missing for the grace.
	cw.clock.Advance(2 * time.Minute)
	lastSeen := rowTime(cw.row(seatConcept, guest.SeatID), "lastSeenAt")
	require.False(t, lastSeen.Before(seen), "the guest's stamp went backwards")
	cw.media.setPresent(roomID, person(started.Join.Identity))
	cw.reconcile()
	cw.clock.Advance(grace - time.Second)
	cw.reconcile()
	require.Equal(t, "in_call", cw.row(seatConcept, guest.SeatID)["status"], "a seat missing for less than the grace was closed")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	gone := cw.row(seatConcept, guest.SeatID)
	require.Equal(t, "left", gone["status"], "a seat missing for the grace was not closed")
	require.Equal(t, lastSeen, rowTime(gone, "leftAt"), "a vanished seat was not closed at its last sign of life")
	require.Equal(t, "open", cw.row(roomConcept, roomID)["status"], "a call with its host in it was ended")
}

// TestReconcileExpiresQueueEntriesAndPromotes: reconciliation marks expired the queue
// entries of every call on a media server whose waiters stopped polling, whichever call
// each is for, and admits nobody: it mints no pass. When a seat frees, the next waiter
// in line is let in on its own poll, ahead of nobody it should not be.
func TestReconcileExpiresQueueEntriesAndPromotes(t *testing.T) {
	cw := newCallWorld(t, "reconcilequeue", 3)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, a := cw.started("Host A") // seat 1 of 3
	_, b := cw.started("Host B") // seat 2
	inA1, a1, err := cw.join(a.Call.GuestToken, "A1")
	require.NoError(t, err) // seat 3: the server is full
	cw.media.setPresent(a.Call.ID, person(a.Join.Identity), person(inA1.Identity))
	cw.media.setPresent(b.Call.ID, person(b.Join.Identity))

	q2, a2, err := cw.join(a.Call.GuestToken, "A2")
	require.NoError(t, err)
	require.Equal(t, 1, q2.Position)
	qb1, b1, err := cw.join(b.Call.GuestToken, "B1")
	require.NoError(t, err)
	require.Equal(t, 2, qb1.Position)
	q3, a3, err := cw.join(a.Call.GuestToken, "A3")
	require.NoError(t, err)
	require.Equal(t, 3, q3.Position)

	// A2 and B1 stop polling; A3 keeps at it.
	ttl := callrules.DefaultPolicy().QueueEntryTTL
	cw.clock.Advance(ttl - time.Minute)
	_, err = cw.svc.Queue(ctx, a3)
	require.NoError(t, err)
	cw.clock.Advance(2 * time.Minute)
	cw.reconcile()
	require.Equal(t, "expired", cw.entryOf(a2.SeatID)["status"], "a quiet waiter's entry was not marked expired")
	require.Equal(t, "expired", cw.entryOf(b1.SeatID)["status"], "a quiet waiter of another call on the server was not marked expired")
	require.Equal(t, "waiting", cw.entryOf(a3.SeatID)["status"], "a waiter still polling lost its place")
	require.Equal(t, "waiting", cw.row(seatConcept, a3.SeatID)["status"], "reconciliation let a waiter in: it mints no pass")

	// A seat frees, and the next in line is let in on its own poll.
	require.NoError(t, cw.svc.Leave(ctx, a1))
	in, err := cw.svc.Queue(ctx, a3)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome, "the waiter first in line once the quiet ones expired was not let in")
	require.NotEmpty(t, in.Pass)
	verifyPass(t, in.Pass, testMediaConfig().APIKey, testAPISecret)
}

// TestReconcileRunsOnlyForItsAutomation: the callsReconcile builtin acts for the engine's
// own reconcileCalls automation, at internal origin, and refuses every other caller --
// a person, the cluster's operator, the cluster acting outside that automation, another
// automation, and an automation of that name from anywhere but the engine's own tree --
// before it reads anything. Made through the engine's dispatch, a person is refused by
// the same handler.
func TestReconcileRunsOnlyForItsAutomation(t *testing.T) {
	cw := newCallWorld(t, "reconcilegate", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, started := cw.started("Host")
	in, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(in.Identity), person("stranger-identity"))

	// Each actor is minted with its origin stated as a constant, as the maintenance
	// list's gate asks of a test (component/automations maintenance_actor_gate_test.go).
	internal := func(ac *auth.AccessContext) context.Context {
		return auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, ac))
	}
	cluster, err := asCluster(ctx)
	require.NoError(t, err)
	for name, caller := range map[string]context.Context{
		"the site's owner":                       actorContext(cw.owner, auth.RoleWriter),
		"the cluster's operator":                 actorContext(cw.operator, auth.RoleOwner),
		"the cluster acting":                     cluster,
		"another automation":                     internal(auth.AutomationActor(auth.AutomationOriginEngine, "reconcileMirroredDomains")),
		"a pack's automation of the same name":   internal(auth.AutomationActor(auth.AutomationOriginOther, reconcileAutomation)),
		"the automation without internal origin": auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, auth.AutomationActor(auth.AutomationOriginEngine, reconcileAutomation))),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cw.calls.handleReconcile(caller, nil, 0)
			require.Error(t, err, "%s ran reconciliation", name)
		})
	}
	require.Empty(t, cw.media.removals(), "a refused caller reached the media server")
	require.Equal(t, "admitted", cw.row(seatConcept, guest.SeatID)["status"], "a refused caller wrote a seat")

	_, err = cw.builtin(actorContext(cw.owner, auth.RoleWriter), "callsReconcile", nil)
	require.Error(t, err, "a person ran reconciliation through the engine")

	_, err = cw.calls.handleReconcile(internal(auth.AutomationActor(auth.AutomationOriginEngine, reconcileAutomation)), nil, 0)
	require.NoError(t, err)
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: BootstrapMediaServerID, Room: started.Call.ID, Identity: "stranger-identity"},
		"the automation's own call did not reconcile")
}
