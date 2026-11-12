package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/money"
)

// derive.go -- what each order's facts sum to: the derived columns of its
// lines, of the order and of its refunds (the 2026-11-11 store statistics
// design, 5.1 and 5.2). deriveOrders runs in the page's own transaction, for
// the orders the page touched: an order whose row, line or refund the page
// carried, a line's or a refund's tombstone included, whose order is the one
// its fact names.
//
// # Over the facts that are not deleted
//
// A deleted line and a deleted refund count nowhere. For each line, its
// refunded_quantity is the sum of the refund-line quantities naming it over
// the order's refunds; sold_quantity, gross and discounts are soldOf's, with
// the line's currency, computed in Go and written back (the rounding is
// core/money's, never SQL's). The order's gross, discounts and units sum its
// lines that are not gift cards; line_count and lines_version are over all its
// lines; lines_complete says the sum of quantity over all its lines is at
// least expected_quantity -- false when a line was never read, which is how a
// line the rules could not read shows on its order. A refund whose detail was
// read has gift_card_refunded, the subtotals of its lines that name a gift-card
// line, units_returned, the quantities of the others, and item_refunds,
// money_refunded less shipping, duties, gift cards and, unless the order's
// prices include tax, tax: the tax setting of an order fact that is not
// deleted, and none for a refund whose order is deleted or not yet folded. All three are null for a refund whose detail is not
// read.
//
// A deleted order's derived columns are kept current like any other's: what
// it sums to is never read, and leaving it out would need a case of its own.
// Its lines and refunds stay live facts (fold.go).
//
// # Exact
//
// A NUMERIC is read back as its decimal text and parsed with money.Parse, and
// written as an Amount's text. A row is written only when a derived column
// changed, so an overlap read again writes nothing here.

// The derived columns as they are, and the inputs they are computed from.
type deriveOrder struct {
	OrderGID         string       `bun:"order_gid"`
	Deleted          bool         `bun:"deleted"`
	ExpectedQuantity int          `bun:"expected_quantity"`
	TaxesIncluded    bool         `bun:"taxes_included"`
	Gross            string       `bun:"gross"`
	Discounts        string       `bun:"discounts"`
	Units            int          `bun:"units"`
	LineCount        int          `bun:"line_count"`
	LinesVersion     sql.NullTime `bun:"lines_version"`
	LinesComplete    bool         `bun:"lines_complete"`
}

type deriveLine struct {
	LineGID            string    `bun:"line_gid"`
	OrderGID           string    `bun:"order_gid"`
	LineVersion        time.Time `bun:"line_version"`
	Currency           string    `bun:"currency"`
	GiftCard           bool      `bun:"gift_card"`
	Quantity           int       `bun:"quantity"`
	CurrentQuantity    int       `bun:"current_quantity"`
	UnitPrice          string    `bun:"unit_price"`
	AllocatedDiscounts string    `bun:"allocated_discounts"`
	RefundedQuantity   int       `bun:"refunded_quantity"`
	SoldQuantity       int       `bun:"sold_quantity"`
	Gross              string    `bun:"gross"`
	Discounts          string    `bun:"discounts"`
}

type deriveRefund struct {
	RefundGID        string         `bun:"refund_gid"`
	OrderGID         string         `bun:"order_gid"`
	DetailFetched    bool           `bun:"detail_fetched"`
	MoneyRefunded    string         `bun:"money_refunded"`
	ShippingRefunded sql.NullString `bun:"shipping_refunded"`
	TaxRefunded      sql.NullString `bun:"tax_refunded"`
	DutiesRefunded   sql.NullString `bun:"duties_refunded"`
	GiftCardRefunded sql.NullString `bun:"gift_card_refunded"`
	ItemRefunds      sql.NullString `bun:"item_refunds"`
	UnitsReturned    sql.NullInt64  `bun:"units_returned"`
}

type deriveRefundLine struct {
	RefundGID string `bun:"refund_gid"`
	LineGID   string `bun:"line_gid"`
	Quantity  int    `bun:"quantity"`
	Subtotal  string `bun:"subtotal"`
}

// errStoredAmount is a NUMERIC column whose text money.Parse refuses: never a
// value of this package's writing. The value is not quoted.
func errStoredAmount(table, column string) error {
	return fmt.Errorf("storestats: a stored %s.%s is not a decimal", table, column)
}

func parseStored(s, table, column string) (money.Amount, error) {
	a, err := money.Parse(s)
	if err != nil {
		return money.Amount{}, errStoredAmount(table, column)
	}
	return a, nil
}

// parseOptional is a nullable NUMERIC: zero when null.
func parseOptional(s sql.NullString, table, column string) (money.Amount, error) {
	if !s.Valid {
		return money.Amount{}, nil
	}
	return parseStored(s.String, table, column)
}

// deriveOrders recomputes the derived columns of the orders named, their
// lines and their refunds, inside tx. See the file's comment.
func deriveOrders(ctx context.Context, tx bun.Tx, storeID string, orderGIDs []string) error {
	if len(orderGIDs) == 0 {
		return nil
	}
	in := bun.In(orderGIDs)
	var orders []deriveOrder
	if err := tx.NewSelect().TableExpr("store_sales_order_fact").
		Column("order_gid", "deleted", "expected_quantity", "taxes_included", "gross", "discounts", "units", "line_count", "lines_version", "lines_complete").
		Where("store_id = ? AND order_gid IN (?)", storeID, in).Scan(ctx, &orders); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the orders to derive: %w", err)
	}
	var lines []deriveLine
	if err := tx.NewSelect().TableExpr("store_sales_line_fact").
		Column("line_gid", "order_gid", "line_version", "currency", "gift_card", "quantity", "current_quantity", "unit_price",
			"allocated_discounts", "refunded_quantity", "sold_quantity", "gross", "discounts").
		Where("store_id = ? AND order_gid IN (?) AND NOT deleted", storeID, in).
		OrderExpr("line_gid").Scan(ctx, &lines); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the lines to derive: %w", err)
	}
	var refunds []deriveRefund
	if err := tx.NewSelect().TableExpr("store_sales_refund_fact").
		Column("refund_gid", "order_gid", "detail_fetched", "money_refunded", "shipping_refunded", "tax_refunded", "duties_refunded",
			"gift_card_refunded", "item_refunds", "units_returned").
		Where("store_id = ? AND order_gid IN (?) AND NOT deleted", storeID, in).
		OrderExpr("refund_gid").Scan(ctx, &refunds); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the refunds to derive: %w", err)
	}
	var refundLines []deriveRefundLine
	if err := tx.NewSelect().TableExpr("store_sales_refund_line_fact").
		Column("refund_gid", "line_gid", "quantity", "subtotal").
		Where("store_id = ? AND order_gid IN (?)", storeID, in).
		OrderExpr("refund_gid, position").Scan(ctx, &refundLines); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the refunds' lines to derive: %w", err)
	}

	d, err := derive(orders, lines, refunds, refundLines)
	if err != nil {
		return err
	}
	for _, l := range d.lines {
		if _, err := tx.NewUpdate().TableExpr("store_sales_line_fact").
			Set("refunded_quantity = ?", l.RefundedQuantity).Set("sold_quantity = ?", l.SoldQuantity).
			Set("gross = ?", l.Gross).Set("discounts = ?", l.Discounts).
			Where("store_id = ? AND line_gid = ?", storeID, l.LineGID).Exec(ctx); err != nil {
			return fmt.Errorf("write a line's derived amounts: %w", err)
		}
	}
	for _, o := range d.orders {
		var version *time.Time
		if o.LinesVersion.Valid {
			v := o.LinesVersion.Time.UTC()
			version = &v
		}
		if _, err := tx.NewUpdate().TableExpr("store_sales_order_fact").
			Set("gross = ?", o.Gross).Set("discounts = ?", o.Discounts).Set("units = ?", o.Units).
			Set("line_count = ?", o.LineCount).Set("lines_version = ?", version).Set("lines_complete = ?", o.LinesComplete).
			Where("store_id = ? AND order_gid = ?", storeID, o.OrderGID).Exec(ctx); err != nil {
			return fmt.Errorf("write an order's derived amounts: %w", err)
		}
	}
	for _, r := range d.refunds {
		var gift, item *string
		var units *int64
		if r.GiftCardRefunded.Valid {
			gift, item, units = &r.GiftCardRefunded.String, &r.ItemRefunds.String, &r.UnitsReturned.Int64
		}
		if _, err := tx.NewUpdate().TableExpr("store_sales_refund_fact").
			Set("gift_card_refunded = ?", gift).Set("item_refunds = ?", item).Set("units_returned = ?", units).
			Where("store_id = ? AND refund_gid = ?", storeID, r.RefundGID).Exec(ctx); err != nil {
			return fmt.Errorf("write a refund's derived amounts: %w", err)
		}
	}
	return nil
}

// derivation is the rows whose derived columns changed, with their new values.
type derivation struct {
	orders  []deriveOrder
	lines   []deriveLine
	refunds []deriveRefund
}

// derive computes the derived columns from the facts read, and answers the
// rows whose columns it changes. Pure.
func derive(orders []deriveOrder, lines []deriveLine, refunds []deriveRefund, refundLines []deriveRefundLine) (derivation, error) {
	var d derivation
	liveRefund := map[string]bool{}
	for _, r := range refunds {
		liveRefund[r.RefundGID] = true
	}
	refunded := map[string]int{}
	for _, rl := range refundLines {
		if liveRefund[rl.RefundGID] {
			refunded[rl.LineGID] += rl.Quantity
		}
	}

	// Each line, and its order's sums.
	type sums struct {
		gross, discounts money.Amount
		units, count     int
		quantity         int
		version          time.Time
	}
	byOrder := map[string]*sums{}
	giftCard := map[string]bool{}
	for _, l := range lines {
		giftCard[l.LineGID] = l.GiftCard
		price, err := parseStored(l.UnitPrice, "store_sales_line_fact", "unit_price")
		if err != nil {
			return d, err
		}
		allocated, err := parseStored(l.AllocatedDiscounts, "store_sales_line_fact", "allocated_discounts")
		if err != nil {
			return d, err
		}
		sold, gross, discounts := soldOf(l.Quantity, l.CurrentQuantity, refunded[l.LineGID], price, allocated, l.Currency)
		next := l
		next.RefundedQuantity, next.SoldQuantity, next.Gross, next.Discounts = refunded[l.LineGID], sold, amountText(gross), amountText(discounts)
		if next != l {
			d.lines = append(d.lines, next)
		}
		s := byOrder[l.OrderGID]
		if s == nil {
			s = &sums{}
			byOrder[l.OrderGID] = s
		}
		s.count++
		s.quantity += l.Quantity
		if l.LineVersion.After(s.version) {
			s.version = l.LineVersion
		}
		if !l.GiftCard {
			s.gross, s.discounts, s.units = s.gross.Add(gross), s.discounts.Add(discounts), s.units+sold
		}
	}

	// Each order.
	taxesIncluded := map[string]bool{}
	for _, o := range orders {
		// A deleted order's tax setting is not read: what it holds depends on
		// whether its live version was folded before its tombstone.
		taxesIncluded[o.OrderGID] = o.TaxesIncluded && !o.Deleted
		s := byOrder[o.OrderGID]
		if s == nil {
			s = &sums{}
		}
		next := o
		next.Gross, next.Discounts, next.Units, next.LineCount = amountText(s.gross), amountText(s.discounts), s.units, s.count
		next.LinesVersion = sql.NullTime{}
		if s.count > 0 {
			next.LinesVersion = sql.NullTime{Time: s.version.UTC(), Valid: true}
		}
		next.LinesComplete = s.quantity >= o.ExpectedQuantity
		if !sameDerivedOrder(o, next) {
			d.orders = append(d.orders, next)
		}
	}

	// Each refund.
	linesOf := map[string][]deriveRefundLine{}
	for _, rl := range refundLines {
		linesOf[rl.RefundGID] = append(linesOf[rl.RefundGID], rl)
	}
	for _, r := range refunds {
		next := r
		next.GiftCardRefunded, next.ItemRefunds, next.UnitsReturned = sql.NullString{}, sql.NullString{}, sql.NullInt64{}
		if r.DetailFetched {
			m, err := parseStored(r.MoneyRefunded, "store_sales_refund_fact", "money_refunded")
			if err != nil {
				return d, err
			}
			shipping, err := parseOptional(r.ShippingRefunded, "store_sales_refund_fact", "shipping_refunded")
			if err != nil {
				return d, err
			}
			tax, err := parseOptional(r.TaxRefunded, "store_sales_refund_fact", "tax_refunded")
			if err != nil {
				return d, err
			}
			duties, err := parseOptional(r.DutiesRefunded, "store_sales_refund_fact", "duties_refunded")
			if err != nil {
				return d, err
			}
			var gift money.Amount
			units := 0
			for _, rl := range linesOf[r.RefundGID] {
				if giftCard[rl.LineGID] {
					subtotal, err := parseStored(rl.Subtotal, "store_sales_refund_line_fact", "subtotal")
					if err != nil {
						return d, err
					}
					gift = gift.Add(subtotal)
				} else {
					units += rl.Quantity
				}
			}
			item := m.Sub(shipping).Sub(duties).Sub(gift)
			if !taxesIncluded[r.OrderGID] {
				item = item.Sub(tax)
			}
			next.GiftCardRefunded = sql.NullString{String: amountText(gift), Valid: true}
			next.ItemRefunds = sql.NullString{String: amountText(item), Valid: true}
			next.UnitsReturned = sql.NullInt64{Int64: int64(units), Valid: true}
		}
		if next != r {
			d.refunds = append(d.refunds, next)
		}
	}
	return d, nil
}

func sameDerivedOrder(a, b deriveOrder) bool {
	return a.Gross == b.Gross && a.Discounts == b.Discounts && a.Units == b.Units && a.LineCount == b.LineCount &&
		a.LinesComplete == b.LinesComplete && a.LinesVersion.Valid == b.LinesVersion.Valid &&
		(!a.LinesVersion.Valid || a.LinesVersion.Time.Equal(b.LinesVersion.Time))
}
