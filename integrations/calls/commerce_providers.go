package calls

import (
	"context"
	"strings"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func (s *Service) dispatchCommerce(ctx context.Context, site callrules.SiteRef, token, key, id string, c CommerceConfig) error {
	v, err := s.store.commerceState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, key)
	if err != nil {
		return err
	}
	if v == nil {
		return allowanceUnavailable()
	}
	o := v.operation(id)
	if o == nil || !o.DispatchClaimed || o.Status != "dispatching" {
		return allowanceUnavailable()
	}
	host, err := s.store.publicHost(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return err
	}
	returnURL := "https://" + host + "/?commerce=" + id
	if o.Kind == "checkout" {
		if c.Payments == nil {
			return accountRefusal("payment_provider_unavailable")
		}
		out, err := c.Payments.CreateCheckout(ctx, providers.CheckoutRequest{OperationID: o.ID, AccountID: key, AmountCents: o.AmountCents, ReturnURL: returnURL, CancelURL: returnURL, SaveMethod: o.SaveMethod, CustomerID: v.CustomerID})
		if err != nil {
			s.markCommerceUnknown(ctx, site, key, id)
			return commerceUnknown(err)
		}
		return s.applyCommerceCheckout(ctx, site, key, id, out)
	}
	if c.Verifications == nil {
		return accountRefusal("verification_provider_unavailable")
	}
	out, err := c.Verifications.CreateVerification(ctx, providers.VerificationRequest{OperationID: o.ID, AccountID: key, CallbackURL: returnURL})
	if err != nil {
		s.markCommerceUnknown(ctx, site, key, id)
		return commerceUnknown(err)
	}
	return s.applyCommerceVerification(ctx, site, key, id, out, false)
}
func (s *Service) refreshCommerce(ctx context.Context, site callrules.SiteRef, key, id string, c CommerceConfig) error {
	v, err := s.store.commerceState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, key)
	if err != nil {
		return err
	}
	if v == nil {
		return allowanceUnavailable()
	}
	o := v.operation(id)
	if o == nil {
		return callrules.ErrNotFound
	}
	if o.ProviderID == "" {
		return accountRefusal("provider_reconciliation_required")
	}
	if o.Kind == "checkout" {
		if c.Payments == nil {
			return accountRefusal("payment_provider_unavailable")
		}
		out, err := c.Payments.ReadCheckout(ctx, o.ProviderID)
		if err != nil {
			return commerceUnknown(err)
		}
		return s.applyCommerceCheckout(ctx, site, key, id, out)
	}
	if o.Kind == "auto_topup" {
		if c.Payments == nil {
			return accountRefusal("payment_provider_unavailable")
		}
		out, err := c.Payments.ReadPayment(ctx, o.ProviderID)
		if err != nil {
			return commerceUnknown(err)
		}
		return s.applyCommercePayment(ctx, site, key, id, out)
	}
	if c.Verifications == nil {
		return accountRefusal("verification_provider_unavailable")
	}
	out, err := c.Verifications.ReadVerification(ctx, o.ProviderID)
	if err != nil {
		return commerceUnknown(err)
	}
	return s.applyCommerceVerification(ctx, site, key, id, out, true)
}
func (s *Service) changeCommerce(ctx context.Context, site callrules.SiteRef, key, id string, change func(*commerceState, *commerceOperation) error) error {
	release, err := s.gates(ctx, accountGate(key), keyGate(key), commerceGlobalGate)
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	v, err := s.store.commerceState(fresh, site.OwnerUserID, site.SiteID, key)
	if err != nil {
		return err
	}
	if v == nil {
		return allowanceUnavailable()
	}
	o := v.operation(id)
	if o == nil {
		return callrules.ErrNotFound
	}
	if err = change(v, o); err != nil {
		return err
	}
	return s.store.saveCommerce(fresh, site.OwnerUserID, v, s.now())
}
func (s *Service) markCommerceUnknown(ctx context.Context, site callrules.SiteRef, key, id string) {
	// Failure to write unknown leaves dispatching, which has the same conservative
	// reservation and no-retry semantics. Neither permits another provider create.
	_ = s.changeCommerce(context.WithoutCancel(ctx), site, key, id, func(_ *commerceState, o *commerceOperation) error {
		if o.Status == "dispatching" && o.ProviderID == "" {
			o.Status = "unknown"
		}
		return nil
	})
}
func (s *Service) applyCommerceCheckout(ctx context.Context, site callrules.SiteRef, key, id string, out providers.Checkout) error {
	return s.changeCommerce(ctx, site, key, id, func(v *commerceState, o *commerceOperation) error {
		if o.Kind != "checkout" || out.ID == "" || out.AccountID != key || out.OperationID != id || out.AmountCents != o.AmountCents || out.Currency != "usd" || (o.ProviderID != "" && o.ProviderID != out.ID) {
			return accountRefusal("provider_receipt_mismatch")
		}
		o.ProviderID = out.ID
		o.PaymentID = out.PaymentID
		o.URL = out.URL
		if out.Refunded || out.Disputed {
			v.FinancialHold = true
			o.Status = "refunded"
			return nil
		}
		if o.Status == "refunded" {
			return nil
		}
		if out.Paid {
			if !o.PaidRecorded {
				v.PaidCents += o.AmountCents
				o.PaidRecorded = true
				// Only the first confirmed payment may designate its opted-in
				// method. Replaying an older successful checkout must not switch
				// the saved card back after the owner replaces it.
				if o.SaveMethod && out.CustomerID != "" && out.PaymentMethodID != "" {
					v.CustomerID = out.CustomerID
					v.PaymentMethodID = out.PaymentMethodID
				}
			}
			o.Status = "paid"
			return s.creditCommerceCheckout(memql.ContextWithFreshRead(ctx), site, v, o)
		}
		if o.Status == "paid" {
			return nil
		}
		if out.Status == "expired" {
			o.Status = "cancelled"
		} else {
			o.Status = "pending"
		}
		return nil
	})
}
func verificationStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "not started":
		return "not_started"
	case "in progress", "resubmitted":
		return "in_progress"
	case "in review":
		return "in_review"
	case "approved":
		return "approved"
	case "declined":
		return "declined"
	case "abandoned":
		return "abandoned"
	case "expired":
		return "expired"
	case "kyc expired":
		return "recheck_required"
	default:
		return "unknown"
	}
}
func (s *Service) applyCommerceVerification(ctx context.Context, site callrules.SiteRef, key, id string, out providers.Verification, authoritative bool) error {
	return s.changeCommerce(ctx, site, key, id, func(v *commerceState, o *commerceOperation) error {
		if o.Kind != "verification" || out.ID == "" || out.AccountID != key || out.OperationID != id || (o.ProviderID != "" && o.ProviderID != out.ID) {
			return accountRefusal("provider_receipt_mismatch")
		}
		o.ProviderID = out.ID
		o.URL = out.URL
		if !authoritative && !verificationPending(o.Status) {
			return nil
		}
		o.Status = verificationStatus(out.Status)
		if latest := v.latest("verification"); latest == nil || latest.ID != o.ID {
			return nil
		}
		if v.Adult.Status == "blocked" {
			o.Status = "declined"
			return nil
		}
		if authoritative && out.Underage {
			o.Status = "declined"
			v.Adult = callrules.AdultEvidence{Status: "blocked", Provider: "didit"}
			return nil
		}
		// Only the authoritative detailed decision proves age and the configured
		// liveness/document/face workflow. A browser redirect or generic Approved
		// status, including the create response, is never adult evidence.
		if authoritative && o.Status == "approved" && out.Adult18 && out.Method != "" {
			// A revoked/expired proof cannot be revived by an older in-flight read.
			// A new, explicitly funded verification must establish a new reference.
			if v.Adult.Status == "recheck_required" && v.AdultReference == out.ID {
				o.Status = "recheck_required"
				return nil
			}
			at := out.VerifiedAt
			if at.IsZero() {
				at = s.now().UTC()
			}
			v.Adult = verifiedAdult(at)
			v.Adult.Method = out.Method
			v.AdultReference = out.ID
			v.AdultWorkflowID, v.AdultWorkflowVersion = out.WorkflowID, out.WorkflowVersion
		} else if authoritative && o.Status == "approved" {
			o.Status = "declined"
			v.Adult = callrules.AdultEvidence{Status: "recheck_required", Provider: "didit"}
		} else if authoritative && v.AdultReference == out.ID && (o.Status == "recheck_required" || o.Status == "declined") {
			v.Adult = callrules.AdultEvidence{Status: "recheck_required", Provider: "didit"}
		}
		return nil
	})
}
func (s *Service) applyCommercePayment(ctx context.Context, site callrules.SiteRef, key, id string, out providers.Payment) error {
	return s.changeCommerce(ctx, site, key, id, func(v *commerceState, o *commerceOperation) error {
		if o.Kind != "auto_topup" || out.ID == "" || out.AccountID != key || out.OperationID != id || out.AmountCents != o.AmountCents || out.Currency != "usd" || (o.ProviderID != "" && o.ProviderID != out.ID) {
			return accountRefusal("provider_receipt_mismatch")
		}
		o.ProviderID = out.ID
		if out.Refunded || out.Disputed {
			o.Status = "refunded"
			v.FinancialHold = true
			return nil
		}
		if o.Status == "refunded" {
			return nil
		}
		if out.Paid {
			if !o.PaidRecorded {
				v.PaidCents += o.AmountCents
				o.PaidRecorded = true
			}
			o.Status = "paid"
			return s.creditCommerceCheckout(memql.ContextWithFreshRead(ctx), site, v, o)
		}
		if o.Status == "paid" {
			return nil
		}
		if out.RequiresAction {
			o.Status = "requires_action"
		} else if out.Status == "canceled" || out.Status == "requires_payment_method" {
			o.Status = "declined"
		} else {
			o.Status = "pending"
		}
		return nil
	})
}

// ReconcileCommerceProvider is a bounded webhook entry point. A verified event
// provides only an object reference; matching the durable provider id and fetching
// the decision prevents event replay/order from crediting or approving a stranger.
func (s *Service) ReconcileCommerceProvider(ctx context.Context, site callrules.SiteRef, provider, objectID string) error {
	c := s.commerceConfig()
	if c.Mode != providers.Sandbox {
		return accountRefusal("provider_webhook_disabled")
	}
	all, err := s.store.commerceAll(ctx)
	if err != nil {
		return err
	}
	for _, v := range all {
		if v.SiteID != site.SiteID || v.Mode != c.Mode {
			continue
		}
		for _, o := range v.Operations {
			if (o.ProviderID == objectID || o.PaymentID == objectID) && ((provider == "stripe" && (o.Kind == "checkout" || o.Kind == "auto_topup")) || (provider == "didit" && o.Kind == "verification")) {
				return s.refreshCommerce(ctx, site, v.KeyID, o.ID, c)
			}
		}
	}
	return callrules.ErrNotFound
}

// ReconcileCommerceProviderEvent also recovers a create whose response was lost.
// Event metadata can locate only an ALREADY reserved, dispatch-claimed effect;
// the fetched provider object must independently match every binding. It cannot
// create an account, a receipt, an allowance or a payment authority.
func (s *Service) ReconcileCommerceProviderEvent(ctx context.Context, site callrules.SiteRef, provider string, event providers.Webhook) error {
	c := s.commerceConfig()
	if c.Mode != providers.Sandbox {
		return accountRefusal("provider_webhook_disabled")
	}
	all, err := s.store.commerceAll(ctx)
	if err != nil {
		return err
	}
	for _, v := range all {
		if v.SiteID != site.SiteID || v.Mode != c.Mode {
			continue
		}
		for _, o := range v.Operations {
			if o.ProviderID == event.ObjectID || o.PaymentID == event.ObjectID {
				return s.ReconcileCommerceProvider(ctx, site, provider, event.ObjectID)
			}
			if !o.DispatchClaimed || o.ID != event.OperationID || v.KeyID != event.AccountID || o.ProviderID != "" {
				continue
			}
			if provider == "didit" && o.Kind == "verification" && c.Verifications != nil {
				out, err := c.Verifications.ReadVerification(ctx, event.ObjectID)
				if err != nil {
					return commerceUnknown(err)
				}
				return s.applyCommerceVerification(ctx, site, v.KeyID, o.ID, out, true)
			}
			if provider == "stripe" && o.Kind == "checkout" && strings.HasPrefix(event.ObjectID, "cs_") && c.Payments != nil {
				out, err := c.Payments.ReadCheckout(ctx, event.ObjectID)
				if err != nil {
					return commerceUnknown(err)
				}
				return s.applyCommerceCheckout(ctx, site, v.KeyID, o.ID, out)
			}
			if provider == "stripe" && o.Kind == "auto_topup" && strings.HasPrefix(event.ObjectID, "pi_") && c.Payments != nil {
				out, err := c.Payments.ReadPayment(ctx, event.ObjectID)
				if err != nil {
					return commerceUnknown(err)
				}
				return s.applyCommercePayment(ctx, site, v.KeyID, o.ID, out)
			}
		}
	}
	return callrules.ErrNotFound
}
