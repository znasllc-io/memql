package calls

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// These current-model regressions exercise the real DB and independent service
// gates. Only the authenticated media control API is replaced by the fixture.
func TestUsageQualificationDistinctStartsAndScheduledOpeningShareOneReservation(t *testing.T) {
	cw := newCallWorld(t, "usage-distinct-start-race", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	quote := meteredQuote(t, cw, k, 2, 60)
	booking, err := cw.svc.Schedule(ctx, host, callrules.ScheduleRequest{
		Start: cw.clock.Now().Add(time.Minute), Minutes: 60, Title: "Concurrent booking",
		QuoteID: quote.QuoteID, IdempotencyKey: "qualification-booking-001",
	})
	require.NoError(t, err)
	services := []*Service{cw.svc, allowanceReplica(t, cw), allowanceReplica(t, cw)}
	var results [3]callrules.JoinResult
	var errs [3]error
	ready := make(chan struct{})
	var running sync.WaitGroup
	for index, service := range services {
		running.Add(1)
		go func(index int, service *Service) {
			defer running.Done()
			<-ready
			if index == 2 {
				results[index], _, errs[index] = service.Join(ctx, cw.ref, nil,
					callrules.JoinBody{Token: booking.GuestToken, Name: "Scheduled guest", Confirm: confirmed})
				return
			}
			var started callrules.StartResult
			started, errs[index] = service.Start(ctx, host, callrules.StartBody{
				Name: "Host", Confirm: confirmed, QuoteID: quote.QuoteID,
				IdempotencyKey: fmt.Sprintf("qualification-distinct-start-%02d", index),
			})
			results[index] = started.Join
		}(index, service)
	}
	close(ready)
	running.Wait()
	wins := 0
	for index, err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.ErrorIs(t, err, callrules.ErrConflict)
		require.NotEqual(t, callrules.OutcomeAdmit, results[index].Outcome)
	}
	require.Equal(t, 1, wins)
	rooms, err := services[1].store.openRoomsForKey(ctx, cw.owner, host.KeyID)
	require.NoError(t, err)
	require.Len(t, rooms, 1)
	require.Equal(t, usageMeteredMode, rooms[0].FundingMode)
	require.Equal(t, quote.MaxCostCents, rooms[0].UsageReservedCents)
	wallet, err := services[2].Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *wallet.Account.BalanceCents)
	require.Equal(t, quote.MaxCostCents, *wallet.Account.ReservedCents)
	require.EqualValues(t, 1000-quote.MaxCostCents, *wallet.Account.AvailableCents)
	require.Greater(t, *wallet.Account.AvailableCents, quote.MaxCostCents,
		"the one-room invariant must hold even when funds could cover another room")
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1, "reservation does not debit the wallet")
}

type qualificationPresenceBackend struct {
	MediaBackend
	unavailable *atomic.Bool
}

func (b qualificationPresenceBackend) Participants(ctx context.Context, room string) ([]MediaParticipant, error) {
	if b.unavailable.Load() {
		return nil, errors.New("fixture presence service unavailable")
	}
	return b.MediaBackend.Participants(ctx, room)
}

func TestUsageQualificationMissingCallbacksAndProviderOutageSettleOnlyProvenTime(t *testing.T) {
	for _, scenario := range []struct {
		name                       string
		newConnection, finalOutage bool
		cents, remainder           int64
	}{
		{"same_connection_recovered", false, false, 2, usageUnitsPerCent / 2},
		{"new_connection_cannot_fill_gap", true, false, 1, usageUnitsPerCent / 2},
		{"end_while_presence_unavailable", false, true, 1, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cw := newCallWorld(t, "usage-missing-callback", 120)
			if cw == nil {
				return
			}
			ctx := context.Background()
			k, host := meteredAccount(t, cw)
			quote := meteredQuote(t, cw, k, 2, 60)
			started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed,
				QuoteID: quote.QuoteID, IdempotencyKey: "qualification-missing-callback"})
			require.NoError(t, err)
			began := cw.clock.Now()
			cw.media.setPresent(started.Call.ID, MediaParticipant{SID: "original-sid", Identity: started.Join.Identity, JoinedAt: began})
			replica := allowanceReplica(t, cw)
			var unavailable atomic.Bool
			for _, service := range []*Service{cw.svc, replica} {
				service.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
					backend, err := cw.media.backend(server, credential)
					return qualificationPresenceBackend{backend, &unavailable}, err
				}
			}
			// No joined/left webhook is delivered anywhere in this scenario.
			cw.clock.Advance(4 * time.Minute)
			require.NoError(t, replica.Reconcile(ctx))
			unavailable.Store(true)
			cw.clock.Advance(3 * time.Minute)
			require.Error(t, cw.svc.Reconcile(ctx))
			cw.clock.Advance(3 * time.Minute)
			if !scenario.finalOutage {
				unavailable.Store(false)
				if scenario.newConnection {
					cw.media.setPresent(started.Call.ID, MediaParticipant{SID: "reconnected-sid", Identity: started.Join.Identity, JoinedAt: began.Add(8 * time.Minute)})
				}
				require.NoError(t, replica.Reconcile(ctx))
				cw.media.setPresent(started.Call.ID) // Missed leave: don't extrapolate.
				cw.clock.Advance(2 * time.Minute)
				require.NoError(t, replica.Reconcile(ctx))
			}
			require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
			unavailable.Store(false)
			require.NoError(t, replica.Reconcile(ctx))
			wallet := remainderWallet(t, cw, k)
			require.EqualValues(t, 1000-scenario.cents, wallet.BalanceCents)
			require.Equal(t, scenario.remainder, wallet.UsageRemainderUnits)
			require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
			room := roomFromRow(cw.row(roomConcept, started.Call.ID))
			require.False(t, room.open())
			require.Equal(t, scenario.cents, *room.UsageCostCents)
			after, err := replica.Account(ctx, cw.ref, host.Token, nil)
			require.NoError(t, err)
			require.Zero(t, *after.Account.ReservedCents)
		})
	}
}

func TestUsageQualificationClockRollbackAndReplicaSkewCannotRepeatSettlement(t *testing.T) {
	cw := newCallWorld(t, "usage-clock-rollback", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	quote := meteredQuote(t, cw, k, 2, 60)
	body := callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: quote.QuoteID, IdempotencyKey: "qualification-clock-start"}
	started, err := cw.svc.Start(ctx, host, body)
	require.NoError(t, err)
	began := cw.clock.Now()
	deadline := began.Add(time.Hour)
	cw.media.setPresent(started.Call.ID, MediaParticipant{SID: "clock-sid", Identity: started.Join.Identity, JoinedAt: began})
	ahead, behind := allowanceReplica(t, cw), allowanceReplica(t, cw)
	readRoom := func() roomRow {
		room, err := cw.svc.store.roomByID(memql.ContextWithFreshRead(ctx), cw.owner, started.Call.ID)
		require.NoError(t, err)
		require.NotNil(t, room)
		return *room
	}
	ahead.now = func() time.Time { return cw.clock.Now().Add(2 * time.Second) }
	behind.now = func() time.Time { return cw.clock.Now().Add(-2 * time.Second) }
	cw.clock.Advance(20 * time.Minute)
	require.NoError(t, ahead.Reconcile(ctx))
	before := readRoom()
	beforeMeter, err := readUsageMeter(before)
	require.NoError(t, err)
	require.Len(t, beforeMeter, 1)
	require.NoError(t, behind.Reconcile(ctx))
	skewed := readRoom()
	skewedMeter, err := readUsageMeter(skewed)
	require.NoError(t, err)
	require.Equal(t, beforeMeter[0].Through, skewedMeter[0].Through, "a behind replica cannot retract proven presence")
	require.False(t, skewed.Version.Before(before.Version))
	require.Equal(t, deadline, skewed.FundingDeadline)
	// A wall-clock rollback must not rewind DB versions or make a negative
	// charge. It can conservatively shorten settled usage; elapsed-time policy
	// under unbounded clock drift is a separate production operating requirement.
	cw.clock.Advance(-5 * time.Minute)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	ended := readRoom()
	require.False(t, ended.open())
	require.True(t, ended.Version.After(before.Version))
	require.Equal(t, deadline, ended.FundingDeadline)
	wallet := remainderWallet(t, cw, k)
	require.EqualValues(t, 997, wallet.BalanceCents)
	require.EqualValues(t, usageUnitsPerCent*3/4, wallet.UsageRemainderUnits)
	for _, advance := range []time.Duration{2 * time.Hour, -2*time.Hour - 5*time.Minute} {
		cw.clock.Advance(advance)
		require.NoError(t, behind.Reconcile(ctx))
		_, err = ahead.Start(ctx, host, body)
		require.ErrorIs(t, err, callrules.ErrEnded)
		require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
		require.Equal(t, wallet.ID, remainderWallet(t, cw, k).ID)
	}
}
