package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// connect_test.go -- the three Connect Shopify builtins (design 12.1, 12.2,
// 12.3, 12.5, 12.8) against the recording engine and a fake Storefront API.
//
// The fakes check this package's half of each conversation: which statements
// it builds, under whose identity, and what it sends Shopify. What they cannot
// check -- the real tier, the real capability gate, the real sealed rows -- is
// connect_db_test.go's.

const (
	connectSiteID   = "v1:platform:site:s1"
	connectShop     = "acme-widgets.myshopify.com"
	connectStoreID  = "acme-widgets"
	connectDev      = "dev-1"
	connectSecret   = "shpss_client_secret_value"
	connectClientID = "client-id-123"
	connectToken    = "storefront-token-value"
)

// connectCall is one statement the engine saw, and who asked.
type connectCall struct {
	query    string
	userId   string
	role     auth.Role
	internal bool
}

// connectEngine is the recording engine plus the write-admission answer the
// real engine gives through MayWriteRow.
type connectEngine struct {
	*fakeEngine
	writable map[string]bool // bare site id -> may the caller write it
	// denyWriter is a user the write admission refuses whatever writable
	// says: the person who may no longer write a site the deployment can.
	denyWriter string

	mu  stdsync.Mutex
	log []connectCall
}

func (e *connectEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	call := connectCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal()}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		call.userId, call.role = ac.UserId, ac.Role
	}
	e.mu.Lock()
	e.log = append(e.log, call)
	e.mu.Unlock()
	return e.fakeEngine.Execute(ctx, q)
}

func (e *connectEngine) MayWriteRow(ctx context.Context, concept, id string) (bool, error) {
	if concept != "v1:platform:site" {
		return false, nil
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && e.denyWriter != "" && ac.UserId == e.denyWriter {
		return false, nil
	}
	return e.writable[memql.BareShortId(id)], nil
}

// MayReadConcept is the store tier's row-free answer: a cluster owner, or a
// caller at or above the developer read floor -- of the roles these tests use,
// the developer. connect_callback_db_test.go asks the real one.
func (e *connectEngine) MayReadConcept(ctx context.Context, concept string) (bool, error) {
	ac, _ := auth.AccessFromContext(ctx)
	return concept == "v1:shopify:store" && ac != nil && (ac.IsClusterOwner() || ac.Role == auth.RoleDeveloper), nil
}

// MayChangeStoreBinding is the real engine's decision, which needs no rows to
// answer for a site in no organization: the store part, from the role.
func (e *connectEngine) MayChangeStoreBinding(ctx context.Context, accountId, priorStoreId, storeId string) bool {
	return (&memql.MemQLEngine{}).MayChangeStoreBinding(ctx, accountId, priorStoreId, storeId)
}

func (e *connectEngine) callsNamed(name string) []connectCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []connectCall
	for _, c := range e.log {
		if callName(c.query) == name {
			out = append(out, c)
		}
	}
	return out
}

// connectWrites are the statement names that change something. A refusal must
// reach none of them.
var connectWrites = []string{"setGlobalSecret", "setGlobalVariable", "createStore", "updateStore", "createAuditEvent", "createGithubConnectState", "updateSiteStoreBinding", "recordStoreHealth"}

func (e *connectEngine) writes() []string {
	var out []string
	for _, name := range connectWrites {
		for _, c := range e.callsNamed(name) {
			out = append(out, c.query)
		}
	}
	return out
}

// fakeStorefront answers the one Storefront request Connect makes.
type fakeStorefront struct {
	server *httptest.Server
	status int

	mu      stdsync.Mutex
	queries []string
	tokens  []string
	urls    []string
}

func newFakeStorefront(t *testing.T) *fakeStorefront {
	t.Helper()
	f := &fakeStorefront{status: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.queries = append(f.queries, body.Query)
		f.tokens = append(f.tokens, r.Header.Get("X-Shopify-Storefront-Access-Token"))
		f.urls = append(f.urls, r.URL.String())
		status := f.status
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"shop": map[string]any{"name": "Acme"}}})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeStorefront) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

type connectHarness struct {
	engine     *connectEngine
	storefront *fakeStorefront
	integ      *Integration
	logs       *bytes.Buffer
}

func newConnectHarness(t *testing.T) *connectHarness {
	t.Helper()
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	engine := &connectEngine{fakeEngine: newFakeEngine(), writable: map[string]bool{"s1": true}}
	storefront := newFakeStorefront(t)
	logs := &bytes.Buffer{}
	conn := NewConnector(engine, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), NewStoreRegistry(engine, nil), NewAdminClient())
	conn.connectAppGate = func(context.Context, string) (func(), error) { return func() {}, nil }
	conn.now = func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }
	conn.storefrontEndpoint = func(domain, version string) (string, error) {
		if _, err := StorefrontEndpoint(domain, version); err != nil {
			return "", err
		}
		return storefront.server.URL + "/api/graphql.json", nil
	}
	h := &connectHarness{engine: engine, storefront: storefront, integ: NewIntegration(conn), logs: logs}
	h.site(connectSiteID, "shopify_storefront")
	h.run(connectSiteID, connectShop)
	return h
}

// site makes siteById answer one row the developer owns.
func (h *connectHarness) site(id, kind string) {
	h.engine.setRows("siteById", []map[string]any{{
		"id": id, "kind": kind, "status": "draft", "ownerUserId": connectDev,
		"packageId": "v1:platform:package:p1", "packageDeployableName": "storefront",
	}})
}

// run makes the package's newest deployment the one that published siteId,
// naming shop in its report.
func (h *connectHarness) run(siteId, shop string) {
	h.engine.setRows("packageDeployments", []map[string]any{
		// A NEWER run that published a different deployable: the resolver
		// must pass over it rather than take the newest run.
		{
			"id": "v1:platform:packageDeployment:r2", "packageId": "v1:platform:package:p1", "status": "succeeded",
			"report": map[string]any{"deployables": []any{
				map[string]any{"name": "web", "kind": "spa"},
			}},
			"deployables": []any{map[string]any{"name": "web", "siteId": "v1:platform:site:other"}},
		},
		{
			"id": "v1:platform:packageDeployment:r1", "packageId": "v1:platform:package:p1", "status": "succeeded",
			"report": map[string]any{"deployables": []any{
				map[string]any{"name": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"store": shop}},
			}},
			"deployables": []any{map[string]any{"name": "storefront", "siteId": siteId}},
		},
	})
}

// store makes storeById answer one row.
func (h *connectHarness) store(fields map[string]any) {
	row := map[string]any{"id": "v1:shopify:store:" + connectStoreID, "domain": connectShop, "status": StatusConfigured}
	for k, v := range fields {
		row[k] = v
	}
	h.engine.setRows("storeById", []map[string]any{row})
}

// invoke runs a handler and decodes its one result node.
func invoke(t *testing.T, ctx context.Context, fn func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error), args map[string]any) map[string]any {
	t.Helper()
	nodes, err := fn(ctx, args, 0)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("handler answered %d nodes, want 1", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return out
}

// actorCtx is a signed-in person's call as the stream carries it: their access
// context, token and claims, the claims naming the browser session the call came
// from (sessionFor) -- which is what Begin binds a state to (D16).
func actorCtx(userId string, role auth.Role) context.Context {
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": userId, "role": string(role), "sid": sessionFor(userId)})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userId, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// sessionFor is the browser session id a test person's calls carry.
func sessionFor(userId string) string { return "sess-" + userId }

func devCtx() context.Context { return actorCtx(connectDev, auth.RoleDeveloper) }

// ---------------------------------------------------------------------------
// 12.1
// ---------------------------------------------------------------------------

func TestTheResolverTakesTheShopFromTheRunThatPublishedTheSite(t *testing.T) {
	h := newConnectHarness(t)
	target, reason, err := h.integ.connector.ResolveConnectSite(devCtx(), "s1")
	if err != nil || reason != "" {
		t.Fatalf("resolve: reason=%q err=%v", reason, err)
	}
	if target.ShopDomain != connectShop || target.StoreID != connectStoreID {
		t.Fatalf("resolved shop %q store %q, want %q / %q", target.ShopDomain, target.StoreID, connectShop, connectStoreID)
	}
	if target.StoreFound {
		t.Fatal("a store with no row reads as found")
	}

	// The site is read AS THE CALLER, and the store AS THE DEPLOYMENT -- by
	// id, never through the registry, never as the connector.
	sites := h.engine.callsNamed("siteById")
	if len(sites) != 1 || sites[0].userId != connectDev || sites[0].internal {
		t.Fatalf("the site was not read as the caller: %+v", sites)
	}
	reads := h.engine.callsNamed("storeById")
	if len(reads) != 1 {
		t.Fatalf("storeById ran %d times, want 1", len(reads))
	}
	if reads[0].role != auth.RoleOwner || !reads[0].internal || reads[0].userId == connectDev {
		t.Fatalf("the store was not read under operatorContext: %+v", reads[0])
	}
	if !strings.Contains(reads[0].query, `"`+connectStoreID+`"`) {
		t.Fatalf("the store was not read by its derived id: %s", reads[0].query)
	}
	if got := h.engine.callsNamed("stores"); len(got) != 0 {
		t.Fatalf("the StoreRegistry was consulted: %v", got)
	}
}

func TestTheResolverFindsAStoreRowWhenOneExists(t *testing.T) {
	h := newConnectHarness(t)
	h.store(map[string]any{"appClientId": "live-client", "adminTokenRef": "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN"})
	target, reason, err := h.integ.connector.ResolveConnectSite(devCtx(), "s1")
	if err != nil || reason != "" {
		t.Fatalf("resolve: reason=%q err=%v", reason, err)
	}
	if !target.StoreFound || target.Store.AppClientID != "live-client" {
		t.Fatalf("the store row was not read: %+v", target)
	}
}

// ---------------------------------------------------------------------------
// 12.2
// ---------------------------------------------------------------------------

func TestConnectStatusReportsEachState(t *testing.T) {
	pendingVar := namedRowsQuery(conceptGlobalVariable, storeSecretName(connectStoreID, suffixPendingClientID))
	pendingSecret := namedRowsQuery(conceptGlobalSecret, storeSecretName(connectStoreID, suffixPendingClientSecret))

	for _, tc := range []struct {
		name                                  string
		store                                 map[string]any
		pending                               bool
		appSaved, pendingApp, connected, sfTk bool
	}{
		{name: "nothing saved"},
		{name: "a pending app", pending: true, appSaved: true, pendingApp: true},
		{name: "a store row with a live app and its webhook secret, not connected", store: map[string]any{"appClientId": "live", "webhookSecretRef": "W"}, appSaved: true},
		// The case that used to disagree: an app id with no secret to verify
		// or exchange with is not an app Begin can use, so it is not saved.
		{name: "a live app id with no webhook secret row", store: map[string]any{"appClientId": "live", "webhookSecretRef": "MISSING"}},
		{name: "a live app id naming no webhook secret", store: map[string]any{"appClientId": "live"}},
		{
			name:     "connected",
			store:    map[string]any{"appClientId": "live", "webhookSecretRef": "W", "adminTokenRef": "A", "storefrontTokenRef": "S", "scopesGranted": []any{"read_products"}},
			appSaved: true, connected: true, sfTk: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConnectHarness(t)
			if tc.store != nil {
				h.store(tc.store)
				h.engine.setRows(namedRowsQuery(conceptGlobalSecret, "W"), []map[string]any{{"id": "sec-w", "name": "W", "encryptedValue": "ZW5j"}})
			}
			if tc.pending {
				h.engine.setRows(pendingVar, []map[string]any{{"id": "var-x", "name": "n", "value": connectClientID}})
				h.engine.setRows(pendingSecret, []map[string]any{{"id": "sec-x", "name": "n", "encryptedValue": "ZW5j"}})
			}
			out := invoke(t, devCtx(), h.integ.handleConnectStatus, map[string]any{"siteId": "s1"})
			if out["reason"] != connectReasonOK || out["storeId"] != connectStoreID || out["shopDomain"] != connectShop {
				t.Fatalf("status: %v", out)
			}
			for key, want := range map[string]bool{
				"appSaved": tc.appSaved, "pendingApp": tc.pendingApp,
				"connected": tc.connected, "storefrontTokenSet": tc.sfTk,
			} {
				if out[key] != want {
					t.Errorf("%s = %v, want %v", key, out[key], want)
				}
			}
			if !reflect.DeepEqual(out["requiredScopes"], toAny(ConnectScopes())) {
				t.Errorf("requiredScopes = %v", out["requiredScopes"])
			}
			if _, ok := out["grantedScopes"].([]any); !ok {
				t.Errorf("grantedScopes must always be a list: %v", out["grantedScopes"])
			}
			if len(h.engine.writes()) != 0 {
				t.Fatalf("a status read wrote: %v", h.engine.writes())
			}
			// One answer to "is there an app to connect with": Begin's.
			wantBegin := connectReasonShopifyAppNotSaved
			if tc.appSaved {
				wantBegin = connectReasonOK
			}
			if got := begin(t, h, map[string]any{"siteId": "s1"}); got["reason"] != wantBegin {
				t.Errorf("status says appSaved=%v and begin answers %v", tc.appSaved, got["reason"])
			}
		})
	}
}

// A refusal answers every key, so the panel never reads a missing field as a
// state.
func TestConnectStatusAnswersEveryKeyOnARefusal(t *testing.T) {
	h := newConnectHarness(t)
	h.engine.writable["s1"] = false
	out := invoke(t, devCtx(), h.integ.handleConnectStatus, map[string]any{"siteId": "s1"})
	if out["reason"] != connectReasonSiteNotWritable {
		t.Fatalf("reason = %v", out["reason"])
	}
	for _, key := range []string{"reason", "storeId", "shopDomain", "appSaved", "pendingApp", "connected", "storefrontTokenSet", "requiredScopes", "grantedScopes"} {
		if _, ok := out[key]; !ok {
			t.Errorf("the refusal carries no %q", key)
		}
	}
	if out["shopDomain"] != "" || out["storeId"] != "" {
		t.Fatalf("a refused site discloses its store: %v", out)
	}
}

// ---------------------------------------------------------------------------
// 12.3
// ---------------------------------------------------------------------------

func TestSavingTheAppWritesPendingCredentialsAndNothingLive(t *testing.T) {
	h := newConnectHarness(t)
	// A CONNECTED store: the case D12 exists for. Saving must not move what
	// verifies its webhooks.
	h.store(map[string]any{
		"appClientId": "live-client", "adminTokenRef": "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
		"webhookSecretRef": "SHOPIFY_ACME-WIDGETS_WEBHOOK_SECRET",
	})
	out := invoke(t, devCtx(), h.integ.handleStoreAppSave, map[string]any{
		"siteId": "s1", "clientId": connectClientID, "clientSecret": connectSecret,
	})
	if out["reason"] != connectReasonOK {
		t.Fatalf("save: %v", out)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), connectSecret) {
		t.Fatalf("the reply carries the secret: %s", b)
	}

	if got := h.engine.callsNamed("createStore"); len(got) != 0 {
		t.Fatalf("a save wrote the store row: %v", got)
	}
	if got := h.engine.callsNamed("updateStore"); len(got) != 0 {
		t.Fatalf("a save wrote the store row: %v", got)
	}

	vars := h.engine.callsNamed("setGlobalVariable")
	if len(vars) != 1 || !strings.Contains(vars[0].query, `"SHOPIFY_ACME-WIDGETS_PENDING_CLIENT_ID"`) ||
		!strings.Contains(vars[0].query, `"`+connectClientID+`"`) {
		t.Fatalf("the pending client id was not written: %+v", vars)
	}
	secrets := h.engine.callsNamed("setGlobalSecret")
	if len(secrets) != 1 || !strings.Contains(secrets[0].query, `"SHOPIFY_ACME-WIDGETS_PENDING_CLIENT_SECRET"`) {
		t.Fatalf("the pending secret was not sealed: %+v", secrets)
	}
	if !strings.Contains(secrets[0].query, `addedBy: "`+connectDev+`"`) || !strings.Contains(secrets[0].query, "Store panel") {
		t.Fatalf("the sealed row does not say who saved it and where: %s", secrets[0].query)
	}
	for _, c := range append(vars, secrets...) {
		if !c.internal || c.role != auth.RoleOwner {
			t.Fatalf("a pending write did not run under operatorContext: %+v", c)
		}
		if strings.Contains(c.query, "WEBHOOK_SECRET") || strings.Contains(c.query, "live-client") {
			t.Fatalf("a save touched a live credential: %s", c.query)
		}
	}

	audits := h.engine.callsNamed("createAuditEvent")
	if len(audits) != 1 {
		t.Fatalf("%d audit events, want 1", len(audits))
	}
	for _, want := range []string{`action: "shopify_app_saved"`, `category: "configuration"`, `targetType: "shopifyStore"`, `actorUserId: "` + connectDev + `"`} {
		if !strings.Contains(audits[0].query, want) {
			t.Errorf("the audit event carries no %s: %s", want, audits[0].query)
		}
	}
	if strings.Contains(audits[0].query, connectClientID) || strings.Contains(audits[0].query, "fingerprint") {
		t.Fatalf("the audit event carries a value: %s", audits[0].query)
	}
	assertNoSecretInAnyStatement(t, h, connectSecret)
}

func TestSeedSecretWritesAtTheOneRowCarryingTheName(t *testing.T) {
	h := newConnectHarness(t)
	name := storeSecretName(connectStoreID, suffixPendingClientSecret)
	h.engine.setRows(namedRowsQuery(conceptGlobalSecret, name), []map[string]any{{"id": "v1:platform:globalSecret:sec-planted-by-hand", "name": name}})
	out := invoke(t, devCtx(), h.integ.handleStoreAppSave, map[string]any{
		"siteId": "s1", "clientId": connectClientID, "clientSecret": connectSecret,
	})
	if out["reason"] != connectReasonOK {
		t.Fatalf("save: %v", out)
	}
	secrets := h.engine.callsNamed("setGlobalSecret")
	if len(secrets) != 1 || !strings.Contains(secrets[0].query, `id: "sec-planted-by-hand"`) {
		t.Fatalf("the secret was not written at the existing row's id: %+v", secrets)
	}
}

func TestTheEnvSeedDescribesItselfAsTheSeed(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", nil)
	h.conn.stores.Invalidate()
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	if _, err := SeedStoreFromEnv(context.Background(), h.engine, h.conn.stores, SeedConfig{
		StoreDomain: "first.myshopify.com", AdminToken: "shpat_secret", APIVersion: "2026-07",
	}); err != nil {
		t.Fatal(err)
	}
	secrets := h.engine.callsTo("setGlobalSecret")
	if len(secrets) != 1 || !strings.Contains(secrets[0], "Seeded from the environment") ||
		!strings.Contains(secrets[0], `addedBy: "system:connector:shopify"`) {
		t.Fatalf("the seed lost its own provenance: %v", secrets)
	}
}

// ---------------------------------------------------------------------------
// 12.5
// ---------------------------------------------------------------------------

func TestAStorefrontTokenIsCheckedThenSealed(t *testing.T) {
	h := newConnectHarness(t)
	h.store(map[string]any{"adminTokenRef": "A", "apiVersion": "2026-07"})
	h.engine.setRows("sitesBoundToStore", []map[string]any{{"id": connectSiteID, "status": "draft", "binding": map[string]any{"storeId": connectStoreID}}})

	out := invoke(t, devCtx(), h.integ.handleStorefrontTokenSet, map[string]any{"siteId": "s1", "token": connectToken})
	if out["reason"] != connectReasonOK {
		t.Fatalf("token set: %v", out)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), connectToken) {
		t.Fatalf("the reply carries the token: %s", b)
	}
	if h.storefront.requests() != 1 {
		t.Fatalf("%d Storefront requests, want exactly 1", h.storefront.requests())
	}
	if q := h.storefront.queries[0]; !strings.Contains(strings.Join(strings.Fields(q), " "), "shop { name }") {
		t.Fatalf("the check asked %q", q)
	}
	if h.storefront.tokens[0] != connectToken {
		t.Fatal("the check did not present the pasted token")
	}
	secrets := h.engine.callsNamed("setGlobalSecret")
	if len(secrets) != 1 || !strings.Contains(secrets[0].query, `"SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN"`) {
		t.Fatalf("the token was not sealed: %+v", secrets)
	}
	updates := h.engine.callsNamed("updateStore")
	if len(updates) != 1 || !strings.Contains(updates[0].query, `storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN"`) ||
		!updates[0].internal || updates[0].role != auth.RoleOwner {
		t.Fatalf("storefrontTokenRef was not set under operatorContext: %+v", updates)
	}
	audits := h.engine.callsNamed("createAuditEvent")
	if len(audits) != 1 || !strings.Contains(audits[0].query, `action: "shopify_storefront_token_set"`) {
		t.Fatalf("audit: %+v", audits)
	}
	assertNoSecretInAnyStatement(t, h, connectToken)
}

func TestAnEmptyTokenClearsTheReference(t *testing.T) {
	h := newConnectHarness(t)
	h.store(map[string]any{"storefrontTokenRef": "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN"})
	h.engine.setRows("sitesBoundToStore", []map[string]any{{"id": connectSiteID, "status": "draft", "binding": map[string]any{"storeId": connectStoreID}}})

	out := invoke(t, devCtx(), h.integ.handleStorefrontTokenSet, map[string]any{"siteId": "s1", "token": ""})
	if out["reason"] != connectReasonOK {
		t.Fatalf("clear: %v", out)
	}
	if h.storefront.requests() != 0 {
		t.Fatal("clearing asked Shopify about a token")
	}
	if got := h.engine.callsNamed("setGlobalSecret"); len(got) != 0 {
		t.Fatalf("clearing sealed something: %v", got)
	}
	updates := h.engine.callsNamed("updateStore")
	if len(updates) != 1 || !strings.Contains(updates[0].query, `storefrontTokenRef: ""`) {
		t.Fatalf("the reference was not cleared: %+v", updates)
	}
	audits := h.engine.callsNamed("createAuditEvent")
	if len(audits) != 1 || !strings.Contains(audits[0].query, `action: "shopify_storefront_token_cleared"`) {
		t.Fatalf("audit: %+v", audits)
	}
}

// A cluster owner changes a shared store's token; the check that makes a
// developer prove reach is the owner's by definition.
func TestAClusterOwnerMayChangeTheTokenOfAStoreInUse(t *testing.T) {
	h := newConnectHarness(t)
	h.engine.writable["s1"] = true
	h.store(map[string]any{"adminTokenRef": "A"})
	h.engine.setRows("sitesBoundToStore", []map[string]any{
		{"id": connectSiteID, "status": "draft", "binding": map[string]any{"storeId": connectStoreID}},
		{"id": "v1:platform:site:someone-elses", "status": "draft", "previewBinding": map[string]any{"storeId": connectStoreID}},
	})
	out := invoke(t, actorCtx("owner-1", auth.RoleOwner), h.integ.handleStorefrontTokenSet, map[string]any{"siteId": "s1", "token": connectToken})
	if out["reason"] != connectReasonOK {
		t.Fatalf("an owner was refused: %v", out)
	}
}

// ---------------------------------------------------------------------------
// 12.8
// ---------------------------------------------------------------------------

func TestTheConnectScopeListIsPinned(t *testing.T) {
	want := []string{
		"unauthenticated_read_product_listings",
		"unauthenticated_read_checkouts",
		"unauthenticated_write_checkouts",
		"unauthenticated_read_customers",
	}
	if !reflect.DeepEqual(StorefrontScopes, want) {
		t.Fatalf("StorefrontScopes = %v, want %v -- the runbook prints this list and Connect refuses without it", StorefrontScopes, want)
	}
	all := ConnectScopes()
	if !reflect.DeepEqual(all[:len(want)], want) || !reflect.DeepEqual(all[len(want):], generated.Scopes) {
		t.Fatalf("ConnectScopes = %v, want the Storefront scopes then generated.Scopes", all)
	}
	// A copy: a caller appending to it must not change the list.
	all[0] = "mutated"
	if ConnectScopes()[0] != want[0] {
		t.Fatal("ConnectScopes hands out the package's own slice")
	}
}

// ---------------------------------------------------------------------------

// assertNoSecretInAnyStatement is D9's sentence, measured: a secret is in no
// statement the engine was handed (the sealed row carries ciphertext), in no
// reply, and nowhere the audit event reaches.
func assertNoSecretInAnyStatement(t *testing.T, h *connectHarness, secretValue string) {
	t.Helper()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, c := range h.engine.log {
		if strings.Contains(c.query, secretValue) {
			t.Fatalf("a secret reached a statement:\n%s", c.query)
		}
	}
	if strings.Contains(h.logs.String(), secretValue) {
		t.Fatalf("a secret reached a log line:\n%s", h.logs.String())
	}
	h.storefront.mu.Lock()
	defer h.storefront.mu.Unlock()
	for _, u := range h.storefront.urls {
		if strings.Contains(u, secretValue) {
			t.Fatalf("a secret reached a URL: %s", u)
		}
	}
}

// A write that fails names the row, never the value it was writing.
func TestAFailedWriteNamesNoSecret(t *testing.T) {
	for _, tc := range []struct {
		name, fail, secretValue string
		args                    map[string]any
		token                   bool
	}{
		{"the pending secret", "setGlobalSecret", connectSecret, map[string]any{"siteId": "s1", "clientId": connectClientID, "clientSecret": connectSecret}, false},
		{"the pending client id", "setGlobalVariable", connectSecret, map[string]any{"siteId": "s1", "clientId": connectClientID, "clientSecret": connectSecret}, false},
		{"the Storefront token", "setGlobalSecret", connectToken, map[string]any{"siteId": "s1", "token": connectToken}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConnectHarness(t)
			h.store(map[string]any{"adminTokenRef": "A"})
			h.engine.fail[tc.fail] = errors.New("the database is down")
			fn := h.integ.handleStoreAppSave
			if tc.token {
				fn = h.integ.handleStorefrontTokenSet
			}
			_, err := fn(devCtx(), tc.args, 0)
			if err == nil {
				t.Fatal("a failed write was answered as a success")
			}
			if strings.Contains(err.Error(), tc.secretValue) {
				t.Fatalf("the error carries the secret: %v", err)
			}
			assertNoSecretInAnyStatement(t, h, tc.secretValue)
		})
	}
}

// A refused token logs the store, and nothing of Shopify's reply.
func TestARefusedTokenLogsNoResponseBody(t *testing.T) {
	h := newConnectHarness(t)
	h.store(map[string]any{"adminTokenRef": "A"})
	h.storefront.status = http.StatusUnauthorized
	out := invoke(t, devCtx(), h.integ.handleStorefrontTokenSet, map[string]any{"siteId": "s1", "token": connectToken})
	if out["reason"] != connectReasonStorefrontTokenInvalid {
		t.Fatalf("reason = %v", out["reason"])
	}
	if !strings.Contains(h.logs.String(), connectStoreID) || strings.Contains(h.logs.String(), "401") {
		t.Fatalf("the refusal's log line: %s", h.logs.String())
	}
	assertNoSecretInAnyStatement(t, h, connectToken)
}
