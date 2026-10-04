package shopify

import (
	"context"
	"fmt"
	"slices"
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

// The APP-LEVEL source's staging allowlist, exactly. The receiver stages
// precisely these request headers on the inboundRequest row and nothing else,
// so this list IS the staged metadata for every compliance delivery: narrow it
// and the topic -- which only travels in X-Shopify-Topic -- never reaches
// Apply, so no privacy job is queued; widen it and whatever was added lands on
// a row any operator can read. The per-store twin is proved end to end
// against Postgres by test/inboundhop's exact-map assertion; this is the
// app-level half, which had only a non-empty check.
//
// The names are spelled as the wire carries them rather than through the
// Header* constants, so a renamed constant cannot move the list and this test
// with it. The per-store source must offer the same list: a delivery's
// metadata must not depend on which of the two URLs Shopify used.
func TestTheAppLevelSourceStagesExactlyTheShopifyDeliveryMetadata(t *testing.T) {
	want := []string{
		"X-Shopify-Api-Version",
		"X-Shopify-Event-Id",
		"X-Shopify-Shop-Domain",
		"X-Shopify-Topic",
		"X-Shopify-Triggered-At",
		"X-Shopify-Webhook-Id",
	}
	sorted := func(in []string) []string {
		out := slices.Clone(in)
		slices.Sort(out)
		return out
	}

	app, ok := managedWebhookHarness(t).conn.InboundSource(context.Background(), ConnectorName)
	if !ok {
		t.Fatal("the app-level source did not resolve for a fully configured managed app")
	}
	if got := sorted(app.ForwardHeaders); !slices.Equal(got, want) {
		t.Errorf("the app-level source stages %q, want exactly %q", got, want)
	}
	for _, h := range app.ForwardHeaders {
		if strings.EqualFold(h, app.SignatureHeader) || strings.EqualFold(h, "Authorization") || strings.EqualFold(h, "Cookie") {
			t.Errorf("the app-level source would stage the credential header %q", h)
		}
	}

	perStore, ok := newHarness(t).conn.InboundSource(context.Background(), ConnectorName+"-"+testStoreID)
	if !ok || perStore.Secret == "" {
		t.Fatal("the per-store source did not resolve for the harness store")
	}
	if got := sorted(perStore.ForwardHeaders); !slices.Equal(got, want) {
		t.Errorf("the per-store source stages %q, want the app-level list %q", got, want)
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

// Whether a store was installed through the managed app is a fact about the
// STORE and the app's CLIENT ID, and it must not move with the state of the
// app's own secret (memql#5707 residual). Connect sealed a copy of that secret
// as the store's webhook secret, and the copy keeps verifying the per-store
// URL when the app's row is gone or cannot be unsealed -- so in every one of
// these states a privacy delivery on the per-store URL is still refused, and
// nothing is queued. Before this, an UNSEALABLE app secret surfaced as an
// error from managedApp, read as "not managed", and the per-store delivery
// was accepted.
func TestAManagedStoreIsKnownByItsClientIDWhateverStateTheAppSecretIsIn(t *testing.T) {
	const body = `{"shop_domain":"acme.myshopify.com","customer":{"id":991},"data_request":{"id":7}}`
	for _, tc := range []struct {
		name   string
		break_ func(h *testHarness)
	}{
		{"secret sealed and resolvable", func(*testHarness) {}},
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
			h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": "managed-app", "status": StatusLive}})
			tc.break_(h)
			store, ok := h.conn.stores.ByID(context.Background(), "acme")
			if !ok {
				t.Fatal("the store did not resolve")
			}
			managed, err := h.conn.storeSignsWithManagedSecret(context.Background(), store)
			if err != nil || !managed {
				t.Fatalf("storeSignsWithManagedSecret = %v, %v; want true, nil -- the client id alone identifies a managed install", managed, err)
			}
			req := memqlsync.InboundRequest{Source: "shopify-acme", Body: []byte(body), Headers: map[string]string{strings.ToLower(HeaderTopic): TopicDataRequest}}
			if _, err := h.conn.Apply(context.Background(), req); err == nil || len(h.engine.callsTo("queueComplianceJob")) != 0 {
				t.Fatalf("a managed store's per-store privacy delivery: err=%v, jobs=%d; want a refusal and no job", err, len(h.engine.callsTo("queueComplianceJob")))
			}
		})
	}
}

// The other side of the same predicate. With no managed client id on the
// cluster nothing identifies a store as one of its installs, and a store whose
// client id is another app's is a custom app's: both keep their per-store
// privacy URL. A client id that cannot be READ is neither answer, so it is an
// error -- the delivery fails with that reason rather than being queued or
// being refused as a misconfigured Partner dashboard.
func TestAStoreNotIdentifiedAsManagedKeepsItsPerStorePrivacyURL(t *testing.T) {
	const body = `{"shop_domain":"acme.myshopify.com","customer":{"id":991},"data_request":{"id":7}}`
	for _, tc := range []struct {
		name, client string
		setup        func(h *testHarness)
		wantManaged  bool
		wantErr      bool
	}{
		{"no managed client id on the cluster", "managed-app", func(h *testHarness) {
			h.engine.setRows(namedRowsQuery(conceptGlobalVariable, managedClientID), nil)
		}, false, false},
		{"another app's client id", "custom-app", func(*testHarness) {}, false, false},
		{"no client id on the store", "", func(*testHarness) {}, false, false},
		{"the client id cannot be read", "managed-app", func(h *testHarness) {
			h.engine.fail[callName(namedRowsQuery(conceptGlobalVariable, managedClientID))] = fmt.Errorf("connection reset")
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := managedWebhookHarness(t)
			h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": tc.client, "status": StatusLive}})
			store, ok := h.conn.stores.ByID(context.Background(), "acme")
			if !ok {
				t.Fatal("the store did not resolve")
			}
			tc.setup(h)
			managed, err := h.conn.storeSignsWithManagedSecret(context.Background(), store)
			if managed != tc.wantManaged || (err != nil) != tc.wantErr {
				t.Fatalf("storeSignsWithManagedSecret = %v, %v; want %v, err=%v", managed, err, tc.wantManaged, tc.wantErr)
			}
			req := memqlsync.InboundRequest{Source: "shopify-acme", Body: []byte(body), Headers: map[string]string{strings.ToLower(HeaderTopic): TopicDataRequest}}
			_, applyErr := h.conn.Apply(context.Background(), req)
			queued := len(h.engine.callsTo("queueComplianceJob"))
			if tc.wantErr {
				if applyErr == nil || queued != 0 || len(h.engine.callsTo("createAuditEvent")) != 0 {
					t.Fatalf("an unreadable client id: err=%v, jobs=%d, audits=%d; want an error, no job and no refusal audit", applyErr, queued, len(h.engine.callsTo("createAuditEvent")))
				}
				return
			}
			if applyErr != nil || queued != 1 {
				t.Fatalf("a store not identified as managed: err=%v, jobs=%d; want its per-store privacy delivery queued", applyErr, queued)
			}
		})
	}
}

// ClaimsInboundSource is the receiver's env-pin collision question
// (memqlsync.InboundSourceClaimer). It must give InboundSource's answer for
// every tenant name, resolve no secret doing it, and report a store list it
// could not read as an ERROR: "no such store" there would admit an env pin on
// a live store's name (memql#5707 follow-up).
func TestClaimsInboundSourceAgreesFailsLoudAndResolvesNoSecret(t *testing.T) {
	h := newHarness(t)
	var resolved []string
	inner := h.conn.stores.secrets
	h.conn.stores.secrets = func(ctx context.Context, name string) (string, error) {
		resolved = append(resolved, name)
		return inner(ctx, name)
	}
	ctx := context.Background()
	for _, name := range []string{ConnectorName + "-" + testStoreID, ConnectorName + "-nobody", "stripe", "shopifyx-" + testStoreID} {
		claimed, err := h.conn.ClaimsInboundSource(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resolved = nil
		_, want := h.conn.InboundSource(ctx, name)
		if claimed != want {
			t.Errorf("%s: ClaimsInboundSource = %v, InboundSource claims %v; the receiver and the dispatcher would disagree", name, claimed, want)
		}
	}
	if claimed, err := h.conn.ClaimsInboundSource(ctx, ConnectorName); err != nil || !claimed {
		t.Errorf("the connector's own name: claimed=%v err=%v; the dispatcher always routes it here", claimed, err)
	}

	resolved = nil
	if _, err := h.conn.ClaimsInboundSource(ctx, ConnectorName+"-"+testStoreID); err != nil || len(resolved) != 0 {
		t.Errorf("the claim resolved %v (err=%v); it needs only the store list", resolved, err)
	}

	h.engine.fail["stores"] = fmt.Errorf("context deadline exceeded")
	if claimed, err := h.conn.ClaimsInboundSource(ctx, ConnectorName+"-"+testStoreID); err == nil || claimed {
		t.Errorf("an unreadable store list answered claimed=%v err=%v; want an error, never \"not mine\"", claimed, err)
	}
}
