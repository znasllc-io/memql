package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// connect_callback_db_test.go -- the callback's Shopify half (design 12.6 steps
// 4, 5, 7-10) against a real engine over a real Postgres, from a state the real
// shopifyConnectBegin wrote: the pending secret comes back through the sealed
// row's resolver, and the person is judged by the real capability catalog, the
// real store read floor and the real site tier, under the role their row holds
// AT THE CALLBACK. Each refusal sits beside the reachable positive over the same
// rows.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.

func TestTheCallbackJudgesThePersonAtTheCallbackAgainstARealEngine(t *testing.T) {
	eng, raw := authorityEngine(t)
	if eng == nil {
		return
	}
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("cd", 32))
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.connect.example.test")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)

	conn := NewConnector(eng, slog.New(slog.DiscardHandler), NewStoreRegistry(eng, eng.ResolveSystemSecret), NewAdminClient())
	if err := eng.RegisterIntegration(NewIntegration(conn)); err != nil {
		t.Fatalf("register the shopify integration: %v", err)
	}
	token := newFakeTokenEndpoint(t)
	conn.connectTokenURL = func(string) string { return token.server.URL + "/admin/oauth/access_token" }

	dev := "cb-dev-" + suffix
	shop := "cb-" + suffix + ".myshopify.com"
	storeID := "cb-" + suffix
	pkg := "v1:platform:package:cb-" + suffix
	mine := "cb-sf-" + suffix
	appSecret := "cb-app-secret-" + suffix

	var cleanup []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range cleanup {
			_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id = $1`, id)
		}
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' LIKE $1`,
			"SHOPIFY_CB-%"+suffix+"%")
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:githubConnectState' AND payload->>'userId' LIKE $1`, "%cb-dev-"+suffix)
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`, shop)
	})
	seed := func(concept, id string, payload map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), string(body))
		if _, err := eng.Execute(seedCtx(), q); err != nil {
			t.Fatalf("seed %s %s: %v", concept, id, err)
		}
		cleanup = append(cleanup, concept+":"+id)
	}
	// setRole writes a new version of the person's row: a demotion between
	// begin and callback, as an admin's edit would make it.
	setRole := func(role auth.Role) {
		seed("v1:identity:user", dev, map[string]any{
			"displayName": "Callback Dev", "primaryEmail": dev + "@connect.example.test", "role": string(role), "active": true,
		})
	}
	setRole(auth.RoleDeveloper)
	seed("v1:platform:site", mine, map[string]any{
		"ownerUserId": dev, "title": mine, "hostname": mine + ".connect.example.test", "status": "draft",
		"kind": "shopify_storefront", "bundleRef": "blob://sites/" + mine + "/v1/",
		"packageId": pkg, "packageDeployableName": "storefront",
	})
	// The package the run names: a packageDeployment's packageId must resolve
	// to a package of the same organization (memql#5598).
	seed("v1:platform:package", strings.TrimPrefix(pkg, "v1:platform:package:"), map[string]any{
		"ownerUserId": dev, "name": "storefront", "sourceKind": "artifact", "status": "active",
	})
	seed("v1:platform:packageDeployment", "cb-run-"+suffix, map[string]any{
		"ownerUserId": dev, "packageId": pkg, "status": "succeeded",
		"report": map[string]any{"deployables": []any{map[string]any{
			"name": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"store": shop},
		}}},
		"deployables": []any{map[string]any{"name": "storefront", "siteId": "v1:platform:site:" + mine}},
	})
	// A store row exists, so "the store reads back" is asked of a real row.
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s)`, langparser.QuoteString(storeID), langparser.QuoteString(shop))); err != nil {
		t.Fatalf("seed the store: %v", err)
	}

	// The real save, then the real begin, as the developer.
	devCtx := actorCtx(dev, auth.RoleDeveloper)
	if _, err := eng.Execute(devCtx, fmt.Sprintf(`builtin shopifyStoreAppSave(siteId: %s, clientId: "cb-client", clientSecret: %s)`,
		langparser.QuoteString(mine), langparser.QuoteString(appSecret))); err != nil {
		t.Fatalf("save: %v", err)
	}
	out := builtinReply(t, eng, devCtx, fmt.Sprintf(`builtin shopifyConnectBegin(siteId: %s, returnPath: "/")`, langparser.QuoteString(mine)))
	if out["reason"] != connectReasonOK {
		t.Fatalf("begin: %v", out)
	}
	authorize, err := url.Parse(out["authorizeUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	stateValue := authorize.Query().Get("state")
	store := &componentIdentity.Store{Engine: eng}
	row, err := store.LookupGithubConnectState(context.Background(), componentIdentity.HashConnectState(stateValue))
	if err != nil || row == nil {
		t.Fatalf("lookup: row=%v err=%v", row, err)
	}

	params := map[string]string{"code": "cb-code", "shop": shop, "state": stateValue, "timestamp": "1337178173"}

	// Steps 4-5, as the identity route runs them: the pending secret from its
	// sealed row, and the state stays unspent either way.
	var verified, forged bool
	_ = asIdentityCallback(func(ctx context.Context) error {
		verified = conn.VerifyShopifyCallback(ctx, row, signedQuery(appSecret, params))
		forged = conn.VerifyShopifyCallback(ctx, row, signedQuery("not-the-app-secret", params))
		return nil
	})
	if !verified || forged {
		t.Fatalf("verify: Shopify's signature %v, a forged one %v", verified, forged)
	}
	if again, _ := store.LookupGithubConnectState(context.Background(), row.StateHash); again == nil || !again.ConsumedAt.IsZero() {
		t.Fatal("a verify spent the state")
	}

	authorizeAs := func() (componentIdentity.ShopifyConnectGrant, string) {
		var grant componentIdentity.ShopifyConnectGrant
		var result string
		_ = asIdentityCallback(func(ctx context.Context) error {
			grant, result = conn.AuthorizeShopifyConnect(ctx, row, "cb-code")
			return nil
		})
		return grant, result
	}

	// The reachable positive: the developer who began passes step 8 and the
	// code is exchanged with the pending pair.
	grant, result := authorizeAs()
	if result != "" || grant.Role != string(auth.RoleDeveloper) || grant.AccessToken != callbackAdminToken || grant.StoreID != storeID {
		t.Fatalf("the developer's callback: result %q grant %v", result, grant)
	}
	if token.count() != 1 || token.forms[0].Get("client_id") != "cb-client" || token.forms[0].Get("client_secret") != appSecret {
		t.Fatalf("the exchange: %d requests %v", token.count(), token.forms)
	}

	// Demoted between begin and callback: permission_lost, and the code never
	// reaches Shopify. An admin holds no store part and ranks below the store's
	// read floor; a writer holds neither.
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleWriter} {
		setRole(role)
		before := token.count()
		if grant, result := authorizeAs(); result != connectReasonPermissionLost || grant.AccessToken != "" {
			t.Errorf("a person who is %s at the callback: result %q", role, result)
		}
		if token.count() != before {
			t.Errorf("a person who is %s at the callback reached Shopify", role)
		}
	}

	// And back: the refusal was the role, not the rows.
	setRole(auth.RoleDeveloper)
	if _, result := authorizeAs(); result != "" {
		t.Fatalf("the developer again: %q", result)
	}
}

// TestAFirstConnectIsJudgedOnTheStoreTierBeforeAnyWrite: a writer GRANTED the
// store part holds execute app:deployables/store and writes their own
// storefront, and ranks below v1:shopify:store's read floor (developer). On a
// FIRST Connect there is no store row to read back, so step 8 has to ask the
// tier itself -- the question the attach's binding guard asks at step 14, once
// step 12 has made a row. Asked any later, the Admin token is sealed, the
// pending app promoted and a store row created before the attach refuses them.
func TestAFirstConnectIsJudgedOnTheStoreTierBeforeAnyWrite(t *testing.T) {
	eng, raw := authorityEngine(t)
	if eng == nil {
		return
	}
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("cd", 32))
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.connect.example.test")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)

	conn := NewConnector(eng, slog.New(slog.DiscardHandler), NewStoreRegistry(eng, eng.ResolveSystemSecret), NewAdminClient())
	if err := eng.RegisterIntegration(NewIntegration(conn)); err != nil {
		t.Fatalf("register the shopify integration: %v", err)
	}
	token := newFakeTokenEndpoint(t)
	conn.connectTokenURL = func(string) string { return token.server.URL + "/admin/oauth/access_token" }
	// A test engine never Starts, so the grant source is installed by hand, and
	// cleared so no later test in the package reads it.
	eng.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})

	writer := "fc-writer-" + suffix
	shop := "fc-" + suffix + ".myshopify.com"
	storeID := "fc-" + suffix
	pkg := "v1:platform:package:fc-" + suffix
	site := "fc-sf-" + suffix
	grantID := auth.GrantRowID("user", writer, "execute", "app:deployables/store")

	var cleanup []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range append(cleanup, "v1:rbac:grant:"+grantID) {
			_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id = $1`, id)
		}
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' LIKE $1`,
			"SHOPIFY_FC-"+suffix+"%")
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:githubConnectState' AND payload->>'userId' LIKE $1`, "%"+writer)
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`, shop)
	})
	seed := func(concept, id string, payload map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), string(body))
		if _, err := eng.Execute(seedCtx(), q); err != nil {
			t.Fatalf("seed %s %s: %v", concept, id, err)
		}
		cleanup = append(cleanup, concept+":"+id)
	}
	setRole := func(role auth.Role) {
		seed("v1:identity:user", writer, map[string]any{
			"displayName": "First Connect", "primaryEmail": writer + "@connect.example.test", "role": string(role), "active": true,
		})
	}
	setRole(auth.RoleWriter)
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf(
		`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "execute", resourceType: "app:deployables/store", effect: "allow", grantedBy: "fc-seeder")`,
		langparser.QuoteString(grantID), langparser.QuoteString(writer))); err != nil {
		t.Fatalf("grant the store part: %v", err)
	}
	// Rows seeded by the deployment land in its own organization ("self",
	// memql#5598), which a developer reaches by rank and a writer only as a
	// member.
	scaffold := auth.ContextWithToken(auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "fc-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	})), &auth.TokenInfo{Subject: "fc-seeder"})
	group := "fc-self-" + suffix
	for _, q := range []string{
		fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "First Connect", kind: "account", accountId: "self", status: "active")`, langparser.QuoteString(group)),
		fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: "self")`,
			langparser.QuoteString(group+"-"+writer), langparser.QuoteString(group), langparser.QuoteString(writer)),
	} {
		if _, err := eng.Execute(scaffold, q); err != nil {
			t.Fatalf("seed the organization membership: %v", err)
		}
	}
	cleanup = append(cleanup, "v1:identity:group:"+group, "v1:identity:groupMembership:"+group+"-"+writer)
	seed("v1:platform:site", site, map[string]any{
		"ownerUserId": writer, "title": site, "hostname": site + ".connect.example.test", "status": "draft",
		"kind": "shopify_storefront", "bundleRef": "blob://sites/" + site + "/v1/",
		"packageId": pkg, "packageDeployableName": "storefront",
	})
	seed("v1:platform:package", strings.TrimPrefix(pkg, "v1:platform:package:"), map[string]any{
		"ownerUserId": writer, "name": "storefront", "sourceKind": "artifact", "status": "active",
	})
	seed("v1:platform:packageDeployment", "fc-run-"+suffix, map[string]any{
		"ownerUserId": writer, "packageId": pkg, "status": "succeeded",
		"report": map[string]any{"deployables": []any{map[string]any{
			"name": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"store": shop},
		}}},
		"deployables": []any{map[string]any{"name": "storefront", "siteId": "v1:platform:site:" + site}},
	})

	// The grant is real: the builtins' @requiresCapability admits the writer,
	// who saves and begins with NO store row anywhere.
	writerCtx := actorCtx(writer, auth.RoleWriter)
	if out := builtinReply(t, eng, writerCtx, fmt.Sprintf(`builtin shopifyStoreAppSave(siteId: %s, clientId: "fc-client", clientSecret: "fc-app-secret")`,
		langparser.QuoteString(site))); out["reason"] != connectReasonOK {
		t.Fatalf("save: %v", out)
	}
	out := builtinReply(t, eng, writerCtx, fmt.Sprintf(`builtin shopifyConnectBegin(siteId: %s, returnPath: "/")`, langparser.QuoteString(site)))
	if out["reason"] != connectReasonOK {
		t.Fatalf("begin: %v", out)
	}
	authorize, err := url.Parse(out["authorizeUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	row, err := (&componentIdentity.Store{Engine: eng}).LookupGithubConnectState(context.Background(), componentIdentity.HashConnectState(authorize.Query().Get("state")))
	if err != nil || row == nil {
		t.Fatalf("lookup: row=%v err=%v", row, err)
	}
	authorizeAs := func() string {
		var result string
		_ = asIdentityCallback(func(ctx context.Context) error {
			_, result = conn.AuthorizeShopifyConnect(ctx, row, "fc-code")
			return nil
		})
		return result
	}

	if got := authorizeAs(); got != connectReasonPermissionLost {
		t.Fatalf("a writer who cannot read stores finished a first Connect's checks: %q", got)
	}
	if token.count() != 0 {
		t.Error("the code reached Shopify for a person the attach would refuse")
	}
	var stores, sealed int
	if err := raw.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`, shop).Scan(&stores); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:platform:globalSecret' AND payload->>'name' IN ($1, $2)`,
		storeSecretName(storeID, suffixAdminToken), storeSecretName(storeID, suffixWebhookSecret)).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if stores != 0 || sealed != 0 {
		t.Fatalf("a refused first Connect wrote %d store rows and %d live credential rows", stores, sealed)
	}
	if pending, err := conn.pendingClientID(context.Background(), storeID); err != nil || pending != "fc-client" {
		t.Fatalf("the pending app did not stay pending: %q %v", pending, err)
	}

	// And the reachable positive over the same rows: at the floor, the same
	// person passes.
	setRole(auth.RoleDeveloper)
	if got := authorizeAs(); got != "" {
		t.Fatalf("a developer's first Connect: %q", got)
	}
}
