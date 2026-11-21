package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

type qualificationUnknownCheckout struct {
	providers.PaymentProvider
	creates, reads int
	created        providers.Checkout
}

func (p *qualificationUnknownCheckout) CreateCheckout(_ context.Context, r providers.CheckoutRequest) (providers.Checkout, error) {
	p.creates++
	p.created = providers.Checkout{ID: "cs_qualification_unknown", AccountID: r.AccountID,
		OperationID: r.OperationID, AmountCents: r.AmountCents, Currency: "usd", Paid: true,
		PaymentID: "pi_qualification_unknown"}
	return providers.Checkout{}, providers.ErrOutcomeUnknown
}
func (p *qualificationUnknownCheckout) ReadCheckout(_ context.Context, id string) (providers.Checkout, error) {
	p.reads++
	if id != p.created.ID {
		return providers.Checkout{}, providers.ErrOutcomeUnknown
	}
	return p.created, nil
}

func TestCommerceUnknownCheckoutWithoutCallbackRequiresReconciliation(t *testing.T) {
	cw, key, config := commerceFixture(t, "commerce-unknown-nohook")
	if cw == nil {
		return
	}
	ctx := context.Background()
	provider := &qualificationUnknownCheckout{}
	config.Mode = providers.Sandbox
	config.Payments = provider
	config.ValidUntil = cw.clock.Now().Add(24 * time.Hour)
	request := &callrules.AccountRequest{Action: "checkout_begin", AccountID: key.result.Account.AccountID,
		AmountCents: 1000, IdempotencyKey: "qualification-unknown-checkout", AdultDeclared: true, TermsAccepted: true}
	_, err := cw.svc.Account(ctx, cw.ref, key.result.Session.Token, request)
	require.Error(t, err)
	require.Equal(t, 1, provider.creates)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *config }
	replayed, err := replica.Account(ctx, cw.ref, key.result.Session.Token, request)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Zero(t, *replayed.Account.BalanceCents)
	state, err := replica.store.commerceState(ctx, cw.owner, cw.site, key.result.Account.AccountID)
	require.NoError(t, err)
	operation := *state.latest("checkout")
	require.Equal(t, "unknown", operation.Status)
	require.Empty(t, operation.ProviderID)
	// No callback arrives. Neither the background reconciler nor a new begin
	// nonce may dispatch a duplicate payment or pretend the purchase was paid.
	cw.clock.Advance(2 * time.Hour)
	require.NoError(t, replica.Reconcile(ctx))
	refresh := &callrules.AccountRequest{Action: "checkout_refresh", AccountID: request.AccountID, OperationID: operation.ID}
	_, err = replica.Account(ctx, cw.ref, key.result.Session.Token, refresh)
	require.ErrorContains(t, err, "provider_reconciliation_required")
	second := *request
	second.IdempotencyKey = "qualification-no-duplicate-checkout"
	pending, err := replica.Account(ctx, cw.ref, key.result.Session.Token, &second)
	require.NoError(t, err)
	require.Equal(t, operation.ID, pending.Lifecycle.Checkout.ID)
	require.Zero(t, *pending.Account.BalanceCents)
	require.Equal(t, 1, provider.creates)
	require.Zero(t, provider.reads)
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
	state, err = replica.store.commerceState(ctx, cw.owner, cw.site, request.AccountID)
	require.NoError(t, err)
	require.Len(t, state.Operations, 1)
	require.Zero(t, state.PaidCents)
	// A later trusted provider reference can recover the paid fact exactly once.
	// This proves safe reconciliation, not an automatic no-callback sweeper.
	event := providers.Webhook{ObjectID: provider.created.ID, AccountID: request.AccountID, OperationID: operation.ID}
	require.NoError(t, replica.ReconcileCommerceProviderEvent(ctx, cw.ref, "stripe", event))
	require.NoError(t, cw.svc.ReconcileCommerceProviderEvent(ctx, cw.ref, "stripe", event))
	after, err := replica.Account(ctx, cw.ref, key.result.Session.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *after.Account.BalanceCents)
	require.Equal(t, 1, provider.creates)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
}
