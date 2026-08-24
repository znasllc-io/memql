package shopify

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// store_test.go -- stores, credentials and the inbound source (#4391).

func TestTwoStoresVerifyWithTheirOwnSecrets(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{
		{"id": "acme", "domain": "acme.myshopify.com", "adminTokenRef": "ACME_ADMIN", "webhookSecretRef": "ACME_WEBHOOK", "status": StatusLive},
		{"id": "beta", "domain": "beta.myshopify.com", "adminTokenRef": "BETA_ADMIN", "webhookSecretRef": "BETA_WEBHOOK", "status": StatusLive},
	})
	h.conn.stores = NewStoreRegistry(h.engine, func(_ context.Context, name string) (string, error) {
		return strings.ToLower(name) + "-value", nil
	})

	ctx := context.Background()
	acme, ok := h.conn.InboundSource(ctx, "shopify-acme")
	if !ok {
		t.Fatal("shopify-acme did not resolve")
	}
	beta, ok := h.conn.InboundSource(ctx, "shopify-beta")
	if !ok {
		t.Fatal("shopify-beta did not resolve")
	}
	if acme.Secret == beta.Secret {
		t.Fatal("two stores resolved the same webhook secret -- one store's delivery would verify against the other's key")
	}
	if acme.Secret != "acme_webhook-value" {
		t.Errorf("acme secret = %q", acme.Secret)
	}
	// The SCHEME is spelled in the receiver's encoding vocabulary, not as a
	// vendor. component/inbound deliberately carries no vendor table.
	if acme.Scheme != "hmac-sha256-base64" || acme.SignatureHeader != HeaderHMAC || acme.DedupeHeader != HeaderWebhookID {
		t.Errorf("source policy = %+v", acme)
	}
}

func TestAnUnknownSourceDoesNotResolve(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, ok := h.conn.InboundSource(ctx, "shopify-nobody"); ok {
		t.Error("a store that does not exist resolved a source")
	}
	if _, ok := h.conn.InboundSource(ctx, "stripe"); ok {
		t.Error("this connector claimed another connector's source")
	}
}

// The source NAME is authoritative and the shop-domain header is the
// fallback, in that order: the source decided which secret verified the
// signature, so trusting a header over it would let a delivery signed for one
// store be attributed to another.
func TestTheSourceNameWinsOverTheShopDomainHeader(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{
		{"id": "acme", "domain": "acme.myshopify.com", "adminTokenRef": "A", "status": StatusLive},
		{"id": "beta", "domain": "beta.myshopify.com", "adminTokenRef": "B", "status": StatusLive},
	})
	h.conn.stores.Invalidate()

	req := memqlsync.InboundRequest{
		Source:  "shopify-acme",
		Headers: map[string]string{"x-shopify-shop-domain": "beta.myshopify.com"},
	}
	store, ok := h.conn.StoreFor(context.Background(), req)
	if !ok || store.ID != "acme" {
		t.Fatalf("resolved %q, want acme -- the header must not override the verified source", store.ID)
	}
}

func TestTheShopDomainHeaderResolvesWhenTheSourceCannot(t *testing.T) {
	h := newHarness(t)
	req := memqlsync.InboundRequest{
		Source:  "shopify-gone",
		Headers: map[string]string{"x-shopify-shop-domain": "acme-widgets.myshopify.com"},
	}
	store, ok := h.conn.StoreFor(context.Background(), req)
	if !ok || store.ID != testStoreID {
		t.Fatalf("resolved %q/%v", store.ID, ok)
	}
}

func TestStoreIdIsDerivedFromTheDomain(t *testing.T) {
	for domain, want := range map[string]string{
		"acme-widgets.myshopify.com": "acme-widgets",
		"acme.myshopify.com":         "acme",
		"ACME-Widgets.myshopify.com": "acme-widgets",
	} {
		got, err := StoreIDForDomain(domain)
		if err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", domain, got, err, want)
		}
	}
	// It becomes a URL path segment, so a domain that cannot make one is a
	// refusal rather than a mangled id.
	if _, err := StoreIDForDomain("Bad Domain!.myshopify.com"); err == nil {
		t.Error("a domain that derives no legal source segment must be refused")
	}
}

// The env path is a SEED. A cluster that already has a store is already
// configured, and a boot-time write reconciling the row against the
// environment would undo an operator's portal edit on every restart --
// silently, and only on the nodes carrying the variables.
func TestTheEnvSeedNeverOverwritesAnExistingStore(t *testing.T) {
	h := newHarness(t)
	id, err := SeedStoreFromEnv(context.Background(), h.engine, h.conn.stores, SeedConfig{
		StoreDomain: "other.myshopify.com", AdminToken: "shpat_x", APIVersion: "2026-07",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("it seeded %q over an existing store", id)
	}
	if got := h.engine.callsTo("createStore"); len(got) != 0 {
		t.Errorf("createStore ran: %v", got)
	}
}

func TestTheEnvSeedCreatesTheFirstStoreAndSealsItsTokens(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", nil)
	h.conn.stores.Invalidate()

	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	id, err := SeedStoreFromEnv(context.Background(), h.engine, h.conn.stores, SeedConfig{
		StoreDomain: "first.myshopify.com", AdminToken: "shpat_secret", WebhookSecret: "hook",
		APIVersion: "2026-07", ProtectedLevel: ProtectedLevel1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "first" {
		t.Fatalf("seeded %q", id)
	}
	create := h.engine.callsTo("createStore")
	if len(create) != 1 {
		t.Fatalf("createStore ran %d times", len(create))
	}
	// The store row holds REFERENCES. A token on it would be a token the
	// portal returns to a browser.
	if strings.Contains(create[0], "shpat_secret") || strings.Contains(create[0], `"hook"`) {
		t.Fatalf("a credential reached the store row:\n%s", create[0])
	}
	if !strings.Contains(create[0], "SHOPIFY_FIRST_ADMIN_TOKEN") {
		t.Errorf("the store row does not reference the sealed token:\n%s", create[0])
	}
	secrets := h.engine.callsTo("setGlobalSecret")
	if len(secrets) != 2 {
		t.Fatalf("sealed %d secrets, want the admin token and the webhook secret", len(secrets))
	}
	for _, call := range secrets {
		if strings.Contains(call, "shpat_secret") {
			t.Errorf("a secret was written in cleartext:\n%s", call)
		}
	}
}

func TestAStoreOnTheWrongApiVersionIsRefused(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{{
		"id": testStoreID, "domain": "acme.myshopify.com", "adminTokenRef": "ACME_ADMIN",
		"apiVersion": "2024-01", "status": StatusLive,
	}})
	h.conn.stores.Invalidate()

	_, err := h.conn.adminCall(context.Background(), h.store(t), "query X { shop { id } }", "X", nil)
	if err == nil || !strings.Contains(err.Error(), "2024-01") {
		t.Fatalf("err = %v, want a refusal naming the pinned version", err)
	}
	if len(h.admin.seen()) != 0 {
		t.Error("the call went out anyway; a version mismatch returns fields the concepts do not declare")
	}
}

func TestAPausedStoreStillIngestsNothingButIsStillConfigured(t *testing.T) {
	live := Store{Status: StatusLive}
	paused := Store{Status: StatusPaused}
	if !live.Ingests() || paused.Ingests() {
		t.Fatal("Ingests() does not follow the status")
	}
}

func TestProtectedLevelOrdering(t *testing.T) {
	l1 := Store{ProtectedDataLevel: ProtectedLevel1}
	if l1.ProtectedLevelAtLeast(ProtectedLevel2) {
		t.Error("level1 must not satisfy level2 -- shopifyqlQuery needs 2")
	}
	if !l1.ProtectedLevelAtLeast(ProtectedLevel1) {
		t.Error("level1 must satisfy level1")
	}
}

func TestTheDeliveryUrlComposesThroughTheFrontDoorHelper(t *testing.T) {
	h := newHarness(t)
	h.conn.deliver = nil
	t.Setenv("MEMQL_DOMAIN", "lab.example.com")
	got := h.conn.deliveryURL(Store{ID: "acme"})
	want := "https://api.lab.example.com/inbound/shopify-acme"
	if got != want {
		t.Errorf("got %q, want %q -- a second spelling of the api host registers subscriptions pointing at a hostname nothing is served at", got, want)
	}
}

func TestSourceNameHasOneSpelling(t *testing.T) {
	// The registrar tells Shopify where to deliver and the receiver resolves
	// what arrived. Two copies of this rule would disagree, and the
	// disagreement is a webhook that 404s with every manifest looking right.
	if got := memqlsync.SourceName(ConnectorName, "acme"); got != "shopify-acme" {
		t.Errorf("got %q", got)
	}
}

func TestStoreRegistryCachesAndInvalidates(t *testing.T) {
	h := newHarness(t)
	h.conn.stores.ttl = time.Minute
	ctx := context.Background()
	if _, err := h.conn.stores.Stores(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(h.engine.callsTo("stores"))
	if _, err := h.conn.stores.Stores(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.engine.callsTo("stores")); got != before {
		t.Errorf("the cache did not hold: %d -> %d reads", before, got)
	}
	h.conn.stores.Invalidate()
	if _, err := h.conn.stores.Stores(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.engine.callsTo("stores")); got == before {
		t.Error("Invalidate did not force a re-read, so an operator's edit would wait out the TTL")
	}
}
