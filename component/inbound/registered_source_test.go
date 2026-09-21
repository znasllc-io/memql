package inbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// registered_source_test.go -- the tier between the environment and the
// connectors: a source this cluster REGISTERED FOR ITSELF at runtime (design
// record 2026-09-20-github-app-setup, D5). The case it exists for is the
// cluster's GitHub App, whose webhook secret GitHub generates while a cluster
// owner registers the app from the product -- so no environment could have
// carried it, and no connector owns it.
//
// The seam is deny-by-default, so what is pinned is what the new tier must NOT
// do: take a name over from the environment, or admit a delivery it cannot
// verify.

const registeredSecret = "registered-app-webhook-secret"

// githubPolicy is the shape app/ wires for the GitHub App's webhook.
func githubPolicy(secret string) RegisteredSource {
	return func(_ context.Context, name string) (SourceConfig, bool) {
		if name != "github" {
			return SourceConfig{}, false
		}
		return SourceConfig{
			Secret: secret, Scheme: SchemeHMACSHA256Hex,
			SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256=",
			DedupeHeader: "X-GitHub-Delivery",
		}, true
	}
}

func githubSigned(t *testing.T, secret, body string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/inbound/github", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	return r
}

func TestARegisteredSourceIsVerifiedAndStaged(t *testing.T) {
	eng := &fakeEngine{}
	h := connectorHandler(t, eng)
	h.SetRegisteredSource(githubPolicy(registeredSecret))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, githubSigned(t, registeredSecret, `{"ref":"refs/heads/main"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if len(eng.calls) != 1 || !strings.Contains(eng.calls[0], "signatureVerified: true") {
		t.Fatalf("calls = %v", eng.calls)
	}
	if !strings.Contains(eng.calls[0], `source: "github"`) {
		t.Errorf("the staged row does not name the source it came in under:\n%s", eng.calls[0])
	}
	if !strings.Contains(eng.calls[0], "delivery-1") {
		t.Errorf("GitHub's delivery id did not reach the staged row, so a redelivery is not free:\n%s", eng.calls[0])
	}
}

func TestARegisteredSourceRefusesADeliverySignedWithAnotherKey(t *testing.T) {
	eng := &fakeEngine{}
	h := connectorHandler(t, eng)
	h.SetRegisteredSource(githubPolicy(registeredSecret))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, githubSigned(t, "somebody-elses-secret", `{"ref":"refs/heads/main"}`))
	if rec.Code != http.StatusUnauthorized || len(eng.calls) != 0 {
		t.Errorf("status %d, %d row(s) staged", rec.Code, len(eng.calls))
	}
}

// ENV WINS, for the reason resolveSource gives: an operator who pinned `github`
// in the environment has made a statement about it, and a row written from a
// browser must not move which secret verifies a live sender.
func TestTheEnvironmentsSourceIsNotTakenOverByARegisteredOne(t *testing.T) {
	eng := &fakeEngine{}
	h := NewHandler(Config{Enabled: true, MaxBodyBytes: 1024, Sources: map[string]SourceConfig{
		"github": {Name: "github", Secret: "the-operators-secret", Scheme: SchemeHMACSHA256Hex,
			SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256="},
	}}, eng, quietLogger())
	h.SetRegisteredSource(githubPolicy(registeredSecret))

	signedForTheRow := httptest.NewRecorder()
	h.ServeHTTP(signedForTheRow, githubSigned(t, registeredSecret, `{}`))
	if signedForTheRow.Code != http.StatusUnauthorized {
		t.Errorf("a delivery signed with the REGISTERED secret verified against an environment-pinned source: %d", signedForTheRow.Code)
	}
	signedForTheEnvironment := httptest.NewRecorder()
	h.ServeHTTP(signedForTheEnvironment, githubSigned(t, "the-operators-secret", `{}`))
	if signedForTheEnvironment.Code != http.StatusAccepted {
		t.Errorf("the environment's own secret stopped verifying: %d", signedForTheEnvironment.Code)
	}
}

// A registration that was REMOVED answers a blank secret. That must be a 404,
// never an unverified source -- the one outcome deny-by-default exists to
// prevent -- and the rule is applied in the handler rather than trusted to
// whoever wired the resolver.
func TestARegisteredSourceWithNoSecretIsRefused(t *testing.T) {
	eng := &fakeEngine{}
	h := connectorHandler(t, eng)
	h.SetRegisteredSource(githubPolicy(""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, githubSigned(t, "", `{}`))
	if rec.Code != http.StatusNotFound || len(eng.calls) != 0 {
		t.Errorf("status %d, %d row(s) staged", rec.Code, len(eng.calls))
	}
}

func TestANameTheRegisteredTierDoesNotAnswerFallsThrough(t *testing.T) {
	// ...to the connectors, exactly as before the tier existed.
	withConnector(t, &fakeConnector{name: "shopify", sources: map[string]memqlsync.InboundSource{
		"shopify-acme": {Name: "shopify-acme", Scheme: SchemeHMACSHA256Base64,
			SignatureHeader: "X-Shopify-Hmac-Sha256", DedupeHeader: "X-Shopify-Webhook-Id", Secret: "acme-secret"},
	}})
	eng := &fakeEngine{}
	h := connectorHandler(t, eng)
	h.SetRegisteredSource(githubPolicy(registeredSecret))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, shopifySigned(t, "/inbound/shopify-acme", "acme-secret", `{"id":1}`))
	if rec.Code != http.StatusAccepted {
		t.Errorf("a connector source stopped resolving once a registered tier was wired: %d", rec.Code)
	}
	// ...and to a 404 for a name nobody claims.
	unknown := httptest.NewRecorder()
	h.ServeHTTP(unknown, httptest.NewRequest(http.MethodPost, "/inbound/nobody", strings.NewReader(`{}`)))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("an unclaimed name answered %d", unknown.Code)
	}
}

func TestNoRegisteredTierIsExactlyTheReceiverAsItWas(t *testing.T) {
	eng := &fakeEngine{}
	h := connectorHandler(t, eng)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, githubSigned(t, registeredSecret, `{}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("with nothing wired, /inbound/github answered %d", rec.Code)
	}
	h.SetRegisteredSource(nil) // and nil removes it without a panic
	var none *Handler
	none.SetRegisteredSource(githubPolicy("x"))
}
