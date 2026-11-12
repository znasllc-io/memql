package shopify

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// refund_detail.go -- every order is read once more, so a refund mirrored
// before its detail gets it.
//
// A refund row written before the mirror recorded a refund's lines, shipping
// lines and adjustments, or written by a bulk line, which selects none of
// them, holds no detail, and an order sweep that reads only what changed since
// its watermark never reads that order again. So one read of the store's whole
// order listing reads every order Shopify still shows, with the detail. It
// rides the store's scheduled order sweeps: nothing new is scheduled and
// nothing runs at boot.
//
// THE STORE'S HEALTH IS THE BOOKKEEPING, not the refund rows. A refund with no
// detail cannot say whether its order is still one Shopify shows, so a search
// for rows that owe a read would find the same unreachable rows at every
// sweep. The health says instead whether the read of every order has ended for
// this store (refundDetailFrom, when it began), and where a read under way
// stands (refundDetailAfter). The read is stamped only when it ended: whatever
// the listing does not show is out of reach, not owed.
//
// WHAT IT CANNOT REACH stays without detail: an order Shopify no longer shows,
// which without read_all_orders is every order older than sixty days. The
// listing ends where Shopify's answer ends, so the read ends and is stamped
// all the same; nothing waits for those orders. A store one of whose orders can
// never be written repeats the read at every sweep until it can, as its
// ordinary sweep already reads again from a watermark that does not move.

const (
	refundDetailFromKey  = "refundDetailFrom"  // health: when the read of every order began, written once it has ended (RFC3339, UTC)
	refundDetailAfterKey = "refundDetailAfter" // health: {"cursor", "began"} of a read under way; absent otherwise
)

// refundDetailConcept is the one domain whose sweep carries the read.
const refundDetailConcept = "order"

var refundDetailRowBudget = relistRowCap // a variable so a test can lower it

// refundDetailOwed reads the store row fresh and says whether its health holds
// no refundDetailFrom, and where a read under way stands: its cursor and when
// it began, or "" and the zero time when none is. A store row that cannot be
// read, or is not there, owes nothing this sweep: the read waits for a sweep
// that can record it.
func (c *Connector) refundDetailOwed(ctx context.Context, store Store) (owed bool, cursor string, began time.Time) {
	fresh, found, err := c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), store.ID)
	if err != nil {
		c.logger.Warn("shopify: could not read whether the store's orders were read once more; this sweep reads only what changed",
			"store", store.ID, "error", err.Error())
		return false, "", time.Time{}
	}
	if !found || mapString(fresh.Health, refundDetailFromKey) != "" {
		return false, "", time.Time{}
	}
	if after, ok := fresh.Health[refundDetailAfterKey].(map[string]any); ok {
		at, err := time.Parse(time.RFC3339, mapString(after, "began"))
		if cursor := mapString(after, "cursor"); cursor != "" && err == nil {
			return true, cursor, at.UTC()
		}
	}
	return true, "", time.Time{}
}

// reconcileRefundDetail is an order sweep of a store that owes the read.
//
// With no read under way the sweep is the read: the listing from its start,
// with no filter, whatever since it was handed (a read of every order reads
// what changed too). With a read under way the sweep first runs as it was
// handed, so the store's ordinary check is never held back by the read, and
// the read then goes on from its cursor. An ordinary check that failed answers
// its error at once and the read waits for the next sweep.
//
// How the read ends decides what the health keeps:
//   - ended, with no error and not cut: refundDetailFrom is when it began,
//     and refundDetailAfter is removed, in one write;
//   - cut at refundDetailRowBudget: nothing is stamped, and refundDetailAfter
//     keeps the cursor and when the read began. Without it a store with more
//     orders than one sweep reads would read its first orders at every sweep
//     and never the rest;
//   - an error (a page failed, or a row could not be written): nothing is
//     stamped, the cursor stays where it was, and the error is the sweep's. A
//     read that Shopify refused on its first page after a stored cursor drops
//     the cursor, so a cursor Shopify no longer takes starts the read again.
//
// A health write that fails is logged, and the next sweep does the same work
// again: reading twice is harmless, since a mirror write is guarded and skips
// a row it already holds.
func (c *Connector) reconcileRefundDetail(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time, cursor string, began time.Time) (memqlsync.ReconcileReport, error) {
	var report memqlsync.ReconcileReport
	if cursor == "" {
		began = c.now().UTC()
	} else {
		ordinary, err := c.reconcileByUpdatedAt(ctx, store, spec, since)
		if err != nil {
			return ordinary, err
		}
		report = ordinary
	}
	read, next, cut, err := c.sweepListing(ctx, store, spec, "", cursor, refundDetailRowBudget)
	report.Checked += read.Checked
	report.Drifted += read.Drifted
	report.Healed += read.Healed
	switch {
	case err != nil:
		if cursor != "" && read.Checked == 0 && cursorRefused(err) {
			c.recordRefundDetail(ctx, store, nil, "the read's cursor was refused; the next sweep reads every order from the start")
		}
		return report, err
	case cut:
		c.recordRefundDetail(ctx, store, map[string]any{
			refundDetailAfterKey: map[string]any{"cursor": next, "began": began.Format(time.RFC3339)},
		}, "the read of every order stopped at its row budget; the next sweep goes on from there")
		return report, nil
	default:
		c.recordRefundDetail(ctx, store, map[string]any{
			refundDetailFromKey: began.Format(time.RFC3339),
		}, "every order was read once more")
		return report, nil
	}
}

// recordRefundDetail writes the read's health: set, and refundDetailAfter
// removed unless set carries it. A failure is logged and the next sweep does
// the same work again.
func (c *Connector) recordRefundDetail(ctx context.Context, store Store, set map[string]any, what string) {
	var drop []string
	if _, keeps := set[refundDetailAfterKey]; !keeps {
		drop = []string{refundDetailAfterKey}
	}
	if err := c.recordStoreHealthKeys(ctx, store.ID, set, drop, nil); err != nil {
		c.logger.Warn("shopify: could not record on the store's health that "+what+"; the next sweep does the same work again",
			"store", store.ID, "error", err.Error())
		return
	}
	c.logger.Info("shopify: "+what, "store", store.ID)
}

// cursorRefused reports whether a failed page is Shopify's answer refusing the
// request rather than a moment's trouble: an answer that is not a throttle or
// a server error. A network failure or a throttle keeps the cursor, so a read
// is not started over for a failure the next sweep does not meet.
func cursorRefused(err error) bool {
	var ae *AdminError
	return errors.As(err, &ae) && !ae.Retryable()
}
