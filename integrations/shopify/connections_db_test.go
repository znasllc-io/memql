package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	identity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
)

// Begin on A, verified approval and persistence on B, concurrent refresh from
// both. Neither node inherits the other's memory or StoreRegistry cache.
func TestSharedShopifyConnectionAcrossNodes(t *testing.T) {
	a, dbA := authorityEngine(t)
	if a == nil {
		return
	}
	b, dbB := authorityEngine(t)
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("cd", 32))
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.example.test")
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "shared-" + suffix
	shop := "shared-" + suffix + ".myshopify.com"
	must := func(eng *memql.MemQLEngine, ctx context.Context, q string) *memql.ExecuteResult {
		t.Helper()
		r, e := eng.Execute(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	seed := func(concept, id string, payload map[string]any) {
		t.Helper()
		data, _ := json.Marshal(payload)
		must(a, seedCtx(), fmt.Sprintf("insert(%q,id=%q,payload=%s)", concept, id, data))
	}
	seed("v1:identity:user", user, map[string]any{"displayName": "Shared connection", "primaryEmail": user + "@example.test", "role": "developer", "active": true})
	must(a, seedCtx(), renderCall("setGlobalVariable", map[string]any{"id": variableRowID(managedClientID), "name": managedClientID, "value": "managed-client"}))
	must(a, seedCtx(), renderCall("setGlobalVariable", map[string]any{"id": variableRowID(managedInstallURL), "name": managedInstallURL, "value": "https://apps.shopify.com/memql-test"}))
	if _, e := seedSecret(seedCtx(), a, managedClientSecret, "managed-secret", "test", user); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		for _, concept := range []string{"v1:identity:user", "v1:identity:githubConnectState", "v1:platform:externalConnection", "v1:shopify:store", "v1:platform:globalSecret"} {
			_, _ = dbA.Exec(`DELETE FROM "MemoryNodes" WHERE concept=$1 AND (id LIKE $2 OR payload::text LIKE $2)`, concept, "%"+suffix+"%")
		}
		_, _ = dbA.Exec(`DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' IN ($1,$2,$3)`, managedClientID, managedClientSecret, managedInstallURL)
	})
	conn := func(e *memql.MemQLEngine, db *sql.DB) *Connector {
		c := NewConnector(e, nil, NewStoreRegistry(e, e.ResolveSystemSecret), NewAdminClient())
		c.WithDatabase(func() *sql.DB { return db })
		c.background = func(func()) {}
		if err := e.RegisterIntegration(NewIntegration(c)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, cb := conn(a, dbA), conn(b, dbB)
	var refreshes atomic.Int32
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "managed-client" || r.Form.Get("client_secret") != "managed-secret" {
			t.Error("exchange lost app credentials")
			w.WriteHeader(400)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			refreshes.Add(1)
			if r.Form.Get("refresh_token") != "refresh-one" {
				t.Error("refresh used stale pair")
			}
		} else if r.Form.Get("expiring") != "1" {
			t.Error("exchange did not request expiring tokens")
		}
		access, refresh := "access-one", "refresh-one"
		if r.Form.Get("grant_type") == "refresh_token" {
			access, refresh = "access-two", "refresh-two"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": 3600, "refresh_token_expires_in": 86400, "scope": strings.Join(StorefrontScopes, ",")})
	}))
	defer token.Close()
	ca.connectTokenURL = func(string) string { return token.URL }
	cb.connectTokenURL = ca.connectTokenURL
	admin := newFakeAdmin(t)
	cb.admin.endpoint = func(Store) string { return admin.server.URL + "/graphql" }
	admin.reply("ShopifyConnectPlan", map[string]any{"shop": map[string]any{"plan": map[string]any{"publicDisplayName": "Basic", "partnerDevelopment": true}}})
	admin.reply("ShopifyStorefrontTokenCreate", map[string]any{"storefrontAccessTokenCreate": map[string]any{"storefrontAccessToken": map[string]any{"accessToken": "public-storefront"}, "userErrors": []any{}}})
	// Real browser claims carry canonical user IDs inside the engine.
	ctx := actorCtx("v1:identity:user:"+user, auth.RoleDeveloper)
	// Connection management does not depend on opening Deployables.
	must(a, seedCtx(), renderCall("writeGrant", map[string]any{"grantId": auth.GrantRowID(auth.SubjectKindUser, user, auth.VerbRead, "app:deployables"), "subjectKind": auth.SubjectKindUser, "subjectId": user, "verb": auth.VerbRead, "resourceType": "app:deployables", "effect": auth.GrantDeny, "grantedBy": "test"}))
	must(a, ctx, "query sourceCredentialsMine()")
	must(a, ctx, "query sourceConnectionsMine()")
	ready := builtinReply(t, a, ctx, "builtin shopifyConnectionProviderStatus()")
	if ready["configured"] != true {
		t.Fatalf("provider not configured: %v", ready)
	}
	out := builtinReply(t, a, ctx, renderCall("shopifyAccountConnectBegin", map[string]any{"signedQuery": signedQuery("managed-secret", map[string]string{"shop": shop, "timestamp": strconv.FormatInt(time.Now().Unix(), 10)}).Encode(), "returnPath": "/?connect=connections&connectApp=settings"}))
	if out["reason"] != "ok" {
		t.Fatalf("begin: %v", out)
	}
	u, _ := url.Parse(out["authorizeUrl"].(string))
	if got := u.Query().Get("scope"); got != "unauthenticated_read_product_listings,unauthenticated_read_checkouts,unauthenticated_write_checkouts,unauthenticated_read_customers,read_products,read_inventory,read_locations,read_orders" {
		t.Fatalf("shared connection requested unrelated mirror permissions: %s", got)
	}
	states := &identity.Store{Engine: b}
	state, err := states.LookupGithubConnectState(context.Background(), identity.HashConnectState(u.Query().Get("state")))
	if err != nil || state == nil {
		t.Fatalf("B cannot read A's state: %v", err)
	}
	if state.SiteID != "" || state.CredentialSource != managedCredentialSource {
		t.Fatal("connection incorrectly belongs to a site")
	}
	if !cb.VerifyShopifyCallback(context.Background(), state, signedQuery("managed-secret", map[string]string{"shop": shop, "code": "authorization-code"})) {
		t.Fatal("B could not verify A's managed state")
	}
	if cb.VerifyShopifyCallback(context.Background(), state, signedQuery("wrong-secret", map[string]string{"shop": shop, "code": "authorization-code"})) {
		t.Fatal("invalid provider signature admitted")
	}
	grant, reason := cb.AuthorizeShopifyConnect(context.Background(), state, "authorization-code")
	if reason != "" {
		t.Fatal(reason)
	}
	result, kept := cb.WriteShopifyConnect(context.Background(), state, grant)
	if result != "connected" || kept != "connected" {
		t.Fatalf("write: %s/%s", result, kept)
	}
	rows := memql.MaterializeRows(must(a, ctx, "query externalConnectionsMine()"))
	if len(rows) != 1 || mapString(rows[0], "label") != shop {
		t.Fatalf("connection did not cross nodes: %v", rows)
	}
	connectionID := mapString(rows[0], "id")
	stranger := actorCtx("stranger-"+suffix, auth.RoleDeveloper)
	if got := memql.MaterializeRows(must(a, stranger, "query externalConnectionsMine()")); len(got) != 0 {
		t.Fatal("another user's connection leaked")
	}
	if _, err := a.Execute(stranger, renderCall("disconnectExternalConnection", map[string]any{"connectionId": connectionID})); err == nil {
		t.Fatal("another user disconnected the connection")
	}
	if _, err := a.Execute(ctx, fmt.Sprintf(`insert("v1:platform:externalConnection",id="forged-%s",payload={"ownerUserId":%q,"provider":"shopify","resourceId":"forged","label":"forged","status":"active"})`, suffix, user)); err == nil {
		t.Fatal("raw client forged a verified connection")
	}
	store, found, err := cb.storeByID(seedCtx(), grant.StoreID)
	if err != nil || !found {
		t.Fatal("no store saved")
	}
	persisted := memql.MaterializeRows(must(a, ctx, renderCall("storeById", map[string]any{"storeId": grant.StoreID})))
	if len(persisted) != 1 || rowValue(persisted[0], "isDevelopment") != true {
		t.Fatal("Shopify's development-store flag was not preserved")
	}
	ca.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	cb.now = ca.now
	var wg sync.WaitGroup
	for _, c := range []*Connector{ca, cb} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := c.adminToken(context.Background(), store)
			if err != nil || token != "access-two" {
				t.Errorf("refresh: %q %v", token, err)
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("replicas rotated refresh token %d times", refreshes.Load())
	}
	must(a, ctx, renderCall("disconnectExternalConnection", map[string]any{"connectionId": connectionID}))
	remaining, found, err := cb.storeByID(seedCtx(), grant.StoreID)
	if err != nil || !found || remaining.AdminTokenRef != store.AdminTokenRef || remaining.StorefrontTokenRef != store.StorefrontTokenRef {
		t.Fatal("disconnect changed deployed store credentials")
	}
}
