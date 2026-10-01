package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// Other packages share CI's database. The real query still runs, but only this
// test's queued row reaches the fake Shopify transport.
type scopedPropagationQueue struct {
	engine *memql.MemQLEngine
	rowID  string
}

func (q scopedPropagationQueue) Execute(ctx context.Context, query string) (any, error) {
	res, err := q.engine.Execute(ctx, query)
	if err != nil || !strings.HasPrefix(query, "query outboxPending(") {
		return res, err
	}
	var rows []map[string]any
	for _, row := range memql.MaterializeRows(res) {
		if ref, _ := row["rowRef"].(string); memql.BareShortId(ref) == q.rowID {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type propagationRoundTripper func(*http.Request) (*http.Response, error)

func (f propagationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This composition belongs in the root module: the worker depends on the
// connector, and the independently buildable integrations module cannot import
// back into the worker. Both production constructors are used unchanged.
func TestProductContentDrainsToShopifyWithSourceAuthority(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	rowID := fmt.Sprintf("dbtest-content-drain-%d", time.Now().UnixNano())
	storeID := rowID + "-store"
	canonicalID := "v1:commerce:productContent:" + rowID
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:outboxEntry' AND payload->>'rowRef' = $1`, canonicalID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:commerce:productContent' AND id = $1`, canonicalID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = $1`, "v1:shopify:store:"+storeID)
	})
	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	owner := auth.ContextWithClaims(context.Background(), claims)
	owner = auth.ContextWithToken(owner, auth.BuildTokenInfo(claims))
	owner = auth.ContextWithAccess(owner, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner})
	if _, err := eng.Execute(owner, fmt.Sprintf(`mutation createStore(storeId: %q, domain: "propagation-test.myshopify.com", adminTokenRef: "test-admin", apiVersion: "2026-07")`, storeID)); err != nil {
		t.Fatalf("write test store: %v", err)
	}
	writeContent := func(summary string) {
		t.Helper()
		call := fmt.Sprintf(`mutation upsertProductContent(contentId: %q, storeId: %q, productGid: "gid://shopify/Product/1", summary: %q, description: "A product description", keywords: ["dental care"], blocks: {tagline: "Source authority"})`, rowID, storeID, summary)
		if _, err := eng.Execute(owner, call); err != nil {
			t.Fatalf("write product content and queue delivery: %v", err)
		}
	}
	writeContent("Earlier copy before an edit")
	read := fmt.Sprintf(`query productContentForPropagation(rowId: %q)`, rowID)
	connector := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
	res, err := eng.Execute(connector, read)
	if err != nil || len(memql.MaterializeRows(res)) != 1 {
		t.Fatalf("named Shopify connector cannot read its source: %v, rows=%v", err, memql.MaterializeRows(res))
	}
	other := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "quickBooks"))
	res, err = eng.Execute(other, read)
	if err == nil && len(memql.MaterializeRows(res)) != 0 {
		t.Fatal("another connector could read Shopify's source")
	}
	if _, err := eng.Execute(owner, read); err == nil {
		t.Fatal("the propagation read was exposed to a browser caller")
	}
	// The first read primed the normal query cache. Both queued versions must
	// deliver the current source, not their old version or a cached projection.
	writeContent("Current copy from the real source")
	var deliveredBodies []string
	previousTransport := http.DefaultTransport
	// This package's tests are serial. Intercept every HTTP call and reject
	// unexpected hosts; the fixture cannot accidentally reach a live store.
	http.DefaultTransport = propagationRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "propagation-test.myshopify.com" {
			return nil, fmt.Errorf("unexpected external request: %s", r.URL.Host)
		}
		var request struct {
			OperationName string          `json:"operationName"`
			Variables     json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		field := "metafieldDefinitionCreate"
		if request.OperationName == "ShopifyMetafieldsSet" {
			deliveredBodies = append(deliveredBodies, string(request.Variables))
			field = "metafieldsSet"
		} else if request.OperationName != "ShopifyMetafieldDefinitionCreate" {
			return nil, fmt.Errorf("unexpected Shopify operation: %s", request.OperationName)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":{"` + field + `":{"userErrors":[]}}}`)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	stores := shopify.NewStoreRegistry(eng, func(context.Context, string) (string, error) { return "test-only-admin-token", nil })
	logger := slog.New(slog.DiscardHandler)
	shopifyConnector := shopify.NewConnector(eng, logger, stores, shopify.NewAdminClient())
	if err := memqlsync.Bind(shopifyConnector); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(shopifyConnector.Name()) })
	worker := NewWorker(scopedPropagationQueue{engine: eng, rowID: rowID}, nil, logger)
	worker.DrainOnce(context.Background())
	if len(deliveredBodies) != 2 {
		t.Fatalf("real outbox delivered %d requests, want both queued versions", len(deliveredBodies))
	}
	for _, body := range deliveredBodies {
		if !strings.Contains(body, "Current copy from the real source") || strings.Contains(body, "Earlier copy before an edit") {
			t.Fatalf("queued version delivered stale source copy: %s", body)
		}
	}
	var delivered int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:platform:outboxEntry' AND payload->>'rowRef' = $1 AND payload->>'status' = 'delivered'`, canonicalID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 2 {
		t.Fatalf("persisted delivered count = %d, want both queued versions delivered", delivered)
	}
}
