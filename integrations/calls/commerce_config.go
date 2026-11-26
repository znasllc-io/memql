package calls

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/integrations/calls/providers"
)

// CommerceConfig is an explicit simulation/sandbox admission envelope. Values
// are ceilings, not a profit forecast. Live payments are deliberately refused.
// AvailableCash excludes pending processor payouts; ProtectedCents includes the
// owner's cash cushion and expected tax/refund/dispute obligations. FixedCents
// covers already-committed infrastructure. A missing or stale observation closes
// NEW effects; readers and reconciliation remain available.
type CommerceConfig struct {
	Mode                                           providers.Mode
	ValidUntil                                     time.Time
	AvailableCashCents, ProtectedCents, FixedCents int64
	// Unit must cover the entire workflow including all permitted hosted SDK
	// retries, optional features, branding fees and tax; a base check price is
	// insufficient because provider-side retries may complete billable steps.
	VerificationUnitCents, VerificationBudgetCents, ProviderCreditCents                          int64
	CallBudgetCents                                                                              int64
	FulfillmentReserveBPS                                                                        int64
	MaxAccounts, MaxPendingVerifications, MaxVerificationAttempts, MaxOpenCalls, MaxParticipants int
	Payments                                                                                     providers.PaymentProvider
	Verifications                                                                                providers.VerificationProvider
	Invalid                                                                                      bool
}

func (s *Service) commerceConfig() CommerceConfig {
	if s.commerce != nil {
		return s.commerce()
	}
	mode := providers.Mode(strings.TrimSpace(os.Getenv("MEMQL_CALLS_COMMERCE_MODE")))
	if mode == "" || mode == providers.Disabled {
		return CommerceConfig{Mode: providers.Disabled}
	}
	c := CommerceConfig{Mode: mode}
	if mode != providers.Mock && mode != providers.Sandbox {
		c.Invalid = true
		return c
	}
	parse := func(name string) int64 {
		n, err := strconv.ParseInt(os.Getenv("MEMQL_CALLS_COMMERCE_"+name), 10, 64)
		if err != nil || n < 0 || n > 1000000000 {
			c.Invalid = true
		}
		return n
	}
	c.AvailableCashCents = parse("CASH_CENTS")
	c.ProtectedCents = parse("PROTECTED_CENTS")
	c.FixedCents = parse("FIXED_CENTS")
	c.VerificationUnitCents = parse("VERIFICATION_UNIT_CENTS")
	c.VerificationBudgetCents = parse("VERIFICATION_BUDGET_CENTS")
	c.ProviderCreditCents = parse("PROVIDER_CREDIT_CENTS")
	c.CallBudgetCents = parse("CALL_BUDGET_CENTS")
	c.FulfillmentReserveBPS = parse("FULFILLMENT_RESERVE_BPS")
	c.MaxAccounts = int(parse("MAX_ACCOUNTS"))
	c.MaxPendingVerifications = int(parse("MAX_PENDING_VERIFICATIONS"))
	c.MaxVerificationAttempts = int(parse("MAX_VERIFICATION_ATTEMPTS"))
	c.MaxOpenCalls = int(parse("MAX_OPEN_CALLS"))
	c.MaxParticipants = int(parse("MAX_PARTICIPANTS"))
	var err error
	c.ValidUntil, err = time.Parse(time.RFC3339, os.Getenv("MEMQL_CALLS_COMMERCE_VALID_UNTIL"))
	if err != nil {
		c.Invalid = true
	}
	if mode == providers.Sandbox {
		c.Payments, err = providers.NewStripe(providers.StripeConfig{Mode: mode, SecretKey: os.Getenv("MEMQL_CALLS_STRIPE_SECRET_KEY"), WebhookSecret: os.Getenv("MEMQL_CALLS_STRIPE_WEBHOOK_SECRET")})
		if err != nil {
			c.Invalid = true
		}
		version, _ := strconv.Atoi(os.Getenv("MEMQL_CALLS_DIDIT_WORKFLOW_VERSION"))
		c.Verifications, err = providers.NewDidit(providers.DiditConfig{Mode: mode, APIKey: os.Getenv("MEMQL_CALLS_DIDIT_API_KEY"), WebhookSecret: os.Getenv("MEMQL_CALLS_DIDIT_WEBHOOK_SECRET"), WorkflowID: os.Getenv("MEMQL_CALLS_DIDIT_WORKFLOW_ID"), WorkflowVersion: version})
		if err != nil {
			c.Invalid = true
		}
	}
	return c
}
func (c CommerceConfig) enabled() bool { return c.Mode != "" && c.Mode != providers.Disabled }
func (c CommerceConfig) admission(now time.Time) error {
	for _, cents := range []int64{c.AvailableCashCents, c.ProtectedCents, c.FixedCents, c.VerificationUnitCents, c.VerificationBudgetCents, c.ProviderCreditCents, c.CallBudgetCents} {
		if cents < 0 || cents > 1000000000 {
			return accountRefusal("admission_unavailable")
		}
	}
	if !c.enabled() {
		return accountRefusal("commerce_disabled")
	}
	if c.Invalid || (c.Mode != providers.Mock && c.Mode != providers.Sandbox) || c.VerificationUnitCents <= 0 || c.FulfillmentReserveBPS < 1 || c.FulfillmentReserveBPS > 10000 || c.MaxAccounts < 1 || c.MaxAccounts >= 1000 || c.MaxVerificationAttempts < 1 || c.MaxVerificationAttempts > 20 || c.MaxPendingVerifications < 1 || c.MaxOpenCalls < 1 || c.MaxParticipants < 1 {
		return accountRefusal("admission_unavailable")
	}
	if c.ValidUntil.IsZero() || !now.Before(c.ValidUntil) {
		return accountRefusal("admission_stale")
	}
	return nil
}

// SetCommerceConfig allows the installed operator/test harness to provide a
// fresh measured envelope without adding an administrator UI. It is never a
// visitor operation and no browser request can change it.
func (s *Service) SetCommerceConfig(config func() CommerceConfig) { s.commerce = config }
