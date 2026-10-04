// Package inboundhop proves the inbound webhook hop across two engines that
// share only Postgres: receipt on engine A through the inbound receiver, then
// the shipped dispatchInboundToConnector automation and an independent Shopify
// connector instance on engine B.
//
// It lives in the root module on purpose. The hop crosses three modules --
// component/inbound (the receiver), component/datasync (the dispatcher, a
// root-module package) and integrations/shopify (the connector) -- and the
// module-boundaries gate refuses the integrations module requiring the other
// two, even as test dependencies. The root module may depend on all of them,
// which is why the cross-module tests sit under test/ (memql#5705 moved a
// db-gated test the same way).
package inboundhop

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/inbound"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/integrations/shopify"
)

type engineAdapter struct{ *memql.MemQLEngine }

func (e engineAdapter) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

// Receipt on A, durable row transfer, the shipped automation and connector on B.
// The two engines and StoreRegistry instances share only Postgres. No request
// headers or receive timestamps are injected into the worker by the test.
func TestWebhookReceiptAndDispatchAcrossEngines(t *testing.T) {
	a, db := engineOverPostgres(t)
	if a == nil {
		return
	}
	b, _ := engineOverPostgres(t)
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ae", 32))
	suffix := fmt.Sprint(time.Now().UnixNano())
	storeID := "webhook-" + suffix
	shop := storeID + ".myshopify.com"
	secretName := "SHOPIFY_WEBHOOK_TEST_" + suffix
	secretValue := "delivery-secret-" + suffix
	must := func(q string) {
		t.Helper()
		if _, err := a.Execute(seedCtx(), q); err != nil {
			t.Fatal(err)
		}
	}
	must(call("createStore", map[string]string{"storeId": storeID, "domain": shop, "name": "Webhook test", "adminTokenRef": "unused", "webhookSecretRef": secretName}))
	must(call("setStoreStatus", map[string]string{"storeId": storeID, "status": shopify.StatusPaused}))
	seedSecret(t, a, secretName, secretValue)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE payload::text LIKE $1 OR id LIKE $1`, "%"+suffix+"%")
	})
	ca := shopify.NewConnector(a, nil, shopify.NewStoreRegistry(a, a.ResolveSystemSecret), shopify.NewAdminClient())
	cb := shopify.NewConnector(b, nil, shopify.NewStoreRegistry(b, b.ResolveSystemSecret), shopify.NewAdminClient())
	if err := memqlsync.Bind(ca); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(shopify.ConnectorName) })
	body := fmt.Sprintf(`{"shop_domain":%q,"customer":{"id":991},"data_request":{"id":%q}}`, shop, suffix)
	mac := hmac.New(sha256.New, []byte(secretValue))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/inbound/shopify-"+storeID, strings.NewReader(body))
	req.Header.Set(shopify.HeaderHMAC, base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set(shopify.HeaderTopic, shopify.TopicDataRequest)
	req.Header.Set(shopify.HeaderShopDomain, shop)
	req.Header.Set("Authorization", "Bearer must-not-persist")
	req.Header.Set("X-Unrequested", "leak-me")
	response := httptest.NewRecorder()
	inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 4096}, engineAdapter{a}, nil).ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("receipt: %d %s", response.Code, response.Body)
	}
	var receipt map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	row := stagedRow(t, b, receipt["id"])
	// Exactly the allowlisted X-Shopify-* metadata is staged: the topic and
	// shop domain the request carried, and neither the unrequested header nor
	// the bearer. (The credential-drop switch itself is proved by
	// TestVerifiedDeliveryStagesOnlyAllowedMetadata, whose allowlist names
	// Authorization; the Shopify allowlist never does.)
	var staged map[string]string
	if err := json.Unmarshal([]byte(rowString(row, "headersJson")), &staged); err != nil {
		t.Fatalf("staged headersJson %q: %v", rowString(row, "headersJson"), err)
	}
	want := map[string]string{"x-shopify-topic": shopify.TopicDataRequest, "x-shopify-shop-domain": shop}
	if !maps.Equal(staged, want) {
		t.Fatalf("staged delivery headers %v, want %v", staged, want)
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
	memqlsync.UnbindForTest(shopify.ConnectorName)
	if err := memqlsync.Bind(cb); err != nil {
		t.Fatal(err)
	}
	run, err := dispatcherOn(t, b)(received)
	if err != nil || run.Status != "completed" {
		t.Fatalf("dispatch: %+v %v", run, err)
	}
	jobID := shopify.ComplianceJobID(storeID, shopify.TopicDataRequest, "gid://shopify/Customer/991\x00"+suffix)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:shopify:complianceJob' AND id=$1`, "v1:shopify:complianceJob:"+jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cross-engine delivery queued %d jobs, want 1", count)
	}
	// The row itself says what happened to it. A job count alone cannot
	// tell a delivery the dispatcher recorded from one it worked and then
	// left `received`, which is the state an operator reads as "never
	// arrived" and which re-fires on every re-stage. A compliance delivery
	// queues its job inside Apply and returns no mirror writes, so the stamp
	// this pins is Dispatch's zero-writes `processed`.
	if status := rowString(stagedRow(t, b, receipt["id"]), "status"); status != "processed" {
		t.Fatalf("after the hop the staged row is %q, want processed", status)
	}
}

// A staged row whose delivery metadata cannot be parsed is stamped FAILED by
// the shipped automation, on the row, rather than left `received`: @createOnly
// keeps the bad headersJson on every re-stage, so a row left `received` would
// re-fire identically forever and say nothing to the operator. The source is a
// store's own, so a connector claims it; a source no connector serves is the
// product automation's and is never stamped (component/datasync's
// TestUnparseableMetadataOnAnUnservedSourceIsLeftAlone).
//
// The receiver never writes such a row, so it is staged here directly,
// through the same @serverOnly mutation under internal origin, the way a
// future stager with a bug would.
func TestAnUnparseableStagedHeaderRowIsStampedFailed(t *testing.T) {
	a, db := engineOverPostgres(t)
	if a == nil {
		return
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	storeID := "badmeta-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE payload::text LIKE $1 OR id LIKE $1`, "%"+suffix+"%")
	})
	if _, err := a.Execute(seedCtx(), call("createStore", map[string]string{"storeId": storeID, "domain": storeID + ".myshopify.com",
		"name": "Bad metadata test", "adminTokenRef": "unused", "webhookSecretRef": "unused"})); err != nil {
		t.Fatal(err)
	}
	conn := shopify.NewConnector(a, nil, shopify.NewStoreRegistry(a, a.ResolveSystemSecret), shopify.NewAdminClient())
	if err := memqlsync.Bind(conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(shopify.ConnectorName) })

	requestID := "inb-badmeta-" + suffix
	if _, err := a.Execute(seedCtx(), fmt.Sprintf(`mutation stageInboundRequest(requestId: %s, source: %s, medium: "webhook", `+
		`body: "{}", headersJson: "[]", signatureVerified: true, receivedAt: "2026-09-27T12:00:00Z")`,
		langparser.QuoteString(requestID), langparser.QuoteString("shopify-"+storeID))); err != nil {
		t.Fatalf("stage: %v", err)
	}
	row := stagedRow(t, a, requestID)
	if _, err := dispatcherOn(t, a)(row); err != nil {
		t.Logf("dispatch returned %v (the builtin errors after stamping, which the run records)", err)
	}
	after := stagedRow(t, a, requestID)
	if status := rowString(after, "status"); status != "failed" {
		t.Fatalf("a row whose headersJson is not an object is %q after dispatch, want failed", status)
	}
	if reason := rowString(after, "lastError"); reason != "invalid staged delivery headers" {
		t.Errorf("lastError = %q, want the reason the dispatcher stamps", reason)
	}
}

// stagedRow reads one staged inboundRequest row through the engine, the way
// the automation's trigger payload is built from it.
func stagedRow(t *testing.T, eng *memql.MemQLEngine, requestID string) map[string]any {
	t.Helper()
	result, err := eng.Execute(seedCtx(), call("inboundRequestById", map[string]string{"requestId": requestID}))
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		t.Fatalf("read %d staged rows for %s, want 1", len(rows), requestID)
	}
	return rows[0]
}

// dispatcherOn wires the SHIPPED dispatchInboundToConnector automation on eng
// through the real executor -- a tree-loaded automation, so its step context
// carries internal origin exactly as it does in production -- and returns a
// function that runs it for one staged row. The datasync integration is
// registered ONCE here, so a test may dispatch any number of rows on one
// engine without a duplicate registration.
func dispatcherOn(t *testing.T, eng *memql.MemQLEngine) func(row map[string]any) (*automations.AutomationExecution, error) {
	t.Helper()
	if err := eng.RegisterIntegration(datasync.NewIntegration(engineAdapter{eng}, nil)); err != nil {
		t.Fatal(err)
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: eng.Logger}).LoadByName("dispatchInboundToConnector")
	if err != nil {
		t.Fatal(err)
	}
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: eng, Logger: eng.Logger, StepRegistry: steps.NewRegistry()})
	t.Cleanup(executor.Close)
	return func(row map[string]any) (*automations.AutomationExecution, error) {
		event := &events.Event{Topic: "graph.node.created.v1:platform:inboundRequest", Kind: events.KindNodeCreated, OriginNodeId: "receiver-a", Payload: row}
		return executor.ExecuteWithEvent(seedCtx(), auto, "webhook-hop-test", event)
	}
}

// engineOverPostgres boots a real engine over dbtest.DSN(), the way the
// connector's own db-gated suite does, and self-skips when none is reachable.
func engineOverPostgres(t *testing.T) (*memql.MemQLEngine, *sql.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "inbound webhook hop", dsn, err)
		return nil, nil
	}
	db := bun.NewDB(raw, pgdialect.New())
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	// Standalone package tests start without the cluster's bootstrap; the
	// store fixtures rely on its self organization.
	if _, err := eng.Execute(seedCtx(), `insert("v1:accounts:account", id="self", payload={"name":"Test cluster","status":"active","domainStatus":"unverified"})`); err != nil {
		t.Fatalf("seed the test cluster organization: %v", err)
	}
	return eng, raw
}

// seedCtx is a real cluster owner stamped internal-origin, the actor the
// connector's own seeds write under.
func seedCtx() context.Context {
	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner})
	return auth.ContextWithInternalOrigin(ctx)
}

// seedSecret seals value under MEMQL_MASTER_KEY and writes it through the
// same setGlobalSecret mutation the connector's own seedSecret uses, at the
// row id it derives for a name no row carries yet, so the store's
// webhookSecretRef resolves the way it does in production.
func seedSecret(t *testing.T, eng *memql.MemQLEngine, name, value string) {
	t.Helper()
	ciphertext, fingerprint, err := secret.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	id := "sec-" + strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	if _, err := eng.Execute(seedCtx(), call("setGlobalSecret", map[string]string{
		"id": id, "name": name, "encryptedValue": ciphertext, "fingerprint": fingerprint,
		"kind": "vendor_api_key", "description": "Webhook hop test", "addedBy": "test",
	})); err != nil {
		t.Fatal(err)
	}
}

// call renders `name(k: "v", ...)` with sorted keys and JSON-quoted strings,
// which is what the connector's renderCall emits for string arguments.
func call(name string, args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		quoted, _ := json.Marshal(args[k])
		parts = append(parts, k+": "+string(quoted))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// rowString reads a string field off a materialized row, at the top level or
// under its payload.
func rowString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	if payload, ok := row["payload"].(map[string]any); ok {
		if v, ok := payload[key].(string); ok {
			return v
		}
	}
	return ""
}
