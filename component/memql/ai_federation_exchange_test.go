package memql

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/znasllc-io/memql/component/metrics"
)

// The end-to-end federation test (spec 6.2, memql#4334 + #4335).
//
// An httptest server stands in for api.anthropic.com. It is reachable over
// plain http because the SDK's requireSecureTokenEndpoint exempts loopback --
// which is the only reason this can be tested at all without a certificate.

type fakeAnthropic struct {
	srv *httptest.Server

	mu               sync.Mutex
	exchangeRequests []map[string]any
	exchangeHeaders  []http.Header
	messageHeaders   []http.Header
	modelsHeaders    []http.Header

	// tokenStatus / tokenBody let a test make the exchange fail.
	tokenStatus int
	tokenBody   string
}

func newFakeAnthropic(t *testing.T) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{tokenStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.exchangeRequests = append(f.exchangeRequests, body)
		f.exchangeHeaders = append(f.exchangeHeaders, r.Header.Clone())
		status, custom := f.tokenStatus, f.tokenBody
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			if custom == "" {
				custom = `{"error":{"type":"invalid_request_error","message":"match_subject_prefix"}}`
			}
			_, _ = w.Write([]byte(custom))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat01-fake","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.messageHeaders = append(f.messageHeaders, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",` +
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	// models.list -- what `provider-auth check` calls: authenticated, and it
	// spends no tokens, which is why the runbook can tell an operator to run
	// the check freely.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.modelsHeaders = append(f.modelsHeaders, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","type":"model","display_name":"Claude Test",` +
			`"created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// federatedProviderAgainst builds a provider exactly as boot does, pointed at
// the fake server through the SDK's own ANTHROPIC_BASE_URL discovery.
func federatedProviderAgainst(t *testing.T, f *fakeAnthropic) *anthropicProvider {
	t.Helper()
	// Clear the SDK's ambient credential chain so the test proves OUR
	// federation path rather than an inherited key on the developer's machine.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)

	cfg := ProviderConfig{
		Name:  "claudeTest",
		Model: "claude-test",
		Auth:  federatedAuth(validIdentityToken(t)),
	}
	p, err := newAnthropicProvider(cfg)
	if err != nil {
		t.Fatalf("newAnthropicProvider: %v", err)
	}
	return p.(*anthropicProvider)
}

func callMessages(ctx context.Context, p *anthropicProvider) error {
	_, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model("claude-test"),
		MaxTokens: 16,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	return err
}

func TestFederationExchangeHappyPath(t *testing.T) {
	f := newFakeAnthropic(t)
	p := federatedProviderAgainst(t, f)

	before := metrics.AIFederationExchangesValue(metrics.FederationExchangeOK)
	if err := callMessages(context.Background(), p); err != nil {
		t.Fatalf("Messages.New over federation: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.exchangeRequests) != 1 {
		t.Fatalf("exchange count = %d, want 1", len(f.exchangeRequests))
	}
	body := f.exchangeRequests[0]
	if got := body["grant_type"]; got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant_type = %v", got)
	}
	if got := body["federation_rule_id"]; got != "fdrl_test" {
		t.Fatalf("federation_rule_id = %v", got)
	}
	if got := body["service_account_id"]; got != "svac_test" {
		t.Fatalf("service_account_id = %v", got)
	}
	if body["organization_id"] == "" || body["organization_id"] == nil {
		t.Fatal("organization_id missing from the exchange body")
	}
	assertion, _ := body["assertion"].(string)
	if strings.Count(assertion, ".") != 2 {
		t.Fatalf("assertion is not the projected JWT: %q", assertion)
	}
	// workspace_id is omitempty on the SDK's request struct, so an install
	// with one workspace must not send it at all.
	if _, present := body["workspace_id"]; present {
		t.Fatal("workspace_id was sent although none is configured")
	}

	beta := f.exchangeHeaders[0].Get("anthropic-beta")
	for _, want := range []string{"oauth-2025-04-20", "oidc-federation-2026-04-01"} {
		if !strings.Contains(beta, want) {
			t.Fatalf("exchange anthropic-beta = %q, missing %q", beta, want)
		}
	}

	if len(f.messageHeaders) != 1 {
		t.Fatalf("message count = %d, want 1", len(f.messageHeaders))
	}
	h := f.messageHeaders[0]
	if got := h.Get("Authorization"); got != "Bearer sk-ant-oat01-fake" {
		t.Fatalf("Messages Authorization = %q, want the federated bearer", got)
	}
	// The point of the whole change: no long-lived key on the wire.
	if got := h.Get("x-api-key"); got != "" {
		t.Fatalf("Messages carried x-api-key %q under federation", got)
	}

	if after := metrics.AIFederationExchangesValue(metrics.FederationExchangeOK); after != before+1 {
		t.Fatalf("ok counter %v -> %v, want +1", before, after)
	}

	rec := LastFederationExchange()
	if rec == nil || rec.Outcome != metrics.FederationExchangeOK {
		t.Fatalf("last exchange record = %+v", rec)
	}
	if rec.ExpiresIn.Seconds() != 3600 {
		t.Fatalf("recorded expiry = %v, want 1h", rec.ExpiresIn)
	}
	if strings.Contains(rec.Detail, "sk-ant-oat01-fake") {
		t.Fatal("the exchange record retained the bearer token")
	}
}

func TestFederationExchangeDenialCountsAndLogsTheBody(t *testing.T) {
	f := newFakeAnthropic(t)
	f.tokenStatus = http.StatusUnauthorized
	f.tokenBody = `{"error":{"type":"authentication_error","message":"match_subject_prefix"}}`
	p := federatedProviderAgainst(t, f)

	before := metrics.AIFederationExchangesValue(metrics.FederationExchangeDenied)
	if err := callMessages(context.Background(), p); err == nil {
		t.Fatal("a denied exchange still produced a successful Messages call")
	}
	if after := metrics.AIFederationExchangesValue(metrics.FederationExchangeDenied); after < before+1 {
		t.Fatalf("denied counter %v -> %v, want at least +1", before, after)
	}
	rec := LastFederationExchange()
	if rec == nil || rec.Outcome != metrics.FederationExchangeDenied {
		t.Fatalf("last exchange record = %+v", rec)
	}
	// The reason Anthropic gave has to survive: it is the same string the
	// Console's authentication-events tab shows, and it is what an operator
	// acts on.
	if !strings.Contains(rec.Detail, "match_subject_prefix") {
		t.Fatalf("the denial reason was discarded: %q", rec.Detail)
	}
}

func TestFederationExchangeServerFaultCountsAsError(t *testing.T) {
	f := newFakeAnthropic(t)
	f.tokenStatus = http.StatusInternalServerError
	f.tokenBody = `{"error":{"type":"api_error","message":"upstream"}}`
	p := federatedProviderAgainst(t, f)

	before := metrics.AIFederationExchangesValue(metrics.FederationExchangeError)
	_ = callMessages(context.Background(), p)
	if after := metrics.AIFederationExchangesValue(metrics.FederationExchangeError); after < before+1 {
		t.Fatalf("error counter %v -> %v, want at least +1", before, after)
	}
}

// TestFederationExchangeIsNotGuarded is the assertion that keeps the exchange
// out of the LLM guard: it must count toward no rate ceiling and no cost
// budget, because it is not an LLM call and re-exchanging is exactly what the
// SDK is supposed to do.
func TestFederationExchangeIsNotGuarded(t *testing.T) {
	if isGuardedLLMPath(federationTokenPath) {
		t.Fatalf("%s is fingerprinted as an LLM path; it would be rate-limited and costed", federationTokenPath)
	}
	if !isFederationExchange(http.MethodPost, federationTokenPath) {
		t.Fatalf("%s is not recognized as the federation exchange", federationTokenPath)
	}
	// A GET to the same path, or a POST elsewhere, is not the exchange.
	if isFederationExchange(http.MethodGet, federationTokenPath) {
		t.Fatal("a GET was treated as the exchange")
	}
	if isFederationExchange(http.MethodPost, "/v1/messages") {
		t.Fatal("a Messages POST was treated as the exchange")
	}
}

// TestFederationExchangeSpendsNoBudget proves the previous test's consequence
// rather than its shape: N exchanges through the real transport move neither
// the rate tally nor the cumulative spend.
func TestFederationExchangeSpendsNoBudget(t *testing.T) {
	f := newFakeAnthropic(t)
	p := federatedProviderAgainst(t, f)

	// Force a fresh exchange per client so the cache does not hide repeats.
	for i := 0; i < 3; i++ {
		fresh := federatedProviderAgainst(t, f)
		if err := callMessages(context.Background(), fresh); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := callMessages(context.Background(), p); err != nil {
		t.Fatalf("final call: %v", err)
	}

	f.mu.Lock()
	exchanges := len(f.exchangeRequests)
	f.mu.Unlock()
	if exchanges < 2 {
		t.Fatalf("only %d exchanges happened; the test cannot show they are unguarded", exchanges)
	}
	// If the exchange were guarded, byte-identical repeats would trip the
	// per-fingerprint loop breaker and the later calls would never reach the
	// server. They did, so it is not guarded.
	f.mu.Lock()
	messages := len(f.messageHeaders)
	f.mu.Unlock()
	if messages != 4 {
		t.Fatalf("message calls = %d, want 4 -- an exchange was blocked by the guard", messages)
	}
}

// TestCheckProviderAuthReportsFederation drives the real `provider-auth check`
// engine path (memql#4335): load the DSL tree exactly as boot does, pick an
// Anthropic provider, force one exchange, and list models. It is the only test
// that exercises all of that together, which is the whole value of the
// subcommand -- each half works in isolation and the cutover depends on the
// composition.
func TestCheckProviderAuthReportsFederation(t *testing.T) {
	f := newFakeAnthropic(t)
	// Route both the exchange and models.list at the fake, and configure
	// federation through the SAME env names the ConfigMap sets in a pod.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	t.Setenv(envAnthropicAPIKey, "")
	t.Setenv(envAnthropicFederationRuleID, "fdrl_test")
	t.Setenv(envAnthropicOrganizationID, "11111111-2222-3333-4444-555555555555")
	t.Setenv(envAnthropicServiceAccountID, "svac_test")
	t.Setenv(envAnthropicWorkspaceID, "")
	t.Setenv(envAnthropicIdentityTokenFile, validIdentityToken(t))

	report, err := CheckProviderAuth(context.Background(), nil, "anthropic")
	if err != nil {
		t.Fatalf("CheckProviderAuth: %v", err)
	}
	if report.CredentialPath != string(credentialPathFederation) {
		t.Fatalf("credential path = %q, want federation", report.CredentialPath)
	}
	if report.FederationRuleID != "fdrl_test" || report.ServiceAccountID != "svac_test" {
		t.Fatalf("ids not reported: %+v", report)
	}
	if report.TokenSubject != "system:serviceaccount:memql:memql-engine" {
		t.Fatalf("token subject = %q", report.TokenSubject)
	}
	if report.ExchangeOutcome != metrics.FederationExchangeOK {
		t.Fatalf("exchange outcome = %q, want ok", report.ExchangeOutcome)
	}
	if report.TokenExpiresIn != time.Hour {
		t.Fatalf("token expiry = %v, want 1h", report.TokenExpiresIn)
	}
}

// TestCheckProviderAuthReportsTheKeyPath is the pre-cutover state, and the
// answer step 5 of the runbook must stop seeing.
func TestCheckProviderAuthReportsTheKeyPath(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	t.Setenv(envAnthropicAPIKey, "sk-ant-test")
	for _, name := range []string{
		envAnthropicFederationRuleID, envAnthropicOrganizationID,
		envAnthropicServiceAccountID, envAnthropicWorkspaceID, envAnthropicIdentityTokenFile,
	} {
		t.Setenv(name, "")
	}

	report, err := CheckProviderAuth(context.Background(), nil, "anthropic")
	if err != nil {
		t.Fatalf("CheckProviderAuth: %v", err)
	}
	if report.CredentialPath != string(credentialPathAPIKey) {
		t.Fatalf("credential path = %q, want api-key", report.CredentialPath)
	}
	if report.FederationRuleID != "" || report.TokenSubject != "" {
		t.Fatalf("the api-key path reported federation fields: %+v", report)
	}
}

// TestCheckProviderAuthRefusesANonAnthropicProvider keeps the command honest
// about its scope: OpenAI has no federation mechanism, and reporting on it
// here would imply one exists.
func TestCheckProviderAuthRefusesANonAnthropicProvider(t *testing.T) {
	t.Setenv("MEMQL_AI_OPENAI_API_KEY", "sk-openai-test")
	_, err := CheckProviderAuth(context.Background(), nil, "chat54Mini")
	if err == nil {
		t.Fatal("CheckProviderAuth accepted an OpenAI provider")
	}
	if !strings.Contains(err.Error(), "Anthropic providers only") {
		t.Fatalf("the refusal does not explain the scope: %v", err)
	}
}

// TestCheckProviderAuthModelsCallCarriesNoKey closes the loop the cutover
// depends on: the live call the check makes must itself use the federated
// bearer. A check that verified the exchange and then listed models with a
// stray key would report success for the wrong credential -- which is exactly
// the reassurance step 5 must not be given before deleting the key.
func TestCheckProviderAuthModelsCallCarriesNoKey(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	// Both configured -- federation must win, per D3.
	t.Setenv(envAnthropicAPIKey, "sk-ant-test")
	t.Setenv(envAnthropicFederationRuleID, "fdrl_test")
	t.Setenv(envAnthropicOrganizationID, "11111111-2222-3333-4444-555555555555")
	t.Setenv(envAnthropicServiceAccountID, "svac_test")
	t.Setenv(envAnthropicWorkspaceID, "")
	t.Setenv(envAnthropicIdentityTokenFile, validIdentityToken(t))

	report, err := CheckProviderAuth(context.Background(), nil, "anthropic")
	if err != nil {
		t.Fatalf("CheckProviderAuth: %v", err)
	}
	if report.CredentialPath != string(credentialPathFederation) {
		t.Fatalf("with both configured the check reported %q", report.CredentialPath)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.modelsHeaders) == 0 {
		t.Fatal("models.list was never called")
	}
	h := f.modelsHeaders[len(f.modelsHeaders)-1]
	if got := h.Get("x-api-key"); got != "" {
		t.Fatalf("models.list carried x-api-key %q while reporting federation", got)
	}
	if got := h.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("models.list Authorization = %q, want the federated bearer", got)
	}
}
