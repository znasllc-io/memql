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
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func enableFunding(t *testing.T, cw *callWorld) {
	t.Helper()
	p := callrules.DefaultPolicy()
	p.SimulatedFunding = true
	_, err := cw.svc.store.savePolicy(context.Background(), cw.owner, nil, cw.site, p, []string{"simulatedFunding"}, cw.clock.Now())
	require.NoError(t, err)
}
func topupRequest(k accountTestKey, amount int64, operation string) *callrules.AccountRequest {
	return &callrules.AccountRequest{Action: "simulate_topup", AccountID: k.result.Account.KeyID, AmountCents: amount, IdempotencyKey: operation}
}
func activateRequest(k accountTestKey, plan, operation string) *callrules.AccountRequest {
	return &callrules.AccountRequest{Action: "activate_pass", AccountID: k.result.Account.KeyID, Plan: plan, IdempotencyKey: operation}
}
func fundAction(t *testing.T, cw *callWorld, k accountTestKey, req *callrules.AccountRequest) callrules.AccountResult {
	t.Helper()
	out, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	return out
}

func TestFundingPersistsReplaysAndPreservesActivePass(t *testing.T) {
	cw := newCallWorld(t, "funding-persist", 20)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	initial, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, "unfunded", initial.Account.FundingMode)
	require.Zero(t, *initial.Account.BalanceCents)
	require.False(t, initial.Capabilities.SimulatedFunding)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, topupRequest(k, 1000, "persist-topup-0001"))
	require.ErrorContains(t, err, "simulation_disabled")
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
	enableFunding(t, cw)
	cw.clock.Advance(123 * time.Nanosecond)
	one := fundAction(t, cw, k, topupRequest(k, 1000, "persist-topup-0001"))
	require.EqualValues(t, 1000, *one.Account.BalanceCents)
	require.Equal(t, "simulation", one.Account.FundingMode)
	require.False(t, one.Replayed)
	require.True(t, one.Receipt.Simulated)
	require.False(t, one.Capabilities.Payments)
	copyService := *cw.svc
	duplicate, err := copyService.Account(ctx, cw.ref, k.result.Session.Token, topupRequest(k, 1000, "persist-topup-0001"))
	require.NoError(t, err)
	require.True(t, duplicate.Replayed)
	require.Equal(t, one.Receipt, duplicate.Receipt)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "logout"})
	require.NoError(t, err)
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	k.result.Session = signed.Session
	require.EqualValues(t, 1000, *signed.Account.BalanceCents)
	two := fundAction(t, cw, k, topupRequest(k, 2000, "persist-topup-0002"))
	require.EqualValues(t, 3000, *two.Account.BalanceCents)
	duplicate = fundAction(t, cw, k, topupRequest(k, 1000, "persist-topup-0001"))
	require.EqualValues(t, 1000, duplicate.Receipt.BalanceCents)
	require.EqualValues(t, 3000, *duplicate.Account.BalanceCents)
	active := fundAction(t, cw, k, activateRequest(k, "month", "persist-activate-01"))
	require.EqualValues(t, 2200, *active.Account.BalanceCents)
	require.EqualValues(t, 800, active.Receipt.DebitedCents)
	require.True(t, active.Account.CanHost)
	require.NotNil(t, active.Account.ActivePass)
	require.EqualValues(t, 72000, active.Account.ActivePass.TotalRemainingSeconds)
	require.EqualValues(t, 10800, active.Account.ActivePass.TodayRemainingSeconds)
	more := fundAction(t, cw, k, topupRequest(k, 1000, "persist-topup-0003"))
	require.EqualValues(t, 3200, *more.Account.BalanceCents)
	require.Equal(t, active.Account.ActivePass, more.Account.ActivePass)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, activateRequest(k, "day", "persist-activate-02"))
	require.ErrorContains(t, err, "active_pass")
	duplicate = fundAction(t, cw, k, activateRequest(k, "month", "persist-activate-01"))
	require.True(t, duplicate.Replayed)
	require.Equal(t, active.Receipt, duplicate.Receipt)
	require.EqualValues(t, 3200, *duplicate.Account.BalanceCents)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, topupRequest(k, 2000, "persist-topup-0001"))
	require.ErrorContains(t, err, "idempotency_conflict")
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, activateRequest(k, "day", "persist-topup-0001"))
	require.ErrorContains(t, err, "idempotency_conflict")
	require.Len(t, cw.rowsOf(simulationWalletConcept), 4)
}

func TestFundingRequiresClaimedCurrentAccountAndBounds(t *testing.T) {
	cw := newCallWorld(t, "funding-authority", 20)
	if cw == nil {
		return
	}
	enableFunding(t, cw)
	ctx := context.Background()
	one := accountRegister(t, cw, newSoftwareAuthenticator(t))
	two := accountRegister(t, cw, newSoftwareAuthenticator(t))
	for _, req := range []*callrules.AccountRequest{topupRequest(one, 1000, "authority-topup-01"), activateRequest(one, "day", "authority-pass-01")} {
		_, err := cw.svc.Account(ctx, cw.ref, two.result.Session.Token, req)
		require.ErrorContains(t, err, "account_changed")
	}
	legacy := cw.host()
	_, err := cw.svc.Account(ctx, cw.ref, legacy.Token, &callrules.AccountRequest{Action: "simulate_topup", AccountID: legacy.KeyID, AmountCents: 1000, IdempotencyKey: "authority-legacy-1"})
	require.ErrorContains(t, err, "legacy_key")
	_, room := cw.started("Host")
	_, guest, err := cw.join(room.Call.GuestToken, "Guest")
	require.NoError(t, err)
	_, err = cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "simulate_topup", AccountID: guest.KeyID, AmountCents: 1000, IdempotencyKey: "authority-guest-01"})
	require.ErrorContains(t, err, "unclaimed_key")
	for _, amount := range []int64{-1, 0, 999, 50001, 1 << 62} {
		_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, topupRequest(one, amount, "authority-invalid1"))
		require.ErrorIs(t, err, callrules.ErrInvalid)
	}
	_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, activateRequest(one, "month", "authority-no-funds"))
	require.ErrorContains(t, err, "insufficient_balance")
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
	funded := fundAction(t, cw, one, topupRequest(one, 50000, "authority-max-fund"))
	require.EqualValues(t, 50000, *funded.Account.BalanceCents)
	_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, topupRequest(one, 1000, "authority-over-cap"))
	require.ErrorContains(t, err, "balance_limit")
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
	cw.asOwnerMutate(policyConcept, policyIDFor(cw.site), "updateCallsPolicy", map[string]any{"policyId": policyIDFor(cw.site), "simulatedFunding": 0})
	_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, activateRequest(one, "day", "authority-disabled"))
	require.ErrorContains(t, err, "simulation_disabled")
	snapshot, err := cw.svc.Account(ctx, cw.ref, one.result.Session.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 50000, *snapshot.Account.BalanceCents)
	require.Equal(t, "simulation", snapshot.Account.FundingMode)
	require.False(t, snapshot.Account.CanHost)
}

func TestFundingConcurrentReceiptsCapAndActivation(t *testing.T) {
	cw := newCallWorld(t, "funding-concurrent", 20)
	if cw == nil {
		return
	}
	enableFunding(t, cw)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	run := func(request func(int) *callrules.AccountRequest) ([]callrules.AccountResult, []error) {
		results := make([]callrules.AccountResult, 12)
		errs := make([]error, 12)
		requests := make([]*callrules.AccountRequest, 12)
		var wg sync.WaitGroup
		for i := range results {
			requests[i] = request(i)
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				replica := *cw.svc
				results[i], errs[i] = replica.Account(ctx, cw.ref, k.result.Session.Token, requests[i])
			}(i)
		}
		wg.Wait()
		// The public write gate deliberately bounds its wait. A twelve-way burst
		// on a slower database can answer Busy before attempting a write. Retry
		// only that typed refusal, once, after the burst drains, with the identical
		// operation. All receipt, balance, cap and activation assertions below stay
		// exact; no arbitrary error or unlimited wait is hidden by this recovery.
		retryCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		for i, err := range errs {
			if errors.Is(err, memql.ErrWriteGateBusy) {
				replica := *cw.svc
				results[i], errs[i] = replica.Account(retryCtx, cw.ref, k.result.Session.Token, requests[i])
			}
		}
		return results, errs
	}
	results, errs := run(func(int) *callrules.AccountRequest { return topupRequest(k, 1000, "concurrent-same-id") })
	commits := 0
	for i, err := range errs {
		require.NoError(t, err)
		require.EqualValues(t, 1000, *results[i].Account.BalanceCents)
		if !results[i].Replayed {
			commits++
		}
	}
	require.Equal(t, 1, commits)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
	fundAction(t, cw, k, topupRequest(k, 48000, "concurrent-fill-cap"))
	results, errs = run(func(i int) *callrules.AccountRequest {
		return topupRequest(k, 1000, fmt.Sprintf("concurrent-cap-%03d", i))
	})
	commits = 0
	for i, err := range errs {
		if err == nil {
			commits++
			require.EqualValues(t, 50000, *results[i].Account.BalanceCents)
		} else {
			require.ErrorContains(t, err, "balance_limit")
		}
	}
	require.Equal(t, 1, commits)
	results, errs = run(func(i int) *callrules.AccountRequest {
		return activateRequest(k, "month", fmt.Sprintf("concurrent-pass-%03d", i))
	})
	commits = 0
	for i, err := range errs {
		if err == nil {
			commits++
			require.EqualValues(t, 49200, *results[i].Account.BalanceCents)
		} else {
			require.ErrorContains(t, err, "active_pass")
		}
	}
	require.Equal(t, 1, commits)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 4)
}

type interruptedFundingEngine struct {
	Engine
	after  bool
	failed bool
}

func (e *interruptedFundingEngine) Execute(ctx context.Context, query string) (any, error) {
	if !e.failed && strings.Contains(query, "createCallsSimulatedWalletEntry(") {
		e.failed = true
		if e.after {
			if _, err := e.Engine.Execute(ctx, query); err != nil {
				return nil, err
			}
		}
		return nil, errors.New("test interrupted simulated funding write")
	}
	return e.Engine.Execute(ctx, query)
}
func TestFundingLostResponseRetriesExactlyOneEffect(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_commit_%v", after), func(t *testing.T) {
			cw := newCallWorld(t, fmt.Sprintf("funding-interrupt-%v", after), 20)
			if cw == nil {
				return
			}
			enableFunding(t, cw)
			k := accountRegister(t, cw, newSoftwareAuthenticator(t))
			store := *cw.svc.store
			broken := &interruptedFundingEngine{Engine: store.engine, after: after}
			store.engine = broken
			service := *cw.svc
			service.store = &store
			req := topupRequest(k, 1000, "interrupted-write-1")
			_, err := service.Account(context.Background(), cw.ref, k.result.Session.Token, req)
			require.Error(t, err)
			require.True(t, broken.failed)
			result := fundAction(t, cw, k, req)
			require.Equal(t, after, result.Replayed)
			require.EqualValues(t, 1000, *result.Account.BalanceCents)
			require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
		})
	}
}

func TestFundingExpiresAndRenewsWithoutResettingBalance(t *testing.T) {
	cw := newCallWorld(t, "funding-renew", 20)
	if cw == nil {
		return
	}
	enableFunding(t, cw)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	fundAction(t, cw, k, topupRequest(k, 1000, "renew-topup-00001"))
	day := fundAction(t, cw, k, activateRequest(k, "day", "renew-pass-000001"))
	require.EqualValues(t, 850, *day.Account.BalanceCents)
	cw.clock.Advance(24*time.Hour + time.Second)
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	k.result.Session = signed.Session
	require.Nil(t, signed.Account.ActivePass)
	require.False(t, signed.Account.CanHost)
	require.EqualValues(t, 850, *signed.Account.BalanceCents)
	newPass := fundAction(t, cw, k, activateRequest(k, "day", "renew-pass-000002"))
	require.EqualValues(t, 700, *newPass.Account.BalanceCents)
	require.NotEqual(t, day.Account.ActivePass.ID, newPass.Account.ActivePass.ID)
	old := fundAction(t, cw, k, activateRequest(k, "day", "renew-pass-000001"))
	require.True(t, old.Replayed)
	require.EqualValues(t, 850, old.Receipt.BalanceCents)
	require.EqualValues(t, 700, *old.Account.BalanceCents)
	require.Equal(t, newPass.Account.ActivePass, old.Account.ActivePass)
}
