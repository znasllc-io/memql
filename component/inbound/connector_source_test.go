package inbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// connector_source_test.go -- the connector tier of source resolution
// (memql#4391).
//
// The env tier is resolved once at boot and cannot express a multi-tenant
// connector: one Shopify connector serves many stores, each with its own
// webhook secret, and a store is added by an operator at runtime. So a source
// name no environment variable named can still be verified, and these tests
// pin the three properties that makes safe.

// fakeConnector is a sync.Connector that only answers InboundSource. The
// other six verbs are never called on the receiver's path, so embedding the
// interface keeps the fake to the one method under test.
type fakeConnector struct {
	memqlsync.Connector
	name    string
	sources map[string]memqlsync.InboundSource
	asked   []string
}

func (f *fakeConnector) Name() string { return f.name }

func (f *fakeConnector) InboundSource(_ context.Context, name string) (memqlsync.InboundSource, bool) {
	f.asked = append(f.asked, name)
	src, ok := f.sources[name]
	return src, ok
}

func withConnector(t *testing.T, c memqlsync.Connector) {
	t.Helper()
	// Declaration and binding are separate halves of one registration
	// (memql#4380): a name is declared from an init() so the engine's boot
	// check can resolve @origin before integrations exist, and the
	// implementation is bound once the runtime can build it. A test needs
	// both, because the receiver resolves through the BOUND set.
	memqlsync.Declare(c.Name())
	if err := memqlsync.Bind(c); err != nil {
		t.Fatalf("bind %s: %v", c.Name(), err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(c.Name()) })
}

func shopifySigned(t *testing.T, path, secret, body string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Shopify-Hmac-Sha256", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-Shopify-Webhook-Id", "wh-1")
	r.Header.Set("X-Shopify-Topic", "orders/updated")
	return r
}

func connectorHandler(t *testing.T, eng Engine) *Handler {
	t.Helper()
	h := NewHandler(Config{Enabled: true, MaxBodyBytes: 1024, Tolerance: 5 * time.Minute,
		Sources: map[string]SourceConfig{}}, eng, quietLogger())
	h.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	return h
}

func TestAConnectorSourceIsVerifiedAndStaged(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {
			Name: "shopify-acme", Scheme: SchemeHMACSHA256Base64,
			SignatureHeader: "X-Shopify-Hmac-Sha256", DedupeHeader: "X-Shopify-Webhook-Id",
			Secret: "acme-secret",
		},
	}})
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	connectorHandler(t, eng).ServeHTTP(rec, shopifySigned(t, "/inbound/shopify-acme", "acme-secret", `{"id":1}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if len(eng.calls) != 1 || !strings.Contains(eng.calls[0], "stageInboundRequest") {
		t.Fatalf("calls = %v", eng.calls)
	}
	if !strings.Contains(eng.calls[0], "signatureVerified: true") {
		t.Errorf("the delivery was staged unverified:\n%s", eng.calls[0])
	}
	// The sender's own idempotency key rides the row, which is what makes a
	// redelivery free.
	if !strings.Contains(eng.calls[0], "wh-1") {
		t.Errorf("the webhook id did not reach the staged row:\n%s", eng.calls[0])
	}
}

// Two stores, two secrets. One store's delivery must not verify against the
// other's key -- which is the whole reason the secret is per source rather
// than per connector.
func TestTwoConnectorSourcesVerifyIndependently(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {Scheme: SchemeHMACSHA256Base64, SignatureHeader: "X-Shopify-Hmac-Sha256", Secret: "acme-secret"},
		"shopify-beta": {Scheme: SchemeHMACSHA256Base64, SignatureHeader: "X-Shopify-Hmac-Sha256", Secret: "beta-secret"},
	}})
	eng := &fakeEngine{}
	h := connectorHandler(t, eng)

	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, shopifySigned(t, "/inbound/shopify-beta", "beta-secret", `{"id":2}`))
	if ok.Code != http.StatusAccepted {
		t.Fatalf("beta's own key was refused: %d", ok.Code)
	}

	crossed := httptest.NewRecorder()
	h.ServeHTTP(crossed, shopifySigned(t, "/inbound/shopify-beta", "acme-secret", `{"id":2}`))
	if crossed.Code != http.StatusUnauthorized {
		t.Fatalf("acme's key verified beta's delivery: %d", crossed.Code)
	}
}

// ENV WINS. An operator who pinned a source in the environment has made a
// statement about it, and a connector claiming the same name later must not
// silently take it over -- that would move which secret verifies a live
// sender with nothing in the environment changed to say so.
func TestAnEnvSourceIsNotTakenOverByAConnector(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"acme": {Scheme: SchemeHMACSHA256Hex, SignatureHeader: "X-Sig", Secret: "connector-secret"},
	}})
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	testHandler(t, eng, hexSource()).ServeHTTP(rec, signedRequest(t, `{"id":1}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the env-configured secret stopped verifying: %d", rec.Code)
	}
}

// An unresolvable credential reference -- a deleted secret row, a
// half-configured store -- must DROP the source rather than admit it
// unverified. That is the one outcome the whole deny-by-default design exists
// to prevent.
func TestAConnectorSourceWithNoSecretIsRefused(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {Scheme: SchemeHMACSHA256Base64, SignatureHeader: "X-Shopify-Hmac-Sha256", SecretRef: "GONE"},
	}})
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	connectorHandler(t, eng).ServeHTTP(rec, shopifySigned(t, "/inbound/shopify-acme", "anything", `{"id":1}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 -- an unresolvable secret must not become an unverified source", rec.Code)
	}
	if len(eng.calls) != 0 {
		t.Errorf("something was staged: %v", eng.calls)
	}
}

func TestASourceNoConnectorClaimsIs404(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{}})
	rec := httptest.NewRecorder()
	connectorHandler(t, &fakeEngine{}).ServeHTTP(rec, shopifySigned(t, "/inbound/shopify-nobody", "x", `{"id":1}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

// A bad signature on a compliance delivery answers 401, which is Shopify's
// stated requirement for customers/data_request, customers/redact and
// shop/redact. Nothing about the topic changes the answer -- the receiver
// verifies before it looks at one -- and this test exists so a future
// special case for compliance topics cannot be added without noticing.
func TestABadSignatureOnAComplianceTopicIs401(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {Scheme: SchemeHMACSHA256Base64, SignatureHeader: "X-Shopify-Hmac-Sha256", Secret: "acme-secret"},
	}})
	for _, topic := range []string{"customers/data_request", "customers/redact", "shop/redact"} {
		t.Run(topic, func(t *testing.T) {
			eng := &fakeEngine{}
			req := shopifySigned(t, "/inbound/shopify-acme", "the-wrong-secret", `{"shop_domain":"acme.myshopify.com"}`)
			req.Header.Set("X-Shopify-Topic", topic)
			rec := httptest.NewRecorder()
			connectorHandler(t, eng).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
			if len(eng.calls) != 0 {
				t.Errorf("a compliance delivery with a bad HMAC was staged: %v", eng.calls)
			}
		})
	}
}

// The receiver has a five-second budget and Shopify deletes a subscription
// after eight consecutive failures, so the request path must do nothing but
// verify, dedupe and stage. It has no way to fetch and must never grow one:
// the whole apply path -- the Admin round trip included -- happens in the
// automation the staged row triggers.
func TestTheRequestPathOnlyStages(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {Scheme: SchemeHMACSHA256Base64, SignatureHeader: "X-Shopify-Hmac-Sha256", Secret: "acme-secret"},
	}})
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	connectorHandler(t, eng).ServeHTTP(rec, shopifySigned(t, "/inbound/shopify-acme", "acme-secret", `{"id":1}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	// EXACTLY ONE engine call, and it is the stage. A second call of any
	// kind -- a lookup, a dispatch, an apply -- is work inside the budget.
	if len(eng.calls) != 1 {
		t.Fatalf("the request path made %d engine calls: %v", len(eng.calls), eng.calls)
	}
	if !strings.HasPrefix(strings.TrimSpace(eng.calls[0]), "mutation stageInboundRequest") {
		t.Errorf("the one call was not the stage:\n%s", eng.calls[0])
	}
}
