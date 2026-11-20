package calls

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func allowanceAccount(t *testing.T, cw *callWorld, plan string) (accountTestKey, callrules.Session, callrules.AccountPass) {
	t.Helper()
	_, err := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 1, "maxPeople": 20})
	require.NoError(t, err)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	sess := *k.result.Session
	_, err = cw.svc.Account(context.Background(), cw.ref, sess.Token, &callrules.AccountRequest{Action: "simulate_topup", AccountID: sess.KeyID, AmountCents: 1000, IdempotencyKey: "allowance-topup-" + id.NewShortId()})
	require.NoError(t, err)
	activated, err := cw.svc.Account(context.Background(), cw.ref, sess.Token, &callrules.AccountRequest{Action: "activate_pass", AccountID: sess.KeyID, Plan: plan, IdempotencyKey: "allowance-pass-" + id.NewShortId()})
	require.NoError(t, err)
	require.NotNil(t, activated.Account.ActivePass)
	require.True(t, activated.Account.CanHost)
	return k, sess, *activated.Account.ActivePass
}

func allowanceReplica(t *testing.T, cw *callWorld) *Service {
	t.Helper()
	pool, err := database.NewGatePool(sharedBoot.dsn, database.DefaultGatePoolSize)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	return New(Deps{Engine: liveEngine{cw.eng}, GateDB: func() *sql.DB { return pool }, ResolveSystemSecret: cw.eng.ResolveSystemSecret, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: cw.clock.Now, NewMediaBackend: cw.media.backend}).Service()
}

func TestSimulatedAllowanceDeadlinePersistsAcrossTopupsHandoverAndReplicas(t *testing.T) {
	cw := newCallWorld(t, "allowance-deadline", 30)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, host, pass := allowanceAccount(t, cw, "day")
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Sponsor", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, started.Join.Outcome)
	deadline := cw.clock.Now().Add(3 * time.Hour)
	room := cw.row(roomConcept, started.Call.ID)
	require.Equal(t, pass.ID, memql.BareShortId(rowString(room, "fundingPassId")))
	require.Equal(t, deadline, rowTime(room, "fundingDeadline"))
	require.Equal(t, cw.clock.Now(), rowTime(room, "fundingStartedAt"))
	// Raising the deployable's cap cannot widen this eight-participant pass.
	for range 7 {
		joined, _, err := cw.join(started.Call.GuestToken, "Guest")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	}
	joined, _, err := cw.join(started.Call.GuestToken, "Ninth participant")
	require.Error(t, err)
	require.Equal(t, "room_full", joined.Reason)
	cw.clock.Advance(time.Hour)
	before, err := cw.svc.passAllowance(ctx, cw.owner, cw.site, host.KeyID, pass, cw.clock.Now())
	require.NoError(t, err)
	require.EqualValues(t, 7200, before.RemainingSeconds)
	_, err = cw.svc.Account(ctx, cw.ref, host.Token, &callrules.AccountRequest{Action: "simulate_topup", AccountID: host.KeyID, AmountCents: 1000, IdempotencyKey: "topup-during-call-" + id.NewShortId()})
	require.NoError(t, err)
	other := allowanceReplica(t, cw)
	view, err := other.SessionCall(ctx, host)
	require.NoError(t, err)
	require.Equal(t, deadline, view.AllowanceEndsAt)
	_, err = other.Account(ctx, cw.ref, host.Token, &callrules.AccountRequest{Action: "activate_pass", AccountID: host.KeyID, Plan: "month", IdempotencyKey: "no-overlap-pass-" + id.NewShortId()})
	requireReason(t, err, callrules.ErrConflict, "active_pass")
	// No polling or browser timer is required to enforce the ceiling. Late
	// cleanup records only the interval that was authorized.
	cw.advanceTo(deadline.Add(40 * time.Second))
	_, err = other.Queue(ctx, host)
	requireReason(t, err, callrules.ErrEnded, endReasonAllowance)
	require.NoError(t, other.Reconcile(ctx))
	room = cw.row(roomConcept, started.Call.ID)
	require.Equal(t, "ended", rowString(room, "status"))
	require.Equal(t, endReasonAllowance, rowString(room, "endReason"))
	require.Equal(t, deadline, rowTime(room, "endedAt"))
	require.Contains(t, cw.media.deletions(), fakeMediaRef{Server: BootstrapMediaServerID, Room: started.Call.ID})
	after, err := other.passAllowance(ctx, cw.owner, cw.site, host.KeyID, pass, cw.clock.Now())
	require.NoError(t, err)
	require.Zero(t, after.RemainingSeconds)
	_, err = other.Start(ctx, host, callrules.StartBody{Name: "Cannot restart budget", Confirm: confirmed})
	requireReason(t, err, callrules.ErrConflict, endReasonAllowance)
}

func TestSimulatedAllowanceDoesNotChargeQueueOrWaitingRoom(t *testing.T) {
	for _, mode := range []string{"queue", "waiting_room"} {
		t.Run(mode, func(t *testing.T) {
			cw := newCallWorld(t, "allowance-wait", 1)
			if cw == nil {
				return
			}
			ctx := context.Background()
			_, host, pass := allowanceAccount(t, cw, "day")
			var roomID, hostToken string
			var release callrules.Session
			if mode == "queue" {
				release, _ = cw.started("Fills server")
				started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Waiting host", Confirm: confirmed})
				require.NoError(t, err)
				require.Equal(t, callrules.OutcomeQueue, started.Join.Outcome)
				roomID = started.Call.ID
			} else {
				booked := cw.booked(host, 5*time.Minute, 60, "Waiting")
				joined, guest, err := cw.join(booked.GuestToken, "Guest waits")
				require.NoError(t, err)
				require.Equal(t, callrules.OutcomeWait, joined.Outcome)
				roomID = guest.RoomID
				hostToken = booked.HostToken
			}
			cw.clock.Advance(4 * time.Minute)
			allowance, err := cw.svc.passAllowance(ctx, cw.owner, cw.site, host.KeyID, pass, cw.clock.Now())
			require.NoError(t, err)
			require.EqualValues(t, 10800, allowance.RemainingSeconds)
			require.True(t, rowTime(cw.row(roomConcept, roomID), "fundingStartedAt").IsZero())
			if mode == "queue" {
				require.NoError(t, cw.svc.Leave(ctx, release))
				joined, err := cw.svc.Queue(ctx, host)
				require.NoError(t, err)
				require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
			} else {
				joined, _, err := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: hostToken, Name: "Host", Confirm: confirmed})
				require.NoError(t, err)
				require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
			}
			stored := cw.row(roomConcept, roomID)
			require.Equal(t, cw.clock.Now(), rowTime(stored, "fundingStartedAt"))
			require.Equal(t, cw.clock.Now().Add(3*time.Hour), rowTime(stored, "fundingDeadline"))
		})
	}
}

func TestSimulatedPassScheduledGuestAndInstantStartShareOneRoomAcrossReplicas(t *testing.T) {
	cw := newCallWorld(t, "allowance-concurrent", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, host, _ := allowanceAccount(t, cw, "month")
	booked := cw.booked(host, time.Minute, 60, "Interview")
	other := allowanceReplica(t, cw)
	var errs [2]error
	var outcomes [2]callrules.JoinResult
	var wg sync.WaitGroup
	ready := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-ready
		var result callrules.StartResult
		result, errs[0] = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Instant", Confirm: confirmed})
		outcomes[0] = result.Join
	}()
	go func() {
		defer wg.Done()
		<-ready
		outcomes[1], _, errs[1] = other.Join(ctx, cw.ref, nil, callrules.JoinBody{Token: booked.GuestToken, Name: "Scheduled guest", Confirm: confirmed})
	}()
	close(ready)
	wg.Wait()
	won := 0
	for i, err := range errs {
		if err == nil {
			won++
		} else {
			require.ErrorIs(t, err, callrules.ErrConflict)
			reason := callrules.ReasonOf(err)
			if reason == "" {
				reason = outcomes[i].Reason
			}
			require.Equal(t, callrules.ReasonKeyInCall, reason)
		}
	}
	require.Equal(t, 1, won)
	open, err := other.store.openRoomsForKey(ctx, cw.owner, host.KeyID)
	require.NoError(t, err)
	require.Len(t, open, 1)
}

type incompleteAllowanceEngine struct {
	Engine
	missing bool
}

func (e incompleteAllowanceEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "callsRoomsForFundingPass(") {
		if e.missing {
			return (*memql.ExecuteResult)(nil), nil
		}
		return &memql.ExecuteResult{Meta: &memql.ResultMeta{HasMore: true}}, nil
	}
	return e.Engine.Execute(ctx, query)
}

func TestSimulatedAllowanceIncompleteUsageRefusesBeforeRoomWrites(t *testing.T) {
	cw := newCallWorld(t, "allowance-incomplete", 10)
	if cw == nil {
		return
	}
	_, host, _ := allowanceAccount(t, cw, "day")
	for _, missing := range []bool{false, true} {
		other := *cw.svc
		store := *other.store
		store.engine = incompleteAllowanceEngine{Engine: store.engine, missing: missing}
		other.store = &store
		_, err := other.Start(context.Background(), host, callrules.StartBody{Name: "No partial total", Confirm: confirmed})
		requireReason(t, err, callrules.ErrConflict, "allowance_unavailable")
		require.Empty(t, cw.rowsOf(roomConcept))
	}
}

func TestSimulatedAllowanceHostDepartureKeepsOriginalBudgetAndCountdown(t *testing.T) {
	cw := newCallWorld(t, "allowance-host-left", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, host, pass := allowanceAccount(t, cw, "day")
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Original payer", Confirm: confirmed})
	require.NoError(t, err)
	join, holder := cw.keyHolderJoin(started.Call.GuestToken, "Another key holder")
	cw.media.setPresent(started.Call.ID, person(join.Identity))
	cw.clock.Advance(time.Minute)
	require.NoError(t, cw.svc.Leave(ctx, host))
	view, err := cw.svc.SessionCall(ctx, holder)
	require.NoError(t, err)
	require.False(t, view.Host, "a guest's key is not consent to take over funding")
	require.Equal(t, cw.clock.Now().Add(5*time.Minute), view.CountdownEndsAt)
	deadline := rowTime(cw.row(roomConcept, started.Call.ID), "fundingDeadline")
	require.Equal(t, deadline, view.AllowanceEndsAt)
	cw.advanceTo(view.CountdownEndsAt)
	require.NoError(t, cw.svc.Reconcile(ctx))
	require.Equal(t, callrules.EndReasonHostLeft, rowString(cw.row(roomConcept, started.Call.ID), "endReason"))
	left, err := cw.svc.passAllowance(ctx, cw.owner, cw.site, host.KeyID, pass, cw.clock.Now())
	require.NoError(t, err)
	require.EqualValues(t, 10800-6*60, left.RemainingSeconds, "the host-left grace is part of the sponsored room interval")
	_, _, err = cw.rejoin(holder, started.Call.GuestToken)
	require.True(t, errors.Is(err, callrules.ErrEnded))
}

func TestSimulatedAllowanceMonthlyTotalSurvivesDailyResetsAndNewSessions(t *testing.T) {
	cw := newCallWorld(t, "allowance-month", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host, pass := allowanceAccount(t, cw, "month")
	first := cw.clock.Now()
	for day := range 7 {
		if day > 0 {
			cw.advanceTo(first.Add(time.Duration(day) * 24 * time.Hour))
			signed, err := accountAssert(t, cw, k, "", "login", "")
			require.NoError(t, err)
			host = *signed.Session
		}
		started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Daily call", Confirm: confirmed})
		require.NoError(t, err)
		span := 3 * time.Hour
		if day == 6 {
			span = 2 * time.Hour
		}
		deadline := cw.clock.Now().Add(span)
		require.Equal(t, deadline, rowTime(cw.row(roomConcept, started.Call.ID), "fundingDeadline"))
		cw.advanceTo(deadline)
		require.NoError(t, cw.svc.Reconcile(ctx))
		allowance, err := cw.svc.passAllowance(ctx, cw.owner, cw.site, host.KeyID, pass, cw.clock.Now())
		require.NoError(t, err)
		require.EqualValues(t, max(0, 20-(day+1)*3)*3600, allowance.RemainingSeconds)
		if day < 6 {
			require.Zero(t, allowance.DailyRemainingSeconds)
		} else {
			require.EqualValues(t, 3600, allowance.DailyRemainingSeconds)
		}
		_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Cannot reset by starting", Confirm: confirmed})
		requireReason(t, err, callrules.ErrConflict, endReasonAllowance)
	}
	snapshot, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.NotNil(t, snapshot.Account.ActivePass, "an exhausted but unexpired pass remains visible")
	require.Equal(t, pass.ID, snapshot.Account.ActivePass.ID)
	require.Zero(t, snapshot.Account.ActivePass.TotalRemainingSeconds)
	require.False(t, snapshot.Account.CanHost)
}

func TestSimulatedAllowanceExpiryBookingAndFailedMediaCleanupCannotRenew(t *testing.T) {
	cw := newCallWorld(t, "allowance-expiry", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host, pass := allowanceAccount(t, cw, "day")
	_, err := cw.book(host, 23*time.Hour+30*time.Minute, 60, "Beyond expiry")
	requireReason(t, err, callrules.ErrConflict, "pass_required")
	booked := cw.booked(host, 23*time.Hour, 30, "Fits the pass")
	_, err = cw.moveTo(host, booked.ID, 23*time.Hour+30*time.Minute, 60, "Beyond expiry")
	requireReason(t, err, callrules.ErrConflict, "pass_required")
	// Turning off simulated spending must not prevent the owner cancelling.
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 0})
	require.NoError(t, err)
	require.NoError(t, cw.svc.Cancel(ctx, host, booked.ID))
	_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Disabled", Confirm: confirmed})
	requireReason(t, err, callrules.ErrForbidden, "simulation_disabled")
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 1})
	require.NoError(t, err)
	cw.advanceTo(pass.ExpiresAt.Add(-30 * time.Minute))
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Last half hour", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, pass.ExpiresAt, rowTime(cw.row(roomConcept, started.Call.ID), "fundingDeadline"))
	cw.advanceTo(pass.ExpiresAt.Add(time.Minute))
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	host = *signed.Session
	_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Expired", Confirm: confirmed})
	requireReason(t, err, callrules.ErrConflict, "pass_required")
	cw.media.mu.Lock()
	cw.media.failDelete = errors.New("temporary media outage")
	cw.media.mu.Unlock()
	require.Error(t, cw.svc.Reconcile(ctx))
	require.Equal(t, "open", rowString(cw.row(roomConcept, started.Call.ID), "status"))
	request := &callrules.AccountRequest{Action: "activate_pass", AccountID: host.KeyID, Plan: "day", IdempotencyKey: "renew-after-expiry-" + id.NewShortId()}
	_, err = cw.svc.Account(ctx, cw.ref, host.Token, request)
	requireReason(t, err, callrules.ErrConflict, "hosted_room_open")
	cw.media.mu.Lock()
	cw.media.failDelete = nil
	cw.media.mu.Unlock()
	require.NoError(t, cw.svc.Reconcile(ctx))
	require.Equal(t, pass.ExpiresAt, rowTime(cw.row(roomConcept, started.Call.ID), "endedAt"))
	renewed, err := cw.svc.Account(ctx, cw.ref, host.Token, request)
	require.NoError(t, err)
	require.NotEqual(t, pass.ID, renewed.Account.ActivePass.ID)
	require.EqualValues(t, 10800, renewed.Account.ActivePass.TotalRemainingSeconds)
	_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "New paid simulation interval", Confirm: confirmed})
	require.NoError(t, err)
}

type interruptedFundingStartEngine struct {
	Engine
	failed bool
}

func (e *interruptedFundingStartEngine) Execute(ctx context.Context, query string) (any, error) {
	if !e.failed && strings.Contains(query, "updateCallsRoom(") && strings.Contains(query, "fundingStartedAt") {
		e.failed = true
		return nil, errors.New("interrupted before the funding start was written")
	}
	return e.Engine.Execute(ctx, query)
}

func TestSimulatedAllowanceRepairsAdmissionFromOriginalSeatTime(t *testing.T) {
	cw := newCallWorld(t, "allowance-repair", 1)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, host, _ := allowanceAccount(t, cw, "day")
	filler, _ := cw.started("Capacity occupied")
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Waiting", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, started.Join.Outcome)
	cw.clock.Advance(time.Minute)
	require.NoError(t, cw.svc.Leave(ctx, filler))
	broken := *cw.svc
	store := *broken.store
	interruption := &interruptedFundingStartEngine{Engine: store.engine}
	store.engine, broken.store = interruption, &store
	_, err = broken.Queue(ctx, host)
	require.Error(t, err)
	require.True(t, interruption.failed)
	began := cw.clock.Now()
	require.True(t, rowTime(cw.row(roomConcept, started.Call.ID), "fundingStartedAt").IsZero())
	cw.clock.Advance(time.Minute)
	admitted, err := allowanceReplica(t, cw).Queue(ctx, host)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, admitted.Outcome)
	room := cw.row(roomConcept, started.Call.ID)
	require.Equal(t, began, rowTime(room, "fundingStartedAt"))
	require.Equal(t, began.Add(3*time.Hour), rowTime(room, "fundingDeadline"))
	// Renewal near the ceiling carries only the remaining admission lifetime.
	cw.advanceTo(began.Add(3*time.Hour - 10*time.Second))
	admitted, err = cw.svc.Queue(ctx, host)
	require.NoError(t, err)
	require.Equal(t, 10*time.Second, admitted.ExpiresIn)
}
