package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// shopify_callback_test.go -- GET /auth/shopify/callback's own half (design
// record 2026-09-23-connect-shopify, 12.6 steps 1-3, 6, 16, 17): the transport,
// the install landing, the state lookup that spends nothing, the spend, and the
// ORDER the Shopify half is asked in.
//
// The Shopify half is a fake here and a real one in
// integrations/shopify/connect_callback_test.go: what this file holds is that
// the handler never asks the next step when one refuses, and that nothing but
// the one spend reaches the engine. The engine is githubFakeEngine, which
// models the state lookup, the consume and the cluster-settings read, and
// records anything else in `unknown`.

const shopifyTestState = "the-plaintext-shopify-state"

// fakeShopifyHook is the Shopify half, recording the order it was asked in.
type fakeShopifyHook struct {
	mu    sync.Mutex
	calls []string

	verify      bool
	onVerify    func()
	authResult  string
	grant       identity.ShopifyConnectGrant
	writeResult string
	// writeKept is what the writes report they kept; blank follows a
	// connected or reconnected writeResult.
	writeKept string

	gotQuery url.Values
	gotCode  string
}

func (h *fakeShopifyHook) VerifyShopifyCallback(_ context.Context, _ *identity.GithubConnectStateRow, q url.Values) bool {
	h.mu.Lock()
	h.calls = append(h.calls, "verify")
	h.gotQuery = q
	h.mu.Unlock()
	if h.onVerify != nil {
		h.onVerify()
	}
	return h.verify
}

func (h *fakeShopifyHook) AuthorizeShopifyConnect(_ context.Context, _ *identity.GithubConnectStateRow, code string) (identity.ShopifyConnectGrant, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "authorize")
	h.gotCode = code
	return h.grant, h.authResult
}

func (h *fakeShopifyHook) WriteShopifyConnect(_ context.Context, _ *identity.GithubConnectStateRow, _ identity.ShopifyConnectGrant) (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "write")
	kept := h.writeKept
	if kept == "" && (h.writeResult == "connected" || h.writeResult == "reconnected") {
		kept = h.writeResult
	}
	return h.writeResult, kept
}

func (h *fakeShopifyHook) order() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, ",")
}

func newShopifyCallbackServer(t *testing.T, eng *githubFakeEngine, hook identity.ShopifyConnect) (*Server, *githubAuditRecorder, *bytes.Buffer) {
	t.Helper()
	audit := &githubAuditRecorder{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Server{
		Cfg:            identity.Config{BaseURL: "https://identity.example.test"},
		Store:          &identity.Store{Engine: eng, Logger: logger, GithubGate: githubHTTPUnitGate},
		Audit:          audit,
		Logger:         logger,
		ShopifyConnect: hook,
	}, audit, logs
}

// shopifyLiveState is a Connect Shopify state begin would have written.
func shopifyLiveState() map[string]string {
	st := liveState()
	st["stateHash"] = identity.HashConnectState(shopifyTestState)
	st["purpose"] = githubconnect.PurposeShopifyConnect
	st["returnPath"] = "/?connect=deployables&open=store"
	st["shopDomain"] = "acme.myshopify.com"
	st["siteId"] = "site-1"
	st["clientId"] = "client-id-1"
	st["credentialSource"] = "pending"
	// Begun from the browser session githubFakeEngine's refresh lookup answers.
	st["sessionId"] = "browser-session"
	return st
}

func shopifyQuery() string {
	return "code=the-shopify-code&hmac=abcdef0123&host=YWRtaW4&shop=acme.myshopify.com&state=" + shopifyTestState + "&timestamp=1337178173"
}

func runShopify(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "https://identity.example.test"+githubconnect.ShopifyCallbackPath+"?"+query, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	// The browser that began: its refresh cookie names the state's session.
	r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "browser-refresh"})
	rec := httptest.NewRecorder()
	s.handleShopifyCallback(rec, r)
	return rec
}

// mutations is every write statement the handler issued. The one legitimate
// write on this side is the spend.
func (f *githubFakeEngine) mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, q := range f.statements {
		if strings.HasPrefix(strings.TrimSpace(q), "mutation ") && !fakeStateConsumeRe.MatchString(q) {
			out = append(out, q)
		}
	}
	return out
}

func assertShopifyRedirect(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q\nwant      %q", got, want)
	}
}

// -----------------------------------------------------------------------------
// Step 1 and step 2
// -----------------------------------------------------------------------------

func TestAPlaintextShopifyCallbackIsRefused(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true}
	s, audit, _ := newShopifyCallbackServer(t, eng, hook)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://identity.example.test"+githubconnect.ShopifyCallbackPath+"?"+shopifyQuery(), nil)
	s.handleShopifyCallback(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want requireSecureRequest's 403 over plaintext", rec.Code)
	}
	if len(eng.statements) != 0 || hook.order() != "" || len(audit.actions()) != 0 {
		t.Errorf("a plaintext callback reached something: statements=%v hook=%q audit=%v", eng.statements, hook.order(), audit.actions())
	}
}

// TestTheInstallLandingWritesNothing is Shopify opening the App URL: no code
// and no state, but everything else Shopify signs. Nothing is looked up, the
// Shopify half is never asked, nothing is audited, and no request value
// reaches the redirect.
func TestTheInstallLandingWritesNothing(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true}
	s, audit, logs := newShopifyCallbackServer(t, eng, hook)

	rec := runShopify(t, s, "hmac=abcdef0123&host=YWRtaW4&shop=evil-store.myshopify.com&timestamp=1337178173")
	assertNoUnknownConstructs(t, eng)

	assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&shopify=installed")
	if eng.consumes != 0 || len(eng.mutations()) != 0 || hook.order() != "" || len(audit.actions()) != 0 {
		t.Errorf("the landing did something: consumes=%d mutations=%v hook=%q audit=%v",
			eng.consumes, eng.mutations(), hook.order(), audit.actions())
	}
	for _, q := range eng.statements {
		if strings.Contains(q, "githubConnectStateByHash") {
			t.Errorf("the landing looked a state up: %s", q)
		}
	}
	if !strings.Contains(logs.String(), "evil-store.myshopify.com") || !strings.Contains(logs.String(), "shopUnverified") {
		t.Errorf("the landing did not log the shop as unverified:\n%s", logs.String())
	}
}

// -----------------------------------------------------------------------------
// Step 3: a state that cannot be finished spends nothing and asks nothing
// -----------------------------------------------------------------------------

func TestAShopifyStateThatCannotFinishIsRefusedAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(st map[string]string) map[string]string
		query  string
		reason string
	}{
		{"expired", func(st map[string]string) map[string]string {
			st["createdAt"] = time.Now().UTC().Add(-11 * time.Minute).Format(time.RFC3339)
			st["expiresAt"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
			return st
		}, "", "state_expired"},
		{"replayed", func(st map[string]string) map[string]string {
			st["consumedAt"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
			return st
		}, "", "state_already_consumed"},
		{"unknown", func(map[string]string) map[string]string { return nil }, "", "state_unknown"},
		{"a GitHub Connect state", func(st map[string]string) map[string]string {
			st["purpose"] = ""
			return st
		}, "", "state_unknown"},
		{"a GitHub App setup state", func(st map[string]string) map[string]string {
			st["purpose"] = githubconnect.PurposeAppSetup
			return st
		}, "", "state_unknown"},
		{"planted, with a lifetime no server writes", func(st map[string]string) map[string]string {
			st["expiresAt"] = "2099-01-01T00:00:00Z"
			return st
		}, "", "state_unknown"},
		{"no state at all, with a code", func(st map[string]string) map[string]string { return st },
			"code=the-shopify-code&shop=acme.myshopify.com", "state_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &githubFakeEngine{state: tc.edit(shopifyLiveState())}
			hook := &fakeShopifyHook{verify: true}
			s, audit, _ := newShopifyCallbackServer(t, eng, hook)

			query := tc.query
			if query == "" {
				query = shopifyQuery()
			}
			rec := runShopify(t, s, query)
			assertNoUnknownConstructs(t, eng)

			// No state was verified, so the redirect names no site.
			assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&shopify=connect_state_invalid")
			if eng.consumes != 0 || len(eng.mutations()) != 0 {
				t.Errorf("consumes=%d mutations=%v, want nothing spent and nothing written", eng.consumes, eng.mutations())
			}
			if got := hook.order(); got != "" {
				t.Errorf("the Shopify half was asked %q for a state that cannot finish", got)
			}
			acts := audit.actions()
			if len(acts) != 1 || acts[0] != "shopify_connect_refused" || audit.events[0].Detail["reason"] != tc.reason {
				t.Fatalf("audit = %v %v, want one shopify_connect_refused with reason %s", acts, audit.events, tc.reason)
			}
			if ev := audit.events[0]; ev.Category != identity.AuditCategoryConfiguration || ev.TargetType != "shopifyStore" || ev.Outcome != identity.AuditOutcomeFailure {
				t.Errorf("audit event %+v, want configuration / shopifyStore / failure", ev)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Step 3, continued: the browser that began (D16)
// -----------------------------------------------------------------------------

// TestACallbackFromAnotherBrowserSpendsNothing: the state is bound to the
// browser session that pressed Connect, as GitHub Connect's is. A callback that
// does not carry that live session -- a link opened in another browser, by
// another person, or after the session ended -- is connect_state_invalid, asks
// the Shopify half nothing and spends nothing, so the person who began can
// still finish.
func TestACallbackFromAnotherBrowserSpendsNothing(t *testing.T) {
	live := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name    string
		session map[string]string
		cookie  bool
	}{
		{name: "no session cookie"},
		{name: "another browser's session", cookie: true, session: map[string]string{"id": "another-session", "userId": "v1:identity:user:asked", "expiresAt": live}},
		{name: "another person's session", cookie: true, session: map[string]string{"id": "browser-session", "userId": "v1:identity:user:someone-else", "expiresAt": live}},
		{name: "a revoked session", cookie: true, session: map[string]string{"id": "browser-session", "userId": "v1:identity:user:asked", "expiresAt": live, "revokedAt": time.Now().UTC().Format(time.RFC3339)}},
		{name: "an expired session", cookie: true, session: map[string]string{"id": "browser-session", "userId": "v1:identity:user:asked", "expiresAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &githubFakeEngine{state: shopifyLiveState(), session: tc.session}
			hook := &fakeShopifyHook{verify: true, writeResult: "connected"}
			s, audit, _ := newShopifyCallbackServer(t, eng, hook)

			r := httptest.NewRequest("GET", "https://identity.example.test"+githubconnect.ShopifyCallbackPath+"?"+shopifyQuery(), nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			if tc.cookie {
				r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "another-browser-refresh"})
			}
			rec := httptest.NewRecorder()
			s.handleShopifyCallback(rec, r)
			assertNoUnknownConstructs(t, eng)

			// Not this browser's Connect, so the redirect names nothing of it.
			assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&shopify=connect_state_invalid")
			if eng.consumes != 0 || eng.state["consumedAt"] != "" || len(eng.mutations()) != 0 {
				t.Errorf("another browser spent or wrote: consumes=%d mutations=%v", eng.consumes, eng.mutations())
			}
			if got := hook.order(); got != "" {
				t.Errorf("the Shopify half was asked %q for another browser's callback", got)
			}
			if acts := audit.actions(); len(acts) != 1 || acts[0] != "shopify_connect_refused" || audit.events[0].Detail["reason"] != "session_invalid" {
				t.Errorf("audit = %v %v, want one refusal with reason session_invalid", acts, audit.events)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Steps 4-5 before step 6
// -----------------------------------------------------------------------------

// TestABadSignatureLeavesTheStateUnspent: Shopify's signature is checked
// BEFORE the spend, so a forged or mistyped callback cannot burn the Connect a
// person began.
func TestABadSignatureLeavesTheStateUnspent(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: false}
	s, audit, _ := newShopifyCallbackServer(t, eng, hook)

	rec := runShopify(t, s, shopifyQuery())
	assertNoUnknownConstructs(t, eng)

	assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&shopify=signature_invalid")
	if eng.consumes != 0 || eng.state["consumedAt"] != "" || len(eng.mutations()) != 0 {
		t.Errorf("a bad signature spent or wrote: consumes=%d consumedAt=%q mutations=%v", eng.consumes, eng.state["consumedAt"], eng.mutations())
	}
	if got := hook.order(); got != "verify" {
		t.Errorf("Shopify half asked %q, want verify alone", got)
	}
	if hook.gotQuery.Get("hmac") != "abcdef0123" || hook.gotQuery.Get("shop") != "acme.myshopify.com" {
		t.Errorf("the verify was not handed the request's query: %v", hook.gotQuery)
	}
	if acts := audit.actions(); len(acts) != 1 || audit.events[0].Detail["reason"] != "signature_invalid" {
		t.Errorf("audit = %v %v, want one refusal with reason signature_invalid", acts, audit.events)
	}
}

// TestASpendLostToAnotherReplicaAsksNothingMore: the state was live when looked
// up and spent by somebody else before this request's spend, which re-checks
// inside the lock.
func TestASpendLostToAnotherReplicaAsksNothingMore(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true}
	hook.onVerify = func() {
		eng.mu.Lock()
		eng.state["consumedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		eng.mu.Unlock()
	}
	s, audit, _ := newShopifyCallbackServer(t, eng, hook)

	rec := runShopify(t, s, shopifyQuery())
	assertNoUnknownConstructs(t, eng)

	assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&open=store&shopify=connect_state_invalid&site=site-1")
	if got := hook.order(); got != "verify" {
		t.Errorf("Shopify half asked %q after losing the spend, want verify alone", got)
	}
	if acts := audit.actions(); len(acts) != 1 || audit.events[0].Detail["reason"] != "state_already_consumed" {
		t.Errorf("audit = %v %v", acts, audit.events)
	}
}

// -----------------------------------------------------------------------------
// Steps 6-17: the order, and what a refusal after the spend does
// -----------------------------------------------------------------------------

func TestAVerifiedCallbackSpendsThenAuthorizesThenWrites(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true, grant: identity.ShopifyConnectGrant{StoreID: "acme"}, writeResult: "connected"}
	var consumedBeforeAuthorize bool
	s, audit, _ := newShopifyCallbackServer(t, eng, &orderProbe{fakeShopifyHook: hook, onAuthorize: func() {
		consumedBeforeAuthorize = eng.consumes == 1
	}})

	rec := runShopify(t, s, shopifyQuery())
	assertNoUnknownConstructs(t, eng)

	assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&open=store&shopify=connected&site=site-1")
	if got := hook.order(); got != "verify,authorize,write" {
		t.Fatalf("Shopify half asked %q, want verify,authorize,write", got)
	}
	if !consumedBeforeAuthorize {
		t.Error("the state was not spent before the code reached Shopify")
	}
	// Step 3's two reads -- the state, then the session that began it -- come
	// before the spend.
	at := func(prefix string) int {
		for i, q := range eng.statements {
			if strings.HasPrefix(q, prefix) {
				return i
			}
		}
		return -1
	}
	lookup, session, spend := at("query githubConnectStateByHash("), at("query authSessionByRefreshTokenHash("), at("mutation consumeGithubConnectState(")
	if lookup < 0 || session < lookup || spend < session {
		t.Errorf("statement order: state lookup %d, session %d, spend %d; want the session checked between them", lookup, session, spend)
	}
	if hook.gotCode != "the-shopify-code" {
		t.Errorf("authorize was handed code %q", hook.gotCode)
	}
	if len(eng.mutations()) != 0 {
		t.Errorf("the identity half wrote %v; its only write is the spend", eng.mutations())
	}
	acts := audit.actions()
	if len(acts) != 1 || acts[0] != "shopify_connected" || audit.events[0].TargetId != "acme" || audit.events[0].ActorUserId != "v1:identity:user:asked" {
		t.Fatalf("audit = %v %+v, want one shopify_connected on store acme by the state's person", acts, audit.events)
	}
	if audit.events[0].Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("outcome %q", audit.events[0].Outcome)
	}
}

func TestARefusalAfterTheSpendWritesNothing(t *testing.T) {
	for _, token := range []string{"connect_state_invalid", "permission_lost", "exchange_failed", "scopes_missing"} {
		t.Run(token, func(t *testing.T) {
			eng := &githubFakeEngine{state: shopifyLiveState()}
			hook := &fakeShopifyHook{verify: true, authResult: token, grant: identity.ShopifyConnectGrant{StoreID: "acme"}}
			s, audit, _ := newShopifyCallbackServer(t, eng, hook)

			rec := runShopify(t, s, shopifyQuery())
			assertNoUnknownConstructs(t, eng)

			assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&open=store&shopify="+token+"&site=site-1")
			if got := hook.order(); got != "verify,authorize" {
				t.Errorf("Shopify half asked %q, want the writes never reached", got)
			}
			if eng.consumes != 1 {
				t.Errorf("consumes = %d; a verified callback spends its state even when a later step refuses", eng.consumes)
			}
			acts := audit.actions()
			if len(acts) != 1 || acts[0] != "shopify_connect_refused" || audit.events[0].Detail["reason"] != token || audit.events[0].TargetId != "acme" {
				t.Errorf("audit = %v %+v", acts, audit.events)
			}
		})
	}
}

func TestAReconnectIsAuditedAsOne(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true, grant: identity.ShopifyConnectGrant{StoreID: "acme"}, writeResult: "reconnected"}
	s, audit, _ := newShopifyCallbackServer(t, eng, hook)
	runShopify(t, s, shopifyQuery())
	if acts := audit.actions(); len(acts) != 1 || acts[0] != "shopify_reconnected" {
		t.Errorf("audit = %v, want shopify_reconnected", acts)
	}
}

// TestACredentialChangeIsAuditedAsOneWhateverEndedItAfter: once the writes have
// kept the Admin token and the store row (steps 11-12), the store's credentials
// HAVE changed, and the trail says so as a success -- the step that could not
// finish after them rides detail.reason, and the person is still sent back with
// it. A refusal is only an outcome where nothing was kept.
func TestACredentialChangeIsAuditedAsOneWhateverEndedItAfter(t *testing.T) {
	for _, tc := range []struct {
		result, kept, action string
	}{
		{"storefront_token_failed", "connected", "shopify_connected"},
		{"permission_lost", "connected", "shopify_connected"},
		{"permission_lost", "reconnected", "shopify_reconnected"},
		{"exchange_failed", "", "shopify_connect_refused"},
	} {
		t.Run(tc.result+"/"+tc.kept, func(t *testing.T) {
			eng := &githubFakeEngine{state: shopifyLiveState()}
			hook := &fakeShopifyHook{verify: true, grant: identity.ShopifyConnectGrant{StoreID: "acme"}, writeResult: tc.result, writeKept: tc.kept}
			s, audit, _ := newShopifyCallbackServer(t, eng, hook)

			rec := runShopify(t, s, shopifyQuery())
			assertShopifyRedirect(t, rec, "https://os.example.test/?connect=deployables&open=store&shopify="+tc.result+"&site=site-1")
			acts := audit.actions()
			if len(acts) != 1 || acts[0] != tc.action {
				t.Fatalf("audit = %v, want one %s", acts, tc.action)
			}
			ev := audit.events[0]
			if ev.Detail["reason"] != tc.result || ev.TargetId != "acme" {
				t.Errorf("detail %v target %q, want reason %s on store acme", ev.Detail, ev.TargetId, tc.result)
			}
			wantOutcome := identity.AuditOutcomeSuccess
			if tc.kept == "" {
				wantOutcome = identity.AuditOutcomeFailure
			}
			if ev.Outcome != wantOutcome {
				t.Errorf("outcome %q, want %q", ev.Outcome, wantOutcome)
			}
		})
	}
}

// TestNoShopifyCallbackValueReachesAStatementALogOrTheRedirect: the code and the
// state are credentials. Neither may land anywhere but the Shopify half.
func TestNoShopifyCallbackValueReachesAStatementALogOrTheRedirect(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true, grant: identity.ShopifyConnectGrant{StoreID: "acme", AccessToken: "shpat_secret_token"}, writeResult: "connected"}
	s, audit, logs := newShopifyCallbackServer(t, eng, hook)

	rec := runShopify(t, s, shopifyQuery())
	if hook.gotCode != "the-shopify-code" {
		t.Fatal("the code never reached the Shopify half, so the scan below proves nothing")
	}
	auditJSON, err := json.Marshal(audit.events)
	if err != nil {
		t.Fatal(err)
	}
	corpus := strings.Join(eng.statements, "\n") + "\n" + string(auditJSON) + "\n" + logs.String() + "\n" + rec.Header().Get("Location")
	for name, value := range map[string]string{
		"the code": "the-shopify-code", "the plaintext state": shopifyTestState, "the Admin token": "shpat_secret_token", "the hmac": "abcdef0123",
	} {
		if strings.Contains(corpus, value) {
			t.Errorf("%s reached a statement, an audit row, a log line or the redirect", name)
		}
	}
	if s := (identity.ShopifyConnectGrant{AccessToken: "shpat_secret_token", ClientSecret: "shpss_client_secret"}).String(); strings.Contains(s, "shpat_secret_token") || strings.Contains(s, "shpss_client_secret") {
		t.Errorf("a grant prints a credential: %s", s)
	}
}

func TestTheShopifyCallbackIs404WithNoShopifyHalf(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	s, _, _ := newShopifyCallbackServer(t, eng, nil)
	rec := runShopify(t, s, shopifyQuery())
	if rec.Code != http.StatusNotFound || eng.consumes != 0 {
		t.Errorf("status = %d consumes = %d, want a 404 that spends nothing", rec.Code, eng.consumes)
	}
}

func TestTheShopifyCallbackIsMounted(t *testing.T) {
	mux := http.NewServeMux()
	eng := &githubFakeEngine{}
	s, _, _ := newShopifyCallbackServer(t, eng, &fakeShopifyHook{})
	s.Mount(mux)
	r := httptest.NewRequest("GET", "https://identity.example.test"+githubconnect.ShopifyCallbackPath, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET %s answered %d, want the landing's 303 (a 404 means the route is not registered)", githubconnect.ShopifyCallbackPath, rec.Code)
	}
}

// orderProbe runs a check at the moment authorize is asked.
type orderProbe struct {
	*fakeShopifyHook
	onAuthorize func()
}

func (p *orderProbe) AuthorizeShopifyConnect(ctx context.Context, st *identity.GithubConnectStateRow, code string) (identity.ShopifyConnectGrant, string) {
	p.onAuthorize()
	return p.fakeShopifyHook.AuthorizeShopifyConnect(ctx, st, code)
}
