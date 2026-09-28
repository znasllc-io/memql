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
			req := memqlsync.InboundRequest{Source: ConnectorName, Body: []byte(fmt.Sprintf(`{"shop_domain":%q,"customer":{"id":991},"orders_to_redact":[]}`, tc.bodyDomain)), Headers: map[string]string{strings.ToLower(HeaderShopDomain): tc.headerDomain, strings.ToLower(HeaderTopic): tc.topic}}
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

// A store installed through the managed app signs its per-store deliveries
// with the app secret, so a captured privacy delivery replays onto the
// per-store URL too. Its privacy topics are accepted only on the app-level
// source, where the webhook id collapses a replay; the same delivery on the
// per-store URL is refused with nothing queued. A custom-app store keeps
// its per-store compliance URL (memql#5707 review).
func TestManagedStorePrivacyArrivesOnlyOnTheAppLevelSource(t *testing.T) {
	const body = `{"shop_domain":"acme.myshopify.com","customer":{"id":991},"data_request":{"id":7}}`
	for _, tc := range []struct {
		name, client, source string
		want                 bool
	}{
		{"managed store on its per-store source", "managed-app", "shopify-acme", false},
		{"managed store on the app-level source", "managed-app", ConnectorName, true},
		{"custom-app store on its per-store source", "custom-app", "shopify-acme", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := managedWebhookHarness(t)
			h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": tc.client, "status": StatusPaused}})
			req := memqlsync.InboundRequest{Source: tc.source, Body: []byte(body), Headers: map[string]string{strings.ToLower(HeaderTopic): TopicDataRequest, strings.ToLower(HeaderShopDomain): "acme.myshopify.com"}}
			_, err := h.conn.Apply(context.Background(), req)
			queued := len(h.engine.callsTo("queueComplianceJob")) == 1
			if queued != tc.want || (err == nil) != tc.want {
				t.Fatalf("queued=%v err=%v, want queued=%v", queued, err, tc.want)
			}
		})
	}
}

// A half-configured managed app -- a client id with no secret row, an empty
// sealed value, or a secret that cannot be unsealed -- claims no app-level
// source AND binds no store: the receiver and the connector answer the same
// predicate. A row staged under `shopify` by any other verifier is refused
// with a reason, nothing queued (memql#5707 review).
func TestAHalfConfiguredManagedAppBindsNoStoreOnTheAppLevelSource(t *testing.T) {
	const body = `{"shop_domain":"acme.myshopify.com","customer":{"id":991},"data_request":{"id":7}}`
	for _, tc := range []struct {
		name   string
		break_ func(h *testHarness)
	}{
		{"no secret row", func(h *testHarness) { h.engine.setRows(namedRowsQuery(conceptGlobalSecret, managedClientSecret), nil) }},
		{"empty sealed value", func(h *testHarness) {
			h.engine.setRows(namedRowsQuery(conceptGlobalSecret, managedClientSecret), []map[string]any{{"encryptedValue": ""}})
		}},
		{"unsealable secret", func(h *testHarness) {
			h.conn.stores.secrets = func(context.Context, string) (string, error) { return "", fmt.Errorf("MEMQL_MASTER_KEY rotated") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := managedWebhookHarness(t)
			h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": "managed-app", "status": StatusPaused}})
			tc.break_(h)
			if _, ok := h.conn.InboundSource(context.Background(), ConnectorName); ok {
				t.Fatal("a half-configured app offered an app-level source to the receiver")
			}
			req := memqlsync.InboundRequest{Source: ConnectorName, Body: []byte(body), Headers: map[string]string{strings.ToLower(HeaderTopic): TopicDataRequest}}
			if _, ok := h.conn.StoreFor(context.Background(), req); ok {
				t.Fatal("a half-configured app bound a store by client id alone")
			}
			_, err := h.conn.Apply(context.Background(), req)
			if err == nil || len(h.engine.callsTo("queueComplianceJob")) != 0 {
				t.Fatalf("app-level row under a half-configured app: err=%v, jobs=%d; want a refusal and no job", err, len(h.engine.callsTo("queueComplianceJob")))
			}
		})
	}
}

// The replay matrix again, on the APP-LEVEL source for a managed store: the
// signed body decides the operation there too.
func TestAReplayedPrivacyBodyUnderAnotherTopicQueuesNothingOnTheAppLevelSource(t *testing.T) {
	const shop = "acme.myshopify.com"
	bodies := map[string]string{
		TopicDataRequest: `{"shop_domain":"` + shop + `","customer":{"id":991},"data_request":{"id":42},"orders_requested":[]}`,
		TopicRedact:      `{"shop_domain":"` + shop + `","customer":{"id":991},"orders_to_redact":[]}`,
		TopicShopRedact:  `{"shop_domain":"` + shop + `","shop_id":954889}`,
	}
	for signedAs, body := range bodies {
		for _, replayedAs := range []string{TopicDataRequest, TopicRedact, TopicShopRedact} {
			t.Run(signedAs+" replayed as "+replayedAs, func(t *testing.T) {
				h := managedWebhookHarness(t)
				h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": shop, "appClientId": "managed-app", "status": StatusPaused}})
				req := memqlsync.InboundRequest{Source: ConnectorName, Body: []byte(body), Headers: map[string]string{strings.ToLower(HeaderTopic): replayedAs, strings.ToLower(HeaderShopDomain): shop}}
				_, err := h.conn.Apply(context.Background(), req)
				queued := len(h.engine.callsTo("queueComplianceJob"))
				if signedAs == replayedAs {
					if err != nil || queued != 1 {
						t.Fatalf("the genuine delivery: queued=%d err=%v", queued, err)
					}
					return
				}
				if err == nil || queued != 0 {
					t.Fatalf("a replay under another topic queued %d jobs (err=%v)", queued, err)
				}
			})
		}
	}
}
