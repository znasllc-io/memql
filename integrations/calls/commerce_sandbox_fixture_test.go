package calls

import (
	"context"
	"sync"
	"testing"

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
