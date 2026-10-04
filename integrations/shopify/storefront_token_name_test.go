package shopify

import "testing"

// THE EDGE PUBLISHES ONE SECRET PER STORE, AND IT NAMES THAT SECRET ITSELF
// (memql#5626). component/edge resolves a store's storefrontTokenRef only when
// it equals the store's Storefront token name; any other ref is refused and
// the storefront reads "unavailable". app/storefront_token_name_parity_test.go
// holds the edge's spelling to StorefrontTokenSecretName, from the one module
// that imports both. What this package guarantees on its own side is that the
// OTHER secrets it seals per store can never share that name, because they are
// refused precisely by differing from it.
func TestNoOtherStoreSecretSharesTheStorefrontTokenName(t *testing.T) {
	for _, storeID := range []string{"acme", "acme-widgets", "a1", "dev_store-2", "x"} {
		published := StorefrontTokenSecretName(storeID)
		for _, suffix := range []string{suffixAdminToken, suffixWebhookSecret, suffixPendingClientSecret} {
			if storeSecretName(storeID, suffix) == published {
				t.Errorf("store %q: %s collides with the one secret name the edge publishes (%q)", storeID, suffix, published)
			}
		}
	}
}
