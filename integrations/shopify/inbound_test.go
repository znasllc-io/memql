package shopify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const restWebhookBody = `{
  "id": 632910392,
  "admin_graphql_api_id": "gid://shopify/Product/632910392",
  "handle": "linen-shirt",
  "title": "Linen shirt",
  "images": [{"src": "https://cdn.example/shirt.jpg"}],
  "variants": [{"price": "29.00", "inventory_quantity": 12}],
  "product_type": "Shirts"
}`

const deleteWebhookBody = `{
  "id": 632910392,
  "admin_graphql_api_id": "gid://shopify/Product/632910392"
}`

const orderWebhookBody = `{
  "id": 99,
  "admin_graphql_api_id": "gid://shopify/Order/99",
  "line_items": [{"product_id": 632910392}]
}`

func TestParseProductDeliveryGID(t *testing.T) {
	gid, handle, ok := ParseProductDelivery("shopify", "products/create", []byte(restWebhookBody))
	if !ok {
		t.Fatal("expected product delivery")
	}
	if gid != "gid://shopify/Product/632910392" {
		t.Fatalf("gid=%q", gid)
	}
	if handle != "linen-shirt" {
		t.Fatalf("handle=%q", handle)
	}
}

func TestParseProductDeliveryNumericID(t *testing.T) {
	body := []byte(`{"id": 42, "handle": "mug"}`)
	gid, handle, ok := ParseProductDelivery("shopify", "", body)
	if !ok || gid != "gid://shopify/Product/42" || handle != "mug" {
		t.Fatalf("gid=%q handle=%q ok=%v", gid, handle, ok)
	}
}

func TestParseProductDeliverySkipsOrder(t *testing.T) {
	if _, _, ok := ParseProductDelivery("shopify", "orders/create", []byte(orderWebhookBody)); ok {
		t.Fatal("order must not be a product delivery")
	}
}

func TestParseProductDeliverySkipsForeignSource(t *testing.T) {
	if _, _, ok := ParseProductDelivery("campaigns-feedback", "", []byte(`{"id": 1}`)); ok {
		t.Fatal("foreign source with numeric id must skip")
	}
}

func TestParseProductDeliveryAcceptsProductGIDAnySource(t *testing.T) {
	body := []byte(`{"admin_graphql_api_id":"gid://shopify/Product/7"}`)
	gid, _, ok := ParseProductDelivery("other", "", body)
	if !ok || gid != "gid://shopify/Product/7" {
		t.Fatalf("gid=%q ok=%v", gid, ok)
	}
}

func TestDecideApplyUpsert(t *testing.T) {
	d, err := DecideApply(Product{ID: "gid://shopify/Product/1", Handle: "linen-shirt", AvailableForSale: true}, nil, "gid://shopify/Product/1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionUpsert || d.Handle != "linen-shirt" || !d.AvailableForSale {
		t.Fatalf("%+v", d)
	}
}

func TestDecideApplyMissDoesNotInvent(t *testing.T) {
	d, err := DecideApply(Product{}, fmtNotFound(), "gid://shopify/Product/9")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionRetire {
		t.Fatalf("want retire, got %+v", d)
	}
	if d.Handle != "" {
		t.Fatal("retire must not invent a handle")
	}
}

func TestDecideApplyTransportErrorIsNotRetire(t *testing.T) {
	_, err := DecideApply(Product{}, errStatus(), "gid://shopify/Product/1")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func fmtNotFound() error { return errString("shopify: product not found") }

type errString string

func (e errString) Error() string { return string(e) }

func errStatus() error { return errString("shopify: graphql status 500") }

func TestApplyInboundUpsertsThreeFieldsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"product":{"id":"gid://shopify/Product/632910392","handle":"linen-shirt","availableForSale":true}}}`))
	}))
	defer srv.Close()
	i := NewIntegration(NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: srv.URL}))
	nodes, err := i.handleApplyInboundProduct(context.Background(), map[string]any{
		"source": "shopify",
		"topic":  "products/update",
		"body":   restWebhookBody,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len=%d", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["action"] != "upsert" {
		t.Fatalf("action=%v", payload["action"])
	}
	if payload["id"] != "gid://shopify/Product/632910392" || payload["handle"] != "linen-shirt" {
		t.Fatalf("payload=%v", payload)
	}
	raw := string(nodes[0].Payload)
	for _, leak := range []string{"images", "price", "inventory", "cdn.example", "29.00"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("merchandising leaked %q in %s", leak, raw)
		}
	}
}

func TestApplyInboundMissingDoesNotInvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"product":null}}`))
	}))
	defer srv.Close()
	i := NewIntegration(NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: srv.URL}))
	nodes, err := i.handleApplyInboundProduct(context.Background(), map[string]any{
		"source": "shopify",
		"body":   deleteWebhookBody,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["action"] != "retire" {
		t.Fatalf("want retire, got %v", payload)
	}
	if payload["handle"] != "" {
		t.Fatal("retire invented a handle")
	}
}

func TestApplyInboundUnconfiguredNoops(t *testing.T) {
	i := NewIntegration(nil)
	nodes, err := i.handleApplyInboundProduct(context.Background(), map[string]any{
		"source": "shopify",
		"body":   restWebhookBody,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &payload)
	if payload["action"] != "skip" {
		t.Fatalf("want skip, got %v", payload)
	}
}

func TestApplyInboundSkipsOtherSources(t *testing.T) {
	i := NewIntegration(NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: "http://127.0.0.1"}))
	nodes, err := i.handleApplyInboundProduct(context.Background(), map[string]any{
		"source": "campaigns-feedback",
		"body":   `{"event":"bounce"}`,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &payload)
	if payload["action"] != "skip" {
		t.Fatalf("want skip, got %v", payload)
	}
}

func TestReconcileProductMissRetires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"product":null}}`))
	}))
	defer srv.Close()
	i := NewIntegration(NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: srv.URL}))
	nodes, err := i.handleReconcileProduct(context.Background(), map[string]any{
		"id": "gid://shopify/Product/9",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &payload)
	if payload["action"] != "retire" {
		t.Fatalf("want retire, got %v", payload)
	}
}

func TestReconcileIndexUnconfiguredNoops(t *testing.T) {
	i := NewIntegration(nil)
	nodes, err := i.handleReconcileIndex(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &payload)
	if payload["action"] != "skip" {
		t.Fatalf("want skip, got %v", payload)
	}
}
