package shopify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

type webhookEngineAdapter struct{ *memql.MemQLEngine }

func (e webhookEngineAdapter) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

// Receipt on A, durable row transfer, the shipped automation and connector on B.
// The two engines and StoreRegistry instances share only Postgres. No request
// headers or receive timestamps are injected into the worker by the test.
func TestWebhookReceiptAndDispatchAcrossEngines(t *testing.T) {
	a, db := authorityEngine(t)
	if a == nil {
		return
	}
	b, _ := authorityEngine(t)
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ae", 32))
	suffix := fmt.Sprint(time.Now().UnixNano())
	storeID := "webhook-" + suffix
	shop := storeID + ".myshopify.com"
	secretName := "SHOPIFY_WEBHOOK_TEST_" + suffix
	secret := "delivery-secret-" + suffix
	must := func(q string) {
		t.Helper()
		if _, err := a.Execute(seedCtx(), q); err != nil {
			t.Fatal(err)
		}
	}
	must(renderCall("createStore", map[string]any{"storeId": storeID, "domain": shop, "name": "Webhook test", "adminTokenRef": "unused", "webhookSecretRef": secretName}))
	must(renderCall("setStoreStatus", map[string]any{"storeId": storeID, "status": StatusPaused}))
	if _, err := seedSecret(seedCtx(), a, secretName, secret, "Webhook test", "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE payload::text LIKE $1 OR id LIKE $1`, "%"+suffix+"%")
	})
	ca := NewConnector(a, nil, NewStoreRegistry(a, a.ResolveSystemSecret), NewAdminClient())
	cb := NewConnector(b, nil, NewStoreRegistry(b, b.ResolveSystemSecret), NewAdminClient())
	if err := memqlsync.Bind(ca); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(ConnectorName) })
	body := fmt.Sprintf(`{"shop_domain":%q,"customer":{"id":991},"data_request":{"id":%q}}`, shop, suffix)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/inbound/shopify-"+storeID, strings.NewReader(body))
	req.Header.Set(HeaderHMAC, base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set(HeaderTopic, TopicDataRequest)
	req.Header.Set(HeaderShopDomain, shop)
	req.Header.Set("Authorization", "Bearer must-not-persist")
	response := httptest.NewRecorder()
	inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 4096}, webhookEngineAdapter{a}, nil).ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("receipt: %d %s", response.Code, response.Body)
	}
	var receipt map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	result, err := b.Execute(seedCtx(), renderCall("inboundRequestById", map[string]any{"requestId": receipt["id"]}))
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		t.Fatalf("engine B read %d staged rows", len(rows))
	}
	row := rows[0]
	if strings.Contains(mapString(row, "headersJson"), "must-not-persist") {
		t.Fatal("credential persisted")
	}
	// Transfer only the durable event over a serialization boundary, then
	// replace the receipt-side connector with B's independent instance.
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var received map[string]any
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatal(err)
	}
	memqlsync.UnbindForTest(ConnectorName)
	if err := memqlsync.Bind(cb); err != nil {
		t.Fatal(err)
	}
	if err := b.RegisterIntegration(datasync.NewIntegration(webhookEngineAdapter{b}, nil)); err != nil {
		t.Fatal(err)
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: b.Logger})
	auto, err := loader.LoadByName("dispatchInboundToConnector")
	if err != nil {
		t.Fatal(err)
	}
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: b, Logger: b.Logger, StepRegistry: steps.NewRegistry()})
	defer executor.Close()
	event := &events.Event{Topic: "graph.node.created.v1:platform:inboundRequest", Kind: events.KindNodeCreated, OriginNodeId: "receiver-a", Payload: received}
	run, err := executor.ExecuteWithEvent(seedCtx(), auto, "webhook-hop-test", event)
	if err != nil || run.Status != "completed" {
		t.Fatalf("dispatch: %+v %v", run, err)
	}
	jobID := ComplianceJobID(storeID, TopicDataRequest, "gid://shopify/Customer/991\x00"+suffix)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:shopify:complianceJob' AND id=$1`, "v1:shopify:complianceJob:"+jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cross-engine delivery queued %d jobs, want 1", count)
	}
}
