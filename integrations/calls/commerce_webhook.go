package calls

import (
	"context"
	"errors"
	"net/http"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

// CommerceWebhook admits provider-authenticated references only. The provider
// payload cannot grant credit or adult status: reconciliation retrieves current
// provider state and matches the durable account operation. Repeated and
// reordered deliveries therefore reach the same idempotent credit operation.
// Admission freshness does not gate this path: stopping new purchases must not
// prevent already-started purchases from being reconciled.
func (s *Service) CommerceWebhook(ctx context.Context, site callrules.SiteRef, provider string, headers http.Header, body []byte) error {
	c := s.commerceConfig()
	if c.Mode != providers.Sandbox {
		return accountRefusal("provider_webhook_disabled")
	}
	var event providers.Webhook
	var err error
	switch provider {
	case "stripe":
		verifier, ok := c.Payments.(interface {
			VerifyWebhook([]byte, string, time.Time) (providers.Webhook, error)
		})
		if !ok {
			return accountRefusal("payment_provider_unavailable")
		}
		event, err = verifier.VerifyWebhook(body, headers.Get("Stripe-Signature"), s.now())
	case "didit":
		verifier, ok := c.Verifications.(interface {
			VerifyWebhook([]byte, http.Header, time.Time) (providers.Webhook, error)
		})
		if !ok {
			return accountRefusal("verification_provider_unavailable")
		}
		event, err = verifier.VerifyWebhook(body, headers, s.now())
	default:
		return callrules.ErrNotFound
	}
	if err != nil {
		// The leaf adapter checks integrity before environment. A signed console
		// sample or wrong-environment event is acknowledged without changing state,
		// so a provider's connectivity test cannot become a real account approval.
		if errors.Is(err, providers.ErrWrongEnvironment) {
			return nil
		}
		return accountRefusal("invalid_provider_webhook")
	}
	if !commerceWebhookEventSupported(provider, event.Type) {
		return nil
	}
	err = s.ReconcileCommerceProviderEvent(ctx, site, provider, event)
	// Providers can deliver unrelated objects. A signed reference may recover an
	// already-reserved create whose response was lost, but never creates an
	// account or authority when no matching operation exists.
	if errors.Is(err, callrules.ErrNotFound) {
		return nil
	}
	return err
}
func commerceWebhookEventSupported(provider, event string) bool {
	if provider == "didit" {
		return event == "status.updated" || event == "data.updated"
	}
	switch event {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.async_payment_failed", "checkout.session.expired",
		"payment_intent.succeeded", "payment_intent.payment_failed", "payment_intent.canceled", "payment_intent.requires_action", "charge.refunded", "charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed", "charge.dispute.funds_withdrawn", "charge.dispute.funds_reinstated":
		return true
	default:
		return false
	}
}
