package campaigns

import (
	"context"
	"fmt"
)

// purchasable.go -- memql#4140.
//
// Marketing send is refused unless the thin Shopify index has at least
// one present product that is availableForSale. Transactional mail stays
// on Shopify; this guard is campaigns only.
//
// The same check runs at start, at schedule, and at fire time. Hours
// pass between a schedule and the send, so a catalog that emptied
// overnight still refuses rather than mailing against nothing.

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

// AnyPurchasableProduct reads shopifyProducts under the caller's
// context. Callers that need clusterOwner (the campaigns worker) must
// pass the engine's system actor. A query error is returned, not
// swallowed -- a broken index is not "purchasable".
func (s *Store) AnyPurchasableProduct(ctx context.Context) (bool, error) {
	rows, err := s.rows(ctx, call("query", "shopifyProducts", arg{"present", true}))
	if err != nil {
		return false, err
	}
	products := make([]IndexProduct, 0, len(rows))
	for _, r := range rows {
		products = append(products, IndexProduct{
			Present:          boolean(r, "present"),
			AvailableForSale: boolean(r, "availableForSale"),
		})
	}
	return ProductPurchasable(products), nil
}

// catalogRefusal is the shared start / schedule / fire-time check.
// Empty string means the guard passes.
func (w *Worker) catalogRefusal(ctx context.Context) string {
	ok, err := w.productPurchasable(ctx)
	if err != nil {
		return "product_purchasable: " + err.Error()
	}
	if !ok {
		return ErrNoPurchasableProduct
	}
	return ""
}

func (w *Worker) productPurchasable(ctx context.Context) (bool, error) {
	if w.catalogPurchasable != nil {
		return w.catalogPurchasable(ctx)
	}
	if w.store == nil {
		return false, fmt.Errorf("no store")
	}
	return w.store.AnyPurchasableProduct(w.systemActorContext(ctx))
}
