package shopify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// connect_callback_test.go -- Connect Shopify's half of the identity node's
// callback (design 12.6 steps 4, 5, 7-10; 12.7), against the recording engine
// and a fake Shopify token endpoint. connect_callback_db_test.go holds the
// person re-check against the real tier and capability catalog.

// The compile-time half of 12.7: the connector IS the identity server's hook.
var _ componentIdentity.ShopifyConnect = (*Connector)(nil)

const (
	callbackCode        = "the-shopify-code"
	callbackAdminToken  = "shpat_admin_token_value"
	callbackPendingName = "SHOPIFY_ACME-WIDGETS_PENDING_CLIENT_SECRET"
	callbackWebhookName = "SHOPIFY_ACME-WIDGETS_WEBHOOK_SECRET"
)

// signedQuery is a callback query Shopify signed with secret.
func signedQuery(secret string, params map[string]string) url.Values {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(q.Encode()))
	q.Set("hmac", hex.EncodeToString(mac.Sum(nil)))
	return q
}

func callbackParams() map[string]string {
	return map[string]string{"code": callbackCode, "host": "YWRtaW4uc2hvcGlmeS5jb20vc3RvcmUvYWNtZQ", "shop": connectShop, "state": "st", "timestamp": "1337178173"}
}

// callbackState is the row shopifyConnectBegin writes for the developer.
func callbackState(source string) *componentIdentity.GithubConnectStateRow {
	now := time.Now().UTC()
	return &componentIdentity.GithubConnectStateRow{
		ID: "v1:identity:githubConnectState:s", UserId: connectDev, Purpose: githubconnect.PurposeShopifyConnect,
		ShopDomain: connectShop, SiteID: "s1", ClientID: connectClientID, CredentialSource: source,
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
}

// fakeTokenEndpoint is Shopify's /admin/oauth/access_token.
type fakeTokenEndpoint struct {
	server *httptest.Server
	status int
	body   string

	mu    stdsync.Mutex
	hits  int
	forms []url.Values
	raw   []string
}

func newFakeTokenEndpoint(t *testing.T) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{status: http.StatusOK,
		body: `{"access_token":"` + callbackAdminToken + `","scope":"` + strings.Join(ConnectScopes(), ",") + `"}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.hits++
		f.forms = append(f.forms, r.PostForm)
		f.raw = append(f.raw, r.Method+" "+r.URL.String()+" "+r.Header.Get("Content-Type"))
		status, body := f.status, f.body
		f.mu.Unlock()
		if status == http.StatusTemporaryRedirect {
			http.Redirect(w, r, "/elsewhere", status)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTokenEndpoint) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// callbackHarness is the connect harness with a secret resolver, a fake token
// endpoint and a developer's user row.
type callbackHarness struct {
	*connectHarness
	secrets map[string]string
	token   *fakeTokenEndpoint
	shops   []string
}

func newCallbackHarness(t *testing.T) *callbackHarness {
	t.Helper()
	h := &callbackHarness{connectHarness: newConnectHarness(t), secrets: map[string]string{callbackPendingName: connectSecret}}
	h.token = newFakeTokenEndpoint(t)
	conn := h.integ.connector
	conn.stores.secrets = func(_ context.Context, name string) (string, error) {
		if v, ok := h.secrets[name]; ok {
			return v, nil
		}
		return "", errors.New("not found")
	}
	conn.connectTokenURL = func(shop string) string {
		h.shops = append(h.shops, shop)
		return h.token.server.URL + "/admin/oauth/access_token"
	}
	h.user(auth.RoleDeveloper, true)
	return h
}

func (h *callbackHarness) user(role auth.Role, active bool) {
	if !active {
		// userByIdSystem filters on isActiveRecord: an inactive row reads as none.
		h.engine.setRows("userByIdSystem", nil)
		return
	}
	h.engine.setRows("userByIdSystem", []map[string]any{{"id": "v1:identity:user:" + connectDev, "role": string(role), "active": true, "primaryEmail": "dev@example.test", "displayName": "Dev"}})
}

// ---------------------------------------------------------------------------
// Step 4: the signature
// ---------------------------------------------------------------------------

// TestTheCallbackSignatureMatchesShopifysWorkedExample is a known answer from
// outside this repository: the worked example in Shopify's OAuth guide (secret
// "hush"). A signing rule written and tested only against itself would agree
// with itself whatever it did.
func TestTheCallbackSignatureMatchesShopifysWorkedExample(t *testing.T) {
	q := url.Values{}
	q.Set("code", "0907a61c0c8d55e99db179b68161bc00")
	q.Set("shop", "some-shop.myshopify.com")
	q.Set("state", "0.6784241404160823")
	q.Set("timestamp", "1337178173")
	q.Set("hmac", "700e2dadb827fcc8609e9d5ce208b2e9cdaab9df07390d2cbca10d7c328fc4bf")
	if !validCallbackHMAC(q, "hush") {
		t.Fatal("Shopify's own example does not verify")
	}
	for name, edit := range map[string]func(url.Values){
		"another secret":   func(v url.Values) {},
		"a changed value":  func(v url.Values) { v.Set("timestamp", "1337178174") },
		"an added value":   func(v url.Values) { v.Set("host", "x") },
		"no hmac":          func(v url.Values) { v.Del("hmac") },
		"an hmac not hex":  func(v url.Values) { v.Set("hmac", "zz"+v.Get("hmac")[2:]) },
		"a truncated hmac": func(v url.Values) { v.Set("hmac", v.Get("hmac")[:32]) },
	} {
		c := url.Values{}
		for k, vs := range q {
			c[k] = append([]string(nil), vs...)
		}
		edit(c)
		secret := "hush"
		if name == "another secret" {
			secret = "hush2"
		}
		if validCallbackHMAC(c, secret) {
			t.Errorf("%s verifies", name)
		}
	}
}

func TestVerifyUsesTheSecretTheStateNamesAndItsShop(t *testing.T) {
	ctx := context.Background()

	t.Run("pending", func(t *testing.T) {
		h := newCallbackHarness(t)
		c := h.integ.connector
		if !c.VerifyShopifyCallback(ctx, callbackState(credentialSourcePending), signedQuery(connectSecret, callbackParams())) {
			t.Fatal("a callback Shopify signed with the pending secret does not verify")
		}
		// Signed with the store's CURRENT secret while the state names the
		// pending one: not this Connect's app.
		h.secrets[callbackWebhookName] = "the-current-secret"
		h.store(map[string]any{"appClientId": "old", "webhookSecretRef": callbackWebhookName})
		if c.VerifyShopifyCallback(ctx, callbackState(credentialSourcePending), signedQuery("the-current-secret", callbackParams())) {
			t.Error("a pending state verified under the current secret")
		}
	})

	t.Run("current", func(t *testing.T) {
		h := newCallbackHarness(t)
		c := h.integ.connector
		h.secrets[callbackWebhookName] = "the-current-secret"
		h.store(map[string]any{"appClientId": connectClientID, "webhookSecretRef": callbackWebhookName, "adminTokenRef": "A"})
		if !c.VerifyShopifyCallback(ctx, callbackState(credentialSourceCurrent), signedQuery("the-current-secret", callbackParams())) {
			t.Fatal("a callback signed with the store's current secret does not verify")
		}
		if c.VerifyShopifyCallback(ctx, callbackState(credentialSourceCurrent), signedQuery(connectSecret, callbackParams())) {
			t.Error("a current state verified under the pending secret")
		}
		// The store read is by id, as the deployment, never the registry.
		for _, call := range h.engine.callsNamed("storeById") {
			if !call.internal || call.role != auth.RoleOwner {
				t.Errorf("the store was read as %+v, want the deployment", call)
			}
		}
		if len(h.engine.callsNamed("stores")) != 0 {
			t.Error("the verify read the StoreRegistry")
		}
	})

	t.Run("step 5: the shop", func(t *testing.T) {
		h := newCallbackHarness(t)
		c := h.integ.connector
		for _, shop := range []string{"other-shop.myshopify.com", "ACME-WIDGETS.myshopify.com.evil.example", "https://" + connectShop, ""} {
			p := callbackParams()
			p["shop"] = shop
			if c.VerifyShopifyCallback(ctx, callbackState(credentialSourcePending), signedQuery(connectSecret, p)) {
				t.Errorf("shop %q verified for a state begun for %s -- a signed request for another shop is not this Connect", shop, connectShop)
			}
		}
		// The shop is normalized before it is compared, as the state's was.
		p := callbackParams()
		p["shop"] = "ACME-Widgets.myshopify.com"
		if !c.VerifyShopifyCallback(ctx, callbackState(credentialSourcePending), signedQuery(connectSecret, p)) {
			t.Error("the state's own shop in another case did not verify")
		}
	})

	t.Run("nothing to verify with", func(t *testing.T) {
		h := newCallbackHarness(t)
		c := h.integ.connector
		delete(h.secrets, callbackPendingName)
		if c.VerifyShopifyCallback(ctx, callbackState(credentialSourcePending), signedQuery(connectSecret, callbackParams())) {
			t.Error("verified with no secret")
		}
		if c.VerifyShopifyCallback(ctx, callbackState("forged"), signedQuery(connectSecret, callbackParams())) {
			t.Error("a credentialSource no begin writes verified")
		}
		if len(h.engine.writes()) != 0 || h.token.count() != 0 {
			t.Errorf("the verify wrote %v or reached Shopify", h.engine.writes())
		}
	})
}

// ---------------------------------------------------------------------------
// Step 9: the exchange
// ---------------------------------------------------------------------------

func TestTheExchangeSendsTheSecretInTheBodyAndKeepsTheBodyOutOfErrors(t *testing.T) {
	h := newCallbackHarness(t)
	c := h.integ.connector
	ctx := context.Background()

	token, scopes, err := c.exchangeConnectCode(ctx, connectShop, connectClientID, connectSecret, callbackCode)
	if err != nil || token != callbackAdminToken || len(scopes) != len(ConnectScopes()) {
		t.Fatalf("exchange: token=%q scopes=%v err=%v", token, scopes, err)
	}
	form := h.token.forms[0]
	if form.Get("client_id") != connectClientID || form.Get("client_secret") != connectSecret || form.Get("code") != callbackCode {
		t.Errorf("the exchange body is %v", form)
	}
	if raw := h.token.raw[0]; raw != "POST /admin/oauth/access_token application/x-www-form-urlencoded" {
		t.Errorf("the exchange was %q: a POST with a form body and nothing in the URL", raw)
	}
	if len(h.shops) != 1 || h.shops[0] != connectShop {
		t.Errorf("the exchange host came from %v, want the state's shop", h.shops)
	}

	for name, arrange := range map[string]func(f *fakeTokenEndpoint){
		"a refusal echoing the secret": func(f *fakeTokenEndpoint) {
			f.status, f.body = http.StatusBadRequest, `{"error":"invalid_request","echo":"`+connectSecret+`"}`
		},
		"a 200 with no token": func(f *fakeTokenEndpoint) { f.body = `{"scope":"read_products","error":"` + connectSecret + `"}` },
		"not JSON":            func(f *fakeTokenEndpoint) { f.body = "<html>" + connectSecret + "</html>" },
		"over a mebibyte": func(f *fakeTokenEndpoint) {
			f.body = `{"access_token":"` + callbackAdminToken + `","scope":"` + strings.Repeat("a", 1<<20) + `"}`
		},
		"a redirect": func(f *fakeTokenEndpoint) { f.status = http.StatusTemporaryRedirect },
	} {
		f := h.token
		f.mu.Lock()
		f.status, f.body = http.StatusOK, ""
		f.mu.Unlock()
		arrange(f)
		before := f.count()
		_, _, err := c.exchangeConnectCode(ctx, connectShop, connectClientID, connectSecret, callbackCode)
		if err == nil {
			t.Errorf("%s: the exchange succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), connectSecret) || strings.Contains(err.Error(), callbackAdminToken) || strings.Contains(err.Error(), callbackCode) {
			t.Errorf("%s: the error carries a credential: %v", name, err)
		}
		if name == "a redirect" && f.count() != before+1 {
			t.Errorf("a redirect was followed (%d requests), re-sending the secret to another location", f.count()-before)
		}
	}

	if _, _, err := c.exchangeConnectCode(ctx, connectShop, connectClientID, connectSecret, ""); err == nil {
		t.Error("an empty code was exchanged")
	}
}

// ---------------------------------------------------------------------------
// Step 10: the scopes
// ---------------------------------------------------------------------------

func TestOnlyAMissingStorefrontScopeRefuses(t *testing.T) {
	if got := missingStorefrontScopes(ConnectScopes()); len(got) != 0 {
		t.Errorf("the whole request reads as missing %v", got)
	}
	// A write scope implies its read, as Shopify's own client reads it.
	implied := []string{"unauthenticated_read_product_listings", "unauthenticated_write_checkouts", "unauthenticated_read_customers"}
	if got := missingStorefrontScopes(implied); len(got) != 0 {
		t.Errorf("an implied read reads as missing %v", got)
	}
	if got := missingStorefrontScopes(StorefrontScopes[1:]); len(got) != 1 || got[0] != StorefrontScopes[0] {
		t.Errorf("missing = %v, want %s", got, StorefrontScopes[0])
	}
	// Admin read scopes are shown, never refused.
	if got := missingStorefrontScopes(StorefrontScopes); len(got) != 0 {
		t.Errorf("missing admin scopes refused: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Steps 7-10, in order, and nothing written on the way
// ---------------------------------------------------------------------------

func TestAuthorizeReachesShopifyOnlyWhenEveryCheckHolds(t *testing.T) {
	ctx := context.Background()

	h := newCallbackHarness(t)
	grant, result := h.integ.connector.AuthorizeShopifyConnect(ctx, callbackState(credentialSourcePending), callbackCode)
	if result != "" {
		t.Fatalf("a developer's own storefront refused %q", result)
	}
	if grant.StoreID != connectStoreID || grant.AccessToken != callbackAdminToken || grant.Role != string(auth.RoleDeveloper) || len(grant.Scopes) != len(ConnectScopes()) {
		t.Fatalf("grant = %v", grant)
	}
	if h.token.count() != 1 || len(h.engine.writes()) != 0 {
		t.Fatalf("exchanges=%d writes=%v; authorize exchanges once and writes nothing", h.token.count(), h.engine.writes())
	}
	// Step 8 judged the PERSON: the site read under their real role, never
	// borrowed, never internal.
	var asPerson bool
	for _, call := range h.engine.callsNamed("siteById") {
		if call.userId == connectDev && call.role == auth.RoleDeveloper && !call.internal {
			asPerson = true
		}
	}
	if !asPerson {
		t.Errorf("the site was never read as the person under their role: %+v", h.engine.callsNamed("siteById"))
	}

	for _, tc := range []struct {
		name    string
		arrange func(h *callbackHarness)
		state   func(st *componentIdentity.GithubConnectStateRow)
		want    string
	}{
		{name: "the site is no longer a storefront", arrange: func(h *callbackHarness) { h.site(connectSiteID, "static") }, want: connectReasonStateInvalid},
		{name: "the site is gone", arrange: func(h *callbackHarness) { h.engine.setRows("siteById", nil) }, want: connectReasonStateInvalid},
		{name: "the site now publishes another shop", arrange: func(h *callbackHarness) { h.run(connectSiteID, "other-shop.myshopify.com") }, want: connectReasonStateInvalid},
		{name: "the store was purged", arrange: func(h *callbackHarness) { h.store(map[string]any{"redactedAt": "2026-09-01T00:00:00Z"}) }, want: connectReasonStateInvalid},
		{name: "the person is gone or inactive", arrange: func(h *callbackHarness) { h.user(auth.RoleDeveloper, false) }, want: connectReasonPermissionLost},
		{name: "the person is a writer now", arrange: func(h *callbackHarness) { h.user(auth.RoleWriter, true) }, want: connectReasonPermissionLost},
		{name: "the person is an admin now", arrange: func(h *callbackHarness) { h.user(auth.RoleAdmin, true) }, want: connectReasonPermissionLost},
		{name: "the person can no longer write the site", arrange: func(h *callbackHarness) { h.engine.denyWriter = connectDev }, want: connectReasonPermissionLost},
		{name: "the state names nobody", state: func(st *componentIdentity.GithubConnectStateRow) { st.UserId = "" }, want: connectReasonPermissionLost},
		{name: "Shopify refuses the code", arrange: func(h *callbackHarness) { h.token.status = http.StatusBadRequest }, want: connectReasonExchangeFailed},
		{name: "the pending secret is gone", arrange: func(h *callbackHarness) { delete(h.secrets, callbackPendingName) }, want: connectReasonExchangeFailed},
		{name: "a storefront scope was not granted", arrange: func(h *callbackHarness) {
			h.token.body = `{"access_token":"` + callbackAdminToken + `","scope":"` + strings.Join(StorefrontScopes[1:], ",") + `"}`
		}, want: connectReasonScopesMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCallbackHarness(t)
			if tc.arrange != nil {
				tc.arrange(h)
			}
			st := callbackState(credentialSourcePending)
			if tc.state != nil {
				tc.state(st)
			}
			grant, result := h.integ.connector.AuthorizeShopifyConnect(ctx, st, callbackCode)
			if result != tc.want {
				t.Fatalf("result %q, want %q", result, tc.want)
			}
			if grant.AccessToken != "" {
				t.Error("a refused authorize handed the writes a token")
			}
			if len(h.engine.writes()) != 0 {
				t.Errorf("a refused authorize wrote %v", h.engine.writes())
			}
			exchanged := h.token.count()
			if tc.want != connectReasonExchangeFailed && tc.want != connectReasonScopesMissing && exchanged != 0 {
				t.Errorf("the code reached Shopify (%d) although step %s refused first", exchanged, tc.want)
			}
			if strings.Contains(h.logs.String(), connectSecret) || strings.Contains(h.logs.String(), callbackAdminToken) || strings.Contains(h.logs.String(), callbackCode) {
				t.Errorf("a credential reached the log:\n%s", h.logs.String())
			}
		})
	}
}
