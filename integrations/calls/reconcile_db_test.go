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
// when it was never seen, once a pass's lifetime has gone by -- before that it may still
// be connecting. A participant the call never let in is disconnected, and so is one
// whose seat left while its connection lingered.
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
	// disconnected. The seat that has not connected yet keeps its place.
	cw.reconcile()
	for _, seatID := range []string{hostSess.SeatID, g1.SeatID, g2.SeatID} {
		seat := cw.row(seatConcept, seatID)
		require.Equal(t, "in_call", seat["status"], "a connected seat is not in the call")
		require.Equal(t, t0, rowTime(seat, "lastSeenAt"))
	}
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: BootstrapMediaServerID, Room: roomID, Identity: stranger},
		"a participant the call never let in was left connected")
	require.Equal(t, "admitted", cw.row(seatConcept, g3.SeatID)["status"], "a seat still connecting on its pass was closed")

	// Seen again inside two minutes, nothing is written.
	versions := cw.versions(seatConcept, g1.SeatID)
	cw.clock.Advance(90 * time.Second)
	cw.reconcile()
	require.Equal(t, versions, cw.versions(seatConcept, g1.SeatID), "a seat seen again within two minutes was written")
	require.Equal(t, "admitted", cw.row(seatConcept, g3.SeatID)["status"])

	// A missed leave: the first drops off without a word. At two minutes the others are
	// seen again, and the two seats nobody is connected for are closed: the first at its
	// last seen time, the one that never connected at its admission.
	cw.clock.Advance(30 * time.Second)
	cw.media.setPresent(roomID, person(started.Join.Identity), person(in2.Identity))
	cw.reconcile()
	gone := cw.row(seatConcept, g1.SeatID)
	require.Equal(t, "left", gone["status"], "a seat stuck in the call with nobody connected was not closed")
	require.Equal(t, t0, rowTime(gone, "leftAt"), "a vanished seat was not closed at its last seen time")
	never := cw.row(seatConcept, g3.SeatID)
	require.Equal(t, "left", never["status"])
	require.Equal(t, joined, rowTime(never, "leftAt"), "a seat never seen was not closed at its admission")
	require.Equal(t, cw.clock.Now(), rowTime(cw.row(seatConcept, g2.SeatID), "lastSeenAt"), "a seat seen two minutes on was not refreshed")
	require.Equal(t, "in_call", cw.row(seatConcept, hostSess.SeatID)["status"])

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
