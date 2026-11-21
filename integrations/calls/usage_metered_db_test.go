package calls

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func meteredAccount(t *testing.T, cw *callWorld) (accountTestKey, callrules.Session) {
	t.Helper()
	_, err := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 1, "callConfiguration": 1, "maxPeople": 100, "maxScheduledMinutes": 1440, "roomJoinsPerMinute": 1000})
	require.NoError(t, err)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	fundAction(t, cw, k, topupRequest(k, 1000, "metered-top-up-0001"))
	return k, *k.result.Session
}
func meteredQuote(t *testing.T, cw *callWorld, k accountTestKey, n, minutes int) *callrules.CallQuote {
	t.Helper()
	out, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.KeyID, ParticipantLimit: n, DurationMinutes: minutes})
	require.NoError(t, err)
	require.NotNil(t, out.Quote)
	return out.Quote
}
func TestMeteredReserveSettleAndReplayAcrossReplica(t *testing.T) {
	cw := newCallWorld(t, "metered-ledger", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, k, 2, 60)
	require.EqualValues(t, 30, q.MaxCostCents)
	body := callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-start-0001"}
	start, err := cw.svc.Start(ctx, host, body)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, start.Join.Outcome)
	replay, err := allowanceReplica(t, cw).Start(ctx, host, body)
	require.NoError(t, err)
	require.Equal(t, start.Call, replay.Call)
	account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *account.Account.BalanceCents)
	require.EqualValues(t, 30, *account.Account.ReservedCents)
	require.EqualValues(t, 970, *account.Account.AvailableCents)
	guest, _, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	_, _, err = cw.join(start.Call.GuestToken, "Third")
	require.Error(t, err)
	began := cw.clock.Now()
	cw.clock.Advance(time.Second)
	for _, p := range []struct{ id, sid string }{{start.Join.Identity, "host-sid"}, {guest.Identity, "guest-sid"}} {
		_, err = cw.svc.applyMediaEvent(ctx, mediaEvent{Kind: eventParticipantJoined, Room: start.Call.ID, Identity: p.id, ParticipantSID: p.sid, At: cw.clock.Now()})
		require.NoError(t, err)
	}
	cw.clock.Advance(10 * time.Minute)
	for _, p := range []struct{ id, sid string }{{start.Join.Identity, "host-sid"}, {guest.Identity, "guest-sid"}} {
		_, err = allowanceReplica(t, cw).applyMediaEvent(ctx, mediaEvent{Kind: eventParticipantLeft, Room: start.Call.ID, Identity: p.id, ParticipantSID: p.sid, At: cw.clock.Now()})
		require.NoError(t, err)
	}
	_ = began
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	after, err := allowanceReplica(t, cw).Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 995, *after.Account.BalanceCents)
	require.Zero(t, *after.Account.ReservedCents)
	require.EqualValues(t, 995, *after.Account.AvailableCents)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
	_, err = cw.svc.Start(ctx, host, body)
	require.ErrorIs(t, err, callrules.ErrEnded)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
}
func TestMeteredQuoteBoundsExpiryAndBookingReplay(t *testing.T) {
	cw := newCallWorld(t, "metered-quote", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, k, 100, 10)
	require.EqualValues(t, 96, q.MaxCostCents)
	for _, r := range []callrules.AccountRequest{{ParticipantLimit: 0, DurationMinutes: 60}, {ParticipantLimit: 101, DurationMinutes: 60}, {ParticipantLimit: 2, DurationMinutes: 11}, {ParticipantLimit: 2, DurationMinutes: 1441}, {ParticipantLimit: 2, DurationMinutes: 60, Insights: true}} {
		r.Action = "quote_call"
		r.AccountID = k.result.Account.KeyID
		_, err := cw.svc.Account(ctx, cw.ref, host.Token, &r)
		require.Error(t, err)
	}
	req := callrules.ScheduleRequest{Start: cw.clock.Now().Add(time.Hour), Minutes: 10, QuoteID: q.QuoteID, IdempotencyKey: "metered-booking-0001", Title: "Configured"}
	booked, err := cw.svc.Schedule(ctx, host, req)
	require.NoError(t, err)
	require.Equal(t, 100, booked.Configuration.ParticipantLimit)
	cw.clock.Advance(6 * time.Minute)
	replay, err := allowanceReplica(t, cw).Schedule(ctx, host, req)
	require.NoError(t, err)
	require.Equal(t, booked.GuestToken, replay.GuestToken)
	require.Equal(t, booked.ID, replay.ID)
	_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "expired-start-0001"})
	require.Error(t, err)
	require.Equal(t, "quote_expired", callrules.ReasonOf(err))
	account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.Zero(t, *account.Account.ReservedCents)
	q2 := meteredQuote(t, cw, k, 3, 30)
	move := callrules.ScheduleRequest{Start: req.Start.Add(time.Hour), Minutes: 30, Title: "Moved", QuoteID: q2.QuoteID, IdempotencyKey: "metered-reschedule-01"}
	moved, err := cw.svc.Reschedule(ctx, host, booked.ID, move)
	require.NoError(t, err)
	require.Equal(t, 3, moved.Configuration.ParticipantLimit)
	again, err := allowanceReplica(t, cw).Reschedule(ctx, host, booked.ID, move)
	require.NoError(t, err)
	require.Equal(t, moved.GuestToken, again.GuestToken)
	_, err = cw.svc.Schedule(ctx, host, req)
	require.Equal(t, "operation_superseded", callrules.ReasonOf(err))
}
func TestMeteredConcurrentStartReservesOnce(t *testing.T) {
	cw := newCallWorld(t, "metered-race", 120)
	if cw == nil {
		return
	}
	k, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, k, 100, 60)
	body := callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-race-start01"}
	var wg sync.WaitGroup
	out := make(chan callrules.StartResult, 2)
	errs := make(chan error, 2)
	for _, svc := range []*Service{cw.svc, allowanceReplica(t, cw)} {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			r, e := svc.Start(context.Background(), host, body)
			out <- r
			errs <- e
		}(svc)
	}
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	var id string
	for r := range out {
		if id == "" {
			id = r.Call.ID
		}
		require.Equal(t, id, r.Call.ID)
	}
	rooms, err := cw.svc.store.openRoomsForKey(context.Background(), cw.owner, k.result.Account.KeyID)
	require.NoError(t, err)
	require.Len(t, rooms, 1)
}

type interruptedMeterEngine struct {
	Engine
	match  string
	failed bool
}

func (e *interruptedMeterEngine) Execute(ctx context.Context, query string) (any, error) {
	if !e.failed && strings.Contains(query, e.match) {
		e.failed = true
		return nil, errors.New("injected metered write interruption")
	}
	return e.Engine.Execute(ctx, query)
}
func brokenMeterService(cw *callWorld, match string) (*Service, *interruptedMeterEngine) {
	s := *cw.svc
	store := *s.store
	e := &interruptedMeterEngine{Engine: store.engine, match: match}
	store.engine = e
	s.store = &store
	return &s, e
}
func TestMeteredFailuresDoNotOrphanOrDoubleDebitReservations(t *testing.T) {
	for _, mutation := range []string{"updateCallsSession(", "createCallsSeat(", "createCallsRoom("} {
		t.Run(mutation, func(t *testing.T) {
			cw := newCallWorld(t, "metered-failed-opening", 1)
			if cw == nil {
				return
			}
			ctx := context.Background()
			k, host := meteredAccount(t, cw)
			q := meteredQuote(t, cw, k, 2, 60)
			body := callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-failed-start1"}
			broken, injected := brokenMeterService(cw, mutation)
			_, err := broken.Start(ctx, host, body)
			require.Error(t, err)
			require.True(t, injected.failed)
			account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
			require.NoError(t, err)
			require.Zero(t, *account.Account.ReservedCents)
			start, err := allowanceReplica(t, cw).Start(ctx, host, body)
			require.NoError(t, err)
			require.Equal(t, callrules.OutcomeAdmit, start.Join.Outcome)
			seats, e := cw.svc.store.seatsForRoom(ctx, cw.owner, start.Call.ID)
			require.NoError(t, e)
			require.Equal(t, 1, occupants(seats, start.Call.ID))
		})
	}
	for _, mutation := range []string{"createCallsSimulatedWalletEntry(", "usageCostCents:"} {
		t.Run(mutation, func(t *testing.T) {
			cw := newCallWorld(t, "metered-failed-settlement", 120)
			if cw == nil {
				return
			}
			ctx := context.Background()
			k, host := meteredAccount(t, cw)
			q := meteredQuote(t, cw, k, 2, 60)
			body := callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-settle-start1"}
			start, err := cw.svc.Start(ctx, host, body)
			require.NoError(t, err)
			began := cw.clock.Now()
			cw.media.setPresent(start.Call.ID, MediaParticipant{SID: "meter-sid", Identity: start.Join.Identity, JoinedAt: began})
			cw.clock.Advance(20 * time.Minute)
			broken, injected := brokenMeterService(cw, mutation)
			require.Error(t, broken.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
			require.True(t, injected.failed)
			account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
			require.NoError(t, err)
			if mutation == "createCallsSimulatedWalletEntry(" {
				require.EqualValues(t, 30, *account.Account.ReservedCents)
				require.EqualValues(t, 1000, *account.Account.BalanceCents)
			} else {
				require.Zero(t, *account.Account.ReservedCents)
				require.EqualValues(t, 995, *account.Account.BalanceCents)
			}
			require.NoError(t, allowanceReplica(t, cw).Reconcile(ctx))
			after, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
			require.NoError(t, err)
			require.EqualValues(t, 995, *after.Account.BalanceCents)
			require.Zero(t, *after.Account.ReservedCents)
			require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
			page := historyPage(t, cw, host, false)
			require.Len(t, page.Calls, 1)
			require.NotNil(t, page.Calls[0].CostCents)
			require.EqualValues(t, 5, *page.Calls[0].CostCents)
		})
	}
}
func TestMeteredCapacity100AndLegacyEightRemainSeparate(t *testing.T) {
	cw := newCallWorld(t, "metered-capacity", 150)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, k, 100, 10)
	start, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-capacity-100"})
	require.NoError(t, err)
	for i := 1; i < 100; i++ {
		joined, _, e := cw.join(start.Call.GuestToken, "Guest or AI agent")
		require.NoError(t, e)
		require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	}
	extra, _, err := cw.join(start.Call.GuestToken, "Number101")
	require.Error(t, err)
	require.Equal(t, "room_full", extra.Reason)
	room := roomFromRow(cw.row(roomConcept, start.Call.ID))
	require.Equal(t, 100, room.FundingMaxPeople)
	// Native agent kinds use the same total, rather than extra agent allowance.
	load := serverLoad{seats: []seatRow{{RoomID: room.ID, Kind: string(callrules.KindAgent), Status: seatInCall}, {RoomID: room.ID, Kind: string(callrules.KindPerson), Status: seatInCall}}}
	require.Equal(t, 2, load.roomState(room).People)
	require.Zero(t, load.roomState(room).Agents)
	require.Equal(t, callrules.KindPerson, fundedSeatKind(room, callrules.KindAgent))
	require.Equal(t, 8, fundedPolicy(callrules.Policy{MaxPeople: 100}, roomFunding{FundingMode: usagePreviewMode, FundingMaxPeople: 8}).MaxPeople)
}
func TestMeteredHostLeftGraceAndHardDeadlineSettleProvenIntervals(t *testing.T) {
	for _, hostLeaves := range []bool{true, false} {
		t.Run(fmt.Sprint(hostLeaves), func(t *testing.T) {
			cw := newCallWorld(t, "metered-deadline", 120)
			if cw == nil {
				return
			}
			ctx := context.Background()
			k, host := meteredAccount(t, cw)
			q := meteredQuote(t, cw, k, 2, 10)
			start, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-deadline-01"})
			require.NoError(t, err)
			guest, _, err := cw.join(start.Call.GuestToken, "Guest")
			require.NoError(t, err)
			began := cw.clock.Now()
			cw.media.setPresent(start.Call.ID, MediaParticipant{SID: "host-sid", Identity: start.Join.Identity, JoinedAt: began}, MediaParticipant{SID: "guest-sid", Identity: guest.Identity, JoinedAt: began})
			cw.clock.Advance(time.Minute)
			if hostLeaves {
				require.NoError(t, cw.svc.Leave(ctx, host))
				cw.clock.Advance(5 * time.Minute)
			} else {
				cw.clock.Advance(9 * time.Minute)
			}
			require.NoError(t, allowanceReplica(t, cw).Reconcile(ctx))
			room := roomFromRow(cw.row(roomConcept, start.Call.ID))
			require.False(t, room.open())
			require.NotNil(t, room.UsageCostCents)
			want := int64(5)
			if hostLeaves {
				want = 1 // The remaining 0.75 cent carries to the next call.
				wallet := remainderWallet(t, cw, k)
				require.EqualValues(t, usageUnitsPerCent*3/4, wallet.UsageRemainderUnits)
			}
			require.Equal(t, want, *room.UsageCostCents)
			account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
			require.NoError(t, err)
			require.EqualValues(t, 1000-want, *account.Account.BalanceCents)
			require.Zero(t, *account.Account.ReservedCents)
		})
	}
}

type interleaveMeterSnapshotEngine struct {
	Engine
	once sync.Once
	hook func()
}

func (e *interleaveMeterSnapshotEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "callsOpenRoomsForKey(") {
		e.once.Do(e.hook)
	}
	return e.Engine.Execute(ctx, query)
}
func TestMeteredWalletSnapshotRetriesConcurrentSettlement(t *testing.T) {
	cw := newCallWorld(t, "metered-snapshot-race", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, k, 2, 60)
	start, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "metered-snapshot-01"})
	require.NoError(t, err)
	began := cw.clock.Now()
	cw.media.setPresent(start.Call.ID, MediaParticipant{SID: "snapshot-sid", Identity: start.Join.Identity, JoinedAt: began})
	cw.clock.Advance(20 * time.Minute)
	replica := *allowanceReplica(t, cw)
	store := *replica.store
	store.engine = &interleaveMeterSnapshotEngine{Engine: store.engine, hook: func() {
		require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	}}
	replica.store = &store
	out, err := replica.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 995, *out.Account.BalanceCents)
	require.EqualValues(t, 995, *out.Account.AvailableCents)
	require.Zero(t, *out.Account.ReservedCents)
}

func TestMeteredOptInDoesNotChargeOrExpandExistingPreview(t *testing.T) {
	cw := newCallWorld(t, "metered-compatible", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := usagePreviewAccount(t, cw)
	old, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "callConfiguration": 1, "maxPeople": 100})
	require.NoError(t, err)
	for range 7 {
		joined, _, e := cw.join(old.Call.GuestToken, "Guest")
		require.NoError(t, e)
		require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	}
	ninth, _, err := cw.join(old.Call.GuestToken, "Ninth")
	require.Error(t, err)
	require.Equal(t, "room_full", ninth.Reason)
	cw.clock.Advance(10 * time.Minute)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *account.Account.BalanceCents)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
	q := meteredQuote(t, cw, k, 2, 60)
	require.EqualValues(t, 30, q.MaxCostCents)
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "usagePreview": 0})
	require.NoError(t, err)
	_, _, err = cw.svc.fundingForOpening(ctx, cw.ref, k.result.Account.KeyID, cw.clock.Now())
	require.Equal(t, "pass_required", callrules.ReasonOf(err), "new metering flag alone never authorizes the old uncharged grant")
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "callConfiguration": 0})
	require.NoError(t, err)
	_, err = cw.svc.Account(ctx, cw.ref, host.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.KeyID, ParticipantLimit: 2, DurationMinutes: 60})
	require.Equal(t, "call_configuration_disabled", callrules.ReasonOf(err))
}
