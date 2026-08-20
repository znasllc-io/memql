package campaigns

import (
	"context"

	"github.com/znasllc-io/memql/integrations/shopify"
)

// purchasable.go -- memql#4140.
//
// Marketing send is refused when the thin Shopify index is IN PLAY and
// has no present product availableForSale. dsl/shopify is core, so an
// empty index is the default on every install -- including products
// that never attached a shop. Fail closed only when Shopify is
// configured or at least one shopifyProduct row exists (present or
// retired). Zero rows + unconfigured passes.
//
// The same check runs at start, at schedule, and at fire time.

// ErrNoPurchasableProduct is the operator-visible refusal. The portal
// surfaces this string as the send-button reason.
const ErrNoPurchasableProduct = "product_purchasable: no available product on the Shopify index"

// IndexProduct is the three-field thin index row the guard reads.
type IndexProduct struct {
	Present          bool
	AvailableForSale bool
}

// ProductPurchasable is true when at least one present product is for
// sale. An empty or fully-retired catalog is false. A miss is never
// invented: present=false does not count.
func ProductPurchasable(products []IndexProduct) bool {
	for _, p := range products {
		if p.Present && p.AvailableForSale {
			return true
		}
	}
	return false
}

// CatalogRefusal is the #4140 scope rule. Empty string means pass.
//
//	unconfigured + zero rows → pass (index is not in play)
//	configured, or any row   → fail closed unless one present product is for sale
func CatalogRefusal(configured bool, products []IndexProduct) string {
	if !configured && len(products) == 0 {
		return ""
	}
	if ProductPurchasable(products) {
		return ""
	}
	return ErrNoPurchasableProduct
}

// ShopifyIndex reads every thin-index row (present and retired).
// Callers that need clusterOwner must pass the engine's system actor.
func (s *Store) ShopifyIndex(ctx context.Context) ([]IndexProduct, error) {
	rows, err := s.rows(ctx, call("query", "shopifyProducts"))
	if err != nil {
		return nil, err
	}
	products := make([]IndexProduct, 0, len(rows))
	for _, r := range rows {
		products = append(products, IndexProduct{
			Present:          boolean(r, "present"),
			AvailableForSale: boolean(r, "availableForSale"),
		})
	}
	return products, nil
}

// catalogRefusal is the shared start / schedule / fire-time check.
func (w *Worker) catalogRefusal(ctx context.Context) string {
	if w.store == nil {
		return "product_purchasable: no store"
	}
	products, err := w.store.ShopifyIndex(w.systemActorContext(ctx))
	if err != nil {
		return "product_purchasable: " + err.Error()
	}
	return CatalogRefusal(w.shopifyIsConfigured(), products)
}

func (w *Worker) shopifyIsConfigured() bool {
	if w.shopifyConfigured != nil {
		return w.shopifyConfigured()
	}
	return shopify.ConfigFromEnv().Configured()
}
