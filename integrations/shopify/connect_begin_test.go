package shopify

import (
	"context"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
)

// connect_begin_test.go -- shopifyConnectBegin (design 12.4) against the
// recording engine: which credentials it picks, the state row it writes
// through component/identity's Store, and the authorize URL it answers.

const connectIdentityBase = "https://identity.lab.example.com"

// pendingApp makes the pending client id and secret rows exist, as a save
// leaves them.
func (h *connectHarness) pendingApp(clientID string) {
	h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeSecretName(connectStoreID, suffixPendingClientID)),
		[]map[string]any{{"id": "var-p", "name": "n", "value": clientID}})
	h.engine.setRows(namedRowsQuery(conceptGlobalSecret, storeSecretName(connectStoreID, suffixPendingClientSecret)),
		[]map[string]any{{"id": "sec-p", "name": "n", "encryptedValue": "ZW5j"}})
}

// currentApp makes the store row carry a live app and its webhook secret row
// exist.
func (h *connectHarness) currentApp(clientID string) {
	ref := storeSecretName(connectStoreID, suffixWebhookSecret)
	h.store(map[string]any{"appClientId": clientID, "webhookSecretRef": ref, "adminTokenRef": "A"})
	h.engine.setRows(namedRowsQuery(conceptGlobalSecret, ref), []map[string]any{{"id": "sec-w", "name": ref, "encryptedValue": "ZW5j"}})
}

func begin(t *testing.T, h *connectHarness, args map[string]any) map[string]any {
	t.Helper()
	t.Setenv("MEMQL_IDENTITY_BASE_URL", connectIdentityBase)
	return invoke(t, devCtx(), h.integ.handleConnectBegin, args)
}

var stateInURL = regexp.MustCompile(`[?&]state=([^&]+)`)

// wantAuthorizeURL is 12.4's URL, spelled out: the order is the spec's, every
// value is query-escaped, and there is no grant_options[] (an offline token).
func wantAuthorizeURL(clientID, state string) string {
	return "https://" + connectShop + "/admin/oauth/authorize" +
		"?client_id=" + url.QueryEscape(clientID) +
		"&scope=" + url.QueryEscape(strings.Join(ConnectScopes(), ",")) +
		"&redirect_uri=" + url.QueryEscape(connectIdentityBase+"/auth/shopify/callback") +
		"&state=" + url.QueryEscape(state)
}

func TestBeginWritesAShopifyStateAndAnswersTheAuthorizeURL(t *testing.T) {
	h := newConnectHarness(t)
	h.pendingApp(connectClientID)
	out := begin(t, h, map[string]any{"siteId": "s1", "returnPath": "/deployables?site=s1"})

	if keys := sortedKeys(out); !reflect.DeepEqual(keys, []string{"authorizeUrl", "reason"}) {
		t.Fatalf("begin answered %v, want exactly {authorizeUrl, reason}", keys)
	}
	if out["reason"] != connectReasonOK {
		t.Fatalf("reason = %v", out["reason"])
	}
	authorizeURL, _ := out["authorizeUrl"].(string)
	m := stateInURL.FindStringSubmatch(authorizeURL)
	if m == nil {
		t.Fatalf("the authorize URL carries no state: %s", authorizeURL)
	}
	state, err := url.QueryUnescape(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if authorizeURL != wantAuthorizeURL(connectClientID, state) {
		t.Fatalf("authorizeUrl =\n  %s\nwant\n  %s", authorizeURL, wantAuthorizeURL(connectClientID, state))
	}
	if strings.Contains(authorizeURL, "grant_options") {
		t.Fatalf("the URL asks for an online token: %s", authorizeURL)
	}
	u, _ := url.Parse(authorizeURL)
	if got := u.Query().Get("scope"); got != strings.Join(ConnectScopes(), ",") {
		t.Fatalf("scope decodes to %q", got)
	}

	writes := h.engine.callsNamed("createGithubConnectState")
	if len(writes) != 1 {
		t.Fatalf("%d state writes, want 1", len(writes))
	}
	w := writes[0]
	// component/identity's Store stamps internal origin and writes under the
	// state's own user (auth.ContextWithUserActor, whose role is its synthetic
	// writer): the actor is the person, never operatorContext.
	if !w.internal || w.userId != connectDev {
		t.Fatalf("the state was not written as the person with internal origin: %+v", w)
	}
	for _, want := range []string{
		`userId: "` + connectDev + `"`,
		`stateHash: "` + componentIdentity.HashConnectState(state) + `"`,
		`expiresAt: "2026-09-23T12:10:00Z"`,
		`returnPath: "/deployables?site=s1"`,
		`purpose: "shopify_connect"`,
		`shopDomain: "` + connectShop + `"`,
		`siteId: "s1"`,
		`clientId: "` + connectClientID + `"`,
		`credentialSource: "pending"`,
		// D16: bound to the browser session that pressed Connect, as GitHub
		// Connect's state is -- and no PKCE verifier, because Shopify's
		// authorization code grant takes none.
		`sessionId: "` + sessionFor(connectDev) + `"`,
		`pkceVerifier: ""`,
	} {
		if !strings.Contains(w.query, want) {
			t.Errorf("the state row is missing %s:\n  %s", want, w.query)
		}
	}
	if strings.Contains(authorizeURL, "code_challenge") {
		t.Errorf("the authorize URL carries a PKCE challenge Shopify does not take: %s", authorizeURL)
	}
	// Begin writes the state and nothing else.
	for _, name := range []string{"setGlobalSecret", "setGlobalVariable", "createStore", "updateStore", "createAuditEvent"} {
		if got := h.engine.callsNamed(name); len(got) != 0 {
			t.Fatalf("begin wrote %s: %v", name, got)
		}
	}
	// The plaintext state lives in the URL alone.
	assertNoSecretInAnyStatement(t, h, state)
	assertNoSecretInAnyStatement(t, h, connectSecret)
}

func TestBeginUsesTheStoresCurrentAppWhenNothingIsPending(t *testing.T) {
	h := newConnectHarness(t)
	h.currentApp("live-client")
	out := begin(t, h, map[string]any{"siteId": "s1"})
	if out["reason"] != connectReasonOK {
		t.Fatalf("reason = %v", out["reason"])
	}
	m := stateInURL.FindStringSubmatch(out["authorizeUrl"].(string))
	if m == nil {
		t.Fatalf("the authorize URL carries no state: %v", out["authorizeUrl"])
	}
	state, _ := url.QueryUnescape(m[1])
	if out["authorizeUrl"] != wantAuthorizeURL("live-client", state) {
		t.Fatalf("authorizeUrl = %v", out["authorizeUrl"])
	}
	w := firstStateWrite(t, h)
	for _, want := range []string{`clientId: "live-client"`, `credentialSource: "current"`, `returnPath: ""`} {
		if !strings.Contains(w, want) {
			t.Errorf("the state row is missing %s:\n  %s", want, w)
		}
	}
}

// A saved app is waiting for an approval: it is what Connect asks Shopify
// about, even over a live one -- that is what the approval promotes.
func TestBeginPrefersThePendingAppOverTheCurrentOne(t *testing.T) {
	h := newConnectHarness(t)
	h.currentApp("live-client")
	h.pendingApp(connectClientID)
	out := begin(t, h, map[string]any{"siteId": "s1"})
	if out["reason"] != connectReasonOK {
		t.Fatalf("reason = %v", out["reason"])
	}
	if u, _ := url.Parse(out["authorizeUrl"].(string)); u.Query().Get("client_id") != connectClientID {
		t.Fatalf("authorizeUrl = %v", out["authorizeUrl"])
	}
	w := firstStateWrite(t, h)
	if !strings.Contains(w, `clientId: "`+connectClientID+`"`) || !strings.Contains(w, `credentialSource: "pending"`) {
		t.Fatalf("the state does not name the pending app:\n  %s", w)
	}
}

func TestBeginWithNoAppSavedWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(h *connectHarness)
	}{
		{"nothing saved, no store", func(*connectHarness) {}},
		{"a pending client id with no secret", func(h *connectHarness) {
			h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeSecretName(connectStoreID, suffixPendingClientID)),
				[]map[string]any{{"id": "var-p", "name": "n", "value": connectClientID}})
		}},
		{"a store with no app", func(h *connectHarness) { h.store(map[string]any{"adminTokenRef": "A"}) }},
		{"a store app with no webhook secret row", func(h *connectHarness) {
			h.store(map[string]any{"appClientId": "live-client", "webhookSecretRef": storeSecretName(connectStoreID, suffixWebhookSecret)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConnectHarness(t)
			tc.arrange(h)
			out := begin(t, h, map[string]any{"siteId": "s1"})
			if out["reason"] != connectReasonShopifyAppNotSaved || out["authorizeUrl"] != "" {
				t.Fatalf("begin = %v, want shopify_app_not_saved and no URL", out)
			}
			if got := h.engine.callsNamed("createGithubConnectState"); len(got) != 0 {
				t.Fatalf("a state was written with no app to connect: %v", got)
			}
		})
	}
}

// A return path the browser supplied that is not a same-origin path is dropped
// for the OS root rather than stored.
func TestBeginCleansTheReturnPath(t *testing.T) {
	for _, raw := range []string{"//evil.example/x", "https://evil.example/", "/ok\r\nSet-Cookie: x"} {
		h := newConnectHarness(t)
		h.pendingApp(connectClientID)
		if out := begin(t, h, map[string]any{"siteId": "s1", "returnPath": raw}); out["reason"] != connectReasonOK {
			t.Fatalf("%q: %v", raw, out)
		}
		if w := firstStateWrite(t, h); !strings.Contains(w, `returnPath: ""`) {
			t.Errorf("%q reached the state row:\n  %s", raw, w)
		}
	}
}

// D16: a Connect state names the browser session that began it, and a call
// carrying no session -- no sid claim, so nothing for the callback to prove --
// writes no state, as githubConnectBegin refuses.
func TestBeginWithNoBrowserSessionIsAnErrorNotAState(t *testing.T) {
	h := newConnectHarness(t)
	h.pendingApp(connectClientID)
	t.Setenv("MEMQL_IDENTITY_BASE_URL", connectIdentityBase)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: connectDev, Role: auth.RoleDeveloper})
	if _, err := h.integ.handleConnectBegin(ctx, map[string]any{"siteId": "s1"}, 0); err == nil {
		t.Fatal("begin answered a call with no browser session")
	}
	if got := h.engine.callsNamed("createGithubConnectState"); len(got) != 0 {
		t.Fatalf("a state was written: %v", got)
	}
}

// The redirect is the cluster's own identity service, never the request's: a
// node that cannot say where that is writes no state it could never finish.
func TestBeginWithNoIdentityBaseURLIsAnErrorNotAState(t *testing.T) {
	h := newConnectHarness(t)
	h.pendingApp(connectClientID)
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "")
	if _, err := h.integ.handleConnectBegin(devCtx(), map[string]any{"siteId": "s1"}, 0); err == nil {
		t.Fatal("begin answered with no identity base URL")
	}
	if got := h.engine.callsNamed("createGithubConnectState"); len(got) != 0 {
		t.Fatalf("a state was written: %v", got)
	}
}

// A state row that cannot be stored is the typed reason GitHub Connect answers,
// with no URL: the person retries.
func TestBeginAnswersConnectStateInvalidWhenTheStateCannotBeStored(t *testing.T) {
	h := newConnectHarness(t)
	h.pendingApp(connectClientID)
	h.engine.fail["createGithubConnectState"] = context.DeadlineExceeded
	out := begin(t, h, map[string]any{"siteId": "s1"})
	if out["reason"] != connectReasonStateInvalid || out["authorizeUrl"] != "" {
		t.Fatalf("begin = %v", out)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The state statement component/identity renders for Begin, held against the
// real tree. An argument the mutation does not declare is not refused -- it is
// silently discarded (TestGeneratedCallArgumentsAreDeclared says why) -- and a
// state row missing its shop is a callback that can verify nothing.
func TestTheBeginStateStatementIsDeclared(t *testing.T) {
	h := newConnectHarness(t)
	h.pendingApp(connectClientID)
	begin(t, h, map[string]any{"siteId": "s1", "returnPath": "/"})
	stmt := firstStateWrite(t, h)

	eng := newRealEngine(t)
	if _, err := eng.Parse(stmt); err != nil {
		t.Fatalf("the engine refused the state write: %v\n  %s", err, stmt)
	}
	fn, err := eng.Functions().Get("createGithubConnectState")
	if err != nil || fn == nil || fn.ArgsSchema == nil {
		t.Fatalf("createGithubConnectState is not in the registry: %v", err)
	}
	declared := map[string]bool{}
	for _, f := range fn.ArgsSchema.Fields {
		declared[f.Name] = true
	}
	args := regexp.MustCompile(`[(,]\s*(\w+): "`).FindAllStringSubmatch(stmt, -1)
	if len(args) < 12 {
		t.Fatalf("read %d arguments out of the statement, want 12:\n  %s", len(args), stmt)
	}
	for _, m := range args {
		if !declared[m[1]] {
			t.Errorf("the state write passes %q, which createGithubConnectState does not declare", m[1])
		}
	}
}

func firstStateWrite(t *testing.T, h *connectHarness) string {
	t.Helper()
	writes := h.engine.callsNamed("createGithubConnectState")
	if len(writes) == 0 {
		t.Fatal("begin wrote no state")
	}
	return writes[0].query
}
