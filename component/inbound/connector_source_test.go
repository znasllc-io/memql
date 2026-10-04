package inbound

import (
	"context"
	"errors"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// An env pin on a name NO bound connector claims is untouched by the
// connector being there: the environment still verifies it with its own
// secret. The collision rule below is about a name the dispatcher would hand
// to a connector, and it must not reach any further than that.
func TestAnEnvSourceBesideABoundConnectorStillVerifies(t *testing.T) {
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {Scheme: SchemeHMACSHA256Hex, SignatureHeader: "X-Sig", Secret: "connector-secret"},
	}})
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	testHandler(t, eng, hexSource()).ServeHTTP(rec, signedRequest(t, `{"id":1}`))
	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("an env source no connector claims stopped verifying: %d, %d staged", rec.Code, len(eng.calls))
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

// An env pin on a name a bound connector CLAIMS is a contradiction, not an
// override, and it is refused with 404 whichever secret signed the delivery.
//
// The dispatcher routes a staged row by its source name alone
// (memqlsync.ConnectorForSource), and the connector reads a row under a name
// it claims as "verified by MY secret for this tenant": its own name as
// signed by the app secret, with the tenant bound off the signed body; a
// tenant's `shopify-<storeId>` as signed by that store's webhook secret, with
// the tenant bound off the NAME. A body the env secret verified would be
// handed to it on that premise -- for a custom-app store, a privacy purge
// queued from a body the operator's env secret signed. So the receiver asks
// the dispatcher's own predicate and refuses the collision (memql#5707
// follow-up).
//
// Refused rather than handed to the connector, for the reason env-wins was
// written: which secret verifies a live sender must not move silently. Here
// it moves in neither direction -- the env secret and the connector's are
// both refused, and an ERROR names the env variable to rename or remove.
// A half-configured app (client id, no sealed secret) claims nothing, and its
// bare name is still refused, because a bound connector's own name always
// routes to it.
func TestAnEnvPinOnAConnectorClaimedNameIsRefused(t *testing.T) {
	const connectorSecret = "connector-secret"
	for _, tc := range []struct {
		name, source string
		sources      map[string]memqlsync.InboundSource
	}{
		{"the connector claims its own name", "shopify", map[string]memqlsync.InboundSource{
			"shopify": {Scheme: SchemeHMACSHA256Hex, SignatureHeader: "X-Sig", Secret: connectorSecret},
		}},
		{"the connector is half-configured and claims nothing", "shopify", map[string]memqlsync.InboundSource{}},
		{"the connector claims a tenant's name", "shopify-acme", map[string]memqlsync.InboundSource{
			"shopify-acme": {Scheme: SchemeHMACSHA256Hex, SignatureHeader: "X-Sig", Secret: connectorSecret},
		}},
		{"the connector claims a tenant whose secret does not resolve", "shopify-acme", map[string]memqlsync.InboundSource{
			"shopify-acme": {Scheme: SchemeHMACSHA256Hex, SignatureHeader: "X-Sig", SecretRef: "GONE"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConnector(t, &fakeConnector{name: "shopify", sources: tc.sources})
			for _, signer := range []struct{ who, secret string }{{"env", testSecret}, {"connector", connectorSecret}} {
				src := hexSource()
				src.Name = tc.source
				eng := &fakeEngine{}
				h := NewHandler(Config{Enabled: true, MaxBodyBytes: 1024, Tolerance: 5 * time.Minute,
					Sources: map[string]SourceConfig{tc.source: src}}, eng, quietLogger())
				body := `{"shop_domain":"acme.myshopify.com"}`
				r := httptest.NewRequest(http.MethodPost, "/inbound/"+tc.source, strings.NewReader(body))
				r.Header.Set("X-Sig", hex.EncodeToString(sign(signer.secret, []byte(body))))
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if rec.Code != http.StatusNotFound || len(eng.calls) != 0 {
					t.Errorf("signed by the %s secret, an env pin on %q was answered %d with %d staged; "+
						"want 404 and nothing staged", signer.who, tc.source, rec.Code, len(eng.calls))
				}
			}
		})
	}
}

// claimingConnector answers the receiver's collision question through the
// claim-only interface, and records whether the secret-resolving one was
// asked at all.
type claimingConnector struct {
	memqlsync.Connector
	name     string
	claims   map[string]bool
	claimErr error
	resolved []string
}

func (c *claimingConnector) Name() string { return c.name }

func (c *claimingConnector) ClaimsInboundSource(_ context.Context, name string) (bool, error) {
	if c.claimErr != nil {
		return false, c.claimErr
	}
	return c.claims[name], nil
}

func (c *claimingConnector) InboundSource(_ context.Context, name string) (memqlsync.InboundSource, bool) {
	c.resolved = append(c.resolved, name)
	return memqlsync.InboundSource{}, false
}

// The collision check FAILS CLOSED. A connector that cannot read its own
// tenants -- a stale cache and a failed store read -- has not said "not mine",
// and reading it that way admits the env pin on a tenant's name, which the
// dispatcher then hands to the connector once the read recovers. So an
// undeterminable claim is refused like a claimed one: 404, nothing staged.
//
// And it asks the CLAIM, never the secret. InboundSource resolves and unseals
// the tenant's webhook secret; the collision check only needs to know the name
// is taken, on a path any unauthenticated caller can hit.
func TestTheEnvPinCollisionCheckFailsClosedAndResolvesNoSecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *claimingConnector
	}{
		{"the connector claims the tenant", &claimingConnector{name: "shopify", claims: map[string]bool{"shopify-acme": true}}},
		{"the connector cannot read its tenants", &claimingConnector{name: "shopify", claimErr: errors.New("list stores: context deadline exceeded")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConnector(t, tc.c)
			src := hexSource()
			src.Name = "shopify-acme"
			eng := &fakeEngine{}
			h := NewHandler(Config{Enabled: true, MaxBodyBytes: 1024, Tolerance: 5 * time.Minute,
				Sources: map[string]SourceConfig{"shopify-acme": src}}, eng, quietLogger())
			body := `{"shop_domain":"acme.myshopify.com"}`
			r := httptest.NewRequest(http.MethodPost, "/inbound/shopify-acme", strings.NewReader(body))
			r.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(body))))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusNotFound || len(eng.calls) != 0 {
				t.Errorf("an env pin the connector claims or cannot rule out was answered %d with %d staged; want 404, nothing staged",
					rec.Code, len(eng.calls))
			}
			if len(tc.c.resolved) != 0 {
				t.Errorf("the collision check resolved the tenant's secret for %v; it needs only the claim", tc.c.resolved)
			}
		})
	}
}

// An env source the connector positively does NOT claim still verifies:
// failing closed is for "cannot tell", not for every name in its namespace.
func TestAnEnvPinTheClaimerDisownsStillVerifies(t *testing.T) {
	withConnector(t, &claimingConnector{name: "shopify", claims: map[string]bool{}})
	src := hexSource()
	src.Name = "shopify-custom"
	eng := &fakeEngine{}
	h := NewHandler(Config{Enabled: true, MaxBodyBytes: 1024, Tolerance: 5 * time.Minute,
		Sources: map[string]SourceConfig{"shopify-custom": src}}, eng, quietLogger())
	body := `{"id":1}`
	r := httptest.NewRequest(http.MethodPost, "/inbound/shopify-custom", strings.NewReader(body))
	r.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(body))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("an env pin no connector claims was answered %d with %d staged; want 202", rec.Code, len(eng.calls))
	}
}
