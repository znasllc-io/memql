package calls

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/database"
)

// A host who abandons the capacity queue must not leave guests an unlimited ad hoc
// call. These cases use real rows and gates, then read and reconcile on a service
// with no state from Start/Leave/Queue, as a different bff replica would.
func TestAdHocCallWhoseQueuedHostGivesUpHasBoundedGrace(t *testing.T) {
	for _, departure := range []string{"leave", "stop_polling"} {
		t.Run(departure, func(t *testing.T) {
			cw, other, host, started, guest, deadline := abandonedQueuedHost(t, departure)
			if cw == nil {
				return
			}
			ctx := context.Background()
			view, err := other.SessionCall(ctx, guest)
			require.NoError(t, err)
			require.Equal(t, deadline, view.CountdownEndsAt, "another replica did not show the abandonment deadline before a reconcile pass")

			cw.advanceTo(deadline.Add(-time.Second))
			require.NoError(t, other.Reconcile(ctx))
			require.Equal(t, "open", cw.row(roomConcept, started.Call.ID)["status"])
			cw.clock.Advance(time.Second)
			require.NoError(t, other.Reconcile(ctx))
			ended := cw.row(roomConcept, started.Call.ID)
			require.Equal(t, "ended", ended["status"])
			require.Equal(t, callrules.EndReasonHostLeft, ended["endReason"])
			require.Contains(t, cw.media.deletions(), fakeMediaRef{Server: BootstrapMediaServerID, Room: started.Call.ID})
			require.Equal(t, seatLeft, cw.row(seatConcept, guest.SeatID)["status"])
			_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host again", Confirm: confirmed})
			require.NoError(t, err, "the abandoned call still held the key's one-call slot")
		})
	}
}

// A renewed polling lease is not a host returning to the media room. Repeated
// requeue/leave requests must not erase or restart a countdown guests already see.
func TestRequeueingAnAbandonedHostDoesNotRestartGrace(t *testing.T) {
	for _, departure := range []string{"leave", "stop_polling"} {
		t.Run(departure, func(t *testing.T) {
			cw, other, host, started, guest, deadline := abandonedQueuedHost(t, departure)
			if cw == nil {
				return
			}
			ctx := context.Background()
			queued, _, err := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{Name: "Host", Confirm: confirmed})
			require.NoError(t, err)
			require.Equal(t, callrules.OutcomeQueue, queued.Outcome)
			view, err := other.SessionCall(ctx, guest)
			require.NoError(t, err)
			require.Equal(t, deadline, view.CountdownEndsAt, "requeueing erased the abandonment deadline")
			cw.clock.Advance(time.Minute)
			_, err = cw.svc.Queue(ctx, host)
			require.NoError(t, err)
			require.NoError(t, cw.svc.Leave(ctx, host))
			view, err = other.SessionCall(ctx, guest)
			require.NoError(t, err)
			require.Equal(t, deadline, view.CountdownEndsAt, "a second leave restarted the abandonment deadline")
			cw.advanceTo(deadline)
			require.NoError(t, other.Reconcile(ctx))
			require.Equal(t, callrules.EndReasonHostLeft, cw.row(roomConcept, started.Call.ID)["endReason"])
		})
	}
}

func TestAbandonedQueuedHostCanReturnBeforeTheDeadline(t *testing.T) {
	cw, other, host, started, guest, deadline := abandonedQueuedHost(t, "leave")
	if cw == nil {
		return
	}
	ctx := context.Background()
	// The guest holds the one media seat. Give the returning host room to enter.
	cfg := testMediaConfig()
	cfg.Seats = 2
	_, err := cw.in.Bootstrap(ctx, cfg)
	require.NoError(t, err)
	back, _, err := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
	cw.media.setPresent(started.Call.ID, person(back.Identity), person(cw.row(seatConcept, guest.SeatID)["identity"].(string)))
	view, err := other.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.True(t, view.CountdownEndsAt.IsZero(), "the admitted host did not clear the derived countdown")
	cw.advanceTo(deadline.Add(time.Minute))
	require.NoError(t, other.Reconcile(ctx))
	require.Equal(t, "open", cw.row(roomConcept, started.Call.ID)["status"])
}

func TestAbandonedQueuedHostHandsOverToAnActiveKeyHolder(t *testing.T) {
	cw, other, _, started, _, _ := abandonedQueuedHost(t, "stop_polling")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cfg := testMediaConfig()
	cfg.Seats = 2
	_, err := cw.in.Bootstrap(ctx, cfg)
	require.NoError(t, err)
	joined, holder := cw.keyHolderJoin(started.Call.GuestToken, "New host")
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	require.NoError(t, other.Reconcile(ctx))
	view, err := other.SessionCall(ctx, holder)
	require.NoError(t, err)
	require.True(t, view.Host)
	require.True(t, view.CountdownEndsAt.IsZero())
	require.Equal(t, holder.SeatID, bare(cw.row(roomConcept, started.Call.ID)["hostSeatId"]))
	require.NoError(t, other.Host(ctx, holder, callrules.HostActionRequest{Action: callrules.HostLock}))
}

func abandonedQueuedHost(t *testing.T, departure string) (*callWorld, *Service, callrules.Session, callrules.StartResult, callrules.Session, time.Time) {
	t.Helper()
	cw := newCallWorld(t, "abandon", 1)
	if cw == nil {
		return nil, nil, callrules.Session{}, callrules.StartResult{}, callrules.Session{}, time.Time{}
	}
	ctx := context.Background()
	filler, _ := cw.started("Using the server")
	host := cw.host()
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Queued host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, started.Join.Outcome)
	queued, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, queued.Outcome)
	cw.clock.Advance(time.Minute)
	if departure == "leave" {
		require.NoError(t, cw.svc.Leave(ctx, host))
	} else {
		// A refreshed host lease proves the clock is the last poll, not room creation.
		_, err = cw.svc.Queue(ctx, host)
		require.NoError(t, err)
		cw.clock.Advance(callrules.DefaultPolicy().QueueEntryTTL - time.Minute)
		_, err = cw.svc.Queue(ctx, guest)
		require.NoError(t, err)
		view, err := cw.svc.SessionCall(ctx, guest)
		require.NoError(t, err)
		require.True(t, view.CountdownEndsAt.IsZero(), "a host's current polling lease started a countdown")
		cw.clock.Advance(time.Minute)
	}
	deadline := cw.clock.Now().Add(callrules.DefaultPolicy().HostLeftGrace)
	require.NoError(t, cw.svc.Leave(ctx, filler))
	joined, err := cw.svc.Queue(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	cw.media.setPresent(started.Call.ID, person(joined.Identity))

	pool, err := database.NewGatePool(sharedBoot.dsn, database.DefaultGatePoolSize)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	other := New(Deps{
		Engine: liveEngine{cw.eng}, GateDB: func() *sql.DB { return pool },
		ResolveSystemSecret: cw.eng.ResolveSystemSecret,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                 cw.clock.Now, NewMediaBackend: cw.media.backend,
	}).Service()
	return cw, other, host, started, guest, deadline
}
