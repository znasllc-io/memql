package shopifypack_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	shopifypack "github.com/znasllc-io/memql/examples/shopifypack"
)

func TestShopifyPackLoadsAndExtends(t *testing.T) {
	logger := slog.Default()
	memqldsl.RegisterTree(shopifypack.Domain, shopifypack.Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(shopifypack.Domain) })

	if _, err := memqldsl.Tree().Open(shopifypack.Domain + "/concepts.memql"); err != nil {
		t.Fatalf("pack concepts.memql not reachable via dsl.Tree(): %v", err)
	}
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts failed: %v", err)
	}
	if _, err := memorynodes.DefaultRegistry().Get("v1:shopifypack:shop"); err != nil {
		t.Fatalf("pack concept v1:shopifypack:shop MUST be registered: %v", err)
	}
}

func TestSecretRefsRefuseTokenValues(t *testing.T) {
	if err := shopifypack.ValidSecretRef("shpat_abcdefghijklmnopqrstuvwxyz012345"); err == nil {
		t.Fatal("Admin token value must be refused")
	}
	if err := shopifypack.ValidSecretRef("shpua_storefronttokenvalue"); err == nil {
		t.Fatal("Storefront token value must be refused")
	}
	if err := shopifypack.ValidSecretRef("not a secret"); err == nil {
		t.Fatal("free text must be refused")
	}
	if err := shopifypack.ValidSecretRef(shopifypack.StorefrontTokenSecret); err != nil {
		t.Fatal(err)
	}
	if err := shopifypack.ValidSecretRef(shopifypack.AdminTokenSecret); err != nil {
		t.Fatal(err)
	}
}

func TestAttachShopStoresNamesNotTokens(t *testing.T) {
	provider, err := shopifypack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	handler := mustCap(t, provider, "attachShop")
	_, err = handler(context.Background(), map[string]any{
		"shopId":                "shop-1",
		"storeDomain":           "demo.myshopify.com",
		"storefrontTokenSecret": "shpat_thisisavalue",
		"adminTokenSecret":      shopifypack.AdminTokenSecret,
	}, 0)
	if err == nil {
		t.Fatal("attachShop must refuse a token value")
	}

	nodes, err := handler(context.Background(), map[string]any{
		"shopId":                "shop-1",
		"storeDomain":           "demo.myshopify.com",
		"storefrontTokenSecret": shopifypack.StorefrontTokenSecret,
		"adminTokenSecret":      shopifypack.AdminTokenSecret,
		"storeDomainSecret":     shopifypack.StoreDomainSecret,
		"apiVersionSecret":      shopifypack.APIVersionSecret,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := payloadOf(t, nodes)
	raw, _ := json.Marshal(payload)
	if strings.Contains(strings.ToLower(string(raw)), "shpat_") || strings.Contains(strings.ToLower(string(raw)), "shpua_") {
		t.Fatalf("payload leaked a token value: %s", raw)
	}
	if payload["storefrontTokenSecret"] != shopifypack.StorefrontTokenSecret {
		t.Fatalf("storefrontTokenSecret=%v", payload["storefrontTokenSecret"])
	}
}

func TestSetShopSyncIsData(t *testing.T) {
	provider, err := shopifypack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	handler := mustCap(t, provider, "setShopSync")
	nodes, err := handler(context.Background(), map[string]any{
		"shopId": "shop-1", "syncEnabled": false,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := payloadOf(t, nodes)
	if payload["syncEnabled"] != false {
		t.Fatalf("syncEnabled=%v", payload["syncEnabled"])
	}
	nodes, err = handler(context.Background(), map[string]any{
		"shopId": "shop-1", "syncEnabled": true,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload = payloadOf(t, nodes)
	if payload["syncEnabled"] != true {
		t.Fatalf("syncEnabled=%v", payload["syncEnabled"])
	}
}

func TestPackDoesNotOwnCheckout(t *testing.T) {
	err := fs.WalkDir(shopifypack.Tree(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, readErr := fs.ReadFile(shopifypack.Tree(), path)
		if readErr != nil {
			return readErr
		}
		src := string(raw)
		if strings.Contains(src, "mutate checkout") || strings.Contains(src, "concept checkout") {
			t.Errorf("%s must not declare a checkout concept or mutation", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustCap(t *testing.T, provider memql.IntegrationProvider, name string) func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	t.Helper()
	for _, c := range provider.Capabilities() {
		if c.Name == name {
			return c.Handler
		}
	}
	t.Fatalf("%s missing", name)
	return nil
}

func payloadOf(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("len=%d", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
