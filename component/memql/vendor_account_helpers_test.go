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
)

// accountVendor stands in for a vendor's API for the account tests. It answers
// the two text endpoints and the model list, records what each request
// carried, and can be told what to answer next.
type accountVendor struct {
	t      *testing.T
	vendor string
	srv    *httptest.Server

	mu       sync.Mutex
	requests []accountVendorRequest
	next     []accountVendorAnswer
}

type accountVendorRequest struct {
	Method string
	Path   string
	Header http.Header
}

type accountVendorAnswer struct {
	Status int
	Body   string
	// ContentType is application/json unless a test answers with a stream.
	ContentType string
}

const anthropicOKBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

const openaiOKBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// newAccountVendor starts a stand-in for one vendor and makes it the address a
// key may be sent to, for this test only. It also gives the test a loop guard
// of its own and a clean sign-in table, as the federation tests do.
func newAccountVendor(t *testing.T, vendor string) *accountVendor {
	t.Helper()
	isolateVendorSignIns(t)
	isolateLLMGuard(t)
	resetKeyGoodStampsForTest()
	t.Cleanup(resetKeyGoodStampsForTest)

	v := &accountVendor{t: t, vendor: vendor}
	v.srv = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.srv.Close)

	prevOrigin := vendorKeyOrigins[vendor]
	vendorKeyOrigins[vendor] = v.srv.URL
	t.Cleanup(func() { vendorKeyOrigins[vendor] = prevOrigin })

	// No ambient credential of the SDKs' own may answer for the engine.
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID"} {
		t.Setenv(name, "")
	}
	if vendor == VendorAnthropic {
		t.Setenv("ANTHROPIC_BASE_URL", v.srv.URL)
	}
	return v
}

func (v *accountVendor) serve(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.requests = append(v.requests, accountVendorRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
	answer := accountVendorAnswer{Status: http.StatusOK}
	if len(v.next) > 0 {
		answer, v.next = v.next[0], v.next[1:]
	}
	v.mu.Unlock()

	contentType := answer.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	if answer.Status != http.StatusOK {
		w.WriteHeader(answer.Status)
		_, _ = w.Write([]byte(answer.Body))
		return
	}
	switch {
	case answer.Body != "":
		_, _ = w.Write([]byte(answer.Body))
	case strings.HasSuffix(r.URL.Path, "/models"):
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","type":"model","object":"model"},{"id":"model-b","type":"model","object":"model"}],"has_more":false}`))
	case v.vendor == VendorAnthropic:
		_, _ = w.Write([]byte(anthropicOKBody))
	default:
		_, _ = w.Write([]byte(openaiOKBody))
	}
}

// answerNext queues answers for the next requests, in order.
func (v *accountVendor) answerNext(answers ...accountVendorAnswer) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.next = append(v.next, answers...)
}

func (v *accountVendor) seen() []accountVendorRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]accountVendorRequest(nil), v.requests...)
}

// config is a text record of this vendor pointed at the stand-in, on a cluster
// NOBODY SET THE VENDOR UP ON: its auth block carries what every pod carries,
// the path of the projected identity token, and no federation id.
func (v *accountVendor) config(name string, streaming bool) ProviderConfig {
	cfg := ProviderConfig{Name: name, Model: name + "-model", Auth: map[string]string{}, Params: map[string]any{}}
	if streaming {
		cfg.Params["streaming"] = true
	}
	switch v.vendor {
	case VendorAnthropic:
		cfg.Type = "Anthropic"
		cfg.Auth[authKeyIdentityTokenFile] = validIdentityToken(v.t)
	default:
		cfg.Type = "OpenAI"
		cfg.Auth[authKeyOpenAITokenFile] = validOpenAIIdentityToken(v.t)
		cfg.Auth["baseURL"] = v.srv.URL + "/v1"
	}
	return cfg
}

// fakeVendorKeys is the key seam with nothing behind it but what a test put
// there, and a record of everything it was asked.
type fakeVendorKeys struct {
	mu sync.Mutex

	usable     []VendorAccount
	passedOver []VendorAccountPassedOver
	// autoUsable, when not nil, is what a question about Auto (Chosen false)
	// is answered with: the keys whose owner switched Auto on. Nil answers
	// both questions with usable.
	autoUsable *[]VendorAccount
	// holder, when set, is the one acting person the keys are offered to.
	// Anybody else who asks has none. Empty offers them to whoever asks.
	holder string
	keysErr    error
	secrets    map[string]string
	secretErr  error

	queries    []VendorAccountQuery
	secretAsks []string
	observed   []observedKeyVerdict
	notify     chan struct{}
}

type observedKeyVerdict struct {
	KeyId   string
	Verdict VendorKeyVerdict
	Detail  string
	At      time.Time
}

func newFakeVendorKeys() *fakeVendorKeys {
	return &fakeVendorKeys{secrets: map[string]string{}, notify: make(chan struct{}, 64)}
}

func (f *fakeVendorKeys) KeysFor(_ context.Context, q VendorAccountQuery) ([]VendorAccount, []VendorAccountPassedOver, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.keysErr != nil {
		return nil, nil, f.keysErr
	}
	if f.holder != "" && q.ActingUserId != f.holder {
		return nil, nil, nil
	}
	usable := f.usable
	if !q.Chosen && f.autoUsable != nil {
		usable = *f.autoUsable
	}
	return append([]VendorAccount(nil), usable...), append([]VendorAccountPassedOver(nil), f.passedOver...), nil
}

func (f *fakeVendorKeys) Usable(_ context.Context, q VendorAccountQuery, keyId string) (VendorAccount, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	for _, a := range f.usable {
		if a.KeyId == keyId {
			return a, "", true
		}
	}
	return VendorAccount{}, "that API key is not one this call can use", false
}

func (f *fakeVendorKeys) Secret(_ context.Context, a VendorAccount) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secretAsks = append(f.secretAsks, a.KeyId)
	if f.secretErr != nil {
		return "", f.secretErr
	}
	value, ok := f.secrets[a.KeyId]
	if !ok {
		return "", errors.New("no such key")
	}
	return value, nil
}

func (f *fakeVendorKeys) Observe(_ context.Context, a VendorAccount, verdict VendorKeyVerdict, at time.Time, detail string) {
	f.mu.Lock()
	f.observed = append(f.observed, observedKeyVerdict{KeyId: a.KeyId, Verdict: verdict, Detail: detail, At: at})
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeVendorKeys) verdicts() []observedKeyVerdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]observedKeyVerdict(nil), f.observed...)
}

func (f *fakeVendorKeys) asks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.secretAsks...)
}

// waitVerdicts waits until at least n verdicts were observed. A served call is
// reported off the request's path, so a test that asserts on it has to wait.
func (f *fakeVendorKeys) waitVerdicts(t *testing.T, n int) []observedKeyVerdict {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if got := f.verdicts(); len(got) >= n {
			return got
		}
		select {
		case <-f.notify:
		case <-deadline:
			t.Fatalf("waited for %d verdicts about a key, saw %d", n, len(f.verdicts()))
		}
	}
}

func keyAccount(vendor, keyId, owner string) VendorAccount {
	return VendorAccount{Kind: VendorAccountKey, Vendor: vendor, KeyId: keyId, OwnerUserId: owner}
}

// resetKeyGoodStampsForTest forgets which keys this process has reported as
// good, so one test's served call does not silence the next test's.
func resetKeyGoodStampsForTest() {
	keyGoodStamps.Range(func(key, _ any) bool {
		keyGoodStamps.Delete(key)
		return true
	})
}
