package calls

import (
	"context"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

// commerceAutoTopup is internal to the independently capped delegation path.
// The caller has durably reserved its funding mandate before entering. Only the
// saved method is used, and no argument can select another card or customer.
func (s *Service) commerceAutoTopup(ctx context.Context, site callrules.SiteRef, token, account string, amount int64, operation string) (callrules.AccountResult, error) {
	if amount < 1000 || amount > 50000 || !fundingOperationID.MatchString(operation) {
		return callrules.AccountResult{}, callrules.ErrInvalid
	}
	c := s.commerceConfig()
	if !c.enabled() {
		return callrules.AccountResult{}, accountRefusal("commerce_disabled")
	}
	h, err := s.fundingHolder(ctx, site, token, account)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID), keyGate(h.row.KeyID), commerceGlobalGate)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	released := false
	unlock := func() {
		if !released {
			released = true
			release()
		}
	}
	defer unlock()
	fresh := memql.ContextWithFreshRead(ctx)
	if _, err = s.fundingHolder(fresh, site, token, account); err != nil {
		return callrules.AccountResult{}, err
	}
	v, err := s.store.commerceState(fresh, site.OwnerUserID, site.SiteID, account)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if v == nil {
		return callrules.AccountResult{}, accountRefusal("purchase_required")
	}
	if err = v.validateMode(c); err != nil {
		return callrules.AccountResult{}, err
	}
	id := callrules.HistoryDigest(nil, "commerce-autotopup", site.SiteID, account, operation)[:32]
	if o := v.operation(id); o != nil {
		if o.AmountCents != amount {
			return callrules.AccountResult{}, fundingConflict("idempotency_conflict")
		}
		if o.Status == "paid" {
			if err = s.creditCommerceCheckout(fresh, site, v, o); err != nil {
				return callrules.AccountResult{}, err
			}
			unlock()
			out, err := s.accountSnapshot(fresh, site, token)
			out.Replayed = true
			return out, err
		}
		providerID := o.ProviderID
		unlock()
		if c.Mode == providers.Sandbox && providerID != "" {
			if err = s.refreshCommerce(ctx, site, account, id, c); err != nil {
				return callrules.AccountResult{}, err
			}
			return s.commerceTopupAnswer(ctx, site, token, account, id)
		}
		return callrules.AccountResult{}, accountRefusal("topup_requires_attention")
	}
	if err = s.delegatedEffectAllowed(fresh, site, account); err != nil {
		return callrules.AccountResult{}, err
	}
	if err = c.admission(s.now()); err != nil {
		return callrules.AccountResult{}, err
	}
	if err = s.commerceAdultAllowed(fresh, site, account); err != nil {
		return callrules.AccountResult{}, err
	}
	if v.CustomerID == "" || v.PaymentMethodID == "" {
		return callrules.AccountResult{}, accountRefusal("saved_payment_method_required")
	}
	if len(v.Operations) >= commerceJournalLimit {
		return callrules.AccountResult{}, accountRefusal("account_journal_capacity")
	}
	wallet, err := s.simulatedWallet(fresh, site.OwnerUserID, site.SiteID, account)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	var balance int64
	if wallet != nil {
		balance = wallet.BalanceCents
	}
	for _, o := range v.Operations {
		if o.Kind == "auto_topup" && paymentPending(o.Status) {
			return callrules.AccountResult{}, accountRefusal("topup_pending")
		}
		if o.Kind != "verification" && paymentPending(o.Status) {
			balance += o.AmountCents
		}
	}
	_, available, _, budgetErr := s.usageWalletSnapshot(fresh, site.OwnerUserID, site.SiteID, account)
	if budgetErr != nil {
		return callrules.AccountResult{}, budgetErr
	}
	if err = s.delegatedTopupAllowed(fresh, site, account, amount, available); err != nil {
		return callrules.AccountResult{}, err
	}
	if balance > 50000-amount {
		return callrules.AccountResult{}, fundingConflict("balance_limit")
	}
	if err = s.commerceCapacity(fresh, c, 0, 0, amount, 0); err != nil {
		return callrules.AccountResult{}, err
	}
	o := commerceOperation{ID: id, Kind: "auto_topup", RequestDigest: callrules.HistoryDigest(nil, "commerce-autotopup-request", operation), AmountCents: amount, Status: "dispatching", CreatedAt: s.now(), DispatchClaimed: true}
	v.Operations = append(v.Operations, o)
	if err = s.store.saveCommerce(fresh, site.OwnerUserID, v, s.now()); err != nil {
		return callrules.AccountResult{}, err
	}
	customer, method := v.CustomerID, v.PaymentMethodID
	unlock()
	var payment providers.Payment
	if c.Mode == providers.Mock {
		payment = providers.Payment{ID: "mock_payment_" + id, AccountID: account, OperationID: id, AmountCents: amount, Currency: "usd", Paid: true, Status: "succeeded", CustomerID: customer, PaymentMethodID: method}
	} else {
		if c.Payments == nil {
			return callrules.AccountResult{}, accountRefusal("payment_provider_unavailable")
		}
		payment, err = c.Payments.ChargeOffSession(ctx, providers.ChargeRequest{OperationID: id, AccountID: account, AmountCents: amount, CustomerID: customer, PaymentMethodID: method})
		if err != nil {
			s.markCommerceUnknown(ctx, site, account, id)
			return callrules.AccountResult{}, commerceUnknown(err)
		}
	}
	if err = s.applyCommercePayment(ctx, site, account, id, payment); err != nil {
		return callrules.AccountResult{}, err
	}
	return s.commerceTopupAnswer(ctx, site, token, account, id)
}
func (s *Service) commerceTopupAnswer(ctx context.Context, site callrules.SiteRef, token, account, id string) (callrules.AccountResult, error) {
	v, err := s.store.commerceState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, account)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if v == nil {
		return callrules.AccountResult{}, allowanceUnavailable()
	}
	o := v.operation(id)
	if o == nil || o.Status != "paid" {
		return callrules.AccountResult{}, accountRefusal("topup_requires_attention")
	}
	return s.accountSnapshot(ctx, site, token)
}
