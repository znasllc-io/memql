package shopify

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// THE ENGINE REFUSES A STORE ID CONNECT WOULD NOT DERIVE, AND NOTHING ELSE
// (memql#5626). component/memql's store guard refuses, at creation, an id
// outside the form StoreIDForDomain produces, and derives no Storefront token
// name for one -- so the two forms must agree, or a store Connect creates
// would be refused, or one it never could create would be admitted. And the
// name the engine derives must be the name this package seals the token
// under, for every id Connect produces.
func TestEveryIdConnectDerivesIsAStoreIdTheEngineAccepts(t *testing.T) {
	for _, domain := range []string{
		"acme.myshopify.com", "acme-widgets.myshopify.com", "Acme-Widgets.myshopify.com",
		"a1.myshopify.com", "dev_store-2.myshopify.com", strings.Repeat("a", 56) + ".myshopify.com",
	} {
		id, err := StoreIDForDomain(domain)
		if err != nil {
			t.Errorf("StoreIDForDomain(%q): %v", domain, err)
			continue
		}
		if !memql.ValidShopifyStoreID(id) {
			t.Errorf("Connect derives %q from %q and the engine would refuse to create it", id, domain)
		}
		if sealed, derived := storeSecretName(id, suffixStorefrontToken), memql.StorefrontTokenSecretName(id); sealed != derived {
			t.Errorf("store %q: sealed as %q, the engine expects %q", id, sealed, derived)
		}
	}
	// Ids Connect cannot produce -- its pattern refuses them, or they hold a
	// character a domain label cannot -- are ids the engine refuses.
	for _, id := range []string{"-acme", "Acme", "acme widgets", `acme"x`, "acme.dev", strings.Repeat("a", 57)} {
		if storeIDPattern.MatchString(id) {
			t.Fatalf("the fixture %q is one Connect's own pattern accepts", id)
		}
		if memql.ValidShopifyStoreID(id) {
			t.Errorf("the engine accepts %q, which Connect cannot derive", id)
		}
	}
}
