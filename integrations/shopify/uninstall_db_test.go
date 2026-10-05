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
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	identity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// uninstall_db_test.go -- an uninstall and a reinstall, end to end, across two
// engines over a real Postgres (memql#5638: G1, G4, G5).
//
// The store's state crosses nodes through the database alone: the delivery is
// worked on one replica, and the other -- which shares no cache with it --
// must read the store as disconnected, stop ingesting it, and show its owner a
// saved connection that no longer reads active. Then shop/redact purges the
// store, and a reinstall through Connect Shopify, begun on one replica and
// finished on the other, must bring it back: the guard that was meant to
// refuse a purged store asked for a status the enum does not have, and the
// write that followed left the store paused and redacted with a dead
// Storefront token.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.
func TestAnUninstallDisconnectsAcrossNodesAndAReinstallRestoresTheStore(t *testing.T) {
	a, dbA := authorityEngine(t)
	if a == nil {
		return
	}
	b, dbB := authorityEngine(t)
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("cd", 32))
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.example.test")
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "uninstall-" + suffix
	shop := "uninstall-" + suffix + ".myshopify.com"
	must := func(eng *memql.MemQLEngine, ctx context.Context, q string) *memql.ExecuteResult {
		t.Helper()
		r, err := eng.Execute(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	data, _ := json.Marshal(map[string]any{"displayName": "Uninstall", "primaryEmail": user + "@example.test", "role": "developer", "active": true})
	must(a, seedCtx(), fmt.Sprintf("insert(%q,id=%q,payload=%s)", "v1:identity:user", user, data))
	must(a, seedCtx(), renderCall("setGlobalVariable", map[string]any{"id": variableRowID(managedClientID), "name": managedClientID, "value": "managed-client"}))
	must(a, seedCtx(), renderCall("setGlobalVariable", map[string]any{"id": variableRowID(managedInstallURL), "name": managedInstallURL, "value": "https://apps.shopify.com/memql-test"}))
	if _, err := seedSecret(seedCtx(), a, managedClientSecret, "managed-secret", "test", user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, concept := range []string{"v1:identity:user", "v1:identity:githubConnectState", "v1:platform:externalConnection", "v1:shopify:store", "v1:platform:globalSecret", "v1:identity:auditEvent"} {
			_, _ = dbA.Exec(`DELETE FROM "MemoryNodes" WHERE concept=$1 AND (id LIKE $2 OR payload::text LIKE $2)`, concept, "%"+suffix+"%")
		}
		_, _ = dbA.Exec(`DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' IN ($1,$2,$3)`, managedClientID, managedClientSecret, managedInstallURL)
	})

	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + suffix, "refresh_token": "refresh-" + suffix,
			"expires_in": 3600, "refresh_token_expires_in": 86400, "scope": strings.Join(managedConnectScopes(), ",")})
	}))
	defer token.Close()
	admin := newFakeAdmin(t)
	admin.reply("ShopifyConnectPlan", map[string]any{"shop": map[string]any{"plan": map[string]any{"publicDisplayName": "Basic", "partnerDevelopment": true}}})
	mint := func(value string) {
		admin.reply("ShopifyStorefrontTokenCreate", map[string]any{"storefrontAccessTokenCreate": map[string]any{"storefrontAccessToken": map[string]any{"accessToken": value}, "userErrors": []any{}}})
	}
	mint("storefront-one-" + suffix)
	storefront := newFakeStorefront(t)
	conn := func(e *memql.MemQLEngine, db *sql.DB) *Connector {
		c := NewConnector(e, nil, NewStoreRegistry(e, e.ResolveSystemSecret), NewAdminClient())
		c.WithDatabase(func() *sql.DB { return db })
		c.background = func(func()) {}
		c.connectTokenURL = func(string) string { return token.URL }
		c.admin.endpoint = func(Store) string { return admin.server.URL + "/graphql" }
		c.storefrontEndpoint = func(string, string) (string, error) { return storefront.server.URL + "/api/graphql.json", nil }
		if err := e.RegisterIntegration(NewIntegration(c)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, cb := conn(a, dbA), conn(b, dbB)
	person := actorCtx("v1:identity:user:"+user, auth.RoleDeveloper)

	// Connect Shopify's managed flow: begun on A, verified and written on B.
	connect := func() (string, string) {
		t.Helper()
		out := builtinReply(t, a, person, renderCall("shopifyAccountConnectBegin", map[string]any{
			"signedQuery": signedQuery("managed-secret", map[string]string{"shop": shop, "timestamp": strconv.FormatInt(time.Now().Unix(), 10)}).Encode(),
			"returnPath":  "/?connect=connections",
		}))
		if out["reason"] != "ok" {
			t.Fatalf("begin: %v", out)
		}
		u, _ := url.Parse(out["authorizeUrl"].(string))
		state, err := (&identity.Store{Engine: b}).LookupGithubConnectState(context.Background(), identity.HashConnectState(u.Query().Get("state")))
		if err != nil || state == nil {
			t.Fatalf("B cannot read A's state: %v", err)
		}
		if !cb.VerifyShopifyCallback(context.Background(), state, signedQuery("managed-secret", map[string]string{"shop": shop, "code": "code-" + suffix})) {
			t.Fatal("B could not verify the callback")
		}
		grant, reason := cb.AuthorizeShopifyConnect(context.Background(), state, "code-"+suffix)
		if reason != "" {
			t.Fatalf("authorize: %s", reason)
		}
		return cb.WriteShopifyConnect(context.Background(), state, grant)
	}
	storeID, err := StoreIDForDomain(shop)
	if err != nil {
		t.Fatal(err)
	}
	// Every read across the two engines is FRESH. There is no event mesh
	// between them, so nothing evicts one engine's result cache when the other
	// writes; in a cluster the broadcast does. What crosses nodes is the row.
	fresh := memql.ContextWithFreshRead
	readStore := func(eng *memql.MemQLEngine) map[string]any {
		t.Helper()
		rows := memql.MaterializeRows(must(eng, fresh(ownerCtx()), renderCall("storeById", map[string]any{"storeId": storeID})))
		if len(rows) != 1 {
			t.Fatalf("storeById answered %d rows", len(rows))
		}
		return rows[0]
	}
	connectionStatus := func(eng *memql.MemQLEngine) string {
		t.Helper()
		for _, row := range memql.MaterializeRows(must(eng, person, "query externalConnectionsMine()")) {
			if mapString(row, "resourceId") == storeID {
				return mapString(row, "status")
			}
		}
		return ""
	}
	audited := func(action string) bool {
		t.Helper()
		var n int
		if err := dbA.QueryRow(`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1 AND payload->>'action' = $2`, storeID, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	if result, kept := connect(); result != "connected" || kept != "connected" {
		t.Fatalf("first connect: %s/%s", result, kept)
	}
	if got := connectionStatus(a); got != "active" {
		t.Fatalf("the saved connection reads %q after connecting", got)
	}
	grantName := storeSecretName(storeID, "OFFLINE_GRANT")
	storefrontName := storeSecretName(storeID, suffixStorefrontToken)
	// B reads the connected store the way a callback does, so B's result cache
	// holds it CONNECTED. Nothing below writes on B before the reinstall's own
	// write reads the store again, so a read served from that cache would see
	// no uninstall and no purge to clear.
	if _, found, err := cb.storeByID(operatorContext(context.Background()), storeID); err != nil || !found {
		t.Fatalf("B cannot read the connected store: found=%v err=%v", found, err)
	}

	// THE UNINSTALL, worked on A. Shopify has ended the grant: it answers 401.
	admin.httpStatus("ShopifyReachable", http.StatusUnauthorized)
	body, _ := json.Marshal(map[string]any{"id": 548380009, "name": "Uninstall", "myshopify_domain": shop, "domain": shop})
	delivery := memqlsync.InboundRequest{
		RequestId: "inb-uninstall-" + suffix, Source: ConnectorName, Topic: TopicAppUninstalled, Body: body,
		Headers: map[string]string{
			strings.ToLower(HeaderTopic): TopicAppUninstalled, strings.ToLower(HeaderShopDomain): shop,
			strings.ToLower(HeaderWebhookID): "wh-" + suffix, strings.ToLower(HeaderTriggeredAt): time.Now().UTC().Format(time.RFC3339),
		},
	}
	if _, err := ca.Apply(auth.ContextWithConnectorActor(context.Background(), ConnectorName), delivery); err != nil {
		t.Fatalf("A could not work the uninstall: %v", err)
	}

	// B, which shares no cache with A, reads the store disconnected -- the
	// fields the edge's storeById projection reads -- with the grant dropped
	// and the Storefront token reference kept for the reinstall to judge.
	row := readStore(b)
	if mapString(row, "uninstalledAt") == "" || mapString(row, "adminTokenRef") != "" || mapString(row, "storefrontTokenRef") != storefrontName {
		t.Fatalf("B reads the store as %v; want uninstalledAt set, no Admin grant, the Storefront token reference kept", row)
	}
	if store, ok := NewStoreRegistry(b, b.ResolveSystemSecret).ByID(fresh(context.Background()), storeID); !ok || store.Ingests() {
		t.Fatalf("B's registry: found=%v ingests=%v; an uninstalled store ingests nothing", ok, store.Ingests())
	}
	if got := connectionStatus(b); got != connectionStatusDisconnected {
		t.Fatalf("the owner's saved connection reads %q on B, want %q", got, connectionStatusDisconnected)
	}
	if v, err := b.ResolveSystemSecret(fresh(ownerCtx()), grantName); err == nil && v != "" {
		t.Fatal("the dead grant is still sealed under its name")
	}
	if !audited("shopify_app_uninstalled") {
		t.Fatal("the uninstall was not audited")
	}

	// shop/redact, 48 hours later, worked on A: the store does not answer, so
	// it is purged.
	purged, ok := NewStoreRegistry(a, a.ResolveSystemSecret).ByID(fresh(context.Background()), storeID)
	if !ok {
		t.Fatal("A cannot read the store to purge it")
	}
	if _, err := ca.PurgeStore(context.Background(), purged); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if row := readStore(a); mapString(row, "redactedAt") == "" || mapString(row, "status") != StatusPaused {
		t.Fatalf("after the purge the store reads %v", row)
	}

	// THE REINSTALL. The shop's staff approve the app again; its old
	// Storefront token is refused, as an uninstall leaves it.
	storefront.status = http.StatusUnauthorized
	mint("storefront-two-" + suffix)
	if result, kept := connect(); result != "connected" || kept != "connected" {
		t.Fatalf("the reinstall: %s/%s -- a purged store must come back on a verified reinstall", result, kept)
	}
	row = readStore(a)
	if mapString(row, "uninstalledAt") != "" || mapString(row, "redactedAt") != "" || mapString(row, "status") != StatusConfigured || mapString(row, "adminTokenRef") != grantName {
		t.Fatalf("after the reinstall the store reads %v; want it connected, configured and no longer redacted", row)
	}
	if store, ok := NewStoreRegistry(a, a.ResolveSystemSecret).ByID(fresh(context.Background()), storeID); !ok || !store.Ingests() {
		t.Fatal("the reinstalled store does not ingest")
	}
	if v, err := a.ResolveSystemSecret(fresh(ownerCtx()), storefrontName); err != nil || v != "storefront-two-"+suffix {
		t.Fatalf("the store's Storefront token is %q (%v), want the one minted for the refused token", v, err)
	}
	if got := connectionStatus(a); got != "active" {
		t.Fatalf("the saved connection reads %q after the reinstall", got)
	}
	if !audited("shopify_store_reinstalled") {
		t.Fatal("the reinstall was not audited")
	}
}
