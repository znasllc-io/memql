package shopify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchProductStorefrontByHandle(t *testing.T) {
	var gotBody map[string]any
	var gotTok string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTok = r.Header.Get("X-Shopify-Storefront-Access-Token")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"data":{"product":{"id":"gid://shopify/Product/1","handle":"linen-shirt","availableForSale":true}}}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		StoreDomain:       "example.myshopify.com",
		StorefrontToken:   "sf-secret",
		StorefrontBaseURL: srv.URL,
		APIVersion:        defaultAPIVersion,
	})
	p, err := c.FetchProduct(context.Background(), "", "linen-shirt")
	if err != nil {
		t.Fatalf("FetchProduct: %v", err)
	}
	if p.ID != "gid://shopify/Product/1" || p.Handle != "linen-shirt" || !p.AvailableForSale {
		t.Fatalf("got %+v", p)
	}
	if gotTok != "sf-secret" {
		t.Fatalf("token header = %q", gotTok)
	}
	vars, _ := gotBody["variables"].(map[string]any)
	if vars["handle"] != "linen-shirt" {
		t.Fatalf("variables = %#v", vars)
	}
}

func TestFetchProductStorefrontByGID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"product":{"id":"gid://shopify/Product/9","handle":"mug","availableForSale":false}}}`))
	}))
	defer srv.Close()
	c := NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: srv.URL})
	p, err := c.FetchProduct(context.Background(), "gid://shopify/Product/9", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.AvailableForSale {
		t.Fatal("expected unavailable")
	}
	if p.Handle != "mug" {
		t.Fatalf("handle = %q", p.Handle)
	}
}

func TestFetchProductAdminRequiresGID(t *testing.T) {
	c := NewClient(Config{StoreDomain: "ex.myshopify.com", AdminToken: "admin-secret"})
	if _, err := c.FetchProduct(context.Background(), "", "only-handle"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchProductAdminByGID(t *testing.T) {
	var gotTok string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTok = r.Header.Get("X-Shopify-Access-Token")
		_, _ = w.Write([]byte(`{"data":{"product":{"id":"gid://shopify/Product/2","handle":"hat","availableForSale":true}}}`))
	}))
	defer srv.Close()
	c := NewClient(Config{AdminToken: "admin-secret", AdminBaseURL: srv.URL})
	p, err := c.FetchProduct(context.Background(), "gid://shopify/Product/2", "")
	if err != nil {
		t.Fatal(err)
	}
	if gotTok != "admin-secret" {
		t.Fatalf("admin token header = %q", gotTok)
	}
	if p.Handle != "hat" {
		t.Fatalf("handle = %q", p.Handle)
	}
}

func TestFetchProductMissingIDAndHandle(t *testing.T) {
	c := NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: "http://127.0.0.1"})
	if _, err := c.FetchProduct(context.Background(), "", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchProductNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"product":null}}`))
	}))
	defer srv.Close()
	c := NewClient(Config{StorefrontToken: "t", StorefrontBaseURL: srv.URL})
	if _, err := c.FetchProduct(context.Background(), "", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigFromEnvOptOut(t *testing.T) {
	t.Setenv("MEMQL_SHOPIFY_STORE_DOMAIN", "")
	t.Setenv("MEMQL_SHOPIFY_STOREFRONT_TOKEN", "")
	t.Setenv("MEMQL_SHOPIFY_ADMIN_TOKEN", "")
	if ConfigFromEnv().Configured() {
		t.Fatal("empty env should not be configured")
	}
	t.Setenv("MEMQL_SHOPIFY_STORE_DOMAIN", "ex.myshopify.com")
	t.Setenv("MEMQL_SHOPIFY_STOREFRONT_TOKEN", "sf")
	if !ConfigFromEnv().Configured() {
		t.Fatal("store + storefront token should configure")
	}
}
