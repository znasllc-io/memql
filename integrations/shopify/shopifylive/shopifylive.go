// Package shopifylive is what every live Shopify test shares: the variables
// that name a development store, and the refusal of a store that is not one.
//
// A TEST-SUPPORT PACKAGE, as component/database/dbtest is: nothing in a
// product build imports it. It exists because the live tests live in two test
// packages -- integrations/shopify's own (the mirror, the wholesale tag route)
// and packs/wholesalepack's (the client's grant automation end to end) -- and a
// test helper in one package's _test.go files cannot be called from another's.
// The refusal is the safety of every live test that writes, so it is written
// once, here, and both call it; neither carries a copy.
//
// It imports nothing of integrations/shopify, so package shopify's own tests
// can import it without a cycle: the caller hands in how to ask the store.
package shopifylive

import (
	"os"
	"strings"
	"testing"
)

// The variables a live test reads. They are test inputs, not a product
// configuration path: nothing outside the live tests reads these names, no
// manifest registers them, and no CI lane sets them.
const (
	// EnvStoreDomain is <shop>.myshopify.com of a partner development store
	// reserved for the live tests.
	EnvStoreDomain = "MEMQL_TEST_SHOPIFY_STORE_DOMAIN"
	// EnvAdminToken is an Admin API token of an app installed on it.
	EnvAdminToken = "MEMQL_TEST_SHOPIFY_ADMIN_TOKEN"
)

// Env reads one variable and skips t, naming the variable, when it is unset.
// what names the test for the skip line ("the live mirror test"). The value is
// never printed: a token is a secret, and a domain is the caller's to print.
func Env(t testing.TB, name, what string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Skipf("%s not set; skipping %s against a development store", name, what)
	}
	return value
}

// ShopPlanDocument asks the shop whether it is a partner development store.
const ShopPlanDocument = `query LiveTestShopPlan {
  shop {
    plan { partnerDevelopment }
  }
}`

// ShopPlanOperation is ShopPlanDocument's operation name.
const ShopPlanOperation = "LiveTestShopPlan"

// RequireDevelopmentStore fails t unless the store says it is a partner
// development store. A live test calls it before its first write to the store
// and before any read that walks the store's data; on a refusal nothing has
// been created.
//
// ask runs ShopPlanDocument against the store and answers the reply's data
// (the caller's own Admin client, so the call is paced and bounded the way the
// caller's other calls are). variable names the variable the store came from,
// for the message; the domain itself is not printed here.
func RequireDevelopmentStore(t testing.TB, variable string, ask func(document, operation string) (map[string]any, error)) {
	t.Helper()
	data, err := ask(ShopPlanDocument, ShopPlanOperation)
	if err != nil {
		t.Fatalf("could not read whether the store named by %s is a development store, so nothing was created: %v", variable, err)
	}
	shop, _ := data["shop"].(map[string]any)
	plan, _ := shop["plan"].(map[string]any)
	dev, ok := plan["partnerDevelopment"].(bool)
	if !ok {
		t.Fatalf("could not read whether the store named by %s is a development store (shop.plan.partnerDevelopment was absent), so nothing was created", variable)
	}
	if !dev {
		t.Fatalf("the store named by %s is not a partner development store (shop.plan.partnerDevelopment is false); this test runs only against a development store reserved for it, and nothing was created", variable)
	}
}
