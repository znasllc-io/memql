package shopify

import (
	"context"
	"errors"
	"fmt"
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
// THE ORDINARY CHECK COMES FIRST AND ITS ANSWER IS THE SWEEP'S. Every order
// sweep runs the filtered check from its watermark, exactly as every other
// domain's sweep does; only after a check that succeeded does one piece of the
// read run, from the read's cursor or from the start. The check is what keeps
// the mirror right, and the read never stands in its way.
//
// THE READ'S FAILURE IS NOT THE SWEEP'S. The mirror's rule that a sweep which
// could not write a row it read must fail holds for the check, unchanged: that
// failure holds the watermark, so the next check reads the row again. The read
// has no watermark to protect. A row's current state is kept by the check and
// the webhooks; the read only adds detail, and it has its own cursor and its
// own recorded failure. So a piece that fails is logged, recorded on the
// store's health (refundDetailErrorKey: when, and a sentence that names no
// order and no person), and tried again at the next sweep from where it
// stopped. The cursor moves past every page whose rows were all written, so a
// row that cannot be written stops the read at its own page, never back at
// the start of the piece.
//
// THE STORE'S HEALTH IS THE BOOKKEEPING, not the refund rows. A refund with no
// detail cannot say whether its order is still one Shopify shows, so a search
// for rows that owe a read would find the same unreachable rows at every
// sweep. The health says instead when the read of every order last began and
// ended (refundDetailFrom), and where a read under way stands
// (refundDetailAfter). The read is stamped only when it ended: whatever the
// listing does not show is out of reach, not owed.
//
// THE STAMP DOES NOT OUTLIVE THE ROWS IT DESCRIBES. A store's health survives a
// purge and a reconnect, and an order backfill writes rows with no detail, so
// a stamp older than the store's last order export says nothing of the rows
// that export wrote. The read is owed when the stamp is absent and when the
// order domain's backfill started its last export after the stamp, as the
// backfill's own cursor records it (finished, backfill.go); every way a
// backfill begins (connect, a restart after an erasure, an operator's
// backfill, a later grant) passes through that one state. While the order
// backfill is queued or running no piece runs: it would read what the export
// is about to write.
//
// WHAT IT COSTS. With a read under way a sweep asks the origin for up to twice
// its row budget, the check's and one piece's, and an order both read is
// counted twice in the sweep's report. Every row the read finds as the mirror
// holds it is skipped by the guarded write, so a store whose rows were all
// current pays the reads and no writes.
//
// WHAT IT CANNOT REACH stays without detail: an order Shopify no longer shows,
// which without read_all_orders is every order older than sixty days. The
// listing ends where Shopify's answer ends, so the read ends and is stamped
// all the same; nothing waits for those orders, and nothing is retired for
// being absent from it. A store one of whose orders can never be written keeps
// its read at that order's page, recorded as failing, until it can.

const (
	refundDetailFromKey  = "refundDetailFrom"  // health: when the read of every order began, written once it has ended (RFC3339, UTC)
	refundDetailAfterKey = "refundDetailAfter" // health: {"cursor", "began"} of a read under way; absent otherwise
	refundDetailErrorKey = "refundDetailError" // health: {"at", "error"} of the last piece of the read that failed; absent once a piece ends clean or cut
)

// refundDetailConcept is the one domain whose sweep carries the read.
const refundDetailConcept = "order"

var refundDetailRowBudget = relistRowCap // a variable so a test can lower it

// reconcileOrders is a store's order sweep: the ordinary check, whose answer is
// the sweep's, and then, after a check that succeeded and for a store that owes
// it, one piece of the read of every order.
func (c *Connector) reconcileOrders(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time) (memqlsync.ReconcileReport, error) {
	report, err := c.reconcileByUpdatedAt(ctx, store, spec, since)
	if err != nil {
		return report, err
	}
	owed, cursor, began := c.refundDetailOwed(ctx, store)
	if !owed {
		return report, nil
	}
	read := c.reconcileRefundDetail(ctx, store, spec, cursor, began)
	report.Checked += read.Checked
	report.Drifted += read.Drifted
	report.Healed += read.Healed
	return report, nil
}

// refundDetailOwed reads the store row and the order domain's sync state fresh
// and says whether a piece of the read runs now, and where a read under way
// stands: its cursor and when it began, or "" and the zero time to begin one.
//
// Owed: the health holds no refundDetailFrom, or the order backfill started its
// last export after it. Not owed this sweep: the order backfill is queued or
// running, or either row cannot be read (logged), or the store row is not
// there; the read waits for a sweep that can judge it. A read under way that
// began before the last export begins again: the export may have written rows
// into pages it already read.
func (c *Connector) refundDetailOwed(ctx context.Context, store Store) (owed bool, cursor string, began time.Time) {
	opCtx := memql.ContextWithFreshRead(operatorContext(ctx))
	fresh, found, err := c.storeByID(opCtx, store.ID)
	if err != nil {
		c.logger.Warn("shopify: could not read whether the store's orders were read once more; this sweep is the ordinary check alone",
			"store", store.ID, "error", err.Error())
		return false, "", time.Time{}
	}
	if !found {
		return false, "", time.Time{}
	}
	stateID := memqlsync.StateID(generated.ConceptID(refundDetailConcept), ConnectorName, string(memqlsync.DirectionInbound), store.ID)
	res, err := c.engine.Execute(opCtx, renderCall("syncStateById", map[string]any{"stateId": stateID}))
	if err != nil {
		c.logger.Warn("shopify: could not read the store's order backfill; this sweep is the ordinary check alone",
			"store", store.ID, "error", err.Error())
		return false, "", time.Time{}
	}
	var exported time.Time
	if rows := memql.MaterializeRows(res); len(rows) > 0 {
		switch mapString(rows[0], "backfillStatus") {
		case "queued", "running":
			return false, "", time.Time{}
		}
		if at, ok := memqlsync.VersionTime(decodeBackfillCursor(mapString(rows[0], "backfillCursor")).At); ok {
			// The stamps are kept to the second; an export started within
			// the second a read began started before that read, which runs
			// only once no backfill is in flight.
			exported = at.UTC().Truncate(time.Second)
		}
	}
	if from, ok := memqlsync.VersionTime(mapString(fresh.Health, refundDetailFromKey)); ok && !exported.After(from) {
		return false, "", time.Time{}
	}
	if after, ok := fresh.Health[refundDetailAfterKey].(map[string]any); ok {
		at, ok := memqlsync.VersionTime(mapString(after, "began"))
		if cursor := mapString(after, "cursor"); cursor != "" && ok && !exported.After(at) {
			return true, cursor, at.UTC()
		}
	}
	return true, "", time.Time{}
}

// reconcileRefundDetail is one piece of the read of every order: the listing,
// with no filter, from cursor ("" begins the read now), for at most
// refundDetailRowBudget rows. Its report is what it read; its failure is
// recorded on the store's health and is never the sweep's.
//
// How the piece ends decides what the health keeps:
//   - ended, with every row written: refundDetailFrom is when the read began;
//     refundDetailAfter and refundDetailError are removed, in one write;
//   - cut at its row budget: refundDetailAfter keeps the cursor and when the
//     read began, and refundDetailError is removed. Without the cursor a store
//     with more orders than one piece reads would read its first orders at
//     every sweep and never the rest;
//   - failed (a page could not be read, a row could not be written, or the
//     listing stalled): refundDetailError says so, and refundDetailAfter keeps
//     the cursor of the page it stopped at. A first page after a stored
//     cursor that Shopify refused (not a throttle, a server error or the
//     network) drops the cursor, so a cursor Shopify no longer takes starts
//     the read again.
//
// A health write that fails is logged, and the next sweep does the same work
// again: reading twice is harmless, since a mirror write is guarded and skips
// a row it already holds.
func (c *Connector) reconcileRefundDetail(ctx context.Context, store Store, spec *generated.TypeSpec, cursor string, began time.Time) memqlsync.ReconcileReport {
	if cursor == "" {
		began = c.now().UTC()
	}
	swept, err := c.sweepListing(ctx, store, spec, "", cursor, refundDetailRowBudget, true)
	switch {
	case err != nil:
		next, sentence := swept.next, "a page of the order listing could not be read; the next check reads it again"
		if cursor != "" && next == cursor && swept.report.Checked == 0 && cursorRefused(err) {
			next, sentence = "", "the order listing could not be read after the read's cursor; the next check reads every order from the start"
		}
		c.failRefundDetail(ctx, store, next, began, sentence, err)
	case swept.failed > 0:
		c.failRefundDetail(ctx, store, swept.next, began,
			fmt.Sprintf("%s of a page of the order listing could not be written to the mirror; the next check reads that page again", rowsCount(swept.failed)), nil)
	case swept.stalled:
		c.failRefundDetail(ctx, store, swept.next, began,
			"the order listing stalled: Shopify answered an empty page and said more followed; the next check asks for that page again", nil)
	case swept.cut:
		c.recordRefundDetail(ctx, store, map[string]any{
			refundDetailAfterKey: map[string]any{"cursor": swept.next, "began": began.Format(time.RFC3339)},
		}, []string{refundDetailErrorKey})
		c.logger.Info("shopify: the read of every order stopped at its row budget; the next sweep goes on from there", "store", store.ID)
	default:
		c.recordRefundDetail(ctx, store, map[string]any{
			refundDetailFromKey: began.Format(time.RFC3339),
		}, []string{refundDetailAfterKey, refundDetailErrorKey})
		c.logger.Info("shopify: every order was read once more", "store", store.ID)
	}
	return swept.report
}

// failRefundDetail records a piece that failed: the sentence, and the cursor
// the next piece goes on from ("" removes it, and the next piece begins the
// read again). cause, when there is one, goes to the log alone.
func (c *Connector) failRefundDetail(ctx context.Context, store Store, next string, began time.Time, sentence string, cause error) {
	args := []any{"store", store.ID, "reason", sentence}
	if cause != nil {
		args = append(args, "error", cause.Error())
	}
	c.logger.Warn("shopify: a piece of the read of every order failed; the order check is not held by it", args...)
	set := map[string]any{refundDetailErrorKey: map[string]any{"at": c.now().UTC().Format(time.RFC3339), "error": sentence}}
	var drop []string
	if next != "" {
		set[refundDetailAfterKey] = map[string]any{"cursor": next, "began": began.Format(time.RFC3339)}
	} else {
		drop = []string{refundDetailAfterKey}
	}
	c.recordRefundDetail(ctx, store, set, drop)
}

// recordRefundDetail writes the read's health keys. A failure is logged and
// the next sweep does the same work again.
func (c *Connector) recordRefundDetail(ctx context.Context, store Store, set map[string]any, drop []string) {
	if err := c.recordStoreHealthKeys(ctx, store.ID, set, drop, nil); err != nil {
		c.logger.Warn("shopify: could not record the read of every order on the store's health; the next sweep does the same work again",
			"store", store.ID, "error", err.Error())
	}
}

// rowsCount is "1 row" or "<n> rows".
func rowsCount(n int) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
}

// cursorRefused reports whether a failed page is Shopify's answer refusing the
// request rather than a moment's trouble: an answer that is not a throttle or
// a server error. A network failure or a throttle keeps the cursor, so a read
// is not started over for a failure the next sweep does not meet.
func cursorRefused(err error) bool {
	var ae *AdminError
	return errors.As(err, &ae) && !ae.Retryable()
}
