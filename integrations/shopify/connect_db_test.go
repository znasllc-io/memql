package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// connect_db_test.go -- Connect Shopify's store resolution and pending save
// against a real engine over a real Postgres (design 12.1, 12.3).
//
// The fakes in connect_test.go cannot hold three things this file does: the
// site tier deciding who reads and writes a storefront, the builtin gate asking
// the store part before the handler runs, and the sealed rows coming back
// through the resolver every other reader uses. Each refusal below sits beside
// a reachable positive over the same rows, so a zero is never unfalsifiable.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.

func TestConnectResolvesAndSavesAgainstARealEngine(t *testing.T) {
	eng, raw := authorityEngine(t)
	if eng == nil {
		return
	}
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("cd", 32))
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)

	conn := NewConnector(eng, slog.New(slog.DiscardHandler), NewStoreRegistry(eng, eng.ResolveSystemSecret), NewAdminClient())
	if err := eng.RegisterIntegration(NewIntegration(conn)); err != nil {
		t.Fatalf("register the shopify integration: %v", err)
	}

	dev := "connect-dev-" + suffix
	stranger := "connect-stranger-" + suffix
	shop := "connect-" + suffix + ".myshopify.com"
	storeID := "connect-" + suffix
	redactedShop := "connect-gone-" + suffix + ".myshopify.com"
	redactedID := "connect-gone-" + suffix

	var cleanup []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range cleanup {
			_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id = $1`, id)
		}
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' LIKE $1`,
			"SHOPIFY_CONNECT-%"+suffix+"%")
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' IN ($1, $2)`, shop, redactedShop)
	})
	seedWith := func(ctx context.Context, concept, id string, payload map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), string(body))
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("seed %s %s: %v", concept, id, err)
		}
		cleanup = append(cleanup, concept+":"+id)
	}
	seed := func(concept, id string, payload map[string]any) {
		t.Helper()
		seedWith(seedCtx(), concept, id, payload)
	}
	siteWith := func(ctx context.Context, id, owner, kind, pkg string) {
		seedWith(ctx, "v1:platform:site", id, map[string]any{
			"ownerUserId": owner, "title": id, "hostname": id + ".connect.example.test", "status": "draft",
			"kind": kind, "bundleRef": "blob://sites/" + id + "/v1/",
			"packageId": pkg, "packageDeployableName": "storefront",
		})
	}
	site := func(id, owner, kind, pkg string) { siteWith(seedCtx(), id, owner, kind, pkg) }
	run := func(id, owner, pkg, siteId, named string) {
		seed("v1:platform:packageDeployment", id, map[string]any{
			"ownerUserId": owner, "packageId": pkg, "status": "succeeded",
			"report": map[string]any{"deployables": []any{map[string]any{
				"name": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"store": named},
			}}},
			"deployables": []any{map[string]any{"name": "storefront", "siteId": "v1:platform:site:" + siteId}},
		})
	}

	pkg := "v1:platform:package:connect-" + suffix
	mine := "connect-sf-" + suffix
	// The package the run names: a packageDeployment's packageId must resolve
	// to a package of the same organization (memql#5598).
	seed("v1:platform:package", strings.TrimPrefix(pkg, "v1:platform:package:"), map[string]any{
		"ownerUserId": dev, "name": "storefront", "sourceKind": "artifact", "status": "active",
	})
	site(mine, dev, "shopify_storefront", pkg)
	run("connect-run-"+suffix, dev, pkg, mine, shop)

	theirs := "connect-theirs-" + suffix
	siteWith(untiedSeedCtx(), theirs, stranger, "shopify_storefront", pkg)

	static := "connect-static-" + suffix
	site(static, dev, "static", pkg)

	unpublished := "connect-unpub-" + suffix
	site(unpublished, dev, "shopify_storefront", "v1:platform:package:connect-none-"+suffix)

	purged := "connect-purged-" + suffix
	purgedPkg := "v1:platform:package:connect-purged-" + suffix
	seed("v1:platform:package", strings.TrimPrefix(purgedPkg, "v1:platform:package:"), map[string]any{
		"ownerUserId": dev, "name": "storefront", "sourceKind": "artifact", "status": "active",
	})
	site(purged, dev, "shopify_storefront", purgedPkg)
	run("connect-purged-run-"+suffix, dev, purgedPkg, purged, redactedShop)
	for _, q := range []string{
		fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s)`, langparser.QuoteString(redactedID), langparser.QuoteString(redactedShop)),
		fmt.Sprintf(`mutation markStoreRedacted(storeId: %s, redactedAt: "2026-09-01T00:00:00Z")`, langparser.QuoteString(redactedID)),
	} {
		if _, err := eng.Execute(seedCtx(), q); err != nil {
			t.Fatalf("seed the purged store: %v", err)
		}
	}

	devCtx := actorCtx(dev, auth.RoleDeveloper)
	reply := func(t *testing.T, ctx context.Context, call string) map[string]any {
		t.Helper()
		return builtinReply(t, eng, ctx, call)
	}
	status := func(t *testing.T, siteId string) map[string]any {
		t.Helper()
		return reply(t, devCtx, fmt.Sprintf(`builtin shopifyConnectStatus(siteId: %s)`, langparser.QuoteString(siteId)))
	}

	// 12.1's refusals, each against rows that exist.
	for siteId, want := range map[string]string{
		theirs:      connectReasonSiteNotWritable,
		static:      connectReasonNotAStorefront,
		unpublished: connectReasonStoreNotNamed,
		purged:      connectReasonStoreRedacted,
	} {
		if got := status(t, siteId)["reason"]; got != want {
			t.Errorf("status of %s: reason %v, want %s", siteId, got, want)
		}
	}

	// The reachable positive for all four: the developer's own published
	// storefront resolves, with nothing saved yet.
	got := status(t, mine)
	if got["reason"] != connectReasonOK || got["storeId"] != storeID || got["shopDomain"] != shop || got["appSaved"] != false {
		t.Fatalf("status of the developer's storefront: %v", got)
	}

	// Below the store part the builtin gate refuses before the handler runs:
	// a writer's save writes nothing.
	writerCtx := actorCtx("connect-writer-"+suffix, auth.RoleWriter)
	save := func(ctx context.Context, clientId, secretValue string) (*memql.ExecuteResult, error) {
		return eng.Execute(ctx, fmt.Sprintf(`builtin shopifyStoreAppSave(siteId: %s, clientId: %s, clientSecret: %s)`,
			langparser.QuoteString(mine), langparser.QuoteString(clientId), langparser.QuoteString(secretValue)))
	}
	if _, err := save(writerCtx, "writer-client", "writer-secret"); err == nil || !strings.Contains(err.Error(), "capability_not_held") {
		t.Fatalf("a writer's save was not refused by the store part: %v", err)
	}
	pendingID := storeSecretName(storeID, suffixPendingClientID)
	pendingSecret := storeSecretName(storeID, suffixPendingClientSecret)
	if v, err := eng.ResolveSystemVariable(ownerCtx(), pendingID); err == nil && v != "" {
		t.Fatalf("the refused save wrote %s", pendingID)
	}

	// The developer's save lands PENDING, and only pending.
	for _, secretValue := range []string{"first-secret-" + suffix, "second-secret-" + suffix} {
		if _, err := save(devCtx, "client-"+secretValue, secretValue); err != nil {
			t.Fatalf("the developer's save: %v", err)
		}
		if v, err := eng.ResolveSystemVariable(ownerCtx(), pendingID); err != nil || v != "client-"+secretValue {
			t.Fatalf("pending client id = %q (%v)", v, err)
		}
		if v, err := eng.ResolveSystemSecret(ownerCtx(), pendingSecret); err != nil || v != secretValue {
			t.Fatalf("the pending secret did not come back through the resolver (%v)", err)
		}
	}
	// The second save wrote at the FIRST save's row: one row per name.
	for _, name := range []string{pendingID, pendingSecret} {
		var n int
		if err := raw.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' = $1`,
			name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%d rows carry %s after two saves, want 1", n, name)
		}
	}
	res, err := eng.Execute(ownerCtx(), fmt.Sprintf(`query storeById(storeId: %s)`, langparser.QuoteString(storeID)))
	if err != nil {
		t.Fatal(err)
	}
	if rows := memql.MaterializeRows(res); len(rows) != 0 {
		t.Fatalf("a save created the store row: %v", rows)
	}
	var audits int
	if err := raw.QueryRowContext(context.Background(),
		`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1 AND payload->>'action' = 'shopify_app_saved' AND payload->>'actorUserId' IN ($2, 'v1:identity:user:' || $2) AND payload::text NOT LIKE $3`,
		storeID, dev, "%secret-"+suffix+"%").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Fatalf("%d clean shopify_app_saved audit events naming the developer, want 2", audits)
	}

	got = status(t, mine)
	if got["appSaved"] != true || got["pendingApp"] != true || got["connected"] != false {
		t.Fatalf("status after a save: %v", got)
	}

	// 12.5 over real bindings. The store exists now, the developer's site is
	// bound to it, and so is the stranger's -- spelled CANONICALLY, which the
	// binding field keeps as written and sitesBoundToStore must still find.
	storefront := newFakeStorefront(t)
	conn.storefrontEndpoint = func(string, string) (string, error) { return storefront.server.URL + "/api/graphql.json", nil }
	// The stranger's site is in no organization, so only server code may
	// change it (memql#5598 refuses an unattributed row's change otherwise).
	for _, w := range []struct {
		ctx context.Context
		q   string
	}{
		{seedCtx(), fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, adminTokenRef: "A")`, langparser.QuoteString(storeID), langparser.QuoteString(shop))},
		{seedCtx(), fmt.Sprintf(`mutation updateSiteStoreBinding(siteId: %s, storeId: %s)`, langparser.QuoteString(mine), langparser.QuoteString(storeID))},
		{untiedSeedCtx(), fmt.Sprintf(`mutation updateSiteStoreBinding(siteId: %s, storeId: %s)`, langparser.QuoteString(theirs), langparser.QuoteString("v1:shopify:store:"+storeID))},
	} {
		if _, err := eng.Execute(w.ctx, w.q); err != nil {
			t.Fatalf("seed the bound store: %s: %v", w.q, err)
		}
	}
	tokenSet := func(ctx context.Context, token string) map[string]any {
		t.Helper()
		return reply(t, ctx, fmt.Sprintf(`builtin shopifyStorefrontTokenSet(siteId: %s, token: %s)`,
			langparser.QuoteString(mine), langparser.QuoteString(token)))
	}
	pasted := "pasted-token-" + suffix
	if got := tokenSet(devCtx, pasted)["reason"]; got != connectReasonStoreInUse {
		t.Fatalf("a developer changed the token of a store bound to a stranger's site: reason %v", got)
	}
	if storefront.requests() != 0 {
		t.Fatal("the refused token set reached Shopify")
	}
	// The reachable positive: a cluster owner may, and the token lands sealed
	// and referenced.
	if got := tokenSet(ownerCtx(), pasted)["reason"]; got != connectReasonOK {
		t.Fatalf("a cluster owner's token set: reason %v", got)
	}
	res, err = eng.Execute(ownerCtx(), fmt.Sprintf(`query storeById(storeId: %s)`, langparser.QuoteString(storeID)))
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 || mapString(rows[0], "storefrontTokenRef") != storeSecretName(storeID, suffixStorefrontToken) {
		t.Fatalf("storefrontTokenRef after the token set: %v", rows)
	}
	if v, err := eng.ResolveSystemSecret(ownerCtx(), storeSecretName(storeID, suffixStorefrontToken)); err != nil || v != pasted {
		t.Fatalf("the Storefront token did not come back through the resolver (%v)", err)
	}
}

// builtinReply runs one top-level builtin call and decodes its one result node.
func builtinReply(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) map[string]any {
	t.Helper()
	res, err := eng.Execute(ctx, call)
	if err != nil {
		t.Fatalf("%s: %v", call, err)
	}
	// A top-level builtin's output is the id-keyed node map, which
	// MaterializeRows does not unwrap in-process; the wire does.
	nodes, _ := res.OutputPayload().(map[string]memorynodes.MemoryNode)
	if len(nodes) != 1 {
		t.Fatalf("%s answered %d nodes", call, len(nodes))
	}
	var out map[string]any
	for _, n := range nodes {
		if err := json.Unmarshal(n.Payload, &out); err != nil {
			t.Fatalf("%s: decode: %v", call, err)
		}
	}
	return out
}

// untiedSeedCtx writes a row as server code (a synthetic system actor), which
// the organization boundary (memql#5598) stamps no accountId on: a site in NO
// organization, owned by whoever its payload names. A cluster owner's seed
// lands in the operator organization, and a developer writes every
// organization-tied site (D3), so another person's site a developer may NOT
// write is one in no organization.
func untiedSeedCtx() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "connect-dbtest-seed"))
}
