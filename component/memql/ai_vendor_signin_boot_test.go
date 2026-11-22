package memql

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
)

// Tests for the sign-in's life around a running engine (design record
// 2026-11-21-vendor-accounts-and-api-keys, A1, A2 and A5): it is made once at
// start, it survives a provider reload while the ids are unchanged, and a
// person pressing Verify or Apply may have a refused sign-in tried once more.

// anthropicFederatedEngine boots an engine on the REAL provider tree with the
// cluster's Anthropic account federated against the fake vendor.
func anthropicFederatedEngine(t *testing.T, f *fakeAnthropic, bus *events.Bus) (*MemQLEngine, *seededResolver, string) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)

	resolver := newSeededResolver()
	installSeededResolver(t, resolver)
	tokenFile := validIdentityToken(t)
	resolver.seed(envAnthropicFederationRuleID, "fdrl_test")
	resolver.seed(envAnthropicOrganizationID, "11111111-2222-3333-4444-555555555555")
	resolver.seed(envAnthropicServiceAccountID, "svac_test")
	resolver.seed(envAnthropicIdentityTokenFile, tokenFile)

	e := engineWithProviders(t, bus)
	if _, err := LoadUnifiedProviders(nil, e.providers); err != nil {
		t.Fatalf("load providers: %v", err)
	}
	return e, resolver, tokenFile
}

func newTestBus(t *testing.T) *events.Bus {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(func() { bus.Close() })
	return bus
}

// callClaude makes one Messages call through a registered provider's client.
// No SDK retries: a refused sign-in must reach the test as one failure, not as
// three attempts with a backoff between them.
func callClaude(t *testing.T, e *MemQLEngine, provider string) error {
	t.Helper()
	entry, ok := e.providers.Entry(provider)
	if !ok || entry == nil || entry.Client == nil {
		t.Fatalf("provider %q is not registered with a client", provider)
	}
	client, err := anthropicClientOf(entry.Client)
	if err != nil {
		t.Fatalf("provider %q: %v", provider, err)
	}
	_, err = client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model("claude-test"),
		MaxTokens: 16,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	}, option.WithMaxRetries(0))
	return err
}

func (f *fakeAnthropic) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.exchangeRequests)
}

func (f *fakeAnthropic) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messageHeaders)
}

func (f *fakeAnthropic) setTokenStatus(status int) {
	f.mu.Lock()
	f.tokenStatus = status
	f.mu.Unlock()
}

func TestReloadKeepsTheSignInWhileTheIdsAreUnchanged(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	e, _, _ := anthropicFederatedEngine(t, f, newTestBus(t))

	if err := callClaude(t, e, "claudeSonnet"); err != nil {
		t.Fatalf("before the reload: %v", err)
	}
	if _, err := e.ReloadAIProviders(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	e.vendorSignInWG.Wait()
	// A different record, built by the reload: it must carry the same bearer.
	if err := callClaude(t, e, "claudeOpus"); err != nil {
		t.Fatalf("after the reload: %v -- the reload threw the sign-in away, and the identity token "+
			"it would sign in with again has already been used", err)
	}
	if got := f.exchangeCount(); got != 1 {
		t.Fatalf("a reload with unchanged ids made %d sign-ins in all, want 1", got)
	}
}

func TestReloadWithNewIdsDropsTheOldBearer(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	e, resolver, tokenFile := anthropicFederatedEngine(t, f, newTestBus(t))

	if err := callClaude(t, e, "claudeSonnet"); err != nil {
		t.Fatalf("before the reload: %v", err)
	}

	resolver.seed(envAnthropicFederationRuleID, "fdrl_fixed")
	if _, err := e.ReloadAIProviders(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	e.vendorSignInWG.Wait()

	// The identity token on disk was accepted under the old ids. Nothing may be
	// presented, and the bearer minted for the old ids must not be sent.
	if err := callClaude(t, e, "claudeSonnet"); err == nil {
		t.Fatal("a call was served after the ids changed, with no new identity token to sign in with")
	}
	if got := f.messageCount(); got != 1 {
		t.Fatalf("%d Messages calls reached the vendor, want 1: the old bearer was sent under the new ids", got)
	}
	if got := f.exchangeCount(); got != 1 {
		t.Fatalf("the used identity token was presented again under the new ids: %d sign-ins", got)
	}

	rotateIdentityToken(t, tokenFile, anthropicAudience)
	if err := callClaude(t, e, "claudeSonnet"); err != nil {
		t.Fatalf("with a new identity token: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.exchangeRequests) != 2 {
		t.Fatalf("sign-ins = %d, want 2", len(f.exchangeRequests))
	}
	if got := f.exchangeRequests[1]["federation_rule_id"]; got != "fdrl_fixed" {
		t.Fatalf("the second sign-in named rule %v, want the new id fdrl_fixed", got)
	}
}

func TestSignInAtStartRecordsTheModelsListed(t *testing.T) {
	f := newFakeAnthropic(t)
	e, _, _ := anthropicFederatedEngine(t, f, newTestBus(t))
	vendorSignInStatusFile.Store(true)

	e.signInToConfiguredVendors(context.Background())

	signIn, ok := currentVendorSignIn(metrics.FederationVendorAnthropic)
	if !ok {
		t.Fatal("no Anthropic sign-in exists after sign-in at start")
	}
	rec, ok := signIn.Record()
	if !ok || rec.Outcome != "ok" || rec.ModelsListed != 1 {
		t.Fatalf("record = %+v (ok=%v), want outcome ok with the 1 model the vendor listed", rec, ok)
	}
	status, ok, err := readVendorSignInStatus(metrics.FederationVendorAnthropic)
	if err != nil || !ok || status.Outcome != "ok" || status.ModelsListed != 1 {
		t.Fatalf("status file = %+v (ok=%v err=%v), want the same record", status, ok, err)
	}
	if got := f.exchangeCount(); got != 1 {
		t.Fatalf("sign-in at start made %d sign-ins, want 1", got)
	}
	// The model list is asked for with the federated bearer and no key. A
	// list made with a stray key would record success for the wrong credential.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.modelsHeaders) != 1 {
		t.Fatalf("models were listed %d times, want 1", len(f.modelsHeaders))
	}
	if got := f.modelsHeaders[0].Get("Authorization"); got != "Bearer sk-ant-oat01-fake" {
		t.Fatalf("the model list carried Authorization %q, want the federated bearer", got)
	}
	if got := f.modelsHeaders[0].Get("x-api-key"); got != "" {
		t.Fatalf("the model list carried x-api-key %q", got)
	}
}

func TestSignInAtStartDoesNothingWithNoVendorConfigured(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	installSeededResolver(t, newSeededResolver())
	e := engineWithProviders(t, newTestBus(t))
	if _, err := LoadUnifiedProviders(nil, e.providers); err != nil {
		t.Fatalf("load providers: %v", err)
	}
	vendorSignInStatusFile.Store(true)

	e.signInToConfiguredVendors(context.Background())

	f.mu.Lock()
	requests := len(f.exchangeRequests) + len(f.modelsHeaders) + len(f.messageHeaders)
	f.mu.Unlock()
	if requests != 0 {
		t.Fatalf("with no vendor set up, sign-in at start sent %d requests", requests)
	}
	if _, ok, _ := readVendorSignInStatus(metrics.FederationVendorAnthropic); ok {
		t.Fatal("a status file was written for a vendor that is not set up")
	}
}

// The identity node runs under a service account the vendor's rule does not
// admit, and never calls a model. Signing in from it at start would put a
// refusal in the vendor's console and in the denial counter on every boot, for
// a node that had no use for the bearer.
func TestSignInAtStartIsSkippedOnANodeThatCallsNoModels(t *testing.T) {
	f := newFakeAnthropic(t)
	e, _, _ := anthropicFederatedEngine(t, f, newTestBus(t))
	e.DisableVendorSignInAtStart()

	e.SignInToConfiguredVendors(context.Background())
	e.vendorSignInWG.Wait()

	if got := f.requestCount(); got != 0 {
		t.Fatalf("a node that calls no models sent %d requests to the vendor at start", got)
	}
}

func TestVerifyAllowsOneMoreTryAfterARefusal(t *testing.T) {
	f := newFakeAnthropic(t)
	f.setTokenStatus(http.StatusUnauthorized)
	e, _, _ := anthropicFederatedEngine(t, f, newTestBus(t))

	if err := callClaude(t, e, "claudeSonnet"); err == nil {
		t.Fatal("a refused sign-in served a call")
	}
	if err := callClaude(t, e, "claudeSonnet"); err == nil {
		t.Fatal("a refused sign-in served a second call")
	}
	if got := f.exchangeCount(); got != 1 {
		t.Fatalf("a refused identity token was presented %d times before anyone asked, want 1", got)
	}

	// The operator fixes the rule at the vendor and presses Verify.
	f.setTokenStatus(http.StatusOK)
	rows, err := e.evaluateProviderVerifyExpression(clusterOwnerCtx("user-owner"), map[string]any{"provider": "claudeSonnet"})
	if err != nil {
		t.Fatalf("providerVerify: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("providerVerify returned %d rows, want 1", len(rows))
	}
	var result struct {
		Verified bool   `json:"verified"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(rows[0].Payload, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Verified {
		t.Fatalf("Verify after the fix: not verified (%s) -- the refused token was not tried again", result.Reason)
	}
	if got := f.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2: one refused, one more on Verify", got)
	}
}

// The hop: an Apply on one node reaches a peer through the providers.reload
// broadcast, and the PEER, whose sign-in was refused, tries once more.
func TestAReloadBroadcastAllowsOneMoreTryOnThePeer(t *testing.T) {
	f := newFakeAnthropic(t)
	f.setTokenStatus(http.StatusUnauthorized)

	busA, busB := newTestBus(t), newTestBus(t)
	busA.Subscribe(providersReloadPattern, func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	peer, _, _ := anthropicFederatedEngine(t, f, busB)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	peer.StartProvidersReloadSubscriber(ctx)

	if err := callClaude(t, peer, "claudeSonnet"); err == nil {
		t.Fatal("a refused sign-in served a call on the peer")
	}

	// The operator fixes the vendor's side and presses Apply on node A. Node A
	// here only broadcasts: whatever happens next happened on the peer.
	f.setTokenStatus(http.StatusOK)
	nodeA := engineWithProviders(t, busA)
	nodeA.PublishProvidersReload("apply-after-the-fix")

	signIn, ok := currentVendorSignIn(metrics.FederationVendorAnthropic)
	if !ok {
		t.Fatal("the peer has no Anthropic sign-in")
	}
	if !eventually(5*time.Second, func() bool {
		rec, ok := signIn.Record()
		return ok && rec.Outcome == "ok"
	}) {
		rec, _ := signIn.Record()
		t.Fatalf("CROSS-NODE FAILURE: the peer's sign-in is still %q after the broadcast -- an Apply on one "+
			"node does not let the others try their refused sign-in again", rec.Outcome)
	}
	if got := f.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2: one refused, one more after the broadcast", got)
	}
}
