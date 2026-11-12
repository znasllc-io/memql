package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// check_end.go -- the end of a check pass (check.go, "The end of a pass").

// ---- the end of a pass ----

// orphanBatch deletes, inside tx, up to deleteBatch facts of the store that no
// row answered for in the pass begun at started: not deleted, not marked by
// it, at or before its cut. A refund's lines go with it, and the orders whose
// lines or refunds went are derived again. Records of rows that could not be
// read that the pass did not meet go in the same batch, within what is left
// of it. deleted counts every row it deleted, facts counts the facts.
func orphanBatch(ctx context.Context, tx bun.Tx, storeID string, started time.Time) (deleted, facts int, err error) {
	if err := passStands(ctx, tx, storeID, started, "", ""); err != nil {
		return 0, 0, err
	}
	cut := started.Add(-checkCut)
	left := deleteBatch
	touched := map[string]bool{}
	unanswered := "store_id = ? AND NOT deleted AND (checked_at IS NULL OR checked_at < ?) AND ? <= ?"

	var orders []string
	if err := tx.NewDelete().TableExpr("store_sales_order_fact").
		Where("ctid IN (SELECT ctid FROM store_sales_order_fact WHERE "+unanswered+" LIMIT ?)", storeID, started, bun.Ident("order_version"), cut, left).
		Returning("order_gid").Scan(ctx, &orders); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("delete the order facts no row answered for: %w", err)
	}
	facts += len(orders)
	left -= len(orders)

	if left > 0 {
		var of []string
		if err := tx.NewDelete().TableExpr("store_sales_line_fact").
			Where("ctid IN (SELECT ctid FROM store_sales_line_fact WHERE "+unanswered+" LIMIT ?)", storeID, started, bun.Ident("line_version"), cut, left).
			Returning("order_gid").Scan(ctx, &of); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, fmt.Errorf("delete the line facts no row answered for: %w", err)
		}
		for _, o := range of {
			touched[o] = true
		}
		facts += len(of)
		left -= len(of)
	}

	if left > 0 {
		var refunds, of []string
		if err := tx.NewDelete().TableExpr("store_sales_refund_fact").
			Where("ctid IN (SELECT ctid FROM store_sales_refund_fact WHERE "+unanswered+" LIMIT ?)", storeID, started, bun.Ident("refund_version"), cut, left).
			Returning("refund_gid, order_gid").Scan(ctx, &refunds, &of); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, fmt.Errorf("delete the refund facts no row answered for: %w", err)
		}
		if len(refunds) > 0 {
			if _, err := tx.NewDelete().TableExpr("store_sales_refund_line_fact").
				Where("store_id = ? AND refund_gid IN (?)", storeID, bun.In(refunds)).Exec(ctx); err != nil {
				return 0, 0, fmt.Errorf("delete the lines of the refunds no row answered for: %w", err)
			}
		}
		for _, o := range of {
			touched[o] = true
		}
		facts += len(refunds)
		left -= len(refunds)
	}

	deleted = facts
	if left > 0 {
		res, err := tx.NewDelete().TableExpr("store_sales_unreadable").
			Where("ctid IN (SELECT ctid FROM store_sales_unreadable WHERE store_id = ? AND (checked_at IS NULL OR checked_at < ?) AND version <= ? LIMIT ?)",
				storeID, started, cut, left).
			Exec(ctx)
		if err != nil {
			return 0, 0, fmt.Errorf("delete the records of rows the pass did not meet: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		deleted += int(n)
	}

	var derive []string
	for o := range touched {
		if o != "" {
			derive = append(derive, o)
		}
	}
	if err := deriveOrders(ctx, tx, storeID, derive); err != nil {
		return 0, 0, err
	}
	if facts > 0 {
		if _, err := tx.NewUpdate().TableExpr("store_sales_check").
			Set("mismatched = mismatched + ?", facts).Set("repaired = repaired + ?", facts).
			Where("store_id = ? AND pass_started_at = ?", storeID, started).Exec(ctx); err != nil {
			return 0, 0, fmt.Errorf("count the facts no row answered for: %w", err)
		}
	}
	return deleted, facts, nil
}

// finishCheckPass ends the pass begun at started, inside tx: see "The end of
// a pass". finished is false when another call ended it first.
func finishCheckPass(ctx context.Context, tx bun.Tx, storeID string, started, now time.Time) (finished bool, disagreeing int, err error) {
	if err := passStands(ctx, tx, storeID, started, "", ""); err != nil {
		return false, 0, err
	}
	if disagreeing, err = disagreeingOrders(ctx, tx, storeID); err != nil {
		return false, 0, err
	}
	res, err := tx.NewUpdate().TableExpr("store_sales_check").
		Set("last_started_at = pass_started_at").
		Set("last_compared = compared").Set("last_mismatched = mismatched").Set("last_repaired = repaired").
		Set("last_disagreeing = ?", disagreeing).
		Set("last_finished_at = ?", now.UTC()).
		Set("clean_at = CASE WHEN mismatched = 0 THEN ?::timestamptz ELSE clean_at END", now.UTC()).
		Set("pass_started_at = NULL").
		Set("order_cursor = ''").Set("line_cursor = ''").Set("refund_cursor = ''").
		Set("order_window_seconds = ?", seconds(defaultWindow)).
		Set("line_window_seconds = ?", seconds(defaultWindow)).
		Set("refund_window_seconds = ?", seconds(defaultWindow)).
		Set("compared = 0").Set("mismatched = 0").Set("repaired = 0").
		Where("store_id = ? AND pass_started_at = ?", storeID, started).Exec(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("end the check's pass: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	return n == 1, disagreeing, nil
}

// disagreeingOrders counts the store's counted orders with complete lines
// whose Shopify totals the facts do not add up to (D14), over facts that are
// not deleted, amounts compared by value: the refunds' money against
// shopify_total_refunded; every line's allocated discounts against
// shopify_total_discounts; and, when no line is partly removed, the lines
// still whole, each unit_price * quantity - allocated_discounts, against
// shopify_current_subtotal. A total the row did not hold disagrees with
// nothing.
func disagreeingOrders(ctx context.Context, tx bun.Tx, storeID string) (int, error) {
	var n int
	err := tx.NewSelect().TableExpr("store_sales_order_fact AS o").ColumnExpr("count(*)").
		Where("o.store_id = ? AND NOT o.deleted AND NOT o.cancelled AND o.lines_complete", storeID).
		Where("(NOT o.test OR EXISTS (SELECT 1 FROM store_sales_store s WHERE s.store_id = o.store_id AND s.development))").
		Where(`(o.shopify_total_refunded <> (SELECT COALESCE(sum(r.money_refunded), 0) FROM store_sales_refund_fact r
					WHERE r.store_id = o.store_id AND r.order_gid = o.order_gid AND NOT r.deleted)
			OR o.shopify_total_discounts <> (SELECT COALESCE(sum(l.allocated_discounts), 0) FROM store_sales_line_fact l
					WHERE l.store_id = o.store_id AND l.order_gid = o.order_gid AND NOT l.deleted)
			OR (NOT EXISTS (SELECT 1 FROM store_sales_line_fact l
					WHERE l.store_id = o.store_id AND l.order_gid = o.order_gid AND NOT l.deleted
					AND l.current_quantity > 0 AND l.current_quantity < l.quantity)
				AND o.shopify_current_subtotal <> (SELECT COALESCE(sum(l.unit_price * l.quantity - l.allocated_discounts), 0) FROM store_sales_line_fact l
					WHERE l.store_id = o.store_id AND l.order_gid = o.order_gid AND NOT l.deleted AND l.current_quantity = l.quantity)))`).
		Scan(ctx, &n)
	if err != nil {
		return 0, fmt.Errorf("count the orders whose totals disagree: %w", err)
	}
	return n, nil
}
