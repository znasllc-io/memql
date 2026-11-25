package calls

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

// commerceFakePayments stands in for Stripe's test mode at the provider
// boundary: hosted checkouts open unpaid, and off-session charges of a saved
// method succeed. The real store, gates and reconciliation stay exercised.
type commerceFakePayments struct {
	mu        sync.Mutex
	checkouts map[string]providers.Checkout
	payments  map[string]providers.Payment
	created   int
	charges   int
}

func (f *commerceFakePayments) CreateCheckout(_ context.Context, r providers.CheckoutRequest) (providers.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	out := providers.Checkout{ID: "cs_test_" + r.OperationID, URL: "https://checkout.stripe.com/c/pay/cs_test_" + r.OperationID, Status: "open", PaymentStatus: "unpaid", AccountID: r.AccountID, OperationID: r.OperationID, AmountCents: r.AmountCents, Currency: "usd"}
	if f.checkouts == nil {
		f.checkouts = map[string]providers.Checkout{}
	}
	f.checkouts[out.ID] = out
	return out, nil
}

func (f *commerceFakePayments) ReadCheckout(_ context.Context, id string) (providers.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, ok := f.checkouts[id]
	if !ok {
		return providers.Checkout{}, providers.ErrOutcomeUnknown
	}
	return out, nil
}

func (f *commerceFakePayments) ChargeOffSession(_ context.Context, r providers.ChargeRequest) (providers.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charges++
	out := providers.Payment{ID: "pi_test_" + r.OperationID, Status: "succeeded", AccountID: r.AccountID, OperationID: r.OperationID, AmountCents: r.AmountCents, Currency: "usd", CustomerID: r.CustomerID, PaymentMethodID: r.PaymentMethodID, Paid: true}
	if f.payments == nil {
		f.payments = map[string]providers.Payment{}
	}
	f.payments[out.ID] = out
	return out, nil
}

func (f *commerceFakePayments) ReadPayment(_ context.Context, id string) (providers.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, ok := f.payments[id]
	if !ok {
		return providers.Payment{}, providers.ErrOutcomeUnknown
	}
	return out, nil
}

func (f *commerceFakePayments) chargeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.charges
}

// commerceSandboxFixture is commerceFixture in sandbox mode with fake provider
// adapters. The tester password stays configured, as it may be on a real
// installation moving from mock to sandbox, and must change nothing there.
func commerceSandboxFixture(t *testing.T, name string) (*callWorld, accountTestKey, *CommerceConfig, *commerceFakePayments) {
	t.Helper()
	cw, k, c := commerceFixture(t, name)
	if cw == nil {
		return nil, accountTestKey{}, nil, nil
	}
	payments := &commerceFakePayments{}
	c.Mode = providers.Sandbox
	c.Payments = payments
	c.Verifications = &commerceFakeVerification{}
	return cw, k, c, payments
}

// commerceSandboxVerified records what a completed sandbox purchase and age
// check leave behind -- one paid $10 checkout with a saved test card, an 18+
// result -- and credits it, without driving the hosted provider pages.
func commerceSandboxVerified(t *testing.T, cw *callWorld, k accountTestKey) {
	t.Helper()
	ctx := memql.ContextWithFreshRead(context.Background())
	key := k.result.Account.AccountID
	at := cw.clock.Now().UTC()
	v := newCommerceState(cw.site, key, providers.Sandbox)
	id := callrules.HistoryDigest(nil, "commerce-operation", cw.site, key, "sandbox-seed-checkout")[:32]
	v.Operations = append(v.Operations, commerceOperation{ID: id, Kind: "checkout", RequestDigest: callrules.HistoryDigest(nil, "commerce-request", "sandbox-seed-checkout"), CreatedAt: at, AmountCents: 1000, SaveMethod: true, Status: "paid", PaidRecorded: true, ProviderID: "cs_test_" + id, PaymentID: "pi_test_" + id})
	v.PaidCents = 1000
	v.CustomerID, v.PaymentMethodID = "cus_test_seed", "pm_test_seed"
	v.Adult = verifiedAdult(at)
	v.AdultReference, v.AdultWorkflowID, v.AdultWorkflowVersion = "didit-sandbox-seed", "sandbox-document-liveness-face-match", 1
	require.NoError(t, cw.svc.store.saveCommerce(ctx, cw.owner, v, cw.clock.Now()))
	require.NoError(t, cw.svc.creditCommerceCheckout(ctx, cw.ref, v, &v.Operations[0]))
}
