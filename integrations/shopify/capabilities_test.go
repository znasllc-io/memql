package shopify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchProductCapability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"product":{"id":"gid://shopify/Product/1","handle":"linen-shirt","availableForSale":true}}}`))
	}))
	defer srv.Close()
	i := NewIntegration(NewClient(Config{StorefrontToken: "sf-secret", StorefrontBaseURL: srv.URL}))
	nodes, err := i.handleFetchProduct(context.Background(), map[string]any{"handle": "linen-shirt"}, 0)
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
	if payload["id"] != "gid://shopify/Product/1" || payload["handle"] != "linen-shirt" {
		t.Fatalf("payload=%v", payload)
	}
	if strings.Contains(string(nodes[0].Payload), "sf-secret") {
		t.Fatal("token leaked into capability payload")
	}
}

func TestFetchProductCapabilityUnconfigured(t *testing.T) {
	i := NewIntegration(nil)
	if _, err := i.handleFetchProduct(context.Background(), map[string]any{"handle": "x"}, 0); err == nil {
		t.Fatal("expected error")
	}
}
