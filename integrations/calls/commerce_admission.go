package calls

import (
	"context"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) commerceAdultAllowed(ctx context.Context, site callrules.SiteRef, keyID string) error {
	c := s.commerceConfig()
	if !c.enabled() {
		return nil
	}
	v, err := s.store.commerceState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, keyID)
	if err != nil {
		return err
	}
	if v == nil {
		return accountRefusal("adult_verification_required")
	}
	if err = v.validateMode(c); err != nil {
		return err
	}
	if v.FinancialHold {
		return accountRefusal("payment_review_required")
	}
	if v.Adult.Status == "blocked" {
		return accountRefusal("age_requirement_not_met")
	}
	if v.Adult.Status != "verified" || v.Adult.VerifiedAt == nil || v.AdultReference == "" {
		return accountRefusal("adult_verification_required")
	}
	return nil
}

// commerceCapacity is called while the GLOBAL admission gate is held, together
// with the account gate. The existing room's final insert is the reservation;
// unfinished/uncertain provider operations retain their whole maximum cost.
// Historical calls conservatively consume their full quote until accounting
// reconciliation can prove a smaller business cost. We prefer closing admission
// to estimating missing costs from optimistic revenue.
func (s *Service) commerceCapacity(ctx context.Context, c CommerceConfig, verificationCost, callCost, incomingCents int64, participants int) error {
	if err := c.admission(s.now()); err != nil {
		return err
	}
	states, err := s.store.commerceAll(memql.ContextWithFreshRead(ctx))
	if err != nil {
		return err
	}
	var verificationTotal int64
	liability := (incomingCents*c.FulfillmentReserveBPS + 9999) / 10000
	pending := 0
	for _, v := range states {
		if v.Mode != c.Mode {
			continue
		}
		_, reserved, spent := v.verificationCosts()
		verificationTotal += reserved + spent
		for _, o := range v.Operations {
			if o.Kind != "verification" && paymentPending(o.Status) {
				liability += (o.AmountCents*c.FulfillmentReserveBPS + 9999) / 10000
			}
			if o.Kind == "checkout" && paymentPending(o.Status) && s.now().Before(o.CapacityUntil) {
				verificationTotal += o.CostCents
				pending++
			}
			if o.Kind == "verification" && verificationPending(o.Status) {
				pending++
			}
		}
		liability += (v.PaidCents*c.FulfillmentReserveBPS + 9999) / 10000
	}
	if verificationCost > 0 && (pending >= c.MaxPendingVerifications || verificationTotal > c.VerificationBudgetCents-verificationCost || verificationTotal > c.ProviderCreditCents-verificationCost) {
		return accountRefusal("verification_capacity")
	}
	rooms, err := s.store.commerceRooms(ctx)
	if err != nil {
		return err
	}
	var callTotal int64
	open, people := 0, 0
	for _, r := range rooms {
		if r.open() && (r.FundingMode != usageMeteredMode || r.FundingMaxPeople < 1 || r.UsageConfig == "" || r.UsageReservedCents <= 0) {
			return accountRefusal("unmetered_commitment")
		}
		if r.FundingMode == usageMeteredMode && r.UsageReservedCents > 0 {
			callTotal += (r.UsageReservedCents*c.FulfillmentReserveBPS + 9999) / 10000
		}
		if r.open() {
			open++
			people += r.FundingMaxPeople
		}
	}
	if callCost > 0 && (open >= c.MaxOpenCalls || participants > c.MaxParticipants-people || callTotal > c.CallBudgetCents-callCost) {
		return accountRefusal("call_capacity")
	}
	if c.AvailableCashCents-c.ProtectedCents-c.FixedCents-liability-verificationTotal-callTotal < verificationCost+callCost {
		return accountRefusal("business_reserve")
	}
	return nil
}
func (s *Service) commerceOpeningAllowed(ctx context.Context, site callrules.SiteRef, keyID string, cfg *callrules.CallConfiguration) error {
	c := s.commerceConfig()
	if cfg != nil {
		if err := s.familyConfigurationAllowed(ctx, site, keyID, *cfg); err != nil {
			return err
		}
	}
	if !c.enabled() {
		return nil
	}
	if err := s.commerceAdultAllowed(ctx, site, keyID); err != nil {
		return err
	}
	if cfg == nil {
		return accountRefusal("configured_call_required")
	}
	cost := (callrules.PreviewReservation(*cfg)*c.FulfillmentReserveBPS + 9999) / 10000
	return s.commerceCapacity(ctx, c, 0, cost, 0, cfg.ParticipantLimit)
}
func (s *Service) commerceRegistrationAllowed(ctx context.Context) error {
	c := s.commerceConfig()
	if !c.enabled() {
		return nil
	}
	if err := c.admission(s.now()); err != nil {
		return err
	}
	count, err := s.store.commerceAccountCount(memql.ContextWithFreshRead(ctx))
	if err != nil {
		return err
	}
	if count >= c.MaxAccounts {
		return accountRefusal("registration_capacity")
	}
	return s.commerceCapacity(ctx, c, c.VerificationUnitCents, 0, 0, 0)
}
func commerceSnapshot(v *commerceState, c CommerceConfig, canHost bool) *callrules.AccountLifecycle {
	out := &callrules.AccountLifecycle{Mode: string(c.Mode), Adult: v.Adult, MinimumTopUpCents: 1000, SavedPaymentMethod: v.CustomerID != "" && v.PaymentMethodID != ""}
	out.VerificationAttempts, out.VerificationReservedCents, out.VerificationSpentCents = v.verificationCosts()
	if o := v.latest("checkout"); o != nil {
		out.Checkout = &callrules.CheckoutState{ID: o.ID, Status: o.Status, URL: o.URL, AmountCents: o.AmountCents}
	}
	if o := v.latest("verification"); o != nil {
		out.Verification = &callrules.VerificationState{ID: o.ID, Status: o.Status, URL: o.URL}
	}
	out.HostingAllowed = canHost && v.Adult.Status == "verified" && v.Adult.VerifiedAt != nil && v.AdultReference != "" && !v.FinancialHold
	switch {
	case v.FinancialHold:
		out.BlockingReason = "payment_review_required"
		out.HostingAllowed = false
	case v.Adult.Status == "blocked":
		out.BlockingReason = "age_requirement_not_met"
	case v.PaidCents < 1000:
		out.BlockingReason = "purchase_required"
	case v.Adult.Status != "verified":
		out.BlockingReason = "adult_verification_required"
	case !canHost:
		out.BlockingReason = "funding_required"
	}
	return out
}
func (s *Service) commerceAccountSnapshot(ctx context.Context, site callrules.SiteRef, out *callrules.AccountResult, key *Key) error {
	c := s.commerceConfig()
	if !c.enabled() {
		return nil
	}
	v, err := s.store.commerceState(ctx, site.OwnerUserID, site.SiteID, key.ID)
	if err != nil {
		return err
	}
	if v == nil {
		v = newCommerceState(site.SiteID, key.ID, c.Mode)
	}
	out.Lifecycle = commerceSnapshot(v, c, out.Account.CanHost)
	out.Account.FundingMode = string(c.Mode)
	out.Capabilities.Payments = true
	if err = c.admission(s.now()); err != nil {
		out.Lifecycle.HostingAllowed = false
		out.Lifecycle.BlockingReason = callrules.ReasonOf(err)
	}
	if err = v.validateMode(c); err != nil {
		out.Lifecycle.HostingAllowed = false
		out.Lifecycle.BlockingReason = callrules.ReasonOf(err)
	}
	out.Account.CanHost = out.Lifecycle.HostingAllowed
	return nil
}

// noExpiry is intentional: age does not decrease. Fresh authentication and
// account continuity are checked independently; a revoked/disputed proof can
// be invalidated by the provider reconciliation path without a yearly fee.
func verifiedAdult(at time.Time) callrules.AdultEvidence {
	return callrules.AdultEvidence{Status: "verified", Provider: "didit", Method: "id_liveness_face_match", VerifiedAt: &at}
}
