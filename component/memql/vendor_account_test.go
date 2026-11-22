package memql

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// The account tests (design record 2026-11-21-vendor-accounts-and-api-keys,
// K3, K8, K9, K10, K11, K17, S2, S4, S5). Each talks to a stand-in vendor: no
// test here reaches a real one, and no value here is a real key.

const standInKey = "sk-ant-stand-in-0000-not-a-real-key"

func hello() []common.ChatMessage {
	return []common.ChatMessage{{Role: "user", Content: "hello"}}
}

// textProvider builds one of the four text providers against a stand-in.
func textProvider(t *testing.T, v *accountVendor, streaming bool) AIProvider {
	t.Helper()
	provider, err := newAIProvider(v.config("record", streaming))
	if err != nil {
		t.Fatalf("build the %s provider: %v", v.vendor, err)
	}
	return provider
}

func boundTo(t *testing.T, provider AIProvider, account VendorAccount, keys VendorKeys) common.ChatAIProvider {
	t.Helper()
	client, ok := provider.(VendorAccountClient)
	if !ok {
		t.Fatalf("%T cannot be paid by an account", provider)
	}
	bound, ok := client.WithVendorAccount(account, keys).(common.ChatAIProvider)
	if !ok {
		t.Fatalf("the bound provider is not a chat provider")
	}
	return bound
}

func TestAKeyBoundCallCarriesTheKeyAndNothingElse(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		for _, streaming := range []bool{false, true} {
			name := vendor + map[bool]string{false: "", true: " streaming record"}[streaming]
			t.Run(name, func(t *testing.T) {
				v := newAccountVendor(t, vendor)
				keys := newFakeVendorKeys()
				keys.secrets["k1"] = standInKey
				bound := boundTo(t, textProvider(t, v, streaming), keyAccount(vendor, "k1", "ana"), keys)

				if _, err := bound.CallChat(context.Background(), hello()); err != nil {
					t.Fatalf("a call paid by a key failed: %v", err)
				}
				requests := v.seen()
				if len(requests) != 1 {
					t.Fatalf("the vendor saw %d requests, want 1", len(requests))
				}
				header := requests[0].Header
				switch vendor {
				case VendorAnthropic:
					if got := header.Get("X-Api-Key"); got != standInKey {
						t.Errorf("X-Api-Key = %q, want the key", got)
					}
					if got := header.Get("Authorization"); got != "" {
						t.Errorf("a key-paid Anthropic call also carried Authorization %q", got)
					}
					if strings.Contains(header.Get("anthropic-beta"), "oauth") {
						t.Errorf("a key-paid call carried the OAuth beta value: %q", header.Get("anthropic-beta"))
					}
				case VendorOpenAI:
					if got := header.Get("Authorization"); got != "Bearer "+standInKey {
						t.Errorf("Authorization = %q, want the key as a bearer", got)
					}
					if header.Get("OpenAI-Project") != "" || header.Get("OpenAI-Organization") != "" {
						t.Errorf("a key-paid call named the cluster's project or organization: %v", header)
					}
				}
				if asks := keys.asks(); len(asks) != 1 || asks[0] != "k1" {
					t.Errorf("the key was unsealed %v, want once for k1", asks)
				}
			})
		}
	}
}

// A key is sent to the vendor's own address and nowhere else (K3). Here the
// client is pointed at the stand-in while the vendor's own address is left as
// it is in production, which is exactly a cluster that overrides the address.
func TestAKeyIsNeverSentToAnotherAddress(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		t.Run(vendor, func(t *testing.T) {
			v := newAccountVendor(t, vendor)
			vendorKeyOrigins[vendor] = map[string]string{
				VendorAnthropic: "https://api.anthropic.com",
				VendorOpenAI:    "https://api.openai.com",
			}[vendor]
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			bound := boundTo(t, textProvider(t, v, false), keyAccount(vendor, "k1", "ana"), keys)

			_, err := bound.CallChat(context.Background(), hello())
			var refusal *VendorAccountError
			if !errors.As(err, &refusal) || refusal.Kind != accountFailureUnavailable {
				t.Fatalf("err = %v, want an account refusal of kind %q", err, accountFailureUnavailable)
			}
			if got := len(v.seen()); got != 0 {
				t.Errorf("the other address received %d requests, want none", got)
			}
			if asks := keys.asks(); len(asks) != 0 {
				t.Errorf("the key was unsealed %d times before the address was checked", len(asks))
			}
			if kind, ok := VendorAccountFailure(err); !ok || kind != accountFailureUnavailable {
				t.Errorf("VendorAccountFailure = (%q, %v): the router must move to the next account", kind, ok)
			}
		})
	}
}

// An unbound call is the cluster's account, exactly as part 1 left it: the
// process's one sign-in, and no key anywhere near it.
func TestAnUnboundCallUsesTheClustersAccount(t *testing.T) {
	t.Run(VendorOpenAI, func(t *testing.T) {
		f := newExchangeServer(t)
		cfg := openaiFederatedConfig(t, f)
		provider, err := newOpenAIProvider(cfg)
		if err != nil {
			t.Fatalf("newOpenAIProvider: %v", err)
		}
		reg := newProviderRegistry()
		keys := newFakeVendorKeys()
		reg.SetVendorKeys(keys)
		entry := &ProviderConfigEntry{Config: cfg, Client: provider, Available: true}
		client, ok := reg.BindVendorAccount(entry, VendorAccount{Kind: VendorAccountCluster, Vendor: VendorOpenAI})
		if !ok {
			t.Fatal("the cluster's account could not be bound on a cluster that has one")
		}
		if _, err := client.(common.ChatAIProvider).CallChat(context.Background(), hello()); err != nil {
			t.Fatalf("a call on the cluster's account failed: %v", err)
		}
		sent := f.apiAuthorizations()
		if len(sent) != 1 || sent[0] != "Bearer sk-federated-1" {
			t.Fatalf("the call carried %v, want the sign-in's bearer", sent)
		}
		if asks := keys.asks(); len(asks) != 0 {
			t.Fatalf("a call on the cluster's account unsealed a key: %v", asks)
		}
	})
	t.Run(VendorAnthropic, func(t *testing.T) {
		f := newFakeAnthropic(t)
		provider := federatedProviderAgainst(t, f)
		reg := newProviderRegistry()
		keys := newFakeVendorKeys()
		reg.SetVendorKeys(keys)
		entry := &ProviderConfigEntry{Config: ProviderConfig{Name: "claudeTest", Type: "Anthropic", Model: "claude-test"}, Client: provider, Available: true}
		client, ok := reg.BindVendorAccount(entry, VendorAccount{Kind: VendorAccountCluster, Vendor: VendorAnthropic})
		if !ok {
			t.Fatal("the cluster's account could not be bound on a cluster that has one")
		}
		if _, err := client.(common.ChatAIProvider).CallChat(context.Background(), hello()); err != nil {
			t.Fatalf("a call on the cluster's account failed: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.messageHeaders) != 1 || f.messageHeaders[0].Get("Authorization") != "Bearer sk-ant-oat01-fake" || f.messageHeaders[0].Get("x-api-key") != "" {
			t.Fatalf("the call carried %v, want the sign-in's bearer and no key", f.messageHeaders)
		}
		if asks := keys.asks(); len(asks) != 0 {
			t.Fatalf("a call on the cluster's account unsealed a key: %v", asks)
		}
	})
}

// The address a record's client dials is known when the record is built, with
// no request sent: adding a key and choosing accounts both ask it before any
// call exists.
func TestAVendorClientKnowsItsAddressBeforeItSendsAnything(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		for _, streaming := range []bool{false, true} {
			v := newAccountVendor(t, vendor)
			provider := textProvider(t, v, streaming)
			client, ok := provider.(VendorAccountClient)
			if !ok {
				t.Fatalf("%T cannot be paid by an account", provider)
			}
			if got := client.VendorOrigin(); got != v.srv.URL {
				t.Errorf("%s (streaming %v): VendorOrigin = %q, want %q", vendor, streaming, got, v.srv.URL)
			}
			if got := len(v.seen()); got != 0 {
				t.Errorf("%s: learning the address sent %d requests", vendor, got)
			}
		}
	}
}

// With no federation the record still builds: it is unavailable to the
// cluster, and a key can pay for a call through it.
func TestNoFederationStillServesAKey(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		t.Run(vendor, func(t *testing.T) {
			v := newAccountVendor(t, vendor)
			reg := newProviderRegistry()
			registerParsedProviders(nil, reg, []parsedProviderConfig{{cfg: ptrTo(v.config("record", false)), origin: "test:record"}})

			entry, ok := reg.Entry("record")
			if !ok {
				t.Fatal("the record did not register")
			}
			if entry.Available {
				t.Fatal("a record with no cluster account registered as available: every reader of Available would hand it to the cluster's own work")
			}
			if entry.Client == nil {
				t.Fatal("the record kept no client, so no key could ever pay through it")
			}
			if entry.Err() == nil || !strings.Contains(entry.Err().Error(), "is not configured on this cluster") {
				t.Fatalf("Err() = %v, want the not-configured sentence", entry.Err())
			}
			if got := VendorOfEntry(entry); got != vendor {
				t.Fatalf("VendorOfEntry = %q, want %q", got, vendor)
			}

			// Unbound, it refuses without a request.
			_, err := entry.Client.(common.ChatAIProvider).CallChat(context.Background(), hello())
			if kind, ok := VendorAccountFailure(err); !ok || kind != accountFailureUnavailable {
				t.Fatalf("an unbound call on a cluster with no account: err = %v", err)
			}
			if got := len(v.seen()); got != 0 {
				t.Fatalf("the unbound call sent %d requests", got)
			}

			// Bound to a key, it answers.
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			reg.SetVendorKeys(keys)
			client, ok := reg.BindVendorAccount(entry, keyAccount(vendor, "k1", "ana"))
			if !ok {
				t.Fatal("the record could not be bound to a key")
			}
			if _, err := client.(common.ChatAIProvider).CallChat(context.Background(), hello()); err != nil {
				t.Fatalf("a key-paid call on a cluster with no account failed: %v", err)
			}
			// And the cluster's account still cannot be bound.
			if _, ok := reg.BindVendorAccount(entry, VendorAccount{Kind: VendorAccountCluster, Vendor: vendor}); ok {
				t.Error("the cluster's account was bound on a cluster that has none")
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

// OpenAI echoes part of a rejected key. Nothing shaped like one may reach the
// error a caller sees, and the SDK builds that error from the body (S4).
func TestKeyRefusalBodyIsScrubbedBeforeTheSDKSeesIt(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		t.Run(vendor, func(t *testing.T) {
			v := newAccountVendor(t, vendor)
			echo := "Incorrect API key provided: sk-proj-abcdEFGH1234****************wxyz."
			body := openaiBody("invalid_request_error", "invalid_api_key", echo)
			if vendor == VendorAnthropic {
				body = anthropicBody("authentication_error", echo)
			}
			v.answerNext(accountVendorAnswer{Status: 401, Body: body})
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			bound := boundTo(t, textProvider(t, v, false), keyAccount(vendor, "k1", "ana"), keys)

			_, err := bound.CallChat(context.Background(), hello())
			if err == nil {
				t.Fatal("a refused key returned no error")
			}
			if kind, ok := VendorAccountFailure(err); !ok || kind != accountFailureRefused {
				t.Fatalf("VendorAccountFailure = (%q, %v), want refused", kind, ok)
			}
			for _, leaked := range []string{"sk-proj-", "wxyz", standInKey} {
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("the error carries %q: %v", leaked, err)
				}
			}
			verdicts := keys.waitVerdicts(t, 1)
			if verdicts[0].Verdict != VendorKeyRefused || verdicts[0].KeyId != "k1" {
				t.Fatalf("verdict = %+v, want k1 refused", verdicts[0])
			}
			if strings.Contains(verdicts[0].Detail, "sk-proj-") || !strings.Contains(verdicts[0].Detail, "Incorrect API key provided") {
				t.Errorf("the recorded detail is not the vendor's words scrubbed: %q", verdicts[0].Detail)
			}
		})
	}
}

func TestKeyOutcomesAreObserved(t *testing.T) {
	t.Run("a served call marks the key good once an hour", func(t *testing.T) {
		v := newAccountVendor(t, VendorOpenAI)
		keys := newFakeVendorKeys()
		keys.secrets["k1"] = standInKey
		bound := boundTo(t, textProvider(t, v, false), keyAccount(VendorOpenAI, "k1", "ana"), keys)
		for i := 0; i < 3; i++ {
			if _, err := bound.CallChat(context.Background(), hello()); err != nil {
				t.Fatal(err)
			}
		}
		verdicts := keys.waitVerdicts(t, 1)
		if len(verdicts) != 1 || verdicts[0].Verdict != VendorKeyGood {
			t.Fatalf("verdicts = %+v, want exactly one good", verdicts)
		}
	})

	t.Run("an exhausted balance holds the key", func(t *testing.T) {
		v := newAccountVendor(t, VendorAnthropic)
		v.answerNext(accountVendorAnswer{Status: 402, Body: anthropicBody("billing_error", "There is an issue with your billing.")})
		keys := newFakeVendorKeys()
		keys.secrets["k1"] = standInKey
		bound := boundTo(t, textProvider(t, v, false), keyAccount(VendorAnthropic, "k1", "ana"), keys)
		if _, err := bound.CallChat(context.Background(), hello()); err == nil {
			t.Fatal("a 402 returned no error")
		}
		verdicts := keys.waitVerdicts(t, 1)
		if verdicts[0].Verdict != VendorKeyCredit {
			t.Fatalf("verdict = %+v, want credit", verdicts[0])
		}
	})

	t.Run("a rate limit and an outage say nothing about the key", func(t *testing.T) {
		for _, status := range []int{429, 500} {
			v := newAccountVendor(t, VendorAnthropic)
			body := anthropicBody("rate_limit_error", "slow down")
			if status == 500 {
				body = anthropicBody("api_error", "oops")
			}
			// The SDK retries these twice; every attempt gets the same answer.
			v.answerNext(accountVendorAnswer{Status: status, Body: body}, accountVendorAnswer{Status: status, Body: body}, accountVendorAnswer{Status: status, Body: body})
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			bound := boundTo(t, textProvider(t, v, false), keyAccount(VendorAnthropic, "k1", "ana"), keys)
			if _, err := bound.CallChat(context.Background(), hello()); err == nil {
				t.Fatalf("a %d returned no error", status)
			}
			if got := keys.verdicts(); len(got) != 0 {
				t.Errorf("a %d was recorded against the key: %+v", status, got)
			}
		}
	})
}

// EVIDENCE IS DATED WHEN THE CALL STARTED (mesh rule M3, Review Focus 5). The
// vendor judges a key when a request arrives, and a non-streaming call answers
// when generation ends, which can be minutes later. Dated on arrival of the
// answer, a long call that began before a key was revoked would report "good"
// with a time AFTER the refusal that another call recorded in between, and
// undo it.
func TestAVerdictIsDatedWhenItsCallStarted(t *testing.T) {
	for name, answer := range map[string]accountVendorAnswer{
		"a refusal":     {Status: 401, Body: anthropicBody("authentication_error", "invalid x-api-key"), Delay: 250 * time.Millisecond},
		"a served call": {Status: http.StatusOK, Delay: 250 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			v := newAccountVendor(t, VendorAnthropic)
			v.answerNext(answer)
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			bound := boundTo(t, textProvider(t, v, false), keyAccount(VendorAnthropic, "k1", "ana"), keys)

			asked := time.Now()
			_, _ = bound.CallChat(context.Background(), hello())
			verdict := keys.waitVerdicts(t, 1)[0]
			arrived := v.seen()[0].At
			if verdict.At.Before(asked) {
				t.Fatalf("the verdict is dated %s, before the call was made at %s", verdict.At, asked)
			}
			if verdict.At.After(arrived) {
				t.Errorf("the verdict is dated %s after the vendor received the call: evidence is the time the call STARTED, "+
					"or a slow call that began before a revocation reports good later and undoes it", verdict.At.Sub(arrived))
			}
		})
	}
}

// THE ENGINE'S OWN BUDGET STOP IS NOT THE VENDOR TALKING. The cost guard sits
// beneath the SDK and answers a 402 of its own once the process budget is
// spent. Read as Anthropic's 402 it would hold the key for an hour on every
// replica, write an audit event, and mail the key's owner that their account
// is out of credit, quoting this engine's own sentence as the vendor's.
func TestTheEnginesOwnBudgetStopSaysNothingAboutAKey(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		t.Run(vendor, func(t *testing.T) {
			v := newAccountVendor(t, vendor)
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			bound := boundTo(t, textProvider(t, v, false), keyAccount(vendor, "k1", "ana"), keys)

			sharedLLMGuard.mu.Lock()
			sharedLLMGuard.tripLocked("the test spent the process budget")
			sharedLLMGuard.mu.Unlock()

			_, err := bound.CallChat(context.Background(), hello())
			if err == nil {
				t.Fatal("a call past the process budget returned no error")
			}
			if len(v.seen()) != 0 {
				t.Fatalf("the vendor was called past the budget: %+v", v.seen())
			}
			if kind, ok := VendorAccountFailure(err); ok {
				t.Errorf("the engine's own budget stop is read as an account failure (%s), so the router would try the next account and word it as the vendor's", kind)
			}
			if got := keys.verdicts(); len(got) != 0 {
				t.Errorf("the engine's own budget stop was recorded against the key: %+v", got)
			}
		})
	}
}

// A KEY DOES NOT FOLLOW A REDIRECT (safety rule S5). Go keeps a custom header
// such as X-Api-Key when a redirect leaves the host, so a redirect from the
// vendor's address would carry a person's key to wherever it pointed. A key is
// sent to the vendor's own address and nowhere else, by construction.
func TestAKeyDoesNotFollowARedirect(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		t.Run(vendor, func(t *testing.T) {
			var elsewhereSaw []http.Header
			var mu sync.Mutex
			elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				elsewhereSaw = append(elsewhereSaw, r.Header.Clone())
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(anthropicOKBody))
			}))
			t.Cleanup(elsewhere.Close)

			v := newAccountVendor(t, vendor)
			v.answerNext(accountVendorAnswer{Status: http.StatusTemporaryRedirect, Location: elsewhere.URL + "/v1/messages"})
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			bound := boundTo(t, textProvider(t, v, false), keyAccount(vendor, "k1", "ana"), keys)

			if _, err := bound.CallChat(context.Background(), hello()); err == nil {
				t.Error("a redirected call was reported as served")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(elsewhereSaw) != 0 {
				t.Fatalf("the call followed a redirect away from %s's address, carrying: %v", vendor, elsewhereSaw)
			}
			if got := keys.verdicts(); len(got) != 0 {
				t.Errorf("a redirect was recorded against the key: %+v", got)
			}
		})
	}
}

// --- which accounts can pay ------------------------------------------------

type scriptedVendorClient struct {
	AIProvider
	origin string
}

func (s *scriptedVendorClient) WithVendorAccount(VendorAccount, VendorKeys) any { return s }
func (s *scriptedVendorClient) VendorOrigin() string                            { return s.origin }

func vendorEntry(vendor string, available bool, origin string) *ProviderConfigEntry {
	providerType := "Anthropic"
	if vendor == VendorOpenAI {
		providerType = "OpenAI"
	}
	return &ProviderConfigEntry{
		Config:    ProviderConfig{Name: "record", Type: providerType, Model: "model"},
		Client:    &scriptedVendorClient{origin: origin},
		Available: available,
	}
}

func personContext(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

func TestVendorAccountsForOrdersKeysBeforeTheCluster(t *testing.T) {
	reg := newProviderRegistry()
	openToEverybody(reg)
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorAnthropic, "mine", "ana"), keyAccount(VendorAnthropic, "lent", "ben")}
	keys.passedOver = []VendorAccountPassedOver{{Account: keyAccount(VendorAnthropic, "bad", "cy"), Reason: "its API key was refused by Anthropic"}}
	reg.SetVendorKeys(keys)

	accounts, passed := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true}, vendorEntry(VendorAnthropic, true, ""))
	var refs []string
	for _, a := range accounts {
		refs = append(refs, a.Ref())
	}
	if got := strings.Join(refs, " "); got != "key:mine key:lent cluster" {
		t.Fatalf("accounts = %q, want the person's key, the lent key, then the cluster's", got)
	}
	if len(passed) != 1 || passed[0].Account.KeyId != "bad" {
		t.Fatalf("passed over = %+v, want the refused key", passed)
	}
	if q := keys.queries[0]; q.Vendor != VendorAnthropic || q.ActingUserId != "ana" || !q.Chosen {
		t.Fatalf("the seam was asked %+v", q)
	}
}

// One read of a person's keys serves every record of the vendor a route names,
// and an answer read for one vendor never offers its keys at another.
func TestOneKeyAnswerServesEveryRecordOfItsVendor(t *testing.T) {
	reg := newProviderRegistry()
	openToEverybody(reg)
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorAnthropic, "mine", "ana")}
	keys.passedOver = []VendorAccountPassedOver{{Account: keyAccount(VendorAnthropic, "bad", "ana"), Reason: "its API key was refused by Anthropic"}}
	reg.SetVendorKeys(keys)

	answer := reg.VendorKeysAt(personContext("ana"), VendorAccountQuery{Vendor: VendorAnthropic, ActingUserId: "ana", Chosen: true})
	for _, available := range []bool{true, false} {
		accounts, passed := reg.VendorAccountsWith(vendorEntry(VendorAnthropic, available, ""), answer)
		if len(accounts) == 0 || accounts[0].Ref() != "key:mine" {
			t.Fatalf("accounts = %+v, want the person's key first", accounts)
		}
		if (len(accounts) == 2) != available {
			t.Fatalf("accounts = %+v with the cluster's account available=%v", accounts, available)
		}
		if len(passed) == 0 || passed[0].Account.KeyId != "bad" {
			t.Fatalf("passed over = %+v, want the refused key", passed)
		}
	}
	if len(keys.queries) != 1 {
		t.Fatalf("the seam was asked %d times, want once", len(keys.queries))
	}

	accounts, passed := reg.VendorAccountsWith(vendorEntry(VendorOpenAI, true, ""), answer)
	if len(accounts) != 1 || accounts[0].Kind != VendorAccountCluster || len(passed) != 0 {
		t.Fatalf("an Anthropic answer at an OpenAI record gave %+v / %+v, want the cluster's account alone", accounts, passed)
	}

	// No vendor named, no person, a visitor: the seam is not asked at all.
	for name, q := range map[string]VendorAccountQuery{
		"no vendor": {ActingUserId: "ana", Chosen: true},
		"no person": {Vendor: VendorAnthropic, Chosen: true},
		"a visitor": {Vendor: VendorAnthropic, ActingUserId: "ana", Chosen: true, ForVisitor: true},
	} {
		if got := reg.VendorKeysAt(personContext("ana"), q); len(got.Usable)+len(got.PassedOver) != 0 {
			t.Errorf("%s: answer = %+v, want nothing", name, got)
		}
	}
	if len(keys.queries) != 1 {
		t.Fatalf("the seam was asked %d times after questions it must not see", len(keys.queries))
	}
}

func TestVendorAccountsForNoPersonIsClusterOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    context.Context
		userId string
	}{
		"no acting person":      {context.Background(), ""},
		"an automation":         {context.Background(), "system:automation:nightly"},
		"the cluster's actor":   {auth.ContextWithSystemActor(context.Background(), "sweep"), ""},
		"the operator's stream": {context.Background(), "cluster:operator"},
	} {
		t.Run(name, func(t *testing.T) {
			reg := newProviderRegistry()
			keys := newFakeVendorKeys()
			keys.usable = []VendorAccount{keyAccount(VendorOpenAI, "k1", "ana")}
			reg.SetVendorKeys(keys)

			accounts, _ := reg.VendorAccountsFor(tc.ctx, VendorAccountQuery{ActingUserId: tc.userId}, vendorEntry(VendorOpenAI, true, ""))
			if len(accounts) != 1 || accounts[0].Kind != VendorAccountCluster {
				t.Fatalf("accounts = %+v, want the cluster's alone", accounts)
			}
			if len(keys.queries) != 0 {
				t.Fatal("the cluster's own work asked for keys")
			}
		})
	}
}

func TestVendorAccountsForAVisitorIsClusterOnly(t *testing.T) {
	reg := newProviderRegistry()
	openToEverybody(reg)
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorOpenAI, "k1", "ana")}
	reg.SetVendorKeys(keys)

	accounts, _ := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true, ForVisitor: true}, vendorEntry(VendorOpenAI, true, ""))
	if len(accounts) != 1 || accounts[0].Kind != VendorAccountCluster {
		t.Fatalf("accounts = %+v, want the cluster's alone", accounts)
	}
	if len(keys.queries) != 0 {
		t.Fatal("a visitor's call asked for the owner's keys")
	}
}

func TestVendorAccountsForPassesKeysOverAtAnotherAddress(t *testing.T) {
	reg := newProviderRegistry()
	openToEverybody(reg)
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorAnthropic, "k1", "ana")}
	reg.SetVendorKeys(keys)

	accounts, passed := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true}, vendorEntry(VendorAnthropic, true, "https://proxy.internal"))
	if len(accounts) != 1 || accounts[0].Kind != VendorAccountCluster {
		t.Fatalf("accounts = %+v, want the cluster's alone", accounts)
	}
	if len(passed) != 1 || !strings.Contains(passed[0].Reason, "another address") {
		t.Fatalf("passed over = %+v, want the key with the address reason", passed)
	}
}

func TestVendorAccountsForSaysWhyNothingCanPay(t *testing.T) {
	reg := newProviderRegistry()
	keys := newFakeVendorKeys()
	keys.keysErr = errors.New("database is down")
	reg.SetVendorKeys(keys)

	accounts, passed := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana"}, vendorEntry(VendorOpenAI, false, ""))
	if len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
	var reasons []string
	for _, p := range passed {
		reasons = append(reasons, p.Reason)
	}
	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "API keys could not be read") || !strings.Contains(joined, "the cluster's account is not set up for OpenAI") {
		t.Fatalf("reasons = %q", joined)
	}
	if strings.Contains(joined, "database is down") {
		t.Errorf("a read error reached a reason every signed-in person can read: %q", joined)
	}
}

func TestVendorAccountUsableResolvesNothingForAStranger(t *testing.T) {
	reg := newProviderRegistry()
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorOpenAI, "k1", "ana")}
	reg.SetVendorKeys(keys)
	entry := vendorEntry(VendorOpenAI, true, "")
	q := VendorAccountQuery{ActingUserId: "ana", Chosen: true}

	if a, ok := reg.VendorAccountUsable(personContext("ana"), q, entry, "key:k1"); !ok || a.KeyId != "k1" {
		t.Fatalf("the person's own key did not resolve: %+v %v", a, ok)
	}
	for _, ref := range []string{"key:nope", "key:", "nonsense", ""} {
		if _, ok := reg.VendorAccountUsable(personContext("ana"), q, entry, ref); ok {
			t.Errorf("reference %q resolved", ref)
		}
	}
	if _, ok := reg.VendorAccountUsable(context.Background(), VendorAccountQuery{}, entry, "key:k1"); ok {
		t.Error("a key resolved for a call with no acting person")
	}
	if a, ok := reg.VendorAccountUsable(context.Background(), VendorAccountQuery{}, entry, "cluster"); !ok || a.Kind != VendorAccountCluster {
		t.Error("the cluster's account did not resolve on a cluster that has one")
	}
	if _, ok := reg.VendorAccountUsable(context.Background(), VendorAccountQuery{}, vendorEntry(VendorOpenAI, false, ""), "cluster"); ok {
		t.Error("the cluster's account resolved on a cluster that has none")
	}
}

func TestVisionAndAudioRecordsTakeNoKey(t *testing.T) {
	for _, providerType := range []string{"OpenAIEmbedding", "OpenAITTS", "OpenAISTT", "Fleet", "SubscriptionApp"} {
		entry := &ProviderConfigEntry{
			Config: ProviderConfig{Name: "r", Type: providerType, Model: "m"},
			Client: &scriptedVendorClient{},
		}
		if got := VendorOfEntry(entry); got != "" {
			t.Errorf("a %s record takes accounts of %q", providerType, got)
		}
	}
	if got := VendorOfEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "r", Type: "OpenAI"}}); got != "" {
		t.Errorf("a record with no client takes accounts of %q", got)
	}
}

func TestAccountReferences(t *testing.T) {
	key := keyAccount(VendorOpenAI, "ab12", "ana")
	cluster := VendorAccount{Kind: VendorAccountCluster, Vendor: VendorOpenAI}
	for name, want := range map[string][2]string{
		JoinAccountReference("chat54", key):     {"chat54", "key:ab12"},
		JoinAccountReference("chat54", cluster): {"chat54", "cluster"},
		"chat54":                                {"chat54", ""},
		"fleet:hf.co/org/model@sha":             {"fleet:hf.co/org/model@sha", ""},
		"chat54@key:":                           {"chat54@key:", ""},
		"@cluster":                              {"@cluster", ""},
	} {
		record, ref := SplitAccountReference(name)
		if record != want[0] || ref != want[1] {
			t.Errorf("SplitAccountReference(%q) = (%q, %q), want (%q, %q)", name, record, ref, want[0], want[1])
		}
	}
}
