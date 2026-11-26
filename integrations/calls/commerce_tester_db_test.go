package calls

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/siteaccess"
)

func testerBegin(t *testing.T, cw *callWorld, k accountTestKey, nonce string) string {
	t.Helper()
	out := commerceDo(t, cw, k, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: nonce, AdultDeclared: true, TermsAccepted: true})
	return out.Lifecycle.Checkout.ID
}

func testerApply(cw *callWorld, svc *Service, k accountTestKey, kind, operation, outcome, code string) (callrules.AccountResult, error) {
	return svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "mock_" + kind + "_complete", AccountID: k.result.Account.AccountID, OperationID: operation, Outcome: outcome, TesterCode: code})
}

func testerState(t *testing.T, cw *callWorld, k accountTestKey) *commerceState {
	t.Helper()
	v, err := cw.svc.store.commerceState(memql.ContextWithFreshRead(context.Background()), cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

func testerBalance(t *testing.T, cw *callWorld, k accountTestKey) int64 {
	t.Helper()
	out, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	if out.Account.BalanceCents == nil {
		return 0
	}
	return *out.Account.BalanceCents
}

func requireTesterIncorrect(t *testing.T, err error, left int) {
	t.Helper()
	var incorrect *callrules.TesterCodeIncorrect
	require.True(t, errors.As(err, &incorrect), "want an incorrect code, got %v", err)
	require.Equal(t, left, incorrect.AttemptsLeft)
}

func requireTesterPaused(t *testing.T, err error, wait time.Duration) {
	t.Helper()
	var paused *callrules.TesterCodePaused
	require.True(t, errors.As(err, &paused), "want a pause, got %v", err)
	require.Equal(t, wait, paused.RetryAfter)
}

func TestCommerceMockResultsNeedTesterCode(t *testing.T) {
	cw, k, _ := commerceFixture(t, "tester-needs-code")
	if cw == nil {
		return
	}
	op := testerBegin(t, cw, k, "tester-needs-checkout-01")

	_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", "")
	require.Equal(t, callrules.ReasonTesterCodeRequired, callrules.ReasonOf(err))
	require.ErrorIs(t, err, callrules.ErrInvalid)
	_, err = testerApply(cw, cw.svc, k, "checkout", op, "paid", testerWrongCode)
	requireTesterIncorrect(t, err, 4)
	cw.svc.testerCodes = fakeTesterCodes{fallback: siteaccess.TesterCodeUnavailable}
	_, err = testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.Equal(t, callrules.ReasonTesterAccessUnavailable, callrules.ReasonOf(err))
	cw.svc.testerCodes = testTesterCodes()
	// An unknown outcome stays invalid and is never counted as a wrong code.
	_, err = testerApply(cw, cw.svc, k, "checkout", op, "refund_me", testerWrongCode)
	require.ErrorIs(t, err, callrules.ErrInvalid)
	_, err = testerApply(cw, cw.svc, k, "checkout", op, "paid", testerWrongCode)
	requireTesterIncorrect(t, err, 3)

	require.Zero(t, testerBalance(t, cw, k))
	state := testerState(t, cw, k)
	require.Equal(t, "pending", state.latest("checkout").Status)
	require.Zero(t, state.PaidCents)
	require.Len(t, state.TesterFailures, 2)

	paid, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
	require.Empty(t, testerState(t, cw, k).TesterFailures, "a live code clears the count")

	verification := commerceBeginVerification(t, cw, k, "tester-needs-verify-01").Lifecycle.Verification.ID
	_, err = testerApply(cw, cw.svc, k, "verification", verification, "approved", "")
	require.Equal(t, callrules.ReasonTesterCodeRequired, callrules.ReasonOf(err))
	_, err = testerApply(cw, cw.svc, k, "verification", verification, "approved", testerWrongCode)
	requireTesterIncorrect(t, err, 4)
	cw.svc.testerCodes = fakeTesterCodes{fallback: siteaccess.TesterCodeUnavailable}
	_, err = testerApply(cw, cw.svc, k, "verification", verification, "approved", testTesterCode)
	require.Equal(t, callrules.ReasonTesterAccessUnavailable, callrules.ReasonOf(err))
	cw.svc.testerCodes = testTesterCodes()
	state = testerState(t, cw, k)
	require.Equal(t, "unverified", state.Adult.Status)
	require.Equal(t, "not_started", state.latest("verification").Status)

	done, err := testerApply(cw, cw.svc, k, "verification", verification, "approved", testTesterCode)
	require.NoError(t, err)
	require.Equal(t, "verified", done.Lifecycle.Adult.Status)
}

func TestCommerceTesterCodePausesOneAccount(t *testing.T) {
	cw, one, _ := commerceFixture(t, "tester-pause")
	if cw == nil {
		return
	}
	two := accountRegister(t, cw, newSoftwareAuthenticator(t))
	first := testerBegin(t, cw, one, "tester-pause-one-0001")
	second := testerBegin(t, cw, two, "tester-pause-two-0001")

	for left := 4; left >= 1; left-- {
		_, err := testerApply(cw, cw.svc, one, "checkout", first, "paid", testerWrongCode)
		requireTesterIncorrect(t, err, left)
	}
	_, err := testerApply(cw, cw.svc, one, "checkout", first, "paid", testerWrongCode)
	requireTesterPaused(t, err, 15*time.Minute)
	_, err = testerApply(cw, cw.svc, one, "checkout", first, "paid", testTesterCode)
	requireTesterPaused(t, err, 15*time.Minute)
	require.Zero(t, testerBalance(t, cw, one))

	// Another account is never affected by this one's pause.
	paid, err := testerApply(cw, cw.svc, two, "checkout", second, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)

	cw.clock.Advance(15 * time.Minute)
	paid, err = testerApply(cw, cw.svc, one, "checkout", first, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
	// The count starts again after the pause and a success; a replay is gated too.
	_, err = testerApply(cw, cw.svc, one, "checkout", first, "paid", testerWrongCode)
	requireTesterIncorrect(t, err, 4)
	require.EqualValues(t, 1000, testerBalance(t, cw, one))
}

func TestCommerceTesterCodeCreditsOnceAcrossReplicas(t *testing.T) {
	cw, k, c := commerceFixture(t, "tester-replicas")
	if cw == nil {
		return
	}
	other := allowanceReplica(t, cw)
	other.commerce = func() CommerceConfig { return *c }
	other.testerCodes = testTesterCodes()
	op := testerBegin(t, cw, k, "tester-replica-check-01")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, svc := range []*Service{cw.svc, other} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			_, errs[i] = testerApply(cw, svc, k, "checkout", op, "paid", testTesterCode)
		}(i, svc)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.EqualValues(t, 1000, testerBalance(t, cw, k))

	// A retry after a lost response that did credit succeeds, credits nothing
	// more and records no wrong code.
	replay, err := testerApply(cw, other, k, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *replay.Account.BalanceCents)
	state := testerState(t, cw, k)
	require.EqualValues(t, 1000, state.PaidCents)
	require.Empty(t, state.TesterFailures)
}

func TestCommerceBackOutNeedsNoTesterCode(t *testing.T) {
	cw, k, _ := commerceFixture(t, "tester-back-out")
	if cw == nil {
		return
	}
	cancelled := testerBegin(t, cw, k, "tester-back-out-check-1")
	out, err := testerApply(cw, cw.svc, k, "checkout", cancelled, "cancelled", "")
	require.NoError(t, err)
	require.Equal(t, "cancelled", out.Lifecycle.Checkout.Status)
	require.Zero(t, testerBalance(t, cw, k))

	commercePurchase(t, cw, k, "tester-back-out-paid-01")
	verification := commerceBeginVerification(t, cw, k, "tester-back-out-verify-1").Lifecycle.Verification.ID
	out, err = testerApply(cw, cw.svc, k, "verification", verification, "abandoned", "")
	require.NoError(t, err)
	require.Equal(t, "abandoned", out.Lifecycle.Verification.Status)
	require.Equal(t, "unverified", out.Lifecycle.Adult.Status)
	require.Empty(t, testerState(t, cw, k).TesterFailures)
}

func TestCommerceSandboxIgnoresTesterCode(t *testing.T) {
	cw, k, _, payments := commerceSandboxFixture(t, "tester-sandbox")
	if cw == nil {
		return
	}
	op := testerBegin(t, cw, k, "tester-sandbox-check-01")
	require.Equal(t, 1, payments.created)
	for _, code := range []string{testerWrongCode, testTesterCode} {
		_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", code)
		require.ErrorContains(t, err, "simulation_disabled")
	}
	state := testerState(t, cw, k)
	require.Empty(t, state.TesterFailures)
	require.True(t, state.TesterPausedUntil.IsZero())
	require.Zero(t, state.PaidCents)
}

func TestCommerceTesterCodeNeverStored(t *testing.T) {
	cw, k, _ := commerceFixture(t, "tester-never-stored")
	if cw == nil {
		return
	}
	op := testerBegin(t, cw, k, "tester-never-stored-0001")
	_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testerWrongCode)
	requireTesterIncorrect(t, err, 4)
	_, err = testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.NoError(t, err)

	rows := 0
	for _, row := range cw.rowsOf(commerceConcept) {
		if memql.BareShortId(rowString(row, "keyId")) != k.result.Account.AccountID {
			continue
		}
		rows++
		data := rowString(row, "stateData")
		for _, secret := range []string{testTesterCode, memql.NormalizeAccessCode(testTesterCode), memql.AccessCodeDigest(testTesterCode), testerWrongCode, memql.NormalizeAccessCode(testerWrongCode)} {
			require.NotContains(t, data, secret)
		}
	}
	require.Equal(t, 1, rows)
}

func TestCommerceGuestJoinUnaffectedByTesterGate(t *testing.T) {
	cw, host, _ := commerceFixture(t, "tester-guests")
	if cw == nil {
		return
	}
	commerceVerified(t, cw, host)
	q := meteredQuote(t, cw, host, 6, 10)
	start, err := cw.svc.Start(context.Background(), *host.result.Session, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "tester-guests-start-01"})
	require.NoError(t, err)

	admitted := func(name string) {
		t.Helper()
		out, _, err := cw.join(start.Call.GuestToken, name)
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, out.Outcome, name)
	}
	admitted("Guest before any refusal")

	// Pause the host's own simulated results: guests are untouched.
	paid := testerState(t, cw, host).latest("checkout").ID
	for left := 4; left >= 1; left-- {
		_, err = testerApply(cw, cw.svc, host, "checkout", paid, "paid", testerWrongCode)
		requireTesterIncorrect(t, err, left)
	}
	_, err = testerApply(cw, cw.svc, host, "checkout", paid, "paid", testerWrongCode)
	requireTesterPaused(t, err, 15*time.Minute)
	admitted("Guest while the host is paused")

	cw.svc.testerCodes = fakeTesterCodes{fallback: siteaccess.TesterCodeUnavailable}
	admitted("Guest on a site that cannot hold tester codes")

	// An unfunded Callkey with its own session joins the same way.
	unfunded := accountRegister(t, cw, newSoftwareAuthenticator(t))
	out, _, err := cw.rejoin(*unfunded.result.Session, start.Call.GuestToken)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, out.Outcome)
}

func TestCommerceExpiredTesterCodeNeitherCountsNorApplies(t *testing.T) {
	cw, k, _ := commerceFixture(t, "tester-expired")
	if cw == nil {
		return
	}
	op := testerBegin(t, cw, k, "tester-expired-check-01")
	for i := 0; i < 6; i++ {
		_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testExpiredTesterCode)
		require.Equal(t, callrules.ReasonTesterCodeExpired, callrules.ReasonOf(err))
		require.ErrorIs(t, err, callrules.ErrForbidden)
	}
	state := testerState(t, cw, k)
	require.Empty(t, state.TesterFailures, "an expired code was counted")
	require.True(t, state.TesterPausedUntil.IsZero())
	require.Equal(t, "pending", state.latest("checkout").Status)
	require.Zero(t, testerBalance(t, cw, k))
}

func TestCommerceTesterCodeLookupFailureAppliesNothing(t *testing.T) {
	cw, k, _ := commerceFixture(t, "tester-lookup-fails")
	if cw == nil {
		return
	}
	op := testerBegin(t, cw, k, "tester-lookup-check-01")
	cw.svc.testerCodes = fakeTesterCodes{err: errors.New("the access-code read failed")}
	_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testTesterCode)
	require.Error(t, err)
	require.Empty(t, callrules.ReasonOf(err))
	state := testerState(t, cw, k)
	require.Empty(t, state.TesterFailures)
	require.Equal(t, "pending", state.latest("checkout").Status)
	require.Zero(t, testerBalance(t, cw, k))
}

// forgeTesterAccessCode writes a calls-testers access code for this world's site
// straight through siteaccess's mutation, as the site's owner, and answers the plain
// code. The siteaccess tests cover minting it in MemQL OS; this is the row that makes.
func forgeTesterAccessCode(t *testing.T, cw *callWorld, codeID string, expires time.Time) string {
	t.Helper()
	code, digest, hint, err := memql.MintAccessCode()
	require.NoError(t, err)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), cw.owner))
	cw.mutate(owner, "createSiteAccessCode", map[string]any{
		"codeId": codeID, "siteId": cw.site, "accountId": cw.account, "label": "Tester", "gate": memql.AccessGateCallsTesters,
		"codeHash": digest, "codeHint": hint, "issuedAt": cw.clock.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		"expiresAt": expires.UTC().Truncate(time.Second).Format(time.RFC3339), "lastChangedBy": cw.owner,
	})
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:platform:siteAccessCode").
			Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
	return code
}

func TestCommerceTesterCodeFromAccessCodes(t *testing.T) {
	cw, k, _ := commerceFixture(t, "tester-access-code")
	if cw == nil {
		return
	}
	cw.svc.testerCodes = siteaccess.NewGraphStore(liveEngine{cw.eng})
	code := forgeTesterAccessCode(t, cw, "tester-code-row", cw.clock.Now().Add(time.Hour))
	op := testerBegin(t, cw, k, "tester-access-check-01")

	_, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", testerWrongCode)
	requireTesterIncorrect(t, err, 4)
	paid, err := testerApply(cw, cw.svc, k, "checkout", op, "paid", strings.ToLower(code))
	require.NoError(t, err, "a live calls-testers code, typed in lower case, must apply the result")
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
	require.Empty(t, testerState(t, cw, k).TesterFailures)
}
