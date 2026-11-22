package memql

import (
	"context"
	"encoding/json"
	"errors"
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

	// refuseRepeats makes the token endpoint behave as Anthropic documents: an
	// identity token is accepted once and a second presentation is a 401.
	refuseRepeats bool
	seen          map[string]bool

	// messageStatusOnce answers the next Messages call with this status.
	messageStatusOnce int
}

func newFakeAnthropic(t *testing.T) *fakeAnthropic {
	t.Helper()
	isolateVendorSignIns(t)
	// A loop guard of this test's own, with the production limits. Every test
	// here sends the same small Messages request, and the process-wide guard
	// counts identical requests across tests: enough of them in one run trips
	// its loop breaker in whichever test happens to come last.
	prevGuard := sharedLLMGuard
	sharedLLMGuard = newLLMGuardFromEnv()
	t.Cleanup(func() { sharedLLMGuard = prevGuard })
	f := &fakeAnthropic{tokenStatus: http.StatusOK, seen: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.exchangeRequests = append(f.exchangeRequests, body)
		f.exchangeHeaders = append(f.exchangeHeaders, r.Header.Clone())
		status, custom := f.tokenStatus, f.tokenBody
		if assertion, _ := body["assertion"].(string); f.refuseRepeats && status == http.StatusOK {
			if f.seen[assertion] {
				status, custom = http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"jti_reused"}}`
			}
			f.seen[assertion] = true
		}
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
		once := f.messageStatusOnce
		f.messageStatusOnce = 0
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if once != 0 {
			w.WriteHeader(once)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid bearer"}}`))
			return
		}
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

// TestAnthropicClientsShareOneSignIn is the regression test for the failure the
// shared sign-in exists to remove (design 2026-11-21, A1): one identity token
// was presented once per provider record, and a vendor that accepts a token
// once refused all but the first.
func TestAnthropicClientsShareOneSignIn(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)

	// One pod, one token file, four records: two chat and two streaming, which
	// is the shape of the shipped Claude catalog.
	auth := federatedAuth(validIdentityToken(t))
	var clients []*anthropic.Client
	for _, name := range []string{"claudeA", "claudeB"} {
		p, err := newAnthropicProvider(ProviderConfig{Name: name, Model: "claude-test", Auth: auth})
		if err != nil {
			t.Fatalf("newAnthropicProvider(%s): %v", name, err)
		}
		clients = append(clients, &p.(*anthropicProvider).client)
	}
	for _, name := range []string{"streamA", "streamB"} {
		p, err := newAnthropicStreamProvider(ProviderConfig{Name: name, Model: "claude-test", Auth: auth})
		if err != nil {
			t.Fatalf("newAnthropicStreamProvider(%s): %v", name, err)
		}
		clients = append(clients, &p.(*anthropicStreamProvider).client)
	}

	for i, client := range clients {
		_, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
			Model:     anthropic.Model("claude-test"),
			MaxTokens: 16,
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		})
		if err != nil {
			t.Fatalf("client %d: Messages.New: %v", i, err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.exchangeRequests) != 1 {
		t.Fatalf("four clients made %d sign-ins with one identity token, want 1", len(f.exchangeRequests))
	}
	if len(f.messageHeaders) != 4 {
		t.Fatalf("message calls = %d, want 4", len(f.messageHeaders))
	}
	for i, h := range f.messageHeaders {
		if got := h.Get("Authorization"); got != "Bearer sk-ant-oat01-fake" {
			t.Fatalf("call %d: Authorization = %q, want the one federated bearer", i, got)
		}
		if got := h.Get("anthropic-beta"); !strings.Contains(got, "oauth-2025-04-20") {
			t.Fatalf("call %d: anthropic-beta = %q, want it to carry oauth-2025-04-20", i, got)
		}
		if got := h.Get("x-api-key"); got != "" {
			t.Fatalf("call %d carried x-api-key %q", i, got)
		}
	}
}

// A sign-in is made through a client, whose middleware names the address the
// vendor is reached at. Asked for directly, with no address, it is REFUSED
// rather than sent to the SDK's default: an identity token must never go to a
// host other than the one the client's requests go to.
func TestAnthropicSignInWithNoClientAddressIsRefused(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	signIn, err := anthropicSignInFor(ProviderConfig{
		Name: "claudeTest", Model: "claude-test", Auth: federatedAuth(validIdentityToken(t)),
	})
	if err != nil || signIn == nil {
		t.Fatalf("anthropicSignInFor: %v", err)
	}

	_, _, err = signIn.Bearer(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no vendor address") {
		t.Fatalf("a sign-in asked for with no client address: err = %v, want a refusal naming the missing address", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.exchangeRequests) != 0 {
		t.Fatalf("%d sign-ins reached the vendor with no client address", len(f.exchangeRequests))
	}
}

// A 401 on an API call means the bearer is dead. The engine drops it, and does
// not present the identity token it has already used a second time.
func TestAnthropicApi401InvalidatesWithoutASecondExchange(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	p := federatedProviderAgainst(t, f)

	if err := callMessages(context.Background(), p); err != nil {
		t.Fatalf("first call: %v", err)
	}
	f.mu.Lock()
	f.messageStatusOnce = http.StatusUnauthorized
	f.mu.Unlock()
	if err := callMessages(context.Background(), p); err == nil {
		t.Fatal("a 401 from the vendor reached the caller as success")
	}
	if err := callMessages(context.Background(), p); err == nil {
		t.Fatal("a call was made with a bearer the vendor had just refused")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.exchangeRequests) != 1 {
		t.Fatalf("the used identity token was presented %d times, want 1", len(f.exchangeRequests))
	}
	sent := 0
	for _, h := range f.messageHeaders {
		if h.Get("Authorization") != "" {
			sent++
		}
	}
	if sent != 2 {
		t.Fatalf("%d Messages calls reached the vendor, want 2: the third had no bearer to send", sent)
	}
}

func TestFederationExchangeHappyPath(t *testing.T) {
	f := newFakeAnthropic(t)
	p := federatedProviderAgainst(t, f)

	before := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeOK)
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

	if after := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeOK); after != before+1 {
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

	before := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeDenied)
	if err := callMessages(context.Background(), p); err == nil {
		t.Fatal("a denied exchange still produced a successful Messages call")
	}
	if after := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeDenied); after < before+1 {
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

	before := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeError)
	_ = callMessages(context.Background(), p)
	if after := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeError); after < before+1 {
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

// The check command (design 2026-11-21, A5).
//
// `memql provider-auth check` used to sign in and list models itself, as a
// second process in the engine's pod. A vendor that accepts an identity token
// once refuses whichever of the two goes second: the check either took the
// engine's sign-in for that rotation or reported a working federation as
// refused. It now reports the record the ENGINE wrote and contacts no vendor.

// anthropicFederationEnv configures federation through the SAME env names the
// ConfigMap sets in a pod, pointed at the fake.
func anthropicFederationEnv(t *testing.T, f *fakeAnthropic) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	t.Setenv(envAnthropicFederationRuleID, "fdrl_test")
	t.Setenv(envAnthropicOrganizationID, "11111111-2222-3333-4444-555555555555")
	t.Setenv(envAnthropicServiceAccountID, "svac_test")
	t.Setenv(envAnthropicWorkspaceID, "")
	t.Setenv(envAnthropicIdentityTokenFile, validIdentityToken(t))
}

// engineRecorded writes a sign-in record the way the engine in this pod would.
func engineRecorded(t *testing.T, rec vendorSignInRecord) {
	t.Helper()
	vendorSignInStatusFile.Store(true)
	if rec.Vendor == "" {
		rec.Vendor = metrics.FederationVendorAnthropic
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	if rec.Ids == nil {
		rec.Ids = map[string]string{
			authKeyFederationRuleID: "fdrl_engine",
			authKeyOrganizationID:   "99999999-2222-3333-4444-555555555555",
			authKeyServiceAccountID: "svac_engine",
		}
	}
	writeVendorSignInStatus(rec)
}

func (f *fakeAnthropic) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.exchangeRequests) + len(f.modelsHeaders) + len(f.messageHeaders)
}

func TestCheckProviderAuthReportsTheEnginesSignIn(t *testing.T) {
	f := newFakeAnthropic(t)
	anthropicFederationEnv(t, f)
	engineRecorded(t, vendorSignInRecord{
		Outcome:       "ok",
		ExpiresAt:     time.Now().Add(time.Hour),
		TokenSubject:  "system:serviceaccount:memql:memql-engine",
		TokenAudience: []string{anthropicAudience},
		ModelsListed:  7,
	})

	report, err := CheckProviderAuth(context.Background(), nil, "anthropic")
	if err != nil {
		t.Fatalf("CheckProviderAuth: %v", err)
	}
	if report.CredentialPath != string(credentialPathFederation) || !report.Recorded {
		t.Fatalf("credential path = %q recorded = %v, want federation and a record", report.CredentialPath, report.Recorded)
	}
	// The ids are the ENGINE's, from its record, not this process's environment:
	// ids applied through Settings win over the environment in the engine.
	if report.FederationRuleID != "fdrl_engine" || report.ServiceAccountID != "svac_engine" {
		t.Fatalf("ids reported = %q / %q, want the engine's fdrl_engine / svac_engine", report.FederationRuleID, report.ServiceAccountID)
	}
	if report.TokenSubject != "system:serviceaccount:memql:memql-engine" {
		t.Fatalf("token subject = %q", report.TokenSubject)
	}
	if report.ExchangeOutcome != metrics.FederationExchangeOK || report.ModelsListed != 7 {
		t.Fatalf("outcome = %q, models listed = %d; want ok and 7", report.ExchangeOutcome, report.ModelsListed)
	}
	if report.TokenExpiresIn <= 59*time.Minute || report.TokenExpiresIn > time.Hour {
		t.Fatalf("token expires in %v, want just under an hour", report.TokenExpiresIn)
	}
	if got := f.requestCount(); got != 0 {
		t.Fatalf("the check sent %d requests to the vendor; it must send none", got)
	}
}

func TestCheckProviderAuthReportsARefusal(t *testing.T) {
	f := newFakeAnthropic(t)
	anthropicFederationEnv(t, f)
	engineRecorded(t, vendorSignInRecord{Outcome: "denied", Status: 401, Detail: "match_subject_prefix"})

	report, err := CheckProviderAuth(context.Background(), nil, "anthropic")
	if err == nil {
		t.Fatal("a refused sign-in was reported as accepted")
	}
	if errors.Is(err, ErrSignInNotVerified) {
		t.Fatalf("a plain refusal was reported as not verified yet: %v", err)
	}
	if !strings.Contains(err.Error(), "match_subject_prefix") {
		t.Fatalf("the error drops the vendor's own reason: %v", err)
	}
	if report.ExchangeOutcome != "denied" {
		t.Fatalf("outcome = %q, want denied", report.ExchangeOutcome)
	}
}

// A refusal that may only be a restarted container re-presenting a used token,
// a wait for the next token, and a sign-in the vendor never answered are all
// "cannot say yet". None of them is the vendor refusing the configuration.
func TestCheckProviderAuthDoesNotCallAnOpenQuestionARefusal(t *testing.T) {
	due := time.Now().Add(6 * time.Minute).UTC().Truncate(time.Second)
	for name, rec := range map[string]vendorSignInRecord{
		"possible replay": {Outcome: "denied", Status: 401, Detail: "jti_reused", MayBeReplay: true, NextTokenDue: due},
		"waiting":         {Outcome: "waiting", NextTokenDue: due},
		"no answer":       {Outcome: "error", Detail: "dial tcp: connection refused"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAnthropic(t)
			anthropicFederationEnv(t, f)
			engineRecorded(t, rec)

			_, err := CheckProviderAuth(context.Background(), nil, "anthropic")
			if !errors.Is(err, ErrSignInNotVerified) {
				t.Fatalf("err = %v, want ErrSignInNotVerified", err)
			}
			if !rec.NextTokenDue.IsZero() && !strings.Contains(err.Error(), due.Format(time.RFC3339)) {
				t.Fatalf("the error does not say when the next identity token is due: %v", err)
			}
		})
	}
}

func TestCheckProviderAuthWaitsForARecord(t *testing.T) {
	f := newFakeAnthropic(t)
	anthropicFederationEnv(t, f)
	go func() {
		time.Sleep(300 * time.Millisecond)
		engineRecorded(t, vendorSignInRecord{Outcome: "ok", ExpiresAt: time.Now().Add(time.Hour), ModelsListed: 1})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := CheckProviderAuth(ctx, nil, "anthropic")
	if err != nil {
		t.Fatalf("CheckProviderAuth did not wait for the engine's record: %v", err)
	}
	if report.ExchangeOutcome != "ok" {
		t.Fatalf("outcome = %q, want ok", report.ExchangeOutcome)
	}
}

func TestCheckProviderAuthSaysWhenNothingIsRecorded(t *testing.T) {
	f := newFakeAnthropic(t)
	anthropicFederationEnv(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	report, err := CheckProviderAuth(ctx, nil, "anthropic")
	if !errors.Is(err, ErrSignInNotVerified) {
		t.Fatalf("err = %v, want ErrSignInNotVerified", err)
	}
	if report.Recorded {
		t.Fatal("the report claims a record that does not exist")
	}
	// Federation IS configured in this environment, and the report says so
	// with the ids it found there.
	if report.CredentialPath != string(credentialPathFederation) || report.FederationRuleID != "fdrl_test" {
		t.Fatalf("report = %+v, want the configured federation ids", report)
	}
	if got := f.requestCount(); got != 0 {
		t.Fatalf("with nothing recorded the check sent %d requests to the vendor; it must send none", got)
	}
}

// Ids applied through Settings live in concept storage, which this command
// cannot read. The engine's record is how it reports them anyway.
func TestCheckProviderAuthReportsIdsAppliedThroughSettings(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	for _, name := range []string{
		envAnthropicFederationRuleID, envAnthropicOrganizationID,
		envAnthropicServiceAccountID, envAnthropicWorkspaceID, envAnthropicIdentityTokenFile,
	} {
		t.Setenv(name, "")
	}
	engineRecorded(t, vendorSignInRecord{Outcome: "ok", ExpiresAt: time.Now().Add(time.Hour), ModelsListed: 3})

	report, err := CheckProviderAuth(context.Background(), nil, "anthropic")
	if err != nil {
		t.Fatalf("CheckProviderAuth: %v", err)
	}
	if report.CredentialPath != string(credentialPathFederation) || report.FederationRuleID != "fdrl_engine" || !report.Recorded {
		t.Fatalf("report = %+v, want the engine's federation record", report)
	}
}

// TestCheckProviderAuthReportsUnavailableWithNoFederation replaces
// TestCheckProviderAuthReportsTheKeyPath, which asserted the pre-cutover state
// the runbook's step 5 was told to stop seeing.
//
// It is gone rather than adapted because the state it described no longer
// exists: there is no manually entered vendor key anywhere in the product
// (epic memql#5088), so a cluster with no federation ids has no credential at
// all. The command must SAY that rather than reporting a path the engine
// cannot take -- an operator running the check on an unconfigured cluster is
// the most likely reader it has.
func TestCheckProviderAuthReportsUnavailableWithNoFederation(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
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
	if report.CredentialPath != string(credentialPathUnavailable) {
		t.Fatalf("credential path = %q, want %q", report.CredentialPath, credentialPathUnavailable)
	}
	if report.FederationRuleID != "" || report.TokenSubject != "" {
		t.Fatalf("an unconfigured cluster reported federation fields: %+v", report)
	}
}

// TestCheckProviderAuthCoversBothFederatingVendors replaces
// TestCheckProviderAuthRefusesANonAnthropicProvider.
//
// That test kept the command honest about a scope it no longer has: it refused
// an OpenAI provider because "OpenAI has no federation mechanism, and
// reporting on it here would imply one exists". OpenAI federates now (epic
// memql#5088), so the refusal would be the dishonest answer -- an operator
// asking whether OpenAI federation is working would be told the question makes
// no sense.
//
// What the command must still refuse is a type NEITHER vendor owns, and the
// second half asserts that. Without it, "covers both vendors" would be
// satisfied by a command that accepted everything.
func TestCheckProviderAuthCoversBothFederatingVendors(t *testing.T) {
	t.Run("an OpenAI provider is accepted", func(t *testing.T) {
		for _, name := range []string{
			envOpenAIIdentityProviderID, envOpenAIServiceAccountID, envOpenAIIdentityTokenFile,
		} {
			t.Setenv(name, "")
		}

		report, err := CheckProviderAuth(context.Background(), nil, "chat54Mini")
		if err != nil {
			t.Fatalf("CheckProviderAuth refused an OpenAI provider: %v", err)
		}
		if report.Vendor != metrics.FederationVendorOpenAI {
			t.Errorf("report vendor = %q, want %q", report.Vendor, metrics.FederationVendorOpenAI)
		}
		// With no ids configured the honest answer is "unavailable", not an
		// error: that is the state of every local cluster.
		if report.CredentialPath != string(credentialPathUnavailable) {
			t.Errorf("credential path = %q, want %q", report.CredentialPath, credentialPathUnavailable)
		}
	})

	t.Run("a vendor neither federates with is refused", func(t *testing.T) {
		_, err := CheckProviderAuth(context.Background(), nil, "no-such-provider-anywhere")
		if err == nil {
			t.Fatal("CheckProviderAuth accepted a provider that is not declared")
		}
	})
}

// TestFederationSurvivesAnAmbientAnthropicKey is the regression test for the
// one failure mode no configuration of ours controls.
//
// anthropic.NewClient prepends DefaultClientOptions, which walks the SDK's own
// credential chain. A bare ANTHROPIC_API_KEY in the process environment --
// an operator's shell, a base image, a Helm chart that sets the conventional
// name -- becomes a persistent X-Api-Key header on the request config, and the
// federation middleware then SKIPS ITSELF because the request already carries
// static auth (SDK internal/auth/middleware.go:34-36).
//
// The cluster would go on working, on a long-lived key, while every log line
// and `provider-auth check` said federation. Nothing else in this change would
// notice: the credential path is decided from our own auth map, which still
// says federate.
func TestFederationSurvivesAnAmbientAnthropicKey(t *testing.T) {
	for _, ambient := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(ambient, func(t *testing.T) {
			f := newFakeAnthropic(t)
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
			t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
			t.Setenv(ambient, "sk-ant-ambient-should-be-ignored")

			cfg := ProviderConfig{
				Name:  "claudeTest",
				Model: "claude-test",
				Auth:  federatedAuth(validIdentityToken(t)),
			}
			p, err := newAnthropicProvider(cfg)
			if err != nil {
				t.Fatalf("newAnthropicProvider: %v", err)
			}
			if err := callMessages(context.Background(), p.(*anthropicProvider)); err != nil {
				t.Fatalf("Messages.New: %v", err)
			}

			f.mu.Lock()
			defer f.mu.Unlock()

			// The exchange must actually have happened. Without the header
			// deletions the middleware short-circuits and this is zero.
			if len(f.exchangeRequests) != 1 {
				t.Fatalf("exchange count = %d, want 1 -- the ambient %s suppressed the federation middleware",
					len(f.exchangeRequests), ambient)
			}
			h := f.messageHeaders[0]
			if got := h.Get("x-api-key"); got != "" {
				t.Fatalf("Messages carried x-api-key %q from the ambient %s", got, ambient)
			}
			if got := h.Get("Authorization"); got != "Bearer sk-ant-oat01-fake" {
				t.Fatalf("Messages Authorization = %q, want the federated bearer", got)
			}
		})
	}
}
