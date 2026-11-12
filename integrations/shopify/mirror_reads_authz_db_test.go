package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// mirror_reads_authz_db_test.go -- who reaches the mirror through a builtin,
// against a real engine over a real Postgres.
//
// The connector reads its mirror as the operator (F10), and under that
// identity the generated reads' actor.isClusterOwner conjunct admits every
// read. So the builtins that read the mirror for a caller are held to the
// caller in the handler: a person who is neither the store's owner nor a
// cluster owner is refused before any mirror read, and the two who may are
// answered with the rows.

const (
	authzStoreA     = "dbtest-mirror-a"
	authzStoreB     = "dbtest-mirror-b"
	authzOwnerA     = "dbtest-owner-a"
	authzOwnerB     = "dbtest-owner-b"
	authzWriter     = "dbtest-writer"
	authzProduct    = "gid://shopify/Product/4801"
	authzOrder      = "gid://shopify/Order/4801"
	authzCompany    = "gid://shopify/Company/4801"
	authzLineItem   = "gid://shopify/LineItem/4801"
	authzOrderCount = 1
)

// reset and ran extend connect_hop_db_test.go's recordingEngine: forget
// what ran, and name which of the given reads ran since.
func (r *recordingEngine) reset() {
	r.mu.Lock()
	r.stmts = nil
	r.mu.Unlock()
}

func (r *recordingEngine) ran(names ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.stmts {
		for _, n := range names {
			if callName(q) == n {
				out = append(out, n)
			}
		}
	}
	return out
}

// seedOwnedStore writes a store row through createStore with its owner.
func seedOwnedStore(t *testing.T, eng *memql.MemQLEngine, db *sql.DB, storeID, owner string) {
	t.Helper()
	domain := storeID + ".myshopify.com"
	call := renderCall("createStore", map[string]any{
		"storeId": storeID, "domain": domain, "name": storeID,
		"adminTokenRef": "shopify-admin-" + storeID, "apiVersion": generated.APIVersion,
		"ownerUserId": owner,
	})
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed store %s: %v", storeID, err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`, domain)
	})
}

// cleanStoreMirror removes one store's rows of the concepts, now and after.
func cleanStoreMirror(t *testing.T, db *sql.DB, storeID string, concepts ...string) {
	t.Helper()
	clean := func() {
		for _, concept := range concepts {
			_, _ = db.ExecContext(context.Background(),
				`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
				generated.ConceptID(concept), storeID)
		}
	}
	clean()
	t.Cleanup(clean)
}

// writeStoreMirrored writes obj for storeID through the mirror write path.
func writeStoreMirrored(t *testing.T, c *Connector, storeID, concept string, obj map[string]any) {
	t.Helper()
	for _, w := range mapObject(generated.Types[concept], storeID, obj, "", time.Now()) {
		if err := c.writeMirror(context.Background(), w); err != nil {
			t.Fatalf("write %s %v for %s: %v", concept, obj["id"], storeID, err)
		}
	}
}

// authzConnector is a connector over the recording engine, with store A's
// product and one company order mirrored, and store B owned by somebody
// else.
func authzConnector(t *testing.T) (*Connector, *recordingEngine) {
	t.Helper()
	eng, db := authorityEngine(t)
	if eng == nil {
		return nil, nil
	}
	seedOwnedStore(t, eng, db, authzStoreA, authzOwnerA)
	seedOwnedStore(t, eng, db, authzStoreB, authzOwnerB)
	cleanStoreMirror(t, db, authzStoreA, "product", "order", "orderLineItem")
	rec := &recordingEngine{MemQLEngine: eng}
	registry := NewStoreRegistry(rec, func(context.Context, string) (string, error) { return "shpat_test", nil })
	registry.ttl = 0
	c := NewConnector(rec, slog.New(slog.DiscardHandler), registry, NewAdminClient())

	created := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	writeStoreMirrored(t, c, authzStoreA, "product", map[string]any{
		"id": authzProduct, "title": "Authz probe", "handle": "authz-probe", "updatedAt": created,
	})
	writeStoreMirrored(t, c, authzStoreA, "order", map[string]any{
		"id": authzOrder, "name": "#4801", "createdAt": created, "updatedAt": created, "fullyPaid": true,
		"purchasingEntity": map[string]any{"__typename": "PurchasingCompany", "company": map[string]any{"id": authzCompany}},
		"lineItems": map[string]any{"nodes": []any{map[string]any{
			"id": authzLineItem, "name": "probe line", "title": "Authz probe", "quantity": 2, "currentQuantity": 2,
			"product": map[string]any{"id": authzProduct},
		}}},
	})
	return c, rec
}

// mirrorReads names every read these builtins make of the mirror.
var mirrorReads = []string{
	"shopifyProductForStore", "shopifyProductByGid", "ordersInWindow", "lineItemsForOrder",
	"ordersByCompany", "paymentTermsOutstanding", "creditLimitsForCompany",
}

func TestOnlyTheStoresOwnerAndAClusterOwnerReadTheMirrorThroughABuiltin(t *testing.T) {
	c, rec := authzConnector(t)
	if c == nil {
		return
	}
	integ := NewIntegration(c)

	type answer func(t *testing.T, payload map[string]any)
	builtins := []struct {
		name   string
		handle mirrorHandler
		args   map[string]any
		check  answer
	}{
		{"shopifyFetchProduct", integ.handleFetchProduct, map[string]any{"handle": "authz-probe"}, func(t *testing.T, p map[string]any) {
			if p["gid"] != authzProduct {
				t.Errorf("shopifyFetchProduct = %v, want the mirrored product", p)
			}
		}},
		{"commerceSold", integ.handleCommerceSold, map[string]any{}, func(t *testing.T, p map[string]any) {
			if p["orders"] != float64(authzOrderCount) || p["units"] != float64(2) {
				t.Errorf("commerceSold = %v, want the one order and its two units", p)
			}
		}},
		{"commerceCompany", integ.handleCommerceCompany, map[string]any{"companyGid": authzCompany}, func(t *testing.T, p map[string]any) {
			if p["orders"] != float64(authzOrderCount) {
				t.Errorf("commerceCompany = %v, want the company's one order", p)
			}
		}},
	}

	refused := map[string]context.Context{
		"a signed-in writer who owns no store": actorCtx(authzWriter, auth.RoleWriter),
		"the owner of another store":           actorCtx(authzOwnerB, auth.RoleWriter),
	}
	admitted := map[string]context.Context{
		"the store's owner": actorCtx(authzOwnerA, auth.RoleWriter),
		"a cluster owner":   ownerCtx(),
	}

	for _, b := range builtins {
		args := withStore(b.args, authzStoreA)
		for who, ctx := range refused {
			rec.reset()
			_, err := b.handle(ctx, args, 0)
			if err == nil || !strings.Contains(err.Error(), actRefusal) {
				t.Errorf("%s for %s: err = %v, want the refusal", b.name, who, err)
			}
			if ran := rec.ran(mirrorReads...); len(ran) != 0 {
				t.Errorf("%s for %s: the mirror was read (%v) before the refusal", b.name, who, ran)
			}
		}
		for who, ctx := range admitted {
			rec.reset()
			nodes, err := b.handle(ctx, args, 0)
			if err != nil {
				t.Errorf("%s for %s: %v", b.name, who, err)
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
				t.Fatalf("%s: %v", b.name, err)
			}
			b.check(t, payload)
			if len(rec.ran(mirrorReads...)) == 0 {
				t.Errorf("%s for %s answered without reading the mirror", b.name, who)
			}
		}
	}
}
