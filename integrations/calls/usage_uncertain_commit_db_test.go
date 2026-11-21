package calls

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// The DB commits successfully, but its response never reaches the integration.
// This differs from a rejected write and must be resolved from durable receipts.
type qualificationCommittedResponseLoss struct {
	Engine
	match  string
	failed bool
}

func (e *qualificationCommittedResponseLoss) Execute(ctx context.Context, query string) (any, error) {
	if !e.failed && strings.Contains(query, e.match) {
		e.failed = true
		if _, err := e.Engine.Execute(ctx, query); err != nil {
			return nil, err
		}
		return nil, errors.New("fixture committed write response lost")
	}
	return e.Engine.Execute(ctx, query)
}

func qualificationAfterCommit(cw *callWorld, match string) (*Service, *qualificationCommittedResponseLoss) {
	s := *cw.svc
	store := *s.store
	fault := &qualificationCommittedResponseLoss{Engine: store.engine, match: match}
	store.engine = fault
	s.store = &store
	return &s, fault
}

func TestUsageQualificationCommittedRoomResponseReplaysAfterQuoteExpiry(t *testing.T) {
	cw := newCallWorld(t, "usage-committed-room-response", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	quote := meteredQuote(t, cw, k, 2, 60)
	body := callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: quote.QuoteID, IdempotencyKey: "qualification-committed-room"}
	broken, fault := qualificationAfterCommit(cw, "createCallsRoom(")
	_, err := broken.Start(ctx, host, body)
	require.Error(t, err)
	require.True(t, fault.failed)
	rooms, err := cw.svc.store.openRoomsForKey(ctx, cw.owner, host.KeyID)
	require.NoError(t, err)
	require.Len(t, rooms, 1)
	roomID := rooms[0].ID
	account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *account.Account.BalanceCents)
	require.Equal(t, quote.MaxCostCents, *account.Account.ReservedCents)
	cw.clock.Advance(usageQuoteLifetime + time.Second)
	services := []*Service{allowanceReplica(t, cw), allowanceReplica(t, cw)}
	var answers [2]callrules.StartResult
	var errs [2]error
	var running sync.WaitGroup
	for index, service := range services {
		running.Add(1)
		go func(index int, service *Service) {
			defer running.Done()
			answers[index], errs[index] = service.Start(ctx, host, body)
		}(index, service)
	}
	running.Wait()
	for index, err := range errs {
		require.NoError(t, err)
		require.Equal(t, roomID, answers[index].Call.ID)
		require.Equal(t, callrules.OutcomeAdmit, answers[index].Join.Outcome)
	}
	seats, err := cw.svc.store.seatsForRoom(ctx, cw.owner, roomID)
	require.NoError(t, err)
	require.Equal(t, 1, occupants(seats, roomID))
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
}

func TestUsageQualificationCommittedSettlementResponseCannotDebitTwice(t *testing.T) {
	cw := newCallWorld(t, "usage-commit-settle", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := meteredAccount(t, cw)
	quote := meteredQuote(t, cw, k, 2, 60)
	start, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed,
		QuoteID: quote.QuoteID, IdempotencyKey: "qualification-committed-settlement"})
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, MediaParticipant{SID: "fixture-metered-host", Identity: start.Join.Identity, JoinedAt: cw.clock.Now()})
	cw.clock.Advance(20 * time.Minute)
	broken, fault := qualificationAfterCommit(cw, "createCallsSimulatedWalletEntry(")
	require.Error(t, broken.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	require.True(t, fault.failed)
	account, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 995, *account.Account.BalanceCents)
	require.Zero(t, *account.Account.ReservedCents, "committed receipt releases its reservation despite missing room-end response")
	cw.clock.Advance(2 * time.Minute)
	services := []*Service{allowanceReplica(t, cw), allowanceReplica(t, cw)}
	var errs [2]error
	var running sync.WaitGroup
	for index, service := range services {
		running.Add(1)
		go func(index int, service *Service) {
			defer running.Done()
			errs[index] = service.Reconcile(ctx)
		}(index, service)
	}
	running.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	after, err := services[0].Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 995, *after.Account.BalanceCents)
	require.Zero(t, *after.Account.ReservedCents)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
	page := historyPage(t, cw, host, false)
	require.Len(t, page.Calls, 1)
	require.NotNil(t, page.Calls[0].CostCents)
	require.EqualValues(t, 5, *page.Calls[0].CostCents)
}
