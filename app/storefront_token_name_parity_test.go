package app

// storefront_token_name_parity_test.go -- the edge and the Shopify connector
// spell a store's Storefront token name the same way (memql#5626).
//
// component/edge publishes a store's Storefront token only when the store's
// storefrontTokenRef equals edge.StorefrontTokenSecretName(storeId); any other
// ref is refused and the storefront reads "unavailable". integrations/shopify
// seals the token under its own spelling of that name -- Connect, the
// pasted-token path and the first-boot seed all do. If the two drift, every
// storefront connected after the drift stops loading its catalog and nothing
// else fails.
//
// The test lives HERE because this is the module that imports both:
// integrations does not require the root module component/edge belongs to, so
// a copy inside integrations/shopify builds in the workspace and fails the
// module-boundaries lane, which builds each module on its own.

import (
	"testing"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/integrations/shopify"
)

func TestTheEdgeAndTheConnectorSpellTheStorefrontTokenNameAlike(t *testing.T) {
	for _, storeID := range []string{"acme", "acme-widgets", "a1", "dev_store-2", "x"} {
		sealed, published := shopify.StorefrontTokenSecretName(storeID), edge.StorefrontTokenSecretName(storeID)
		if sealed != published {
			t.Errorf("store %q: the connector seals its Storefront token as %q and the edge publishes only %q",
				storeID, sealed, published)
		}
	}
}
