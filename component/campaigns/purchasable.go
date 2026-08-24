package campaigns

import (
	"context"
)

// purchasable.go -- memql#4140, re-pointed at the Shopify MIRROR (memql#4389).
//
// Marketing send is refused when a Shopify catalog is IN PLAY and has nothing
// available for sale. The rule is unchanged; what it reads is not. The thin
// three-field product index this used to walk was superseded by the generated
// mirror, so the question "is anything purchasable" is now asked of
// v1:shopify:productVariant -- which knows about variants, and therefore
// knows a product whose only variant is out of stock is not purchasable.
//
// "In play" is a store ROW now rather than an env var. That is the honest
// reading of the same rule: the connector is multi-store and configured at
// runtime, so a deployment with a store row has a catalog to answer for and a
// deployment with none does not.
//
// The same check runs at start, at schedule, and at fire time.

// ErrNoPurchasableProduct is the operator-visible refusal. The portal
// surfaces this string as the send-button reason.
const ErrNoPurchasableProduct = "product_purchasable: no available product on the Shopify mirror"

// IndexProduct is one row the guard reads: a mirrored variant.
type IndexProduct struct {
	Present          bool
	AvailableForSale bool
}

// ProductPurchasable is true when at least one live variant is for sale. An
// empty or fully-tombstoned catalog is false. A miss is never invented: a
// tombstoned row does not count.
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
//	no store + zero rows  -> pass (no catalog is in play)
//	a store, or any row   -> fail closed unless one live variant is for sale
func CatalogRefusal(configured bool, products []IndexProduct) string {
	if !configured && len(products) == 0 {
		return ""
	}
	if ProductPurchasable(products) {
		return ""
	}
	return ErrNoPurchasableProduct
}

// ShopifyIndex reads whether any mirrored variant is currently for sale.
//
// purchasableVariants paginates to ONE row, because the caller is asking a
// boolean: a guard that paged the whole catalog to answer it would get slower
// as the store grew, on a path that runs at every send.
// Callers that need clusterOwner must pass the engine's system actor.
func (s *Store) ShopifyIndex(ctx context.Context) ([]IndexProduct, error) {
	rows, err := s.rows(ctx, call("query", "purchasableVariants"))
	if err != nil {
		return nil, err
	}
	products := make([]IndexProduct, 0, len(rows))
	for _, r := range rows {
		products = append(products, IndexProduct{
			Present:          !boolean(r, "deleted"),
			AvailableForSale: boolean(r, "availableForSale"),
		})
	}
	return products, nil
}

// shopifyStoreConfigured reports whether any Shopify store row exists.
func (s *Store) shopifyStoreConfigured(ctx context.Context) bool {
	rows, err := s.rows(ctx, call("query", "stores"))
	if err != nil {
		return false
	}
	return len(rows) > 0
}

// catalogRefusal is the shared start / schedule / fire-time check.
func (w *Worker) catalogRefusal(ctx context.Context) string {
	if w.store == nil {
		return "product_purchasable: no store"
	}
	systemCtx := w.systemActorContext(ctx)
	products, err := w.store.ShopifyIndex(systemCtx)
	if err != nil {
		return "product_purchasable: " + err.Error()
	}
	return CatalogRefusal(w.shopifyIsConfigured(systemCtx), products)
}

func (w *Worker) shopifyIsConfigured(ctx context.Context) bool {
	if w.shopifyConfigured != nil {
		return w.shopifyConfigured()
	}
	if w.store == nil {
		return false
	}
	return w.store.shopifyStoreConfigured(ctx)
}
