package memql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/common"
)

// --- helpers ---------------------------------------------------------------

// validOpenAIIdentityToken writes a projected token carrying OpenAI's audience.
func validOpenAIIdentityToken(t *testing.T) string {
	t.Helper()
	return fixtureIdentityToken(t, map[string]any{
		"aud": []any{openaiAudience},
		"sub": "system:serviceaccount:memql:memql-engine",
		"exp": 4102444800,
	})
}

func openaiFederatedAuth(tokenFile string) map[string]string {
	return map[string]string{
		authKeyIdentityProviderID:     "idp_test",
		authKeyOpenAIServiceAccountID: "svc_test",
		authKeyOpenAITokenFile:        tokenFile,
	}
}

// exchangeServer is a fake OpenAI auth host plus a fake API, so one test can
// follow a bearer from the exchange all the way onto an API request.
type exchangeServer struct {
	srv *httptest.Server

	mu             sync.Mutex
	exchangeBodies []map[string]any
	apiAuth        []string
	token          string
	expiresIn      int
	status         int
	denyBody       string

	// apiStatusOnce answers the next API call with this status and apiBodyOnce.
	apiStatusOnce int
	apiBodyOnce   string
}

func newExchangeServer(t *testing.T) *exchangeServer {
	t.Helper()
	isolateVendorSignIns(t)
	isolateLLMGuard(t)
	f := &exchangeServer{token: "sk-federated-1", expiresIn: 3600, status: http.StatusOK}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/oauth/token") {
			body, _ := readAllBody(req)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)

			f.mu.Lock()
			f.exchangeBodies = append(f.exchangeBodies, parsed)
			status, token, expiresIn, denyBody := f.status, f.token, f.expiresIn, f.denyBody
			f.mu.Unlock()

			if status != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(denyBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": token,
				"expires_in":   expiresIn,
				"token_type":   "Bearer",
			})
			return
		}

		f.mu.Lock()
		f.apiAuth = append(f.apiAuth, req.Header.Get("Authorization"))
		once, onceBody := f.apiStatusOnce, f.apiBodyOnce
		f.apiStatusOnce, f.apiBodyOnce = 0, ""
		f.mu.Unlock()
		if once != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(once)
			_, _ = w.Write([]byte(onceBody))
			return
		}
		writeJSON(w, chatCompletionResponse())
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// failNextAPICall makes the API answer its next call with status and body.
func (f *exchangeServer) failNextAPICall(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiStatusOnce, f.apiBodyOnce = status, body
}

func (f *exchangeServer) exchanges() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.exchangeBodies))
	copy(out, f.exchangeBodies)
	return out
}

func (f *exchangeServer) apiAuthorizations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.apiAuth))
	copy(out, f.apiAuth)
	return out
}

func (f *exchangeServer) setToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = token
}

func (f *exchangeServer) deny(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.denyBody = status, body
}

// rotateIdentityToken replaces the identity token at path the way kubelet
// rotates a projected one: same path, new content.
func rotateIdentityToken(t *testing.T, path, audience string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"aud": []any{audience},
		"sub": "system:serviceaccount:memql:memql-engine",
		"exp": 4102444800,
		"jti": fmt.Sprintf("rotated-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
}

func readAllBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer func() { _ = req.Body.Close() }()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(req.Body)
	return buf.Bytes(), err
}

// openaiFederatedConfig points a provider at the fake for BOTH the exchange
// and the API.
func openaiFederatedConfig(t *testing.T, f *exchangeServer) ProviderConfig {
	t.Helper()
	auth := openaiFederatedAuth(validOpenAIIdentityToken(t))
	auth[authKeyOpenAITokenEndpoint] = f.srv.URL + "/oauth/token"
	auth["baseURL"] = f.srv.URL + "/v1"
	return ProviderConfig{Name: "openaiTest", Type: "OpenAI", Model: "gpt-5.4-mini", Auth: auth}
}

// --- the three-way switch (design D2) --------------------------------------

func TestOpenAICredentialFederatesWhenAllThreeAreSet(t *testing.T) {
	cfg := ProviderConfig{Name: "openaiTest", Auth: openaiFederatedAuth(validOpenAIIdentityToken(t))}

	opts, source, path, err := openaiCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("openaiCredential: %v", err)
	}
	if path != credentialPathFederation {
		t.Fatalf("credential path = %q, want %q", path, credentialPathFederation)
	}
	if source == nil {
		t.Fatal("the federated path returned no bearer source; the Realtime WebSocket and provider-auth check both need it")
	}
	if len(opts) == 0 {
		t.Fatal("the federated path returned no request options")
	}
}

func TestOpenAICredentialIsUnavailableWhenNoneIsSet(t *testing.T) {
	// NOT AN ERROR. This is a fresh cloud cluster and every local cluster, and
	// after key removal there is nothing else it could mean. Reporting it as a
	// failure would make "no AI configured" indistinguishable from "AI is
	// broken" on the one screen an operator checks first.
	cfg := ProviderConfig{Name: "openaiTest", Auth: map[string]string{}}

	opts, source, path, err := openaiCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("an unconfigured provider errored rather than reporting unavailable: %v", err)
	}
	if path != credentialPathUnavailable {
		t.Fatalf("credential path = %q, want %q", path, credentialPathUnavailable)
	}
	if source != nil || len(opts) != 0 {
		t.Fatal("the unavailable path produced a credential")
	}
}

func TestOpenAICredentialRefusesOneOrTwo(t *testing.T) {
	tokenFile := validOpenAIIdentityToken(t)
	full := openaiFederatedAuth(tokenFile)

	// Every proper non-empty subset must refuse, and must name both halves,
	// EXCEPT the token file on its own, which is not a configuration at all
	// (TestOpenAITokenFileAloneIsNotAConfiguration).
	subsets := [][]string{
		{authKeyIdentityProviderID},
		{authKeyOpenAIServiceAccountID},
		{authKeyIdentityProviderID, authKeyOpenAIServiceAccountID},
		{authKeyIdentityProviderID, authKeyOpenAITokenFile},
		{authKeyOpenAIServiceAccountID, authKeyOpenAITokenFile},
	}
	for _, subset := range subsets {
		auth := map[string]string{}
		for _, key := range subset {
			auth[key] = full[key]
		}
		cfg := ProviderConfig{Name: "openaiTest", Auth: auth}

		_, _, path, err := openaiCredential(cfg, guardedHTTPClient(nil))
		if err == nil {
			t.Errorf("partial set %v was accepted with path %q; it must refuse", subset, path)
			continue
		}
		if path == credentialPathUnavailable {
			t.Errorf("partial set %v was read as 'not configured'", subset)
		}
		// The operator is mid-runbook: the message has to say which ids they
		// have and which they lack, not merely that something is wrong.
		for _, key := range subset {
			envName := map[string]string{
				authKeyIdentityProviderID:     envOpenAIIdentityProviderID,
				authKeyOpenAIServiceAccountID: envOpenAIServiceAccountID,
				authKeyOpenAITokenFile:        envOpenAIIdentityTokenFile,
			}[key]
			if !strings.Contains(err.Error(), envName) {
				t.Errorf("subset %v: error does not name the PRESENT %s: %v", subset, envName, err)
			}
		}
	}
}

// The token file's path is set by every Deployment on every cluster, so it is
// not something an operator configured: see
// TestAnthropicTokenFileAloneIsNotAConfiguration for what counting it cost.
func TestOpenAITokenFileAloneIsNotAConfiguration(t *testing.T) {
	cfg := ProviderConfig{Name: "openaiTest", Auth: map[string]string{
		authKeyOpenAITokenFile: validOpenAIIdentityToken(t),
	}}
	opts, source, path, err := openaiCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("a pod with the token file and no ids was refused as misconfigured: %v", err)
	}
	if path != credentialPathUnavailable || source != nil || len(opts) != 0 {
		t.Fatalf("credential path = %q, want %q and no credential", path, credentialPathUnavailable)
	}

	_, err = newOpenAIClient(cfg, guardedHTTPClient(nil))
	if err == nil {
		t.Fatal("a client was built with no credential")
	}
	if strings.Contains(err.Error(), "HALF-CONFIGURED") {
		t.Errorf("a cluster nobody set up is told it is half-configured: %v", err)
	}
}

func TestOpenAIIdsWithoutATokenFileRefuse(t *testing.T) {
	auth := openaiFederatedAuth(validOpenAIIdentityToken(t))
	delete(auth, authKeyOpenAITokenFile)
	_, _, _, err := openaiCredential(ProviderConfig{Name: "openaiTest", Auth: auth}, guardedHTTPClient(nil))
	if err == nil {
		t.Fatal("ids with no token file were accepted")
	}
	if !strings.Contains(err.Error(), "HALF-CONFIGURED") || !strings.Contains(err.Error(), envOpenAIIdentityTokenFile) {
		t.Errorf("the refusal does not name the missing token file: %v", err)
	}
}

func TestOpenAICredentialRefusesATokenWithTheWrongAudience(t *testing.T) {
	// The cheapest wrong implementation is mounting the Anthropic projected
	// token at OpenAI's path. It is a well-formed service-account JWT and
	// every structural check passes; only the audience tells them apart.
	anthropicToken := validIdentityToken(t)
	auth := openaiFederatedAuth(anthropicToken)
	cfg := ProviderConfig{Name: "openaiTest", Auth: auth}

	_, _, _, err := openaiCredential(cfg, guardedHTTPClient(nil))
	if err == nil {
		t.Fatal("a token minted for Anthropic's audience was accepted for OpenAI")
	}
	if !strings.Contains(err.Error(), openaiAudience) {
		t.Errorf("the refusal does not name the audience it wanted: %v", err)
	}
}

// --- the exchange ----------------------------------------------------------

// TestOpenAIClientsAndTranscriptionShareOneSignIn: every consumer of the
// cluster's OpenAI account takes its bearer from the one sign-in the process
// keeps (design 2026-11-21, A1). Each used to build an exchanger of its own,
// and the transcription socket built another every time it asked.
func TestOpenAIClientsAndTranscriptionShareOneSignIn(t *testing.T) {
	f := newExchangeServer(t)
	cfg := openaiFederatedConfig(t, f)
	ctx := context.Background()

	chat, err := newOpenAIProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAIProvider: %v", err)
	}
	stream, err := newOpenAIStreamProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAIStreamProvider: %v", err)
	}
	speech, err := newOpenAITTSProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAITTSProvider: %v", err)
	}
	transcribe, err := newOpenAISTTProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAISTTProvider: %v", err)
	}
	embed, err := newOpenAIEmbeddingProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAIEmbeddingProvider: %v", err)
	}

	registry := newProviderRegistry()
	registry.setEntry(&ProviderConfigEntry{Config: cfg, Client: chat, Available: true})
	socket, ok := (&MemQLEngine{providers: registry}).OpenAIBearer()
	if !ok {
		t.Fatal("OpenAIBearer found no federated OpenAI provider")
	}

	var clients []*openai.Client
	for _, p := range []AIProvider{chat, stream, speech} {
		client, ok := openAIClientOf(p)
		if !ok {
			t.Fatalf("%T carries no OpenAI client", p)
		}
		clients = append(clients, client)
	}
	clients = append(clients, transcribe.(*openAISTTProvider).client)
	for i, client := range clients {
		if _, err := client.Chat.Completions.New(ctx, buildMinimalChatParams()); err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
	embedBearer, _, err := embed.(*OpenAIEmbeddingClient).bearer.Bearer(ctx)
	if err != nil {
		t.Fatalf("embedding bearer: %v", err)
	}
	socketBearer, _, err := socket.Bearer(ctx)
	if err != nil {
		t.Fatalf("transcription bearer: %v", err)
	}

	if n := len(f.exchanges()); n != 1 {
		t.Fatalf("six consumers of one OpenAI account made %d sign-ins, want 1", n)
	}
	auths := f.apiAuthorizations()
	if len(auths) != len(clients) {
		t.Fatalf("API requests = %d, want %d", len(auths), len(clients))
	}
	for i, auth := range auths {
		if auth != "Bearer sk-federated-1" {
			t.Errorf("client %d sent Authorization %q, want the one federated bearer", i, auth)
		}
	}
	if embedBearer != "sk-federated-1" || socketBearer != "sk-federated-1" {
		t.Errorf("embedding bearer %q, transcription bearer %q; want the one federated bearer", embedBearer, socketBearer)
	}
}

func TestOpenAIBearerIsTheSharedSignIn(t *testing.T) {
	f := newExchangeServer(t)
	cfg := openaiFederatedConfig(t, f)
	ctx := context.Background()

	_, source, _, err := openaiCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("openaiCredential: %v", err)
	}
	registry := newProviderRegistry()
	registry.setEntry(&ProviderConfigEntry{Config: cfg, Available: true})
	engine := &MemQLEngine{providers: registry}

	first, ok := engine.OpenAIBearer()
	if !ok {
		t.Fatal("OpenAIBearer found no federated OpenAI provider")
	}
	second, _ := engine.OpenAIBearer()

	// One sign-in behind all three: the provider clients' source and both
	// sources the engine handed out give the same bearer from one exchange.
	want, _, err := source.Bearer(ctx)
	if err != nil {
		t.Fatalf("the clients' bearer: %v", err)
	}
	for i, src := range []BearerSource{first, second} {
		got, _, err := src.Bearer(ctx)
		if err != nil {
			t.Fatalf("OpenAIBearer source %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("OpenAIBearer source %d handed out %q, want the clients' bearer %q", i, got, want)
		}
	}
	if n := len(f.exchanges()); n != 1 {
		t.Fatalf("the clients' source and two OpenAIBearer sources made %d sign-ins, want 1", n)
	}
}

// Transcription is the one OpenAI consumer that takes a bearer source and no
// SDK client, and it takes it ONCE, when the node is wired (app/
// integrations_stt.go). The source used to be the sign-in object of that
// moment. After an operator corrected an id and pressed Apply, every other
// consumer moved to a sign-in under the new ids and transcription stayed on
// the old one until the node restarted: a second sign-in in the process, under
// ids somebody had just replaced. The source now asks the registry each time.
func TestTheTranscriptionBearerFollowsAnApply(t *testing.T) {
	f := newExchangeServer(t)
	cfg := openaiFederatedConfig(t, f)
	ctx := context.Background()

	chat, err := newOpenAIProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAIProvider: %v", err)
	}
	registry := newProviderRegistry()
	registry.setEntry(&ProviderConfigEntry{Config: cfg, Client: chat, Available: true})
	engine := &MemQLEngine{providers: registry}

	socket, ok := engine.OpenAIBearer() // kept by the transcription path for the life of the process
	if !ok {
		t.Fatal("OpenAIBearer found no federated OpenAI provider")
	}
	if got, _, err := socket.Bearer(ctx); err != nil || got != "sk-federated-1" {
		t.Fatalf("before the Apply: bearer %q, err %v", got, err)
	}

	// The Apply: the same record, rebuilt from the corrected ids.
	fixed := cfg
	fixed.Auth = map[string]string{}
	for k, v := range cfg.Auth {
		fixed.Auth[k] = v
	}
	fixed.Auth[authKeyOpenAIServiceAccountID] = "svc_fixed"
	f.setToken("sk-federated-2")
	rebuilt, err := newOpenAIProvider(fixed)
	if err != nil {
		t.Fatalf("newOpenAIProvider after the Apply: %v", err)
	}
	registry.setEntry(&ProviderConfigEntry{Config: fixed, Client: rebuilt, Available: true})

	got, _, err := socket.Bearer(ctx)
	if err != nil {
		t.Fatalf("after the Apply: %v", err)
	}
	if got != "sk-federated-2" {
		t.Fatalf("after the Apply transcription was handed %q, the bearer of the ids the operator replaced", got)
	}
	exchanges := f.exchanges()
	if len(exchanges) != 2 {
		t.Fatalf("sign-ins = %d, want 2: one under each set of ids", len(exchanges))
	}
	if id := exchanges[1]["service_account_id"]; id != "svc_fixed" {
		t.Fatalf("the second sign-in named service account %v, want svc_fixed", id)
	}
}

// And when the ids are taken away, the source says so instead of going on
// handing out a bearer for an account the cluster no longer has.
func TestTheTranscriptionBearerEndsWhenOpenAIIsRemoved(t *testing.T) {
	f := newExchangeServer(t)
	cfg := openaiFederatedConfig(t, f)
	ctx := context.Background()

	registry := newProviderRegistry()
	registry.setEntry(&ProviderConfigEntry{Config: cfg, Available: true})
	engine := &MemQLEngine{providers: registry}
	socket, ok := engine.OpenAIBearer()
	if !ok {
		t.Fatal("OpenAIBearer found no federated OpenAI provider")
	}
	if _, _, err := socket.Bearer(ctx); err != nil {
		t.Fatalf("before the ids were removed: %v", err)
	}

	removed := cfg
	removed.Auth = map[string]string{authKeyOpenAITokenFile: cfg.Auth[authKeyOpenAITokenFile]}
	registry.setEntry(&ProviderConfigEntry{Config: removed})

	if got, _, err := socket.Bearer(ctx); err == nil {
		t.Fatalf("with OpenAI's ids removed, transcription was still handed a bearer (%q)", got)
	}
}

func TestOpenAIExchangeHappyPath(t *testing.T) {
	f := newExchangeServer(t)
	cfg := openaiFederatedConfig(t, f)

	provider, err := newOpenAIProvider(cfg)
	if err != nil {
		t.Fatalf("newOpenAIProvider: %v", err)
	}
	if _, err := provider.(common.ChatAIProvider).CallChat(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "hello"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}

	exchanges := f.exchanges()
	if len(exchanges) != 1 {
		t.Fatalf("expected exactly one token exchange, got %d", len(exchanges))
	}
	body := exchanges[0]

	// The five RFC 8693 fields, by name and value.
	want := map[string]string{
		"grant_type":           tokenExchangeGrantType,
		"subject_token_type":   subjectTokenTypeJWT,
		"identity_provider_id": "idp_test",
		"service_account_id":   "svc_test",
	}
	for key, value := range want {
		if got, _ := body[key].(string); got != value {
			t.Errorf("exchange body %s = %q, want %q", key, got, value)
		}
	}
	if subject, _ := body["subject_token"].(string); strings.Count(subject, ".") != 2 {
		t.Errorf("exchange body subject_token is not a JWS compact serialization: %q", subject)
	}

	// And the bearer it answered reached the API request.
	auths := f.apiAuthorizations()
	if len(auths) != 1 {
		t.Fatalf("expected one API request, got %d", len(auths))
	}
	if auths[0] != "Bearer sk-federated-1" {
		t.Errorf("API request carried Authorization %q, want the federated bearer", auths[0])
	}
}

func TestOpenAIExchangeSendsJSON(t *testing.T) {
	// OpenAI's token endpoint takes a JSON body, not a form encoding. A form
	// POST is the shape most OAuth endpoints take and is what a reader would
	// reach for; it fails with a 400 that talks about grant_type.
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		contentType = req.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)

	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          validOpenAIIdentityToken(t),
		TokenEndpoint:      srv.URL + "/oauth/token",
	}
	if _, _, err := newOpenAIExchanger(fed, guardedHTTPClient(nil)).Bearer(context.Background()); err != nil {
		t.Fatalf("Bearer: %v", err)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("exchange Content-Type = %q, want application/json", contentType)
	}
}

func TestOpenAIExchangeCachesAndReExchangesBeforeExpiry(t *testing.T) {
	f := newExchangeServer(t)
	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          validOpenAIIdentityToken(t),
		TokenEndpoint:      f.srv.URL + "/oauth/token",
	}
	exchanger := newOpenAIExchanger(fed, guardedHTTPClient(nil))

	base := time.Now()
	exchanger.now = func() time.Time { return base }

	first, expiresAt, err := exchanger.Bearer(context.Background())
	if err != nil {
		t.Fatalf("first Bearer: %v", err)
	}
	if first != "sk-federated-1" {
		t.Fatalf("first bearer = %q", first)
	}

	// Well inside the lifetime: the cached token is handed back, no exchange.
	exchanger.now = func() time.Time { return base.Add(30 * time.Minute) }
	if again, _, err := exchanger.Bearer(context.Background()); err != nil || again != first {
		t.Fatalf("mid-life Bearer = %q, %v; want the cached token", again, err)
	}
	if n := len(f.exchanges()); n != 1 {
		t.Fatalf("a cached token still cost %d exchanges", n)
	}

	// Near expiry with the SAME identity token on disk: the bearer in hand is
	// still valid and the token has already bought it, so nothing is presented.
	exchanger.now = func() time.Time { return expiresAt.Add(-15 * time.Second) }
	if again, _, err := exchanger.Bearer(context.Background()); err != nil || again != first {
		t.Fatalf("near-expiry Bearer with no new identity token = %q, %v; want the cached token", again, err)
	}
	if n := len(f.exchanges()); n != 1 {
		t.Fatalf("the same identity token was presented again: %d exchanges", n)
	}

	// Kubelet rotates the token. Near expiry it must sign in again rather than
	// go on handing out a bearer that is about to stop working. The call that
	// starts the renewal is answered with the bearer in hand, which is still
	// valid; the renewal runs behind it and the next call has the new one.
	rotateIdentityToken(t, fed.TokenFile, openaiAudience)
	f.setToken("sk-federated-2")
	if during, _, err := exchanger.Bearer(context.Background()); err != nil || during != first {
		t.Fatalf("the call that started the renewal got %q, %v; want the bearer still in hand", during, err)
	}
	settle(t, exchanger)
	renewed, _, err := exchanger.Bearer(context.Background())
	if err != nil {
		t.Fatalf("renewal Bearer: %v", err)
	}
	if renewed != "sk-federated-2" {
		t.Errorf("bearer near expiry = %q, want the re-exchanged token", renewed)
	}
	if n := len(f.exchanges()); n != 2 {
		t.Errorf("expected a second exchange, saw %d in total", n)
	}
}

func TestOpenAIExchangeKeepsTheLastGoodBearerThroughADenial(t *testing.T) {
	// A denial arrives while a valid token is still in hand. Dropping it would
	// turn a misconfiguration the operator has up to an hour to fix into an
	// immediate outage -- and the counter is what makes the window visible.
	f := newExchangeServer(t)
	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          validOpenAIIdentityToken(t),
		TokenEndpoint:      f.srv.URL + "/oauth/token",
	}
	exchanger := newOpenAIExchanger(fed, guardedHTTPClient(nil))
	base := time.Now()
	exchanger.now = func() time.Time { return base }

	first, expiresAt, err := exchanger.Bearer(context.Background())
	if err != nil {
		t.Fatalf("first Bearer: %v", err)
	}

	rotateIdentityToken(t, fed.TokenFile, openaiAudience)
	f.deny(http.StatusForbidden, `{"error":{"message":"subject does not match the mapping"}}`)
	exchanger.now = func() time.Time { return expiresAt.Add(-15 * time.Second) }

	still, _, err := exchanger.Bearer(context.Background())
	if err != nil {
		t.Fatalf("a denial dropped a still-valid bearer: %v", err)
	}
	if still != first {
		t.Errorf("bearer after a denial = %q, want the last good one %q", still, first)
	}
	settle(t, exchanger)
	if after, _, err := exchanger.Bearer(context.Background()); err != nil || after != first {
		t.Fatalf("once the denial arrived the bearer is %q, %v; want the last good one", after, err)
	}
	if n := len(f.exchanges()); n != 2 {
		t.Fatalf("exchanges = %d, want 2: the rotated token must have been presented and refused", n)
	}

	// Once it has actually expired there is nothing to fall back to, and the
	// error must surface rather than hand out a dead token.
	exchanger.now = func() time.Time { return expiresAt.Add(time.Minute) }
	if _, _, err := exchanger.Bearer(context.Background()); err == nil {
		t.Error("an expired bearer was handed out after a denial")
	}
}

func TestOpenAIExchangeDenialNamesTheVendorsOwnReason(t *testing.T) {
	f := newExchangeServer(t)
	f.deny(http.StatusForbidden, `{"error":{"message":"identity_provider_id not found"}}`)
	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          validOpenAIIdentityToken(t),
		TokenEndpoint:      f.srv.URL + "/oauth/token",
	}

	_, _, err := newOpenAIExchanger(fed, guardedHTTPClient(nil)).Bearer(context.Background())
	if err == nil {
		t.Fatal("a denied exchange returned no error")
	}
	if !strings.Contains(err.Error(), "identity_provider_id not found") {
		t.Errorf("the error drops the vendor's own reason, which is the one string an operator can act on: %v", err)
	}
	if !strings.Contains(err.Error(), "openai-federation.md") {
		t.Errorf("the error does not point at the runbook: %v", err)
	}
}

// TestOpenAIExchangeNeverLogsTheSubjectToken is the disclosure control.
//
// The subject token is a signed assertion of this pod's identity. It is sent,
// so it is not secret in transit -- but a copy of it in a log sink is a copy
// that outlives the exchange and sits somewhere read more widely than the
// cluster is.
func TestOpenAIExchangeNeverLogsTheSubjectToken(t *testing.T) {
	f := newExchangeServer(t)
	f.deny(http.StatusForbidden, `{"error":{"message":"nope"}}`)

	tokenFile := validOpenAIIdentityToken(t)
	subject, err := readFileTrimmed(tokenFile)
	if err != nil {
		t.Fatalf("read fixture token: %v", err)
	}

	var sink bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          tokenFile,
		TokenEndpoint:      f.srv.URL + "/oauth/token",
	}
	_, _, exchangeErr := newOpenAIExchanger(fed, guardedHTTPClient(nil)).Bearer(context.Background())
	if exchangeErr == nil {
		t.Fatal("expected the denial to error")
	}

	if strings.Contains(sink.String(), subject) {
		t.Error("the subject token appeared in a log line")
	}
	if strings.Contains(exchangeErr.Error(), subject) {
		t.Error("the subject token appeared in the returned error")
	}
	// A REACHABLE POSITIVE for the assertion above: prove the sink was
	// actually being written to, or "the token is absent" is satisfied by an
	// empty buffer.
	if sink.Len() == 0 {
		t.Fatal("no log output was captured at all, so the absence above proves nothing")
	}
}

// --- the observer ----------------------------------------------------------

func TestFederationExchangeVendorAttribution(t *testing.T) {
	// Anthropic's path is /v1/oauth/token and OpenAI's is /oauth/token, so the
	// OpenAI suffix matches both. Getting this backwards moves one vendor's
	// entire exchange rate onto the other's series and leaves a permanent zero
	// where a real number belongs -- silently, since neither series is missing.
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodPost, "/v1/oauth/token", metrics.FederationVendorAnthropic},
		{http.MethodPost, "/oauth/token", metrics.FederationVendorOpenAI},
		{http.MethodPost, "/v1/chat/completions", ""},
		{http.MethodGet, "/v1/oauth/token", ""},
	}
	for _, tc := range cases {
		if got := federationExchangeVendor(tc.method, tc.path); got != tc.want {
			t.Errorf("federationExchangeVendor(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestOpenAIExchangeIsCountedAgainstTheOpenAIVendor(t *testing.T) {
	before := metrics.AIFederationExchangesValue(metrics.FederationVendorOpenAI, metrics.FederationExchangeOK)
	anthropicBefore := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeOK)

	f := newExchangeServer(t)
	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          validOpenAIIdentityToken(t),
		TokenEndpoint:      f.srv.URL + "/oauth/token",
	}
	if _, _, err := newOpenAIExchanger(fed, guardedHTTPClient(nil)).Bearer(context.Background()); err != nil {
		t.Fatalf("Bearer: %v", err)
	}

	if after := metrics.AIFederationExchangesValue(metrics.FederationVendorOpenAI, metrics.FederationExchangeOK); after != before+1 {
		t.Errorf("openai ok counter went %v -> %v, want +1", before, after)
	}
	// The negative half: it must not have been attributed to Anthropic.
	if after := metrics.AIFederationExchangesValue(metrics.FederationVendorAnthropic, metrics.FederationExchangeOK); after != anthropicBefore {
		t.Errorf("an OpenAI exchange moved Anthropic's counter %v -> %v", anthropicBefore, after)
	}
}

// --- what an API answer means for the sign-in ------------------------------

// openaiClientAgainst builds a provider the way boot does and returns its SDK
// client and the process's sign-in behind it.
func openaiClientAgainst(t *testing.T, f *exchangeServer) (*openai.Client, *vendorSignIn) {
	t.Helper()
	provider, err := newOpenAIProvider(openaiFederatedConfig(t, f))
	if err != nil {
		t.Fatalf("newOpenAIProvider: %v", err)
	}
	client, ok := openAIClientOf(provider)
	if !ok {
		t.Fatal("the provider carries no OpenAI client")
	}
	signIn, ok := currentVendorSignIn(metrics.FederationVendorOpenAI)
	if !ok {
		t.Fatal("constructing a federated provider registered no OpenAI sign-in")
	}
	return client, signIn
}

func chatOnce(client *openai.Client) error {
	_, err := client.Chat.Completions.New(context.Background(), buildMinimalChatParams(), option.WithMaxRetries(0))
	return err
}

// OpenAI answers 401 for more than a bearer it no longer accepts: a service
// account that lacks one endpoint's permission gets a 401 on that endpoint and
// is served everywhere else. The middleware used to drop the process's bearer
// on ANY 401 (added in part 1 of this work, with no test), so one such call
// signed every OpenAI consumer on the node out, and the identity token that
// bought the bearer could not buy another for five seconds at best. The bearer
// is kept until it expires, as it was before.
func TestOpenAIKeepsItsBearerThroughA401(t *testing.T) {
	f := newExchangeServer(t)
	client, _ := openaiClientAgainst(t, f)

	if err := chatOnce(client); err != nil {
		t.Fatalf("first call: %v", err)
	}
	f.failNextAPICall(http.StatusUnauthorized,
		`{"error":{"message":"You have insufficient permissions for this operation. Missing scopes: model.request","type":"invalid_request_error"}}`)
	if err := chatOnce(client); err == nil {
		t.Fatal("the vendor's 401 reached the caller as success")
	}
	if err := chatOnce(client); err != nil {
		t.Fatalf("the call after a 401: %v -- one refused request signed the whole process out of OpenAI", err)
	}

	if n := len(f.exchanges()); n != 1 {
		t.Fatalf("sign-ins = %d, want 1: a 401 on one API call is not a reason to sign in again", n)
	}
	sent := f.apiAuthorizations()
	if len(sent) != 3 {
		t.Fatalf("%d API calls reached the vendor, want 3", len(sent))
	}
	for i, got := range sent {
		if got != "Bearer sk-federated-1" {
			t.Fatalf("call %d carried Authorization %q, want the one federated bearer", i, got)
		}
	}
}

// A served call proves a bearer as well as a model list does, and it is the
// only proof a bearer gets when a renewal bought it, long after the start.
func TestAServedOpenAICallProvesTheBearer(t *testing.T) {
	f := newExchangeServer(t)
	client, signIn := openaiClientAgainst(t, f)

	if err := chatOnce(client); err != nil {
		t.Fatalf("the call: %v", err)
	}
	rec, ok := signIn.Record()
	if !ok || rec.Outcome != signInOutcomeOK {
		t.Fatalf("record = %+v (found %v), want an accepted sign-in", rec, ok)
	}
	if rec.Proof != signInProofOK {
		t.Fatalf("proof = %q after the vendor served a call with the bearer, want %q: the check command "+
			"would go on saying this vendor is not verified", rec.Proof, signInProofOK)
	}
}

// A refused call proves nothing, and must not be written down as proof.
func TestARefusedOpenAICallIsNotProof(t *testing.T) {
	f := newExchangeServer(t)
	client, signIn := openaiClientAgainst(t, f)

	f.failNextAPICall(http.StatusForbidden, `{"error":{"message":"Country, region, or territory not supported"}}`)
	if err := chatOnce(client); err == nil {
		t.Fatal("the vendor's 403 reached the caller as success")
	}
	if rec, _ := signIn.Record(); rec.Proof == signInProofOK {
		t.Fatal("a call the vendor refused was recorded as proof that the bearer works")
	}
}

// 408, 425 and 429 are 4xx statuses and are not refusals: the vendor asked to
// be asked again. They were recorded as the vendor refusing the configuration,
// which is never retried without a person, so a sign-in that met one rate limit
// left the node without the vendor until its identity token was replaced.
func TestAnOpenAISignInToldToComeBackIsTriedAgain(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusTooEarly} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newExchangeServer(t)
			f.deny(status, `{"error":{"message":"Rate limit reached for requests"}}`)
			fed := openaiFederation{
				IdentityProviderID: "idp_test",
				ServiceAccountID:   "svc_test",
				TokenFile:          validOpenAIIdentityToken(t),
				TokenEndpoint:      f.srv.URL + "/oauth/token",
			}
			var clockMu sync.Mutex
			now := time.Now()
			clock := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}
			s := newVendorSignIn(openaiSignInConfig(fed, guardedHTTPClient(nil)), clock, now)

			deniedBefore := metrics.AIFederationExchangesValue(metrics.FederationVendorOpenAI, metrics.FederationExchangeDenied)
			if _, _, err := s.Bearer(context.Background()); err == nil {
				t.Fatal("a sign-in the vendor did not serve handed out a bearer")
			}
			rec, _ := s.Record()
			if rec.Outcome != signInOutcomeError {
				t.Fatalf("outcome = %q, want %q: status %d asks the engine to come back, it says nothing "+
					"about the configuration", rec.Outcome, signInOutcomeError, status)
			}
			if got := metrics.AIFederationExchangesValue(metrics.FederationVendorOpenAI, metrics.FederationExchangeDenied); got != deniedBefore {
				t.Fatalf("the denial counter moved %v -> %v on status %d: operators alert on that counter, "+
					"and a rate limit is not a denial", deniedBefore, got, status)
			}

			// The vendor recovers. The same identity token is presented again once
			// the pause has passed, with nobody asking.
			f.deny(http.StatusOK, "")
			clockMu.Lock()
			now = now.Add(vendorSignInRetryBackoff + time.Second)
			clockMu.Unlock()
			bearer, _, err := s.Bearer(context.Background())
			if err != nil || bearer != "sk-federated-1" {
				t.Fatalf("after the pause: bearer %q, err %v -- the sign-in was never tried again", bearer, err)
			}
			if n := len(f.exchanges()); n != 2 {
				t.Fatalf("sign-ins = %d, want 2", n)
			}
		})
	}
}

// --- the middleware --------------------------------------------------------

// staticBearer hands out whatever it is told to, so the middleware can be
// tested without an exchange.
type staticBearer struct {
	mu    sync.Mutex
	token string
	calls int
}

func (s *staticBearer) Bearer(context.Context) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.token, time.Now().Add(time.Hour), nil
}

func (s *staticBearer) set(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func TestOpenAIMiddlewareSetsTheCurrentBearerOnEveryRequest(t *testing.T) {
	// THE POINT OF THE MIDDLEWARE. A header fixed at construction serves the
	// first bearer for the life of the process, and the failure arrives an hour
	// later as 401s on every node at once.
	isolateLLMGuard(t)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = append(seen, req.Header.Get("Authorization"))
		writeJSON(w, chatCompletionResponse())
	}))
	t.Cleanup(srv.Close)

	source := &staticBearer{token: "first"}
	cfg := ProviderConfig{
		Name: "openaiTest", Type: "OpenAI", Model: "gpt-5.4-mini",
		Auth: map[string]string{"baseURL": srv.URL + "/v1"},
	}
	client := newTestOpenAIClientWithBearer(cfg, source)

	if _, err := client.Chat.Completions.New(context.Background(), buildMinimalChatParams()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	source.set("second")
	if _, err := client.Chat.Completions.New(context.Background(), buildMinimalChatParams()); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected two requests, saw %d", len(seen))
	}
	if seen[0] != "Bearer first" {
		t.Errorf("first request carried %q", seen[0])
	}
	if seen[1] != "Bearer second" {
		t.Errorf("second request carried %q -- a stale bearer survived a rotation", seen[1])
	}
	if source.calls != 2 {
		t.Errorf("the bearer source was consulted %d times for 2 requests", source.calls)
	}
}

// newTestOpenAIClientWithBearer builds a client the way openaiCredential does
// on the federated path -- the guarded transport, the bearer middleware, and
// the ambient-credential deletion -- without needing a token file or an
// exchange server. It is the middleware under test, not the switch.
func newTestOpenAIClientWithBearer(cfg ProviderConfig, source BearerSource) openai.Client {
	opts := []option.RequestOption{
		option.WithHTTPClient(guardedHTTPClient(nil)),
		option.WithMiddleware(openaiBearerMiddleware(source)),
		option.WithHeaderDel("Authorization"),
	}
	if baseURL := strings.TrimSpace(cfg.Auth["baseURL"]); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return openai.NewClient(opts...)
}

func buildMinimalChatParams() openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model:    openai.ChatModel("gpt-5.4-mini"),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
	}
}

// TestOpenAIExchangeIsPerformedOnceUnderConcurrentCallers backs the claim the
// sign-in's single flight makes.
//
// Every engine node runs one sign-in behind a dozen providers, so a process
// starting under load means many goroutines arrive at once with no bearer. The
// first starts the one exchange and the rest wait for it (ai_vendor_signin.go,
// Bearer); an exchange per caller would present one identity token many times,
// against a vendor that rate-limits the endpoint.
//
// This is not a probabilistic race test: the assertion is an EXACT count of
// requests the server saw, which is 1 whatever order the goroutines run in. A
// broken single flight gives some number greater than 1 rather than a
// timing-dependent flake.
func TestOpenAIExchangeIsPerformedOnceUnderConcurrentCallers(t *testing.T) {
	f := newExchangeServer(t)
	fed := openaiFederation{
		IdentityProviderID: "idp_test",
		ServiceAccountID:   "svc_test",
		TokenFile:          validOpenAIIdentityToken(t),
		TokenEndpoint:      f.srv.URL + "/oauth/token",
	}
	exchanger := newOpenAIExchanger(fed, guardedHTTPClient(nil))

	const callers = 24
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})

	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			token, _, err := exchanger.Bearer(context.Background())
			tokens[i], errs[i] = token, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if tokens[i] != "sk-federated-1" {
			t.Errorf("caller %d got %q, want the one exchanged bearer", i, tokens[i])
		}
	}
	if n := len(f.exchanges()); n != 1 {
		t.Errorf("%d callers caused %d token exchanges, want exactly 1 -- the lock is not held "+
			"across the exchange, so each caller performed its own", callers, n)
	}
}

// TestVerifyProviderCredentialMakesTheLiveCallForOpenAI covers the arm
// providerVerify gained in this epic.
//
// It was an honest REFUSAL before -- "a well-formed key and a reachable base
// URL is not the same as an accepted one" -- and that refusal is obsolete for a
// better reason than being inconvenient: a federated OpenAI provider that
// constructs has a credential OpenAI itself must accept, and models.list is
// what turns "must" into observed.
//
// The second half is the one that matters. A provider TYPE this function
// recognises whose entry carries no reachable client must REFUSE rather than
// report success -- several OpenAI types are placeholders with no HTTP client
// behind them, and reporting those as verified would be the page claiming an
// observation nobody made.
func TestVerifyProviderCredentialMakesTheLiveCallForOpenAI(t *testing.T) {
	t.Run("a constructed provider is verified live", func(t *testing.T) {
		var listed int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasSuffix(req.URL.Path, "/oauth/token") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
				return
			}
			listed++
			writeJSON(w, map[string]any{
				"object": "list",
				"data": []any{
					map[string]any{"id": "gpt-5.4-mini", "object": "model"},
					map[string]any{"id": "gpt-5.4", "object": "model"},
				},
			})
		}))
		t.Cleanup(srv.Close)

		auth := openaiFederatedAuth(validOpenAIIdentityToken(t))
		auth[authKeyOpenAITokenEndpoint] = srv.URL + "/oauth/token"
		auth["baseURL"] = srv.URL + "/v1"
		cfg := ProviderConfig{Name: "chatTest", Type: "OpenAI", Model: "gpt-5.4-mini", Auth: auth}

		provider, err := newOpenAIProvider(cfg)
		if err != nil {
			t.Fatalf("newOpenAIProvider: %v", err)
		}

		count, err := verifyProviderCredential(context.Background(),
			&ProviderConfigEntry{Config: cfg, Client: provider, Available: true})
		if err != nil {
			t.Fatalf("verifyProviderCredential: %v", err)
		}
		if count != 2 {
			t.Errorf("models listed = %d, want 2", count)
		}
		if listed != 1 {
			t.Errorf("the models endpoint was called %d times, want 1", listed)
		}
	})

	// THE PLACEHOLDER SUBTEST IS DELETED WITH ITS SUBJECT (epic memql#5137, D3).
	//
	// It asserted that a provider carrying no HTTP client refuses verification
	// rather than reporting success, using `&openAIPlaceholderProvider{...}` as
	// the clientless provider. There is no clientless provider any more: the
	// eleven placeholder records and the type behind them are gone, because a
	// registered, AVAILABLE provider that answers "the Go client is not wired
	// yet" to every call is worse than an absent one -- a policy can name it, a
	// page lists it, and the failure arrives three layers from the record that
	// promised the capability.
	//
	// The property is not abandoned so much as made unreachable. If a provider
	// type ever again has no client behind it, this subtest is the shape to
	// restore, against that type.
}
