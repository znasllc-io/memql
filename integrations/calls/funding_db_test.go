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
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/calls/providers"
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

// seedSimulatedCredit adds credit the way a completed payment does
// (creditCommerceCheckout): a new immutable wallet entry that carries the
// balance, active pass and usage remainder forward. simulate_topup is retired,
// so tests of the wallet's other effects start from this.
func seedSimulatedCredit(t *testing.T, cw *callWorld, keyID string, cents int64) {
	t.Helper()
	require.NoError(t, seedSimulatedCreditErr(cw, keyID, cents))
}

// seedSimulatedCreditErr is seedSimulatedCredit for goroutines. It holds the
// account and key gates, as every wallet writer does.
func seedSimulatedCreditErr(cw *callWorld, keyID string, cents int64) error {
	release, err := cw.svc.gates(context.Background(), accountGate(keyID), keyGate(keyID))
	if err != nil {
		return err
	}
	defer release()
	ctx := memql.ContextWithFreshRead(context.Background())
	previous, err := cw.svc.simulatedWallet(ctx, cw.owner, cw.site, keyID)
	if err != nil {
		return err
	}
	now := cw.clock.Now().UTC().Truncate(time.Microsecond)
	nonce := id.NewShortId()
	entry := simulationWalletEntry{ID: callrules.HistoryDigest(nil, "test-seed-credit", cw.site, keyID, nonce)[:32], SiteID: cw.site, KeyID: keyID, Kind: "topup", RequestDigest: callrules.HistoryDigest(nil, "test-seed-request", nonce), CreditedCents: cents, Version: now}
	if previous != nil {
		entry.BalanceCents, entry.Pass, entry.UsageRemainderUnits = previous.BalanceCents, previous.Pass, previous.UsageRemainderUnits
		entry.Version = memql.VersionTimeAfter(previous.Version, now)
	}
	entry.BalanceCents += cents
	return cw.svc.store.createSimulatedWalletEntry(ctx, cw.owner, entry)
}

// TestSimulatedTopupRefusedInEveryMode: credit only ever comes from a payment
// result, which is gated in mock mode, so the direct simulated top-up is gone.
func TestSimulatedTopupRefusedInEveryMode(t *testing.T) {
	cw := newCallWorld(t, "funding-topup-retired", 20)
	if cw == nil {
		return
	}
	enableFunding(t, cw)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	ctx := context.Background()
	_, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, topupRequest(k, 1000, "retired-topup-0001"))
	require.ErrorContains(t, err, "checkout_required")
	cw.svc.commerce = func() CommerceConfig { return CommerceConfig{Mode: providers.Mock} }
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, topupRequest(k, 1000, "retired-topup-0002"))
	require.ErrorContains(t, err, "checkout_required")
	cw.svc.commerce = nil
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
	// Activation still spends credit that arrived another way.
	seedSimulatedCredit(t, cw, k.result.Account.KeyID, 1000)
	day := fundAction(t, cw, k, activateRequest(k, "day", "retired-pass-000001"))
	require.EqualValues(t, 850, *day.Account.BalanceCents)
	require.NotNil(t, day.Account.ActivePass)
}

func fundAction(t *testing.T, cw *callWorld, k accountTestKey, req *callrules.AccountRequest) callrules.AccountResult {
	t.Helper()
	out, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	return out
}

func TestFundingActivationPersistsAndReplays(t *testing.T) {
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
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, activateRequest(k, "month", "persist-activate-00"))
	require.ErrorContains(t, err, "simulation_disabled")
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
	enableFunding(t, cw)
	seedSimulatedCredit(t, cw, k.result.Account.KeyID, 3000)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "logout"})
	require.NoError(t, err)
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	k.result.Session = signed.Session
	require.EqualValues(t, 3000, *signed.Account.BalanceCents)
	require.Equal(t, "simulation", signed.Account.FundingMode)
	cw.clock.Advance(123 * time.Nanosecond)
	active := fundAction(t, cw, k, activateRequest(k, "month", "persist-activate-01"))
	require.EqualValues(t, 2200, *active.Account.BalanceCents)
	require.EqualValues(t, 800, active.Receipt.DebitedCents)
	require.True(t, active.Receipt.Simulated)
	require.False(t, active.Capabilities.Payments)
	require.True(t, active.Account.CanHost)
	require.NotNil(t, active.Account.ActivePass)
	require.EqualValues(t, 72000, active.Account.ActivePass.TotalRemainingSeconds)
	require.EqualValues(t, 10800, active.Account.ActivePass.TodayRemainingSeconds)
	copyService := *cw.svc
	duplicate, err := copyService.Account(ctx, cw.ref, k.result.Session.Token, activateRequest(k, "month", "persist-activate-01"))
	require.NoError(t, err)
	require.True(t, duplicate.Replayed)
	require.Equal(t, active.Receipt, duplicate.Receipt)
	// Credit arriving while a pass is active keeps the pass.
	seedSimulatedCredit(t, cw, k.result.Account.KeyID, 1000)
	more, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 3200, *more.Account.BalanceCents)
	require.Equal(t, active.Account.ActivePass, more.Account.ActivePass)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, activateRequest(k, "day", "persist-activate-02"))
	require.ErrorContains(t, err, "active_pass")
	duplicate = fundAction(t, cw, k, activateRequest(k, "month", "persist-activate-01"))
	require.True(t, duplicate.Replayed)
	require.Equal(t, active.Receipt, duplicate.Receipt)
	require.EqualValues(t, 3200, *duplicate.Account.BalanceCents)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, activateRequest(k, "day", "persist-activate-01"))
	require.ErrorContains(t, err, "idempotency_conflict")
	require.Len(t, cw.rowsOf(simulationWalletConcept), 3)
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
	_, err := cw.svc.Account(ctx, cw.ref, two.result.Session.Token, activateRequest(one, "day", "authority-pass-01"))
	require.ErrorContains(t, err, "account_changed")
	legacy := cw.host()
	_, err = cw.svc.Account(ctx, cw.ref, legacy.Token, &callrules.AccountRequest{Action: "activate_pass", AccountID: legacy.KeyID, Plan: "day", IdempotencyKey: "authority-legacy-1"})
	require.ErrorContains(t, err, "legacy_key")
	_, room := cw.started("Host")
	_, guest, err := cw.join(room.Call.GuestToken, "Guest")
	require.NoError(t, err)
	_, err = cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "activate_pass", AccountID: guest.KeyID, Plan: "day", IdempotencyKey: "authority-guest-01"})
	require.ErrorContains(t, err, "unclaimed_key")
	for _, amount := range []int64{-1, 0, 999, 50001, 1 << 62} {
		_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, topupRequest(one, amount, "authority-invalid1"))
		require.ErrorIs(t, err, callrules.ErrInvalid)
	}
	_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, activateRequest(one, "month", "authority-no-funds"))
	require.ErrorContains(t, err, "insufficient_balance")
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
	seedSimulatedCredit(t, cw, one.result.Account.KeyID, 50000)
	_, err = cw.svc.Account(ctx, cw.ref, one.result.Session.Token, topupRequest(one, 1000, "authority-over-cap"))
	require.ErrorContains(t, err, "checkout_required")
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

func TestFundingConcurrentActivation(t *testing.T) {
	cw := newCallWorld(t, "funding-concurrent", 20)
	if cw == nil {
		return
	}
	enableFunding(t, cw)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	seedSimulatedCredit(t, cw, k.result.Account.KeyID, 50000)
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
		// operation. All receipt, balance and activation assertions below stay
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
	results, errs := run(func(int) *callrules.AccountRequest { return activateRequest(k, "month", "concurrent-same-pass") })
	commits := 0
	for i, err := range errs {
		require.NoError(t, err)
		require.EqualValues(t, 49200, *results[i].Account.BalanceCents)
		if !results[i].Replayed {
			commits++
		}
	}
	require.Equal(t, 1, commits)
	results, errs = run(func(i int) *callrules.AccountRequest {
		return activateRequest(k, "day", fmt.Sprintf("concurrent-pass-%03d", i))
	})
	for _, err := range errs {
		require.ErrorContains(t, err, "active_pass")
	}
	require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
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
			seedSimulatedCredit(t, cw, k.result.Account.KeyID, 1000)
			store := *cw.svc.store
			broken := &interruptedFundingEngine{Engine: store.engine, after: after}
			store.engine = broken
			service := *cw.svc
			service.store = &store
			req := activateRequest(k, "day", "interrupted-write-1")
			_, err := service.Account(context.Background(), cw.ref, k.result.Session.Token, req)
			require.Error(t, err)
			require.True(t, broken.failed)
			result := fundAction(t, cw, k, req)
			require.Equal(t, after, result.Replayed)
			require.EqualValues(t, 850, *result.Account.BalanceCents)
			require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
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
	seedSimulatedCredit(t, cw, k.result.Account.KeyID, 1000)
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
