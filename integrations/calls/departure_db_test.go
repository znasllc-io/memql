package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func departureAsk(t *testing.T, svc *Service, sess callrules.Session, mode, id string) callrules.LeaveRequest {
	t.Helper()
	call, err := svc.SessionCall(context.Background(), sess)
	require.NoError(t, err)
	require.NotEmpty(t, call.LeaveToken)
	require.NotEmpty(t, call.Identity)
	req := callrules.LeaveRequest{Mode: mode, LeaveToken: call.LeaveToken, IdempotencyKey: id}
	wire, err := json.Marshal(req)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), int(memql.CallsSmallBodyBytes), "bound leaves must fit the existing route body budget")
	return req
}
func configuredDepartureRoom(t *testing.T, cw *callWorld, n, minutes int) (callrules.Session, callrules.StartResult) {
	t.Helper()
	k, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, k, n, minutes)
	started, err := cw.svc.Start(context.Background(), host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "departure-start-0001"})
	require.NoError(t, err)
	return host, started
}
func TestDepartureKeepRunsToOriginalDeadlineAndStillEndsEmpty(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			cw := newCallWorld(t, "departure-keep", 120)
			if cw == nil {
				return
			}
			ctx := context.Background()
			host, start := configuredDepartureRoom(t, cw, 2, 10)
			guest, guestSession, err := cw.join(start.Call.GuestToken, "Guest")
			require.NoError(t, err)
			began := cw.clock.Now()
			cw.media.setPresent(start.Call.ID, MediaParticipant{SID: "host", Identity: start.Join.Identity, JoinedAt: began}, MediaParticipant{SID: "guest", Identity: guest.Identity, JoinedAt: began})
			cw.clock.Advance(time.Minute)
			ask := departureAsk(t, cw.svc, host, callrules.LeaveKeepRunning, "keep-running-0001")
			require.NoError(t, cw.svc.LeaveWithOptions(ctx, host, ask))
			cw.clock.Advance(6 * time.Minute)
			replica := allowanceReplica(t, cw)
			require.NoError(t, replica.LeaveWithOptions(ctx, host, ask))
			require.NoError(t, replica.Reconcile(ctx))
			room := roomFromRow(cw.row(roomConcept, start.Call.ID))
			require.True(t, room.open())
			require.Equal(t, began.Add(10*time.Minute), room.FundingDeadline)
			call, err := replica.SessionCall(ctx, guestSession)
			require.NoError(t, err)
			require.True(t, call.CountdownEndsAt.IsZero())
			if empty {
				require.NoError(t, replica.LeaveWithOptions(ctx, guestSession, departureAsk(t, replica, guestSession, "", "guest-leave-0001")))
			} else {
				cw.clock.Advance(3 * time.Minute)
				require.NoError(t, replica.Reconcile(ctx))
			}
			room = roomFromRow(cw.row(roomConcept, start.Call.ID))
			require.False(t, room.open())
			require.NotNil(t, room.UsageCostCents)
			wallet, err := replica.Account(ctx, cw.ref, host.Token, nil)
			require.NoError(t, err)
			require.Zero(t, *wallet.Account.ReservedCents)
		})
	}
}
func TestDepartureWrapUpSurvivesOtherHostRejoinAndDelayedSettlement(t *testing.T) {
	cw := newCallWorld(t, "departure-firm", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := configuredDepartureRoom(t, cw, 3, 60)
	co, coSession, err := cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Co-host", Confirm: confirmed})
	require.NoError(t, err)
	guest, guestSession, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	began := cw.clock.Now()
	cw.media.setPresent(start.Call.ID, MediaParticipant{SID: "host", Identity: start.Join.Identity, JoinedAt: began}, MediaParticipant{SID: "co", Identity: co.Identity, JoinedAt: began}, MediaParticipant{SID: "guest", Identity: guest.Identity, JoinedAt: began})
	cw.clock.Advance(time.Minute)
	deadline := cw.clock.Now().Add(5 * time.Minute)
	ask := departureAsk(t, cw.svc, host, callrules.LeaveWrapUp, "wrap-up-request-0001")
	var wg sync.WaitGroup
	answers := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); answers <- allowanceReplica(t, cw).LeaveWithOptions(ctx, host, ask) }()
	}
	wg.Wait()
	close(answers)
	for err := range answers {
		require.NoError(t, err)
	}
	call, err := cw.svc.SessionCall(ctx, coSession)
	require.NoError(t, err)
	require.True(t, call.Host)
	require.Equal(t, "wrap_up", call.CountdownKind)
	require.Equal(t, deadline, call.CountdownEndsAt)
	cw.clock.Advance(time.Minute)
	_, returned, err := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{Name: "Host again", Confirm: confirmed})
	require.NoError(t, err)
	removed := len(cw.media.removed)
	require.NoError(t, allowanceReplica(t, cw).LeaveWithOptions(ctx, returned, ask))
	require.Len(t, cw.media.removed, removed)
	current, err := cw.svc.SessionCall(ctx, returned)
	require.NoError(t, err)
	require.Equal(t, deadline, current.CountdownEndsAt)
	keep := departureAsk(t, cw.svc, coSession, callrules.LeaveKeepRunning, "cohost-keep-0001")
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, coSession, keep))
	current, err = cw.svc.SessionCall(ctx, guestSession)
	require.NoError(t, err)
	require.Equal(t, deadline, current.CountdownEndsAt)
	cw.clock.Advance(5 * time.Minute) // one minute after the firm deadline
	refused, _, err := cw.join(start.Call.GuestToken, "Too late")
	require.Error(t, err)
	require.Equal(t, "wrap_up", refused.Reason)
	require.NoError(t, allowanceReplica(t, cw).Reconcile(ctx))
	room := roomFromRow(cw.row(roomConcept, start.Call.ID))
	require.False(t, room.open())
	require.Equal(t, deadline, room.EndedAt)
	require.Equal(t, "wrap_up", room.EndReason)
	require.NotNil(t, room.UsageCostCents)
	require.EqualValues(t, 2, *room.UsageCostCents)
	wallet, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.Zero(t, *wallet.Account.ReservedCents)
}
func TestDepartureAuthorityAndFrozenGuestAdmission(t *testing.T) {
	cw := newCallWorld(t, "departure-authority", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := configuredDepartureRoom(t, cw, 2, 10)
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	guestAsk := departureAsk(t, cw.svc, guest, "", "guest-bound-0001")
	forged := guestAsk
	forged.Mode = callrules.LeaveWrapUp
	require.ErrorIs(t, cw.svc.LeaveWithOptions(ctx, guest, forged), callrules.ErrForbidden)
	require.ErrorIs(t, cw.svc.LeaveWithOptions(ctx, host, guestAsk), callrules.ErrInvalid)
	forged = guestAsk
	forged.LeaveToken += "x"
	require.ErrorIs(t, cw.svc.LeaveWithOptions(ctx, guest, forged), callrules.ErrInvalid)
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, guest, guestAsk))
	_, back, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Guest again", Confirm: confirmed})
	require.NoError(t, err)
	before := len(cw.media.removed)
	require.NoError(t, allowanceReplica(t, cw).LeaveWithOptions(ctx, back, guestAsk))
	require.Len(t, cw.media.removed, before)
	forged = guestAsk
	forged.IdempotencyKey = "new-nonce-old-token"
	require.ErrorIs(t, cw.svc.LeaveWithOptions(ctx, back, forged), callrules.ErrConflict)
	current, err := cw.svc.SessionCall(ctx, back)
	require.NoError(t, err)
	require.NotEmpty(t, current.LeaveToken)
	newAsk := departureAsk(t, cw.svc, back, "", "guest-bound-0002")
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, back, newAsk))
	require.ErrorIs(t, cw.svc.LeaveWithOptions(ctx, back, guestAsk), callrules.ErrConflict)
}
func TestDepartureAtomicWriteFailureAndShorterHardDeadline(t *testing.T) {
	cw := newCallWorld(t, "departure-atomic", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := configuredDepartureRoom(t, cw, 2, 10)
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	cw.clock.Advance(8 * time.Minute)
	ask := departureAsk(t, cw.svc, host, callrules.LeaveWrapUp, "wrap-up-atomic-0001")
	original := cw.svc.store.engine
	injected := &interruptedMeterEngine{Engine: original, match: "updateCallsSeat("}
	cw.svc.store.engine = injected
	require.Error(t, cw.svc.LeaveWithOptions(ctx, host, ask))
	require.True(t, injected.failed)
	call, err := cw.svc.SessionCall(ctx, host)
	require.NoError(t, err)
	require.True(t, call.Host)
	require.True(t, call.CountdownEndsAt.IsZero())
	cw.svc.store.engine = original
	require.NoError(t, allowanceReplica(t, cw).LeaveWithOptions(ctx, host, ask))
	call, err = cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, call.AllowanceEndsAt, call.CountdownEndsAt)
	cw.clock.Advance(3 * time.Minute)
	cw.media.failDelete = errors.New("provider unavailable")
	require.Error(t, cw.svc.Reconcile(ctx))
	require.True(t, roomFromRow(cw.row(roomConcept, start.Call.ID)).open())
	cw.media.failDelete = nil
	require.NoError(t, allowanceReplica(t, cw).Reconcile(ctx))
	room := roomFromRow(cw.row(roomConcept, start.Call.ID))
	require.False(t, room.open())
	require.Equal(t, room.FundingDeadline, room.EndedAt)
}

func TestDeparturePromotedGuestMustChooseAndLegacyEmptyStillWorks(t *testing.T) {
	cw := newCallWorld(t, "departure-promotion", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := configuredDepartureRoom(t, cw, 2, 10)
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	ask := departureAsk(t, cw.svc, guest, "", "promoted-guest-0001")
	// Presenting the authorized host invitation promotes this same seat without
	// changing its admission. A frozen former-guest request must still recheck role.
	_, promoted, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	err = cw.svc.LeaveWithOptions(ctx, promoted, ask)
	require.ErrorIs(t, err, callrules.ErrConflict)
	require.Equal(t, "leave_choice_required", callrules.ReasonOf(err))
	state, err := cw.svc.SessionCall(ctx, promoted)
	require.NoError(t, err)
	require.True(t, state.Host)
	// Old clients keep the original empty-body behavior, including hosts.
	require.NoError(t, cw.svc.Leave(ctx, promoted))
	require.NoError(t, cw.svc.Leave(ctx, host))
	require.False(t, roomFromRow(cw.row(roomConcept, start.Call.ID)).open())
}
func TestDepartureLegacyHostCanWrapUpButCannotKeepAnUnboundedRoom(t *testing.T) {
	cw := newCallWorld(t, "departure-legacy", 30)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := cw.started("Host")
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	keep := departureAsk(t, cw.svc, host, callrules.LeaveKeepRunning, "legacy-keep-0001")
	err = cw.svc.LeaveWithOptions(ctx, host, keep)
	require.ErrorIs(t, err, callrules.ErrConflict)
	require.Equal(t, "keep_running_unavailable", callrules.ReasonOf(err))
	wrap := departureAsk(t, cw.svc, host, callrules.LeaveWrapUp, "legacy-wrap-0001")
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, host, wrap))
	state, err := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, "wrap_up", state.CountdownKind)
	require.Equal(t, cw.clock.Now().Add(5*time.Minute), state.CountdownEndsAt)
}

func TestDepartureCommittedSeatRepairsPostWriteFailureWithoutRestartingTimer(t *testing.T) {
	cw := newCallWorld(t, "departure-repair", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := configuredDepartureRoom(t, cw, 2, 10)
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	ask := departureAsk(t, cw.svc, host, callrules.LeaveWrapUp, "repair-leave-0001")
	deadline := cw.clock.Now().Add(5 * time.Minute)
	interrupted, injected := brokenMeterService(cw, "hostLeftAt")
	require.Error(t, interrupted.LeaveWithOptions(ctx, host, ask))
	require.True(t, injected.failed)
	call, err := cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, deadline, call.CountdownEndsAt)
	cw.clock.Advance(time.Minute)
	require.NoError(t, allowanceReplica(t, cw).LeaveWithOptions(ctx, host, ask))
	call, err = cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, deadline, call.CountdownEndsAt)
	room := roomFromRow(cw.row(roomConcept, start.Call.ID))
	require.True(t, room.open())
}
func TestDepartureReplayCannotRemoveARejoinedWaitingGuest(t *testing.T) {
	cw := newCallWorld(t, "departure-waiting", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, start := configuredDepartureRoom(t, cw, 2, 10)
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	ask := departureAsk(t, cw.svc, guest, "", "waiting-replay-0001")
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, guest, ask))
	require.NoError(t, cw.act(host, callrules.HostLock, ""))
	joined, back, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, joined.Outcome)
	require.NoError(t, allowanceReplica(t, cw).LeaveWithOptions(ctx, back, ask))
	seat := seatFromRow(cw.row(seatConcept, back.SeatID))
	require.Equal(t, seatWaiting, seat.Status)
	// A locked waiting-room seat has no capacity-queue entry. Its next
	// poll must still find that same live wait after the old receipt replays.
	poll, err := cw.svc.Queue(ctx, back)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, poll.Outcome)
}
