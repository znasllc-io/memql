package shopify

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shop.go -- every connected store has its shop row.
//
// v1:shopify:shop is a singleton: no listing, no backfill, and no sweep
// (Reconcile "none"). Until this file the only write of it was a SHOP_UPDATE
// delivery, so a store whose settings never changed had no row, and with it
// no currencyCode and no ianaTimezone -- which every figure drawn from the
// store needs. So the shop is read, with the generated ShopifyFetchShop, at
// the moments a store starts or starts again to record:
//
//   - a connect (writeConnect), for the store it connected;
//   - a boot (Bootstrap), for each store that ingests and has NO live shop row
//     (onlyMissing): a node starts often, and a store that has its row is
//     asked nothing, so a boot of N such stores makes no Admin call at all;
//   - the daily subscription pass (handleEnsureSubscriptions), for every store
//     that ingests, so a SHOP_UPDATE that was lost is healed within a day;
//   - a resume, by the act (resumeStore) and by the store watch's pass that
//     sees the store leave its pause (resumeSeenByWatch).
//
// SHOP_UPDATE stays as it was. The write is heal's, the same as a sweep's:
// judged against the stored row by the mirror's own guard, so an answer read
// before a newer delivery loses to it (the shop publishes its own updatedAt,
// and the write carries it as the origin's version); fenced on the row it was
// judged against; and skipped when the row already holds every value. A boot
// on every node, or one resume seen by both the act and the watch, writes
// once.
//
// NOTHING HERE FAILS ITS CALLER. A store that is paused or not connected is
// not read; one whose grant holds none of the shop's scopes is not asked; a
// clean answer with no shop retires nothing; and a read or a write that
// fails -- or panics -- is logged with the store's id and the caller goes on.
// Each read is bounded by the context it is given and by shopReadTimeout, so
// a hung origin holds its caller no longer than that.

// shopReadTimeout bounds one store's shop read and its write. The Admin
// client already bounds each attempt at a minute and retries a throttle; a
// connect's browser and a resume's act wait on this read, so it is held
// shorter than that.
const shopReadTimeout = 30 * time.Second

// refreshShop reads the store's shop and heals its mirror row. A store that
// does not ingest, or whose connection or grant does not reach the shop's
// scopes, is not asked and answers nil. A clean answer with no shop writes
// nothing and answers nil.
func (c *Connector) refreshShop(ctx context.Context, store Store) (err error) {
	spec := generated.Types["shop"]
	if spec == nil || !store.Ingests() || !domainInConnection(store, spec) || len(scopesMissingFor(store, spec)) > 0 {
		return nil
	}
	defer func() {
		// A malformed answer must not take a connect, a boot or a pass down
		// with it: the shop is read again at the next of them.
		if r := recover(); r != nil {
			err = fmt.Errorf("shopify: reading the shop of store %q panicked: %v", store.ID, r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, shopReadTimeout)
	defer cancel()
	writes, gone, _, err := c.fetchAndMap(ctx, store, spec, "")
	if err != nil {
		return fmt.Errorf("shopify: reading the shop of store %q: %w", store.ID, err)
	}
	if gone || len(writes) == 0 {
		// The origin answered, cleanly, with no shop. A shop does not go away
		// while its store is connected, so this is not a deletion to record.
		return nil
	}
	if _, failed := c.heal(ctx, writes); failed > 0 {
		return fmt.Errorf("shopify: %d of the shop rows of store %q could not be written", failed, store.ID)
	}
	return nil
}

// refreshShopLogged is refreshShop for a caller it must not fail: its error
// is logged with the store's id.
func (c *Connector) refreshShopLogged(ctx context.Context, store Store) {
	if err := c.refreshShop(ctx, store); err != nil {
		c.logger.Warn("shopify: could not read the store's shop; it is read again at the next boot, daily pass or resume",
			"store", store.ID, "error", err.Error())
	}
}

// refreshShops reads the shop of every store that ingests, one after another:
// one store's failure is logged and the next is still read. With onlyMissing,
// a store whose live shop row the mirror already holds is skipped without an
// Admin call; a store whose row could not be looked up is skipped too, and
// logged, so a mirror that cannot be read does not send every store to the
// origin.
func (c *Connector) refreshShops(ctx context.Context, onlyMissing bool) {
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		c.logger.Warn("shopify: could not list the stores; their shops are read at the next pass", "error", err.Error())
		return
	}
	for _, store := range stores {
		if ctx.Err() != nil {
			return
		}
		if !store.Ingests() {
			continue
		}
		if onlyMissing {
			has, err := c.hasShopRow(ctx, store)
			if err != nil {
				c.logger.Warn("shopify: could not tell whether the mirror holds the store's shop; it is not read now",
					"store", store.ID, "error", err.Error())
				continue
			}
			if has {
				continue
			}
		}
		c.refreshShopLogged(ctx, store)
	}
}

// hasShopRow reports whether the mirror holds a live shop row for the store,
// through the generated read, as the operator (the read filters on
// actor.isClusterOwner, which the connector actor never is).
func (c *Connector) hasShopRow(ctx context.Context, store Store) (bool, error) {
	spec := generated.Types["shop"]
	if spec == nil || spec.ForStoreFn == "" {
		return false, fmt.Errorf("shopify: the shop has no read by store")
	}
	res, err := c.engine.Execute(operatorContext(ctx), renderCall(spec.ForStoreFn, map[string]any{"storeId": store.ID}))
	if err != nil {
		return false, err
	}
	return len(memql.MaterializeRows(res)) > 0, nil
}
