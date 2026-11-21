package calls

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func commerceWebhookSignature(body []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
func TestCommerceWebhookRejectsForgedEventsBeforeAnyStoreRead(t *testing.T) {
	now := time.Unix(1800000000, 0)
	stripe, _ := providers.NewStripe(providers.StripeConfig{Mode: providers.Sandbox, SecretKey: "sk_test_fixture", WebhookSecret: "fixture"})
	didit, _ := providers.NewDidit(providers.DiditConfig{Mode: providers.Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 1, WebhookSecret: "fixture"})
	s := &Service{commerce: func() CommerceConfig {
		return CommerceConfig{Mode: providers.Sandbox, Payments: stripe, Verifications: didit}
	}, now: func() time.Time { return now }}
	for _, provider := range []string{"stripe", "didit"} {
		t.Run(provider, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("X-Didit-Test-Webhook", "true")
			if err := s.CommerceWebhook(context.Background(), callrules.SiteRef{}, provider, headers, []byte(`{"status":"Approved"}`)); err == nil {
				t.Fatal("forged event accepted")
			}
		})
	}
}
func TestCommerceWebhookIgnoresAuthenticatedSamplesAndUnrelatedEvents(t *testing.T) {
	now := time.Unix(1800000000, 0)
	secret := "fixture"
	stripe, _ := providers.NewStripe(providers.StripeConfig{Mode: providers.Sandbox, SecretKey: "sk_test_fixture", WebhookSecret: secret})
	didit, _ := providers.NewDidit(providers.DiditConfig{Mode: providers.Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 1, WebhookSecret: secret})
	s := &Service{commerce: func() CommerceConfig {
		return CommerceConfig{Mode: providers.Sandbox, Payments: stripe, Verifications: didit}
	}, now: func() time.Time { return now }}
	// No store is installed: ignored events must not read or mutate account data.
	stripeBody := []byte(`{"id":"evt_1","type":"customer.created","livemode":false,"data":{"object":{"id":"cus_1"}}}`)
	stripeHeaders := http.Header{}
	stripeHeaders.Set("Stripe-Signature", "t=1800000000,v1="+commerceWebhookSignature(append([]byte("1800000000."), stripeBody...), secret))
	if err := s.CommerceWebhook(context.Background(), callrules.SiteRef{}, "stripe", stripeHeaders, stripeBody); err != nil {
		t.Fatal(err)
	}
	diditBody := []byte(`{"event_id":"evt_1","webhook_type":"status.updated","session_id":"session_1","status":"Approved","environment":"sandbox","timestamp":1800000000}`)
	diditHeaders := http.Header{}
	diditHeaders.Set("X-Timestamp", "1800000000")
	diditHeaders.Set("X-Signature", commerceWebhookSignature(diditBody, secret))
	diditHeaders.Set("X-Didit-Test-Webhook", "true")
	if err := s.CommerceWebhook(context.Background(), callrules.SiteRef{}, "didit", diditHeaders, diditBody); err != nil {
		t.Fatal(err)
	}
}
func TestCommerceWebhookMockModeCannotAcceptProviderEvents(t *testing.T) {
	s := &Service{commerce: func() CommerceConfig { return CommerceConfig{Mode: providers.Mock} }}
	if err := s.CommerceWebhook(context.Background(), callrules.SiteRef{}, "stripe", http.Header{}, []byte(`{}`)); err == nil {
		t.Fatal("mock account accepted provider input")
	}
}
