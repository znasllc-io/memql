package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// shopify_store_guard_db_test.go -- memql#5626, the store write guard through
// the real createStore / updateStore / setStoreStatus mutations.
//
// TestTheStoreGuardHoldsTheTokenReferenceToTheStoresOwnName drives the guard
// directly, which would keep passing if executeWrite stopped calling it. This
// proves it is reached from every writer the console and the connector use,
// and that a store written before the rule -- seeded here straight into the
// table, the way such a row exists on a running cluster -- stays writable.
//
// Postgres-gated like its neighbours; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.

func TestTheStoreGuardIsReachedFromTheStoreMutations(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("storeguard")
	storeID := "store-guard-" + suffix
	legacyID := "store-legacy-" + suffix
	t.Cleanup(func() {
		for _, id := range []string{storeID, legacyID, "Store-Guard-" + suffix} {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", conceptShopifyStore+":"+id).Exec(context.Background())
		}
	})
	ctx := systemSiteCtx()

	// A FREE-TEXT NAME IS REFUSED AT CREATION, naming the one it must be --
	// the case MemQL OS's "Register a store" form produced.
	_, err := runSiteMutation(t, ctx, eng, "createStore", map[string]any{
		"storeId": storeID, "domain": storeID + ".myshopify.com", "storefrontTokenRef": "ACME_STOREFRONT_TOKEN",
	})
	if err == nil || !strings.Contains(err.Error(), StorefrontTokenSecretName(storeID)) {
		t.Fatalf("createStore with a free-text token name: want a refusal naming %s, got %v", StorefrontTokenSecretName(storeID), err)
	}
	// A STORE ID THAT CANNOT NAME A TOKEN IS REFUSED AT CREATION.
	if _, err := runSiteMutation(t, ctx, eng, "createStore", map[string]any{
		"storeId": "Store-Guard-" + suffix, "domain": "upper-" + suffix + ".myshopify.com",
	}); err == nil || !strings.Contains(err.Error(), "store id") {
		t.Fatalf("createStore with an upper-case id: want a store-id refusal, got %v", err)
	}

	// THE REACHABLE POSITIVE, and the changes after it.
	if _, err := runSiteMutation(t, ctx, eng, "createStore", map[string]any{
		"storeId": storeID, "domain": storeID + ".myshopify.com", "storefrontTokenRef": StorefrontTokenSecretName(storeID),
	}); err != nil {
		t.Fatalf("createStore naming the store's own token was refused: %v", err)
	}
	if _, err := runSiteMutation(t, ctx, eng, "updateStore", map[string]any{
		"storeId": storeID, "storefrontTokenRef": "SHOPIFY_" + strings.ToUpper(storeID) + "_ADMIN_TOKEN",
	}); err == nil {
		t.Fatal("updateStore re-pointed the Storefront token at the Admin token")
	}
	if _, err := runSiteMutation(t, ctx, eng, "updateStore", map[string]any{
		"storeId": storeID, "storefrontTokenRef": "",
	}); err != nil {
		t.Fatalf("clearing the Storefront token reference was refused: %v", err)
	}

	// A STORE WRITTEN BEFORE THE RULE STAYS WRITABLE. Seeded straight into the
	// table with a free-text reference, as such a row sits on a running
	// cluster today; the connector's status writes and an unrelated rename
	// both carry that inherited reference through the read-merge.
	payload, err := json.Marshal(map[string]any{
		"domain": legacyID + ".myshopify.com", "name": "legacy", "status": "configured",
		"storefrontTokenRef": "LEGACY_STOREFRONT_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
		 VALUES (?, ?, 'store-guard-test', ?, 'object', '{}'::jsonb, ?::jsonb, '{}'::jsonb, '{"kind":"direct","name":"store-guard-test"}'::jsonb)`,
		conceptShopifyStore+":"+legacyID, time.Now().UTC(), conceptShopifyStore, string(payload)); err != nil {
		t.Fatalf("seeding the legacy store: %v", err)
	}
	if _, err := runSiteMutation(t, ctx, eng, "setStoreStatus", map[string]any{"storeId": legacyID, "status": "paused"}); err != nil {
		t.Fatalf("a status write to a store from before the rule was refused: %v", err)
	}
	if _, err := runSiteMutation(t, ctx, eng, "updateStore", map[string]any{"storeId": legacyID, "name": "renamed"}); err != nil {
		t.Fatalf("a rename of a store from before the rule was refused: %v", err)
	}
	// And it is repaired by naming its own token.
	if _, err := runSiteMutation(t, ctx, eng, "updateStore", map[string]any{
		"storeId": legacyID, "storefrontTokenRef": StorefrontTokenSecretName(legacyID),
	}); err != nil {
		t.Fatalf("repairing the legacy store's reference was refused: %v", err)
	}
}
