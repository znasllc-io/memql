package shopify

import (
	"context"
	"fmt"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

func managedWebhookHarness(t *testing.T) *testHarness {
	h := newHarness(t)
	h.engine.setRows(namedRowsQuery(conceptGlobalVariable, managedClientID), []map[string]any{{"value": "managed-app"}})
	h.engine.setRows(namedRowsQuery(conceptGlobalSecret, managedClientSecret), []map[string]any{{"encryptedValue": "sealed-test"}})
	h.conn.stores.secrets = func(_ context.Context, name string) (string, error) {
		if name == managedClientSecret {
			return "managed-secret", nil
		}
		return "", fmt.Errorf("missing secret")
	}
	return h
}

func TestManagedPrivacyEndpointExistsWithoutAnyStoreOrListing(t *testing.T) {
	h := managedWebhookHarness(t)
	h.engine.setRows("stores", nil)
	src, ok := h.conn.InboundSource(context.Background(), ConnectorName)
	if !ok || src.Secret != "managed-secret" || src.SignatureHeader != HeaderHMAC || len(src.ForwardHeaders) == 0 {
		t.Fatal("managed privacy source did not resolve before the first installation")
	}
	h.engine.setRows(namedRowsQuery(conceptGlobalSecret, managedClientSecret), nil)
	if _, ok := h.conn.InboundSource(context.Background(), ConnectorName); ok {
		t.Fatal("missing app secret admitted a managed source")
	}
}

func TestManagedComplianceBindsSignedShopToRegisteredApp(t *testing.T) {
	for _, tc := range []struct {
		name, client, bodyDomain, headerDomain, topic string
		want                                          bool
	}{
		{"matching paused store", "managed-app", "acme.myshopify.com", "acme.myshopify.com", TopicRedact, true},
		{"signed body without header", "managed-app", "acme.myshopify.com", "", TopicRedact, true},
		{"another app", "another-app", "acme.myshopify.com", "acme.myshopify.com", TopicRedact, false},
		{"forged header", "managed-app", "beta.myshopify.com", "acme.myshopify.com", TopicRedact, false},
		{"mismatched header", "managed-app", "acme.myshopify.com", "beta.myshopify.com", TopicRedact, false},
		{"missing signed identity", "managed-app", "", "acme.myshopify.com", TopicRedact, false},
		{"ordinary topic", "managed-app", "acme.myshopify.com", "acme.myshopify.com", "products/update", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := managedWebhookHarness(t)
			h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": tc.client, "status": StatusPaused}})
			req := memqlsync.InboundRequest{Source: ConnectorName, Body: []byte(fmt.Sprintf(`{"shop_domain":%q,"customer":{"id":991}}`, tc.bodyDomain)), Headers: map[string]string{strings.ToLower(HeaderShopDomain): tc.headerDomain, strings.ToLower(HeaderTopic): tc.topic}}
			_, err := h.conn.Apply(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(h.engine.callsTo("queueComplianceJob")) == 1; got != tc.want {
				t.Fatalf("privacy job queued = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPerStorePrivacyRejectsAnotherSignedShop(t *testing.T) {
	h := newHarness(t)
	_, err := h.conn.Apply(context.Background(), complianceDelivery(TopicShopRedact, map[string]any{"shop_domain": "another.myshopify.com"}))
	if err == nil || len(h.engine.callsTo("queueComplianceJob")) != 0 {
		t.Fatal("another store's signed payload queued a purge")
	}
}
