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
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func commerceFixture(t *testing.T, name string) (*callWorld, accountTestKey, *CommerceConfig) {
	t.Helper()
	cw := newCallWorld(t, name, 120)
	if cw == nil {
		return nil, accountTestKey{}, nil
	}
	_, err := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 1, "callConfiguration": 1, "maxPeople": 100, "maxScheduledMinutes": 1440})
	require.NoError(t, err)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	c := &CommerceConfig{Mode: providers.Mock, ValidUntil: cw.clock.Now().Add(time.Hour), AvailableCashCents: 1000000, ProtectedCents: 10000, FixedCents: 10000, VerificationUnitCents: 100, VerificationBudgetCents: 10000, ProviderCreditCents: 10000, CallBudgetCents: 100000, FulfillmentReserveBPS: 5000, MaxAccounts: 900, MaxPendingVerifications: 50, MaxVerificationAttempts: 3, MaxOpenCalls: 20, MaxParticipants: 1000}
	cw.svc.commerce = func() CommerceConfig { return *c }
	return cw, k, c
}
func commerceDo(t *testing.T, cw *callWorld, k accountTestKey, req callrules.AccountRequest) callrules.AccountResult {
	t.Helper()
	req.AccountID = k.result.Account.AccountID
	out, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &req)
	require.NoError(t, err)
	return out
}
func commercePurchase(t *testing.T, cw *callWorld, k accountTestKey, nonce string) callrules.AccountResult {
	t.Helper()
	out := commerceDo(t, cw, k, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: nonce, AdultDeclared: true, TermsAccepted: true, SaveMethod: true})
	return commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_checkout_complete", OperationID: out.Lifecycle.Checkout.ID, Outcome: "paid"})
}
func commerceBeginVerification(t *testing.T, cw *callWorld, k accountTestKey, nonce string) callrules.AccountResult {
	t.Helper()
	return commerceDo(t, cw, k, callrules.AccountRequest{Action: "verification_begin", IdempotencyKey: nonce, AdultDeclared: true, TermsAccepted: true})
}
func commerceVerified(t *testing.T, cw *callWorld, k accountTestKey) callrules.AccountResult {
	t.Helper()
	commercePurchase(t, cw, k, "commerce-purchase-001")
	out := commerceBeginVerification(t, cw, k, "commerce-verify-0001")
	return commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_verification_complete", OperationID: out.Lifecycle.Verification.ID, Outcome: "approved"})
}

func TestCommercePurchaseFirstReuseAndReplica(t *testing.T) {
	cw, k, c := commerceFixture(t, "commerce-reuse")
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: k.result.Account.AccountID, IdempotencyKey: "commerce-no-purchase", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "purchase_required")
	require.Empty(t, cw.rowsOf(commerceConcept))
	pending := commerceDo(t, cw, k, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: "commerce-checkout-001", AdultDeclared: true, TermsAccepted: true, SaveMethod: true})
	require.Zero(t, *pending.Account.BalanceCents)
	require.False(t, pending.Account.CanHost)
	paid := commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_checkout_complete", OperationID: pending.Lifecycle.Checkout.ID, Outcome: "paid"})
	require.EqualValues(t, 1000, *paid.Account.BalanceCents)
	require.False(t, paid.Account.CanHost)
	replay := commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_checkout_complete", OperationID: pending.Lifecycle.Checkout.ID, Outcome: "paid"})
	require.EqualValues(t, 1000, *replay.Account.BalanceCents)
	v := commerceBeginVerification(t, cw, k, "commerce-verification-001")
	require.Equal(t, "not_started", v.Lifecycle.Verification.Status)
	require.EqualValues(t, 100, v.Lifecycle.VerificationReservedCents)
	done := commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_verification_complete", OperationID: v.Lifecycle.Verification.ID, Outcome: "approved"})
	require.True(t, done.Account.CanHost)
	require.EqualValues(t, 100, done.Lifecycle.VerificationSpentCents)
	other := allowanceReplica(t, cw)
	other.commerce = func() CommerceConfig { return *c }
	out, err := other.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: k.result.Account.AccountID, IdempotencyKey: "commerce-reuse-proof", AdultDeclared: true, TermsAccepted: true})
	require.NoError(t, err)
	require.True(t, out.Replayed)
	require.Equal(t, 1, out.Lifecycle.VerificationAttempts)
	// A second authenticator binds the same account, without another provider call.
	add, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "add_begin"})
	require.NoError(t, err)
	challenge, rp, _ := accountCreation(t, add)
	added, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "add_finish", ChallengeID: add.ChallengeID, Label: "Backup", Credential: newSoftwareAuthenticator(t).Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	require.Len(t, added.Credentials, 2)
	require.Equal(t, "verified", added.Lifecycle.Adult.Status)
	require.Equal(t, 1, added.Lifecycle.VerificationAttempts)
	q := meteredQuote(t, cw, k, 2, 60)
	start, err := other.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "commerce-start-0001"})
	require.NoError(t, err)
	require.NotEmpty(t, start.Call.ID)
}
func TestCommerceCapsRetriesAndUnderage(t *testing.T) {
	cw, k, c := commerceFixture(t, "commerce-retries")
	if cw == nil {
		return
	}
	commercePurchase(t, cw, k, "commerce-purchase-001")
	c.MaxVerificationAttempts = 2
	first := commerceBeginVerification(t, cw, k, "commerce-first-check")
	second := commerceBeginVerification(t, cw, k, "commerce-resume-check")
	require.Equal(t, first.Lifecycle.Verification.ID, second.Lifecycle.Verification.ID)
	require.Equal(t, 1, second.Lifecycle.VerificationAttempts)
	review := commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_verification_complete", OperationID: first.Lifecycle.Verification.ID, Outcome: "in_review"})
	require.EqualValues(t, 100, review.Lifecycle.VerificationReservedCents)
	resumed := commerceBeginVerification(t, cw, k, "commerce-review-resume")
	require.Equal(t, 1, resumed.Lifecycle.VerificationAttempts)
	commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_verification_complete", OperationID: first.Lifecycle.Verification.ID, Outcome: "declined"})
	next := commerceBeginVerification(t, cw, k, "commerce-second-check")
	require.Equal(t, 2, next.Lifecycle.VerificationAttempts)
	require.EqualValues(t, 100, next.Lifecycle.VerificationSpentCents)
	commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_verification_complete", OperationID: next.Lifecycle.Verification.ID, Outcome: "declined"})
	_, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: k.result.Account.AccountID, IdempotencyKey: "commerce-third-check", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "verification_attempt_limit")
	// Underage is a terminal account gate, not a switch a new nonce defeats.
	c.MaxVerificationAttempts = 3
	last := commerceBeginVerification(t, cw, k, "commerce-underage-check")
	blocked := commerceDo(t, cw, k, callrules.AccountRequest{Action: "mock_verification_complete", OperationID: last.Lifecycle.Verification.ID, Outcome: "underage"})
	require.Equal(t, "blocked", blocked.Lifecycle.Adult.Status)
	require.False(t, blocked.Account.CanHost)
	_, err = cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "checkout_begin", AccountID: k.result.Account.AccountID, AmountCents: 1000, IdempotencyKey: "commerce-after-underage", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "age_requirement_not_met")
}
func TestCommerceFreshnessCashAndConcurrentVerificationCapacity(t *testing.T) {
	cw, one, c := commerceFixture(t, "commerce-caps")
	if cw == nil {
		return
	}
	two := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commercePurchase(t, cw, one, "commerce-first-buy")
	commercePurchase(t, cw, two, "commerce-second-buy")
	c.ValidUntil = cw.clock.Now()
	_, err := cw.svc.Account(context.Background(), cw.ref, one.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: one.result.Account.AccountID, IdempotencyKey: "commerce-stale-check", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "admission_stale")
	c.ValidUntil = cw.clock.Now().Add(time.Hour)
	c.AvailableCashCents = c.ProtectedCents + c.FixedCents
	_, err = cw.svc.Account(context.Background(), cw.ref, one.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: one.result.Account.AccountID, IdempotencyKey: "commerce-no-cash-check", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "business_reserve")
	c.AvailableCashCents = 1000000
	c.MaxPendingVerifications = 1
	other := allowanceReplica(t, cw)
	other.commerce = func() CommerceConfig { return *c }
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, k := range []accountTestKey{one, two} {
		wg.Add(1)
		go func(i int, k accountTestKey) {
			defer wg.Done()
			svc := cw.svc
			if i == 1 {
				svc = other
			}
			_, err := svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: k.result.Account.AccountID, IdempotencyKey: "commerce-racing-check", AdultDeclared: true, TermsAccepted: true})
			results <- err
		}(i, k)
	}
	wg.Wait()
	close(results)
	good, bad := 0, 0
	for err := range results {
		if err == nil {
			good++
		} else {
			require.ErrorContains(t, err, "verification_capacity")
			bad++
		}
	}
	require.Equal(t, 1, good)
	require.Equal(t, 1, bad)
}
func TestCommerceUnverifiedCallAndOldFundingCannotBypass(t *testing.T) {
	cw, k, _ := commerceFixture(t, "commerce-bypass")
	if cw == nil {
		return
	}
	ctx := context.Background()
	commercePurchase(t, cw, k, "commerce-buy-no-proof")
	_, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, topupRequest(k, 1000, "commerce-old-topup"))
	require.ErrorContains(t, err, "checkout_required")
	q := meteredQuote(t, cw, k, 2, 60)
	_, err = cw.svc.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "commerce-no-adult-start"})
	require.ErrorContains(t, err, "adult_verification_required")
	require.Empty(t, cw.rowsOf(roomConcept))
	require.Empty(t, cw.media.rooms(), "adult proof is checked before any media effect")
}
func TestCommerceCheckoutCapacityReservedBeforePurchase(t *testing.T) {
	cw, one, c := commerceFixture(t, "commerce-checkout-cap")
	if cw == nil {
		return
	}
	two := accountRegister(t, cw, newSoftwareAuthenticator(t))
	c.MaxPendingVerifications = 1
	pending := commerceDo(t, cw, one, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: "commerce-hold-first", AdultDeclared: true, TermsAccepted: true})
	_, err := cw.svc.Account(context.Background(), cw.ref, two.result.Session.Token, &callrules.AccountRequest{Action: "checkout_begin", AccountID: two.result.Account.AccountID, AmountCents: 1000, IdempotencyKey: "commerce-hold-second", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "verification_capacity")
	commerceDo(t, cw, one, callrules.AccountRequest{Action: "mock_checkout_complete", OperationID: pending.Lifecycle.Checkout.ID, Outcome: "cancelled"})
	commerceDo(t, cw, two, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: "commerce-hold-second", AdultDeclared: true, TermsAccepted: true})
}

// Provider interfaces are faked only at the HTTP boundary. The real DSL store,
// authorization, reserves and independent-replica service remain exercised.
type commerceFakeVerification struct {
	result  providers.Verification
	creates int
	err     error
}

func (f *commerceFakeVerification) CreateVerification(_ context.Context, r providers.VerificationRequest) (providers.Verification, error) {
	f.creates++
	return f.result, f.err
}
func (f *commerceFakeVerification) ReadVerification(context.Context, string) (providers.Verification, error) {
	return f.result, nil
}
func TestCommerceUnknownCreateWebhookRecoveryAndStrictAge(t *testing.T) {
	cw, k, c := commerceFixture(t, "commerce-webhook-recovery")
	if cw == nil {
		return
	}
	commercePurchase(t, cw, k, "commerce-sandbox-buy")
	// Migrate only this TEST fixture to the sandbox mode with a trusted prior
	// test purchase. Production mode changes are refused by validateMode.
	ctx := context.Background()
	v, err := cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	v.Mode = providers.Sandbox
	require.NoError(t, cw.svc.store.saveCommerce(ctx, cw.owner, v, cw.clock.Now()))
	c.Mode = providers.Sandbox
	fake := &commerceFakeVerification{err: providers.ErrOutcomeUnknown}
	c.Verifications = fake
	req := &callrules.AccountRequest{Action: "verification_begin", AccountID: k.result.Account.AccountID, IdempotencyKey: "commerce-unknown-create", AdultDeclared: true, TermsAccepted: true}
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, req)
	require.Error(t, err)
	require.Equal(t, 1, fake.creates)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	require.Equal(t, 1, fake.creates)
	state, err := cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	op := state.latest("verification")
	fake.result = providers.Verification{ID: "provider_recovered_session", AccountID: k.result.Account.AccountID, OperationID: op.ID, Status: "Approved"}
	event := providers.Webhook{ObjectID: fake.result.ID, AccountID: fake.result.AccountID, OperationID: op.ID}
	require.NoError(t, cw.svc.ReconcileCommerceProviderEvent(ctx, cw.ref, "didit", event))
	answer, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.NotEqual(t, "verified", answer.Lifecycle.Adult.Status, "generic Approved does not prove 18+")
	fake.result.Adult18 = true
	fake.result.Method = "id_liveness_face_match"
	fake.result.VerifiedAt = cw.clock.Now()
	require.NoError(t, cw.svc.ReconcileCommerceProviderEvent(ctx, cw.ref, "didit", event))
	answer, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, "verified", answer.Lifecycle.Adult.Status)
	require.Equal(t, 1, fake.creates)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "mock_verification_complete", AccountID: k.result.Account.AccountID, OperationID: op.ID, Outcome: "approved"})
	require.ErrorContains(t, err, "simulation_disabled")
	require.False(t, errors.Is(err, providers.ErrOutcomeUnknown))
	// A late create timeout cannot replace an already-reconciled approval.
	cw.svc.markCommerceUnknown(ctx, cw.ref, k.result.Account.AccountID, op.ID)
	state, err = cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	require.Equal(t, "approved", state.latest("verification").Status)
	// An older in-flight approval cannot undo an authoritative underage result.
	fake.result.Status = "Declined"
	fake.result.Adult18 = false
	fake.result.Underage = true
	require.NoError(t, cw.svc.ReconcileCommerceProviderEvent(ctx, cw.ref, "didit", event))
	fake.result.Status = "Approved"
	fake.result.Adult18 = true
	fake.result.Underage = false
	require.NoError(t, cw.svc.ReconcileCommerceProviderEvent(ctx, cw.ref, "didit", event))
	state, err = cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	require.Equal(t, "blocked", state.Adult.Status)
}

type commerceInterruptedCredit struct {
	Engine
	failed bool
}

func (e *commerceInterruptedCredit) Execute(ctx context.Context, query string) (any, error) {
	if !e.failed && strings.Contains(query, "createCallsSimulatedWalletEntry(") {
		e.failed = true
		return nil, errors.New("test crash before credit receipt")
	}
	return e.Engine.Execute(ctx, query)
}
func TestCommercePaidFactSurvivesInterruptedCredit(t *testing.T) {
	cw, k, c := commerceFixture(t, "commerce-credit-crash")
	if cw == nil {
		return
	}
	ctx := context.Background()
	pending := commerceDo(t, cw, k, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: "commerce-credit-crash", AdultDeclared: true, TermsAccepted: true})
	store := *cw.svc.store
	broken := &commerceInterruptedCredit{Engine: store.engine}
	store.engine = broken
	service := *cw.svc
	service.store = &store
	_, err := service.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "mock_checkout_complete", AccountID: k.result.Account.AccountID, OperationID: pending.Lifecycle.Checkout.ID, Outcome: "paid"})
	require.Error(t, err)
	require.True(t, broken.failed)
	state, err := cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	require.EqualValues(t, 1000, state.PaidCents)
	require.Equal(t, "paid", state.latest("checkout").Status)
	c.ValidUntil = cw.clock.Now() // Stale admission must not block already-paid reconciliation.
	out := commerceDo(t, cw, k, callrules.AccountRequest{Action: "checkout_refresh", OperationID: pending.Lifecycle.Checkout.ID})
	require.EqualValues(t, 1000, *out.Account.BalanceCents)
	commerceDo(t, cw, k, callrules.AccountRequest{Action: "checkout_refresh", OperationID: pending.Lifecycle.Checkout.ID})
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
}
func TestCommerceRefundRetainsAdultProofAndStopsSpend(t *testing.T) {
	cw, k, _ := commerceFixture(t, "commerce-refund")
	if cw == nil {
		return
	}
	commerceVerified(t, cw, k)
	ctx := context.Background()
	state, err := cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	o := state.latest("checkout")
	out := providers.Checkout{ID: "cs_test_refund", AccountID: k.result.Account.AccountID, OperationID: o.ID, AmountCents: o.AmountCents, Currency: "usd", Paid: true, PaymentID: "pi_refund", Refunded: true}
	require.NoError(t, cw.svc.applyCommerceCheckout(ctx, cw.ref, k.result.Account.AccountID, o.ID, out))
	answer, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, "verified", answer.Lifecycle.Adult.Status)
	require.False(t, answer.Account.CanHost)
	require.Equal(t, "payment_review_required", answer.Lifecycle.BlockingReason)
	out.Refunded = false // A late paid read cannot undo an observed refund.
	require.NoError(t, cw.svc.applyCommerceCheckout(ctx, cw.ref, k.result.Account.AccountID, o.ID, out))
	state, err = cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	require.EqualValues(t, 1000, state.PaidCents)
	require.Equal(t, "refunded", state.latest("checkout").Status)
	require.True(t, state.FinancialHold)
}
func TestCommerceApprovedProofHasNoArbitraryAnnualExpiry(t *testing.T) {
	cw, k, _ := commerceFixture(t, "commerce-no-age-expiry")
	if cw == nil {
		return
	}
	commerceVerified(t, cw, k)
	cw.clock.Advance(5 * 365 * 24 * time.Hour)
	require.NoError(t, cw.svc.commerceAdultAllowed(context.Background(), cw.ref, k.result.Account.AccountID))
	state, err := cw.svc.store.commerceState(context.Background(), cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	attempts, _, _ := state.verificationCosts()
	require.Equal(t, 1, attempts)
}

func TestCommerceRegistrationCapIsAtomicAcrossReplicas(t *testing.T) {
	cw, _, c := commerceFixture(t, "commerce-register-cap")
	if cw == nil {
		return
	}
	ctx := context.Background()
	baseline, countErr := cw.svc.store.commerceAccountCount(ctx)
	require.NoError(t, countErr)
	c.MaxAccounts = baseline + 1
	first, err := cw.svc.Account(ctx, cw.ref, "", &callrules.AccountRequest{Action: "register_begin"})
	require.NoError(t, err)
	second, err := cw.svc.Account(ctx, cw.ref, "", &callrules.AccountRequest{Action: "register_begin"})
	require.NoError(t, err)
	a, rp, _ := accountCreation(t, first)
	b, _, _ := accountCreation(t, second)
	requests := []callrules.AccountRequest{{Action: "register_finish", ChallengeID: first.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(a, rp, "https://"+rp)}, {Action: "register_finish", ChallengeID: second.ChallengeID, Credential: newSoftwareAuthenticator(t).Attest(b, rp, "https://"+rp)}}
	other := allowanceReplica(t, cw)
	other.commerce = cw.svc.commerce
	var wg sync.WaitGroup
	answers := make(chan error, 2)
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req callrules.AccountRequest) {
			defer wg.Done()
			svc := cw.svc
			if i == 1 {
				svc = other
			}
			_, err := svc.Account(ctx, cw.ref, "", &req)
			answers <- err
		}(i, req)
	}
	wg.Wait()
	close(answers)
	success, refused := 0, 0
	for err := range answers {
		if err == nil {
			success++
		} else {
			require.ErrorContains(t, err, "registration_capacity")
			refused++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, refused)
	require.Empty(t, cw.rowsOf(commerceConcept), "registering a passkey never starts a billable verification")
}

func TestCommerceLegacyCommitmentsCloseNewPurchases(t *testing.T) {
	cw, k, c := commerceFixture(t, "commerce-legacy-block")
	if cw == nil {
		return
	}
	cw.svc.commerce = nil
	_, started := cw.started("Legacy host")
	require.NotEmpty(t, started.Call.ID)
	cw.svc.commerce = func() CommerceConfig { return *c }
	_, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "checkout_begin", AccountID: k.result.Account.AccountID, AmountCents: 1000, IdempotencyKey: "commerce-legacy-checkout", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "unmetered_commitment")
	require.Empty(t, cw.rowsOf(commerceConcept))
}
func TestCommerceVerificationCannotSpendServiceFulfillmentFunds(t *testing.T) {
	cw, k, c := commerceFixture(t, "commerce-margin")
	if cw == nil {
		return
	}
	commercePurchase(t, cw, k, "commerce-margin-first")
	c.FulfillmentReserveBPS = 9500
	_, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "verification_begin", AccountID: k.result.Account.AccountID, IdempotencyKey: "commerce-margin-check", AdultDeclared: true, TermsAccepted: true})
	require.ErrorContains(t, err, "verification_funding_required")
	// Explicitly purchasing enough new credit restores fee coverage; it does not
	// debit the credit or pretend another passkey is another paying customer.
	commercePurchase(t, cw, k, "commerce-margin-second")
	out := commerceBeginVerification(t, cw, k, "commerce-margin-check")
	require.EqualValues(t, 2000, *out.Account.BalanceCents)
	require.Equal(t, 1, out.Lifecycle.VerificationAttempts)
}
