package memql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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
	// AND IT SAYS SO. Only the engine can tell whether a vendor's ids exist
	// (they may be in the cluster's storage, which the check command cannot
	// read), so it records "not set up" for the check to report. Without the
	// record the check could not tell this from an engine that has not
	// started signing in yet.
	for _, vendor := range []string{metrics.FederationVendorAnthropic, metrics.FederationVendorOpenAI} {
		rec, ok, err := readVendorSignInStatus(vendor)
		if err != nil || !ok || rec.Outcome != signInOutcomeNotConfigured {
			t.Fatalf("%s: record = %+v (found %v, err %v), want notConfigured", vendor, rec, ok, err)
		}
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
	vendorSignInStatusFile.Store(true)

	e.SignInToConfiguredVendors(context.Background())
	e.vendorSignInWG.Wait()

	if got := f.requestCount(); got != 0 {
		t.Fatalf("a node that calls no models sent %d requests to the vendor at start", got)
	}
	// AND IT SAYS SO, for both vendors. With nothing recorded the check command
	// would wait out its whole timeout on this node and then report a sign-in
	// that is merely late.
	for _, vendor := range signInVendors {
		rec, ok, err := readVendorSignInStatus(vendor)
		if err != nil || !ok || rec.Outcome != signInOutcomeNotUsed {
			t.Fatalf("%s: record = %+v (found %v, err %v), want notUsed", vendor, rec, ok, err)
		}
	}
}

// Ids that are partly set are refused by the engine itself, before any vendor
// is asked. Only the engine can say so: the missing value may be in the
// cluster's storage, which the check command cannot read. Recorded as "no
// federation ids are set", the check told an operator halfway through the
// runbook that nobody had set the vendor up.
func TestSignInAtStartRecordsAVendorTheEngineRefuses(t *testing.T) {
	f := newFakeAnthropic(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	for _, name := range []string{envAnthropicOrganizationID, envAnthropicServiceAccountID, envAnthropicWorkspaceID} {
		t.Setenv(name, "")
	}
	resolver := newSeededResolver()
	installSeededResolver(t, resolver)
	resolver.seed(envAnthropicFederationRuleID, "fdrl_test")
	resolver.seed(envAnthropicIdentityTokenFile, validIdentityToken(t))

	e := engineWithProviders(t, newTestBus(t))
	if _, err := LoadUnifiedProviders(nil, e.providers); err != nil {
		t.Fatalf("load providers: %v", err)
	}
	vendorSignInStatusFile.Store(true)

	e.signInToConfiguredVendors(context.Background())

	rec, ok, err := readVendorSignInStatus(metrics.FederationVendorAnthropic)
	if err != nil || !ok || rec.Outcome != signInOutcomeMisconfigured {
		t.Fatalf("record = %+v (found %v, err %v), want %q", rec, ok, err, signInOutcomeMisconfigured)
	}
	for _, missing := range []string{envAnthropicOrganizationID, envAnthropicServiceAccountID} {
		if !strings.Contains(rec.Detail, missing) {
			t.Fatalf("the record does not name the missing %s: %q", missing, rec.Detail)
		}
	}
	if got := f.requestCount(); got != 0 {
		t.Fatalf("a vendor the engine refuses was sent %d requests", got)
	}
	// The vendor nobody touched is still just not set up.
	other, ok, err := readVendorSignInStatus(metrics.FederationVendorOpenAI)
	if err != nil || !ok || other.Outcome != signInOutcomeNotConfigured {
		t.Fatalf("openai: record = %+v (found %v, err %v), want notConfigured", other, ok, err)
	}
}

// An engine with no provider registry at all (a tool, a partly built test
// engine) has no vendor set up. It says so; it does not fall over.
func TestSignInAtStartWithNoRegistryIsNotSetUp(t *testing.T) {
	isolateVendorSignIns(t)
	vendorSignInStatusFile.Store(true)

	(&MemQLEngine{}).signInToConfiguredVendors(context.Background())

	for _, vendor := range signInVendors {
		rec, ok, err := readVendorSignInStatus(vendor)
		if err != nil || !ok || rec.Outcome != signInOutcomeNotConfigured {
			t.Fatalf("%s: record = %+v (found %v, err %v), want notConfigured", vendor, rec, ok, err)
		}
	}
}

// --- the next identity token ------------------------------------------------

// projectedTokenAt replaces the identity token at path the way kubelet writes a
// projected one, issued at iat and expiring at exp, and returns it.
func projectedTokenAt(t *testing.T, path, jti string, iat, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"aud": []any{anthropicAudience},
		"sub": "system:serviceaccount:memql:memql-engine",
		"jti": jti,
		"iat": iat.Unix(),
		"exp": exp.Unix(),
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return token
}

// stubFollowUpWait replaces the engine's wait for the next identity token for
// one test, so a test does not sit out the minutes a real token takes.
func stubFollowUpWait(t *testing.T, fn func(ctx context.Context, d time.Duration) bool) {
	t.Helper()
	prev := vendorSignInFollowUpWait
	vendorSignInFollowUpWait = fn
	t.Cleanup(func() { vendorSignInFollowUpWait = prev })
}

// tokenUsedByTheLastProcess puts on disk a token issued before this process
// started and already accepted by the vendor: what a container that restarted
// inside its pod finds. The next one is due in about five minutes.
func tokenUsedByTheLastProcess(t *testing.T, f *fakeAnthropic, tokenFile string) (due time.Time) {
	t.Helper()
	issued := vendorSignInProcessStarted.Add(-time.Minute)
	due = time.Now().Add(5 * time.Minute)
	// kubelet replaces a token at 80 percent of its life.
	expires := issued.Add(due.Sub(issued) * 10 / 8)
	used := projectedTokenAt(t, tokenFile, "used-by-the-last-process", issued, expires)
	f.mu.Lock()
	f.seen[used] = true
	f.mu.Unlock()
	return due
}

func nearly(got, want time.Duration) bool {
	return got > want-5*time.Second && got < want+5*time.Second
}

// A refusal that may only be a used token, and a wait for the next token, are
// both settled by the next token and by nothing else. The engine presents that
// token when it arrives, with nobody calling a model: a node with no traffic
// used to stay "not verified" for as long as it stayed idle, and a rule that
// really was wrong was never reported as refused.
func TestTheEngineSignsInAgainWhenTheNextTokenArrives(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	e, _, tokenFile := anthropicFederatedEngine(t, f, newTestBus(t))
	vendorSignInStatusFile.Store(true)
	due := tokenUsedByTheLastProcess(t, f, tokenFile)

	var waits []time.Duration
	stubFollowUpWait(t, func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		now := time.Now() // kubelet replaces the token while the engine waits
		projectedTokenAt(t, tokenFile, "the-next-token", now, now.Add(10*time.Minute))
		return true
	})

	e.SignInToConfiguredVendors(context.Background())
	e.vendorSignInWG.Wait()

	signIn, ok := currentVendorSignIn(metrics.FederationVendorAnthropic)
	if !ok {
		t.Fatal("no Anthropic sign-in")
	}
	rec, _ := signIn.Record()
	if rec.Outcome != signInOutcomeOK || rec.Proof != signInProofOK {
		t.Fatalf("record = %+v, want an accepted and proven sign-in: the engine did not present the next token", rec)
	}
	if got := f.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2: the used token refused, the next one accepted", got)
	}
	if len(waits) != 1 {
		t.Fatalf("the engine waited %d times, want once: %v", len(waits), waits)
	}
	if want := time.Until(due) + vendorSignInFollowUpGrace; !nearly(waits[0], want) {
		t.Fatalf("the engine waited %v, want about %v: until the next token is due and a little more", waits[0], want)
	}
}

// kubelet replaces the file on its own schedule, which can be a minute or two
// after the token says. With the old token still on disk there is nothing to
// present, so the engine looks again shortly rather than giving up.
func TestTheFollowUpLooksAgainWhenTheTokenIsLate(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	e, _, tokenFile := anthropicFederatedEngine(t, f, newTestBus(t))
	// A used token whose replacement is already overdue: kubelet is late.
	issued := vendorSignInProcessStarted.Add(-20 * time.Minute)
	used := projectedTokenAt(t, tokenFile, "used-by-the-last-process", issued, issued.Add(10*time.Minute))
	f.mu.Lock()
	f.seen[used] = true
	f.mu.Unlock()

	var waits []time.Duration
	stubFollowUpWait(t, func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 2 {
			now := time.Now()
			projectedTokenAt(t, tokenFile, "the-next-token", now, now.Add(10*time.Minute))
		}
		return true
	})

	e.SignInToConfiguredVendors(context.Background())
	e.vendorSignInWG.Wait()

	signIn, _ := currentVendorSignIn(metrics.FederationVendorAnthropic)
	if rec, _ := signIn.Record(); rec.Outcome != signInOutcomeOK {
		t.Fatalf("record = %+v, want an accepted sign-in once the late token arrived", rec)
	}
	if len(waits) != 2 || waits[0] != vendorSignInFollowUpGrace || waits[1] != vendorSignInFollowUpGrace {
		t.Fatalf("waits = %v, want two short looks: the token was overdue, and still the old one at the first", waits)
	}
	if got := f.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2: nothing is presented while the old token is still on disk", got)
	}
}

// An operator corrects an id and presses Apply. The token on disk was accepted
// under the old ids and cannot be presented again, so the engine has to wait
// for the next one. It then signs in by itself.
func TestAfterAnApplyTheEngineSignsInWhenTheNextTokenArrives(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	e, resolver, tokenFile := anthropicFederatedEngine(t, f, newTestBus(t))
	issued := time.Now()
	projectedTokenAt(t, tokenFile, "the-first-token", issued, issued.Add(10*time.Minute))

	var waits []time.Duration
	stubFollowUpWait(t, func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		now := time.Now()
		projectedTokenAt(t, tokenFile, "the-next-token", now, now.Add(10*time.Minute))
		return true
	})

	e.SignInToConfiguredVendors(context.Background())
	e.vendorSignInWG.Wait()
	if len(waits) != 0 {
		t.Fatalf("an accepted sign-in waited for another token: %v", waits)
	}

	resolver.seed(envAnthropicFederationRuleID, "fdrl_fixed")
	if _, err := e.ReloadAIProviders(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	e.vendorSignInWG.Wait()

	signIn, _ := currentVendorSignIn(metrics.FederationVendorAnthropic)
	rec, _ := signIn.Record()
	if rec.Outcome != signInOutcomeOK || rec.Proof != signInProofOK {
		t.Fatalf("record = %+v, want an accepted and proven sign-in under the new ids", rec)
	}
	if len(waits) != 1 {
		t.Fatalf("the engine waited %d times after the Apply, want once: %v", len(waits), waits)
	}
	if want := time.Until(issued.Add(8*time.Minute)) + vendorSignInFollowUpGrace; !nearly(waits[0], want) {
		t.Fatalf("the engine waited %v, want about %v", waits[0], want)
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

// A reload starts a pass of its own, with a wait of its own. The wait an
// earlier pass left behind must not also sign in when it ends.
func TestAReloadSupersedesTheWaitForTheNextToken(t *testing.T) {
	f := newFakeAnthropic(t)
	f.refuseRepeats = true
	e, _, tokenFile := anthropicFederatedEngine(t, f, newTestBus(t))
	tokenUsedByTheLastProcess(t, f, tokenFile)

	release := make(chan struct{})
	var waiting atomic.Int32
	stubFollowUpWait(t, func(context.Context, time.Duration) bool {
		waiting.Add(1)
		<-release
		return true
	})

	e.SignInToConfiguredVendors(context.Background())
	waitUntilSignIn(t, "the first pass to wait for the next token", func() bool { return waiting.Load() == 1 })
	e.SignInToConfiguredVendors(context.Background()) // what a reload does
	waitUntilSignIn(t, "the second pass to wait for the next token", func() bool { return waiting.Load() == 2 })

	now := time.Now()
	projectedTokenAt(t, tokenFile, "the-next-token", now, now.Add(10*time.Minute))
	close(release)
	e.vendorSignInWG.Wait()

	if got := f.exchangeCount(); got != 2 {
		t.Fatalf("sign-ins = %d, want 2", got)
	}
	f.mu.Lock()
	lists := len(f.modelsHeaders)
	f.mu.Unlock()
	if lists != 1 {
		t.Fatalf("the vendor's models were listed %d times once the token arrived, want 1: the wait a "+
			"reload had superseded signed in as well", lists)
	}
}

// Nothing waits for a token when the sign-in gave an answer: accepted, or
// refused for a reason the next token would not change.
func TestNoWaitForTheNextTokenWhenTheSignInGaveAnAnswer(t *testing.T) {
	for name, status := range map[string]int{"accepted": http.StatusOK, "refused": http.StatusUnauthorized} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAnthropic(t)
			f.setTokenStatus(status)
			e, _, tokenFile := anthropicFederatedEngine(t, f, newTestBus(t))
			issued := time.Now() // a token issued to THIS process: a refusal of it is an answer
			projectedTokenAt(t, tokenFile, "issued-to-this-process", issued, issued.Add(10*time.Minute))

			waited := false
			stubFollowUpWait(t, func(context.Context, time.Duration) bool {
				waited = true
				return true
			})
			e.SignInToConfiguredVendors(context.Background())
			e.vendorSignInWG.Wait()
			if waited {
				t.Fatal("the engine waited for another identity token after the vendor gave its answer")
			}
		})
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
