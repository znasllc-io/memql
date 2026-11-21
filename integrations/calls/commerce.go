package calls

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func commerceAction(action string) bool {
	switch action {
	case "checkout_begin", "checkout_refresh", "verification_begin", "verification_refresh", "mock_checkout_complete", "mock_verification_complete":
		return true
	}
	return false
}
func validCommerceRequest(r callrules.AccountRequest) bool {
	if !commerceAction(r.Action) || r.AccountID == "" || len(r.AccountID) > 128 || r.Key != "" || r.ChallengeID != "" || len(r.Credential) != 0 || r.CredentialID != "" || r.Label != "" || r.Plan != "" || r.ParticipantLimit != 0 || r.DurationMinutes != 0 || r.Insights {
		return false
	}
	switch r.Action {
	case "checkout_begin":
		return fundingOperationID.MatchString(r.IdempotencyKey) && r.AmountCents >= 1000 && r.AmountCents <= 50000 && r.OperationID == "" && r.Outcome == "" && r.AdultDeclared && r.TermsAccepted
	case "verification_begin":
		return fundingOperationID.MatchString(r.IdempotencyKey) && r.AmountCents == 0 && r.OperationID == "" && r.Outcome == "" && r.AdultDeclared && r.TermsAccepted && !r.SaveMethod
	default:
		return r.OperationID != "" && len(r.OperationID) <= 128 && r.IdempotencyKey == "" && r.AmountCents == 0 && !r.AdultDeclared && !r.TermsAccepted && !r.SaveMethod && len(r.Outcome) <= 32
	}
}
func (s *Service) accountCommerce(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest) (callrules.AccountResult, error) {
	c := s.commerceConfig()
	if !c.enabled() {
		return callrules.AccountResult{}, accountRefusal("commerce_disabled")
	}
	h, err := s.fundingHolder(ctx, site, token, req.AccountID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID), keyGate(h.row.KeyID), commerceGlobalGate)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	// Release explicitly before provider I/O. The durable dispatch claim carries
	// authority across that gap and a second replica never creates another effect.
	released := false
	unlock := func() {
		if !released {
			released = true
			release()
		}
	}
	defer unlock()
	fresh := memql.ContextWithFreshRead(ctx)
	h, err = s.fundingHolder(fresh, site, token, req.AccountID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	v, err := s.store.commerceState(fresh, site.OwnerUserID, site.SiteID, h.row.KeyID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if v == nil {
		v = newCommerceState(site.SiteID, h.row.KeyID, c.Mode)
	}
	if err = v.validateMode(c); err != nil {
		return callrules.AccountResult{}, err
	}
	operationID := req.OperationID
	if strings.HasSuffix(req.Action, "_begin") {
		if err = c.admission(s.now()); err != nil {
			return callrules.AccountResult{}, err
		}
		if v.FinancialHold {
			return callrules.AccountResult{}, accountRefusal("payment_review_required")
		}
		if v.Adult.Status == "blocked" {
			return callrules.AccountResult{}, accountRefusal("age_requirement_not_met")
		}
		kind := strings.TrimSuffix(req.Action, "_begin")
		operationID = callrules.HistoryDigest(nil, "commerce-operation", site.SiteID, h.row.KeyID, req.IdempotencyKey)[:32]
		raw, _ := json.Marshal(req)
		digest := callrules.HistoryDigest(nil, "commerce-request", string(raw))
		if prior := v.operation(operationID); prior != nil {
			if prior.RequestDigest != digest {
				return callrules.AccountResult{}, fundingConflict("idempotency_conflict")
			}
			unlock()
			out, err := s.accountSnapshot(fresh, site, token)
			out.Replayed = true
			return out, err
		}
		// An existing proof is an account fact, not another provider request. Adding
		// passkeys, signing in or requesting another call does not contact Didit.
		if kind == "verification" && v.Adult.Status == "verified" {
			unlock()
			out, err := s.accountSnapshot(fresh, site, token)
			out.Replayed = true
			return out, err
		}
		if len(v.Operations) >= commerceJournalLimit {
			return callrules.AccountResult{}, accountRefusal("account_journal_capacity")
		}
		for _, o := range v.Operations {
			if (kind == "verification" && o.Kind == kind && verificationPending(o.Status)) || (kind == "checkout" && o.Kind == kind && paymentPending(o.Status)) {
				unlock()
				return s.accountSnapshot(fresh, site, token)
			}
		}
		o := commerceOperation{ID: operationID, Kind: kind, RequestDigest: digest, CreatedAt: s.now().UTC(), AmountCents: req.AmountCents, SaveMethod: req.SaveMethod, Status: "pending"}
		verificationCost := int64(0)
		if kind == "verification" {
			attempts, reserved, spent := v.verificationCosts()
			if v.PaidCents < 1000 {
				return callrules.AccountResult{}, accountRefusal("purchase_required")
			}
			if attempts >= c.MaxVerificationAttempts {
				return callrules.AccountResult{}, accountRefusal("verification_attempt_limit")
			}
			if commerceVerificationFunds(v, c, 0)-reserved-spent < c.VerificationUnitCents {
				return callrules.AccountResult{}, accountRefusal("verification_funding_required")
			}
			verificationCost = c.VerificationUnitCents
			o.CostCents = verificationCost
			o.Status = "not_started"
		} else {
			wallet, err := s.simulatedWallet(fresh, site.OwnerUserID, site.SiteID, h.row.KeyID)
			if err != nil {
				return callrules.AccountResult{}, err
			}
			var balance int64
			if wallet != nil {
				balance = wallet.BalanceCents
			}
			for _, pending := range v.Operations {
				if pending.Kind != "verification" && paymentPending(pending.Status) {
					balance += pending.AmountCents
				}
			}
			if balance > 50000-req.AmountCents {
				return callrules.AccountResult{}, fundingConflict("balance_limit")
			}
			if v.Adult.Status != "verified" {
				_, held, spent := v.verificationCosts()
				if commerceVerificationFunds(v, c, req.AmountCents)-held-spent < c.VerificationUnitCents {
					return callrules.AccountResult{}, accountRefusal("verification_funding_required")
				}
				verificationCost = c.VerificationUnitCents
				o.CostCents = verificationCost
				o.CapacityUntil = s.now().Add(15 * time.Minute)
			}
		}
		if err = s.commerceCapacity(fresh, c, verificationCost, 0, req.AmountCents, 0); err != nil {
			return callrules.AccountResult{}, err
		}
		if c.Mode == providers.Sandbox {
			o.Status = "dispatching"
			o.DispatchClaimed = true
		}
		v.Operations = append(v.Operations, o)
		if err = s.store.saveCommerce(fresh, site.OwnerUserID, v, s.now()); err != nil {
			return callrules.AccountResult{}, err
		}
		unlock()
		if c.Mode == providers.Sandbox {
			if err = s.dispatchCommerce(ctx, site, token, h.row.KeyID, operationID, c); err != nil {
				return callrules.AccountResult{}, err
			}
		}
		return s.accountSnapshot(fresh, site, token)
	}
	o := v.operation(operationID)
	if o == nil {
		return callrules.AccountResult{}, callrules.ErrNotFound
	}
	if strings.HasPrefix(req.Action, "mock_") {
		if c.Mode != providers.Mock {
			return callrules.AccountResult{}, accountRefusal("simulation_disabled")
		}
		if err = s.completeMockCommerce(fresh, site, v, o, req); err != nil {
			return callrules.AccountResult{}, err
		}
		if err = s.store.saveCommerce(fresh, site.OwnerUserID, v, s.now()); err != nil {
			return callrules.AccountResult{}, err
		}
		unlock()
		return s.accountSnapshot(fresh, site, token)
	}
	if req.Outcome != "" || (req.Action == "checkout_refresh" && o.Kind != "checkout") || (req.Action == "verification_refresh" && o.Kind != "verification") {
		return callrules.AccountResult{}, callrules.ErrInvalid
	}
	if o.Status == "paid" {
		if err = s.creditCommerceCheckout(fresh, site, v, o); err != nil {
			return callrules.AccountResult{}, err
		}
	}
	unlock()
	if c.Mode == providers.Sandbox {
		if err = s.refreshCommerce(ctx, site, h.row.KeyID, operationID, c); err != nil {
			return callrules.AccountResult{}, err
		}
	}
	return s.accountSnapshot(fresh, site, token)
}

func (s *Service) completeMockCommerce(ctx context.Context, site callrules.SiteRef, v *commerceState, o *commerceOperation, req callrules.AccountRequest) error {
	if req.Action == "mock_checkout_complete" && o.Kind == "checkout" {
		switch req.Outcome {
		case "paid", "declined", "requires_action", "cancelled", "pending":
		default:
			return callrules.ErrInvalid
		}
		if !paymentPending(o.Status) {
			if o.Status == req.Outcome {
				return s.creditCommerceCheckout(ctx, site, v, o)
			}
			return fundingConflict("operation_finished")
		}
		o.Status = req.Outcome
		if req.Outcome == "paid" {
			if !o.PaidRecorded {
				v.PaidCents += o.AmountCents
				o.PaidRecorded = true
			}
			if o.SaveMethod {
				v.CustomerID = "mock_customer_" + v.ID
				v.PaymentMethodID = "mock_method_" + v.ID
			}
			return s.creditCommerceCheckout(ctx, site, v, o)
		}
		return nil
	}
	if req.Action == "mock_verification_complete" && o.Kind == "verification" {
		switch req.Outcome {
		case "approved", "underage", "declined", "in_review", "abandoned", "expired":
		default:
			return callrules.ErrInvalid
		}
		if !verificationPending(o.Status) {
			if o.Status == req.Outcome || req.Outcome == "underage" && v.Adult.Status == "blocked" {
				return nil
			}
			return fundingConflict("operation_finished")
		}
		o.Status = req.Outcome
		if req.Outcome == "approved" {
			at := s.now().UTC()
			v.Adult = verifiedAdult(at)
			v.AdultReference = o.ID
			v.AdultWorkflowID, v.AdultWorkflowVersion = "local-document-liveness-face-match", 1
		}
		if req.Outcome == "underage" {
			o.Status = "declined"
			v.Adult = callrules.AdultEvidence{Status: "blocked", Provider: "didit"}
		}
		return nil
	}
	return callrules.ErrInvalid
}

// The immutable wallet receipt is independently idempotent. Save the provider
// paid fact first, then reconcile this effect: a crash between them cannot turn
// a paid purchase into an unpaid one or duplicate the credit.
func (s *Service) creditCommerceCheckout(ctx context.Context, site callrules.SiteRef, v *commerceState, o *commerceOperation) error {
	if o.Status != "paid" {
		return nil
	}
	if err := s.store.saveCommerce(ctx, site.OwnerUserID, v, s.now()); err != nil {
		return err
	}
	id := callrules.HistoryDigest(nil, "commerce-credit", site.SiteID, v.KeyID, o.ID)[:32]
	existing, err := s.store.simulatedWalletEntryByID(ctx, site.OwnerUserID, id)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.RequestDigest != o.RequestDigest || existing.CreditedCents != o.AmountCents {
			return allowanceUnavailable()
		}
		return nil
	}
	previous, err := s.simulatedWallet(ctx, site.OwnerUserID, site.SiteID, v.KeyID)
	if err != nil {
		return err
	}
	entry := simulationWalletEntry{ID: id, SiteID: site.SiteID, KeyID: v.KeyID, Kind: "topup", RequestDigest: o.RequestDigest, CreditedCents: o.AmountCents, Version: s.now().UTC().Truncate(time.Microsecond)}
	if previous != nil {
		entry.UsageRemainderUnits = previous.UsageRemainderUnits
		entry.BalanceCents = previous.BalanceCents
		entry.Pass = previous.Pass
		entry.Version = memql.VersionTimeAfter(previous.Version, entry.Version)
	}
	if entry.BalanceCents > 50000-o.AmountCents {
		return fundingConflict("balance_limit")
	}
	entry.BalanceCents += o.AmountCents
	return s.store.createSimulatedWalletEntry(ctx, site.OwnerUserID, entry)
}

// Verification spends only the customer-funded margin AFTER conservatively
// reserving fulfillment of the full advertised credit. A past purchase whose
// service obligation already used its funding cannot become a company advance.
func commerceVerificationFunds(v *commerceState, c CommerceConfig, incoming int64) int64 {
	return (v.PaidCents + incoming) * (10000 - c.FulfillmentReserveBPS) / 10000
}
