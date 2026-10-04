package shopify

import (
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

// THE EDGE PUBLISHES ONE SECRET PER STORE, AND IT NAMES THAT SECRET ITSELF
// (memql#5626). component/edge resolves a store's storefrontTokenRef only when
// it equals edge.StorefrontTokenSecretName(storeId); any other ref is refused
// and the storefront reads "unavailable". Every writer in this package seals
// the token under storeSecretName(storeId, suffixStorefrontToken) -- Connect,
// the pasted-token path and the first-boot seed -- so the two spellings must
// stay one spelling. If this fails, a convention moved on one side only, and
// every storefront connected after the move would stop loading its catalog.
func TestTheStorefrontTokenIsSealedUnderTheNameTheEdgePublishes(t *testing.T) {
	for _, storeID := range []string{"acme", "acme-widgets", "a1", "dev_store-2", "x"} {
		sealed := storeSecretName(storeID, suffixStorefrontToken)
		if published := edge.StorefrontTokenSecretName(storeID); sealed != published {
			t.Errorf("store %q: the connector seals its Storefront token as %q and the edge publishes only %q",
				storeID, sealed, published)
		}
	}
	// The other suffixes are what the edge must NEVER publish, and they are
	// refused precisely because they differ from the one name above.
	for _, suffix := range []string{suffixAdminToken, suffixWebhookSecret, suffixPendingClientSecret} {
		if storeSecretName("acme", suffix) == edge.StorefrontTokenSecretName("acme") {
			t.Errorf("%s collides with the one secret name the edge publishes", suffix)
		}
	}
}
