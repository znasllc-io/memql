package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/money"
)

// figures.go -- the figures of a store's answer (the 2026-11-11 store
// statistics design, 5.3 and 6.5): every key of Statistics from Figures down,
// summed with SQL over the store_sales_ facts, in the readTx readStatistics
// opened, after the frame.
//
// # What counts
//
// A counted order is an order fact that is not deleted, not cancelled and,
// unless the store is a development store, not a test. A counted refund is a
// refund fact that is not deleted whose order fact is counted. A line counts
// when it is not deleted, is not a gift card, and its order is counted. Every
// statement here reads the facts through those filters (countedOrder); none
// reads a fact without them.
//
// Money is summed only over the orders in the store's currency, and over the
// refunds of those orders. The orders in another currency are counted in
// Orders and Units sold, and listed in otherCurrencies.
//
// # How few statements
//
// A read makes the same statements whatever the store holds: the orders of
// the period by bucket and by wholesale side, its refunds the same way, the
// other currencies, the buyers, the products, the same two sums for the
// previous period, and the count of rows the fold could not read. Buckets are
// cut with width_bucket over the bucket starts Go computed for the store's
// zone (statsperiod), compared as instants: no AT TIME ZONE, so every boundary
// comes from the one zone database that cut the period.
//
// # Exact
//
// A sum is NUMERIC in SQL, read back as its decimal text and parsed with
// money.Parse; it is added and subtracted in Go with core/money. An empty sum
// is answered "0". The one division, the average order value, is rounded
// half to even at the currency's minor units, and answered so.
//
// # Null, never zero
//
// A figure not measured is nil, answered null. See nullRules for each.

// unreadableCode is the code of the reason the answer gains when the fold
// could not read some of the store's rows (ruling 38).
const unreadableCode = "statistics_rows_unreadable"

// unreadableSentence says how many of the store's rows the figures leave out.
func unreadableSentence(n int64) string {
	if n == 1 {
		return "1 record of this store could not be read and is left out of these figures."
	}
	return strconv.FormatInt(n, 10) + " records of this store could not be read and are left out of these figures."
}

// floorFigures are the figures that are floors while an order's lines are
// incomplete: the ones made from its lines.
var floorFigures = []string{"grossSales", "discounts", "netSales", "totalSales", "averageOrderValue", "unitsSold"}

// topRows is how many rows a top list carries.
const topRows = 25

// readFigures fills every key of the answer from Figures down. It reads only
// store_sales_ tables, inside tx. See the file's comment.
func readFigures(ctx context.Context, tx bun.Tx, s *Statistics) error {
	if err := readUnreadable(ctx, tx, s); err != nil {
		return err
	}
	if !s.measured() {
		s.unmeasured()
		return nil
	}
	scope := figureScope{storeID: s.StoreID, development: s.Development != nil && *s.Development}
	if s.Currency != nil {
		scope.currency = *s.Currency
	}
	b := s.Bounds
	from, until := b.Start, b.End
	if s.AsOf != nil && s.AsOf.Before(until) {
		until = *s.AsOf
	}
	starts := make([]time.Time, 0, len(s.Series))
	for _, e := range s.Series {
		starts = append(starts, e.At)
	}

	// The previous period, when it is answered, is read in the same two
	// statements as a leading bucket: it ends where this period starts.
	comparable := s.comparable()
	readFrom, readStarts, lead := from, starts, 0
	if comparable {
		readFrom, readStarts, lead = b.PrevStart, append([]time.Time{b.PrevStart}, starts...), 1
	}
	both, err := readSums(ctx, tx, scope, readStarts, readFrom, until)
	if err != nil {
		return err
	}
	period, prev := both.split(lead)
	all := period.total()
	s.Figures = scope.figures(all, true)
	s.IncompleteOrders, s.IncompleteRefunds, s.RefundsNotRead = countOf(all.incompleteOrders), countOf(all.incompleteRefunds), countOf(all.notRead)
	s.Floors = map[string]bool{}
	if all.incompleteOrders > 0 {
		for _, k := range floorFigures {
			s.Floors[k] = true
		}
	}
	for i := range s.Series {
		if s.bucketUnmeasured(i) {
			continue
		}
		f := scope.figures(period.bucket(i+1), false)
		e := &s.Series[i]
		e.TotalSales, e.NetSales, e.GrossSales, e.Discounts, e.Refunds = f.TotalSales, f.NetSales, f.GrossSales, f.Discounts, f.Refunds
		e.Orders, e.UnitsSold = f.Orders, f.UnitsSold
	}
	s.Wholesale = scope.wholesale(s.wholesaleEnabled, period)

	if s.OtherCurrencies, err = readOtherCurrencies(ctx, tx, scope, from, until); err != nil {
		return err
	}
	if s.Buyers, err = readBuyers(ctx, tx, scope, from, until); err != nil {
		return err
	}
	if err := readProducts(ctx, tx, scope, from, until, all, s); err != nil {
		return err
	}

	if comparable {
		f := scope.figures(prev.total(), true)
		s.Previous = &f
	}
	return nil
}

// measured reports whether the store's figures can be given at all. They
// cannot (every figure, bucket and list null):
//   - for a store the walk holds no row for, or whose zone is not recorded or
//     does not load: no day of it can be cut;
//   - while the store is starting, rebuilding or purging, or the walk has not
//     yet reached the clock for every source (walked_through null);
//   - while something stands in the way and there was no clear run (blocked
//     with no asOf), or the clear run is not after the period's start: no
//     instant of the period was measured.
func (s *Statistics) measured() bool {
	switch {
	case s.Bounds == nil, !s.zoneLoaded:
		return false
	case s.Recording.State == "starting", s.Recording.State == statePurging, s.Recording.State == stateRebuilding:
		return false
	case s.Rebuilding != nil && *s.Rebuilding, !s.walked:
		return false
	case s.Recording.Blocked && s.AsOf == nil:
		return false
	case s.AsOf != nil && !s.AsOf.After(s.Bounds.Start):
		return false
	}
	return true
}

// unmeasured answers every key from Figures down as not known.
func (s *Statistics) unmeasured() {
	s.Figures, s.Previous, s.Floors = Figures{}, nil, map[string]bool{}
	s.ProductsBySales, s.ProductsByUnits, s.OtherCurrencies = nil, nil, nil
	s.ProductCount, s.RefundsWithoutItem, s.IncompleteOrders, s.IncompleteRefunds, s.RefundsNotRead = nil, nil, nil, nil, nil
	s.Buyers, s.Wholesale = nil, nil
}

// bucketUnmeasured reports whether bucket i holds no measured instant: it
// starts at or after an answered asOf, or ends at or before history begins.
func (s *Statistics) bucketUnmeasured(i int) bool {
	start := s.Series[i].At
	end := s.Bounds.End
	if i+1 < len(s.Series) {
		end = s.Series[i+1].At
	}
	if s.AsOf != nil && !start.Before(*s.AsOf) {
		return true
	}
	return s.HistorySince != nil && !end.After(*s.HistorySince)
}

// comparable reports whether the previous period is answered: there is one
// (not for 12 months), history covers it, and this period is summed whole --
// a period cut short at asOf is not compared with a whole one (ruling 44).
// readFigures asks it only of a measured store.
func (s *Statistics) comparable() bool {
	b := s.Bounds
	return b.HasPrevious && s.AsOf == nil && s.HistorySince != nil && !b.PrevStart.Before(*s.HistorySince)
}

// readUnreadable adds the reason for the rows the fold could not read, in any
// state. It blocks nothing and nulls nothing.
func readUnreadable(ctx context.Context, tx bun.Tx, s *Statistics) error {
	var n int64
	if err := tx.NewSelect().Table("store_sales_unreadable").ColumnExpr("count(*)").
		Where("store_id = ?", s.StoreID).Scan(ctx, &n); err != nil {
		return fmt.Errorf("count the rows of store %s that could not be read: %w", s.StoreID, err)
	}
	if n > 0 {
		s.Recording.Reasons = append(s.Recording.Reasons, Reason{Code: unreadableCode, Sentence: unreadableSentence(n)})
	}
	return nil
}

// figureScope is what every statement filters by: the store, the store's
// currency ("" when it has none, which no order matches) and whether its
// test orders count.
type figureScope struct {
	storeID, currency string
	development       bool
}

// countedOrder is the filter of a counted order fact aliased o, and its
// arguments.
func (c figureScope) countedOrder() (string, []any) {
	return "o.store_id = ? AND NOT o.deleted AND NOT o.cancelled AND (? OR NOT o.test)", []any{c.storeID, c.development}
}

// sums is what one bucket and one wholesale side sum to. Money is the store's
// currency only.
type sums struct {
	orders, storeOrders, units                int64
	gross, discounts, charges                 money.Amount
	incompleteOrders                          int64
	refunds, refundedOut                      money.Amount // R(F), and M(F) - RG(F)
	returnedUnits, notRead, incompleteRefunds int64
}

func (a sums) add(b sums) sums {
	return sums{
		orders: a.orders + b.orders, storeOrders: a.storeOrders + b.storeOrders, units: a.units + b.units,
		gross: a.gross.Add(b.gross), discounts: a.discounts.Add(b.discounts), charges: a.charges.Add(b.charges),
		incompleteOrders: a.incompleteOrders + b.incompleteOrders,
		refunds:          a.refunds.Add(b.refunds), refundedOut: a.refundedOut.Add(b.refundedOut),
		returnedUnits: a.returnedUnits + b.returnedUnits, notRead: a.notRead + b.notRead,
		incompleteRefunds: a.incompleteRefunds + b.incompleteRefunds,
	}
}

// sideKey is a bucket (1-based, width_bucket's) and a wholesale side.
type sideKey struct {
	bucket    int
	wholesale bool
}

// periodSums is a period's sums by bucket and side.
type periodSums map[sideKey]sums

func (p periodSums) total() sums {
	var t sums
	for _, s := range p {
		t = t.add(s)
	}
	return t
}

// split is the sums of the period's own buckets, renumbered from 1, and of
// the lead buckets before them.
func (p periodSums) split(lead int) (period, before periodSums) {
	period, before = periodSums{}, periodSums{}
	for k, v := range p {
		if k.bucket <= lead {
			before[k] = v
			continue
		}
		period[sideKey{k.bucket - lead, k.wholesale}] = v
	}
	return period, before
}

func (p periodSums) bucket(i int) sums {
	return p[sideKey{i, false}].add(p[sideKey{i, true}])
}

func (p periodSums) side(wholesale bool) sums {
	var t sums
	for k, s := range p {
		if k.wholesale == wholesale {
			t = t.add(s)
		}
	}
	return t
}

// figures turns sums into figures, by the null rules:
//   - with no currency, no money figure;
//   - with a counted refund whose detail is not read, no Refunds, Net sales,
//     Total sales or Units sold (D22);
//   - with a counted refund holding a full page of lines, no Units sold;
//   - the average, asked only for a period, with no order in the store's
//     currency, none.
func (c figureScope) figures(s sums, average bool) Figures {
	f := Figures{Orders: countOf(s.orders)}
	refundsKnown := s.notRead == 0
	if refundsKnown && s.incompleteRefunds == 0 {
		f.UnitsSold = countOf(s.units - s.returnedUnits)
	}
	if c.currency == "" {
		return f
	}
	gross, discounts := s.gross, s.discounts
	f.GrossSales, f.Discounts = &gross, &discounts
	if refundsKnown {
		refunds := s.refunds
		net := gross.Sub(discounts).Sub(refunds)
		total := gross.Sub(discounts).Add(s.charges).Sub(s.refundedOut)
		f.Refunds, f.NetSales, f.TotalSales = &refunds, &net, &total
	}
	if average && s.storeOrders > 0 {
		// MulRatio's errors are a zero denominator, answered above, and a
		// negative scale, which MinorUnits never gives.
		aov, _ := gross.Sub(discounts).MulRatio(1, s.storeOrders, money.MinorUnits(c.currency))
		f.AverageOrderValue = &aov
	}
	return f
}

// wholesale is the split of Orders and Net sales between the wholesale
// pack's buyers and everybody else; nil unless the store is enabled. A
// refund is on its order's side. A side's Net sales is null with no currency
// or with a refund on that side whose detail is not read.
func (c figureScope) wholesale(enabled bool, p periodSums) *Wholesale {
	if !enabled {
		return nil
	}
	side := func(wholesale bool) Side {
		f := c.figures(p.side(wholesale), false)
		return Side{Orders: *f.Orders, NetSales: f.NetSales}
	}
	return &Wholesale{Wholesale: side(true), Retail: side(false)}
}

// orderSumsRow and refundSumsRow are one bucket and side of a period.
type orderSumsRow struct {
	Bucket      int    `bun:"bucket"`
	Wholesale   bool   `bun:"wholesale"`
	Orders      int64  `bun:"orders"`
	StoreOrders int64  `bun:"store_orders"`
	Gross       string `bun:"gross"`
	Discounts   string `bun:"discounts"`
	Charges     string `bun:"charges"`
	Units       int64  `bun:"units"`
	Incomplete  int64  `bun:"incomplete"`
}

type refundSumsRow struct {
	Bucket     int    `bun:"bucket"`
	Wholesale  bool   `bun:"wholesale"`
	NotRead    int64  `bun:"not_read"`
	Incomplete int64  `bun:"incomplete"`
	Refunds    string `bun:"refunds"`
	Out        string `bun:"refunded_out"`
	Units      int64  `bun:"units"`
}

// instantArray is the bucket starts as a timestamptz[] literal's elements.
func instantArray(starts []time.Time) string {
	out := "{"
	for i, t := range starts {
		if i > 0 {
			out += ","
		}
		out += `"` + t.UTC().Format(time.RFC3339Nano) + `"`
	}
	return out + "}"
}

// readSums is a period's orders and refunds, by bucket and wholesale side,
// over [from, until): two statements.
func readSums(ctx context.Context, tx bun.Tx, c figureScope, starts []time.Time, from, until time.Time) (periodSums, error) {
	out := periodSums{}
	if !from.Before(until) {
		return out, nil
	}
	buckets := instantArray(starts)
	counted, countedArgs := c.countedOrder()

	var orders []orderSumsRow
	args := append([]any{buckets, c.currency, c.currency, c.currency, c.currency}, countedArgs...)
	args = append(args, from.UTC(), until.UTC())
	if err := tx.NewSelect().TableExpr(`(SELECT width_bucket(o.ordered_at, ?::timestamptz[]) AS bucket, o.wholesale,
			count(*) AS orders,
			count(*) FILTER (WHERE o.currency = ?) AS store_orders,
			COALESCE(sum(o.gross) FILTER (WHERE o.currency = ?), 0)::text AS gross,
			COALESCE(sum(o.discounts) FILTER (WHERE o.currency = ?), 0)::text AS discounts,
			COALESCE(sum(o.shipping + o.tax + o.duties) FILTER (WHERE o.currency = ?), 0)::text AS charges,
			COALESCE(sum(o.units), 0) AS units,
			count(*) FILTER (WHERE NOT o.lines_complete) AS incomplete
		FROM store_sales_order_fact o
		WHERE `+counted+` AND o.ordered_at >= ? AND o.ordered_at < ?
		GROUP BY 1, 2) AS f`, args...).ColumnExpr("f.*").Scan(ctx, &orders); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sum the orders of store %s: %w", c.storeID, err)
	}
	for _, r := range orders {
		gross, err := parseStored(r.Gross, "store_sales_order_fact", "gross")
		if err != nil {
			return nil, err
		}
		discounts, err := parseStored(r.Discounts, "store_sales_order_fact", "discounts")
		if err != nil {
			return nil, err
		}
		charges, err := parseStored(r.Charges, "store_sales_order_fact", "shipping")
		if err != nil {
			return nil, err
		}
		k := sideKey{r.Bucket, r.Wholesale}
		out[k] = out[k].add(sums{orders: r.Orders, storeOrders: r.StoreOrders, units: r.Units, gross: gross, discounts: discounts,
			charges: charges, incompleteOrders: r.Incomplete})
	}

	var refunds []refundSumsRow
	args = append([]any{buckets, c.currency, c.currency, c.storeID}, countedArgs...)
	args = append(args, from.UTC(), until.UTC())
	if err := tx.NewSelect().TableExpr(`(SELECT width_bucket(r.refunded_at, ?::timestamptz[]) AS bucket, o.wholesale,
			count(*) FILTER (WHERE NOT r.detail_fetched) AS not_read,
			count(*) FILTER (WHERE r.detail_fetched AND NOT r.lines_complete) AS incomplete,
			COALESCE(sum(r.item_refunds) FILTER (WHERE o.currency = ?), 0)::text AS refunds,
			COALESCE(sum(r.money_refunded - r.gift_card_refunded) FILTER (WHERE o.currency = ?), 0)::text AS refunded_out,
			COALESCE(sum(r.units_returned), 0) AS units
		FROM store_sales_refund_fact r
		JOIN store_sales_order_fact o ON o.store_id = r.store_id AND o.order_gid = r.order_gid
		WHERE r.store_id = ? AND NOT r.deleted AND `+counted+` AND r.refunded_at >= ? AND r.refunded_at < ?
		GROUP BY 1, 2) AS f`, args...).ColumnExpr("f.*").Scan(ctx, &refunds); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sum the refunds of store %s: %w", c.storeID, err)
	}
	for _, r := range refunds {
		item, err := parseStored(r.Refunds, "store_sales_refund_fact", "item_refunds")
		if err != nil {
			return nil, err
		}
		refundedOut, err := parseStored(r.Out, "store_sales_refund_fact", "money_refunded")
		if err != nil {
			return nil, err
		}
		k := sideKey{r.Bucket, r.Wholesale}
		out[k] = out[k].add(sums{refunds: item, refundedOut: refundedOut, returnedUnits: r.Units, notRead: r.NotRead,
			incompleteRefunds: r.Incomplete})
	}
	return out, nil
}

// readOtherCurrencies is the period's counted orders in a currency other than
// the store's, by currency, most first; nil when the store has no currency,
// since then no order's currency is the store's or another.
func readOtherCurrencies(ctx context.Context, tx bun.Tx, c figureScope, from, until time.Time) ([]CurrencyCount, error) {
	if c.currency == "" {
		return nil, nil
	}
	out := []CurrencyCount{}
	if !from.Before(until) {
		return out, nil
	}
	counted, args := c.countedOrder()
	args = append(args, from.UTC(), until.UTC(), c.currency)
	var rows []struct {
		Currency string `bun:"currency"`
		Orders   int64  `bun:"orders"`
	}
	if err := tx.NewSelect().TableExpr("store_sales_order_fact AS o").ColumnExpr("o.currency, count(*) AS orders").
		Where(counted+" AND o.ordered_at >= ? AND o.ordered_at < ? AND o.currency <> ?", args...).
		GroupExpr("o.currency").OrderExpr("orders DESC, o.currency").Scan(ctx, &rows); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("count the other currencies of store %s: %w", c.storeID, err)
	}
	for _, r := range rows {
		out = append(out, CurrencyCount{Currency: r.Currency, Orders: r.Orders})
	}
	return out, nil
}

// readBuyers splits the period's counted orders by their buyer (D11): a
// buyer is new when their first counted order in the store's facts is in the
// period, returning when it is before it; an order with no buyer is unknown.
func readBuyers(ctx context.Context, tx bun.Tx, c figureScope, from, until time.Time) (*Buyers, error) {
	b := &Buyers{}
	if !from.Before(until) {
		return b, nil
	}
	counted, countedArgs := c.countedOrder()
	args := append(append([]any{}, countedArgs...), from.UTC(), until.UTC())
	args = append(args, countedArgs...)
	args = append(args, from.UTC())
	var row struct {
		Everyone  int64 `bun:"everyone"`
		Returning int64 `bun:"returning"`
		Unknown   int64 `bun:"unknown"`
	}
	// A buyer of the period is returning when they hold a counted order
	// before it: one probe of the buyer index each, not a scan of their
	// history.
	if err := tx.NewSelect().TableExpr(`(WITH p AS (
			SELECT o.buyer_key FROM store_sales_order_fact o WHERE `+counted+` AND o.ordered_at >= ? AND o.ordered_at < ?
		), buyers AS (
			SELECT DISTINCT buyer_key FROM p WHERE buyer_key <> ''
		)
		SELECT (SELECT count(*) FROM buyers) AS everyone,
			(SELECT count(*) FROM buyers b WHERE EXISTS (SELECT 1 FROM store_sales_order_fact o
				WHERE `+counted+` AND o.buyer_key = b.buyer_key AND o.ordered_at < ?)) AS returning,
			(SELECT count(*) FROM p WHERE buyer_key = '') AS unknown) AS f`, args...).ColumnExpr("f.*").Scan(ctx, &row); err != nil {
		return nil, fmt.Errorf("count the buyers of store %s: %w", c.storeID, err)
	}
	b.New, b.Returning, b.Unknown = row.Everyone-row.Returning, row.Returning, row.Unknown
	return b, nil
}

type productRow struct {
	List  string         `bun:"list"`
	Key   string         `bun:"product_key"`
	Title string         `bun:"title"`
	Units int64          `bun:"units"`
	Sales sql.NullString `bun:"sales"`
}

// readProducts fills the two top lists, productCount and refundsWithoutItem,
// in one statement.
//
// A product's figures are its lines sold in the period's orders, less its
// refund lines in the period's refunds: by sales, the lines' gross less their
// discounts less the refund lines' subtotals, in the store's currency only;
// by units, the units sold less those returned, in every currency. A line
// with no product is grouped by its title (its fact's key). A product's title
// is that of its newest line SOLD in the period: the line of its latest order
// in the period, then the newest version of the line, then the greatest
// title. A refund of an older line never renames it. A product the period
// holds only through refunds of older lines takes the title of the line of
// its latest refund. productCount is how many products
// the period has, the list by units' whole length.
//
// A list is null when a figure it is made of is not measured: by sales with
// no currency; both, with productCount and refundsWithoutItem, with a refund
// not read or one holding a full page of lines, whose lines past the page
// may name any product.
func readProducts(ctx context.Context, tx bun.Tx, c figureScope, from, until time.Time, all sums, s *Statistics) error {
	// A refund whose lines fill a page may hold lines past it: their units
	// and their money are on no product, so neither list is known, and
	// neither is what names no product.
	linesKnown := all.notRead == 0 && all.incompleteRefunds == 0
	bySales := c.currency != "" && linesKnown
	byUnits := linesKnown
	s.ProductsBySales, s.ProductsByUnits, s.ProductCount, s.RefundsWithoutItem = nil, nil, nil, nil
	if !bySales && !byUnits {
		return nil
	}
	counted, countedArgs := c.countedOrder()
	args := append([]any{c.currency, c.currency}, countedArgs...)
	args = append(args, from.UTC(), until.UTC(), c.currency, c.currency, c.currency, c.storeID)
	args = append(args, countedArgs...)
	args = append(args, from.UTC(), until.UTC(), topRows, topRows)
	var rows []productRow
	// Each branch is summed by product as it is read, by hash, so the lines
	// are never kept or sorted; a product's newest line is the greatest of
	// newestKey over its sold lines, or over its refunded lines when it has
	// none, and its title what follows the key's two instants.
	if err := tx.NewSelect().TableExpr(`(WITH sold AS (
			SELECT l.product_key, sum(l.sold_quantity) AS units,
				sum(CASE WHEN o.currency = ? THEN l.gross - l.discounts END) AS sales,
				count(*) FILTER (WHERE o.currency = ?) AS priced, 0::numeric AS returned,
				max(`+newestKey("o.ordered_at")+`) AS newest, false AS refunded
			FROM store_sales_order_fact o
			JOIN store_sales_line_fact l ON l.store_id = o.store_id AND l.order_gid = o.order_gid
			WHERE `+counted+` AND o.ordered_at >= ? AND o.ordered_at < ? AND NOT l.deleted AND NOT l.gift_card
			GROUP BY l.product_key
		), back AS (
			SELECT l.product_key, -sum(rl.quantity) AS units,
				-sum(CASE WHEN o.currency = ? THEN rl.subtotal END) AS sales,
				count(*) FILTER (WHERE o.currency = ?) AS priced,
				COALESCE(sum(CASE WHEN o.currency = ? THEN rl.subtotal END), 0) AS returned,
				max(`+newestKey("r.refunded_at")+`) AS newest, true AS refunded
			FROM store_sales_refund_fact r
			JOIN store_sales_order_fact o ON o.store_id = r.store_id AND o.order_gid = r.order_gid
			JOIN store_sales_refund_line_fact rl ON rl.store_id = r.store_id AND rl.refund_gid = r.refund_gid
			JOIN store_sales_line_fact l ON l.store_id = rl.store_id AND l.line_gid = rl.line_gid
			WHERE r.store_id = ? AND NOT r.deleted AND `+counted+` AND r.refunded_at >= ? AND r.refunded_at < ?
				AND NOT l.deleted AND NOT l.gift_card
			GROUP BY l.product_key
		), per AS (
			SELECT product_key, substr(COALESCE(max(newest) FILTER (WHERE NOT refunded), max(newest)), 41) AS title, sum(units)::bigint AS units, sum(sales) AS sales,
				sum(priced) AS priced, sum(returned) AS returned
			FROM (SELECT * FROM sold UNION ALL SELECT * FROM back) AS every GROUP BY product_key
		)
		(SELECT 'sales' AS list, product_key, title, units, sales::text AS sales FROM per WHERE priced > 0
			ORDER BY per.sales DESC, title, product_key LIMIT ?)
		UNION ALL
		(SELECT 'units', product_key, title, units, NULL FROM per ORDER BY units DESC, title, product_key LIMIT ?)
		UNION ALL
		SELECT 'count', '', '', count(*), NULL FROM per
		UNION ALL
		SELECT 'returned', '', '', 0, COALESCE(sum(returned), 0)::text FROM per
		) AS f`, args...).ColumnExpr("f.*").Scan(ctx, &rows); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("list the products of store %s: %w", c.storeID, err)
	}
	sales, units := []Product{}, []Product{}
	var count int64
	var returned money.Amount
	for _, r := range rows {
		switch r.List {
		case "sales":
			a, err := parseStored(r.Sales.String, "store_sales_line_fact", "gross")
			if err != nil {
				return err
			}
			sales = append(sales, Product{Key: r.Key, Title: r.Title, Amount: &a})
		case "units":
			n := r.Units
			units = append(units, Product{Key: r.Key, Title: r.Title, Count: &n})
		case "count":
			count = r.Units
		case "returned":
			a, err := parseStored(r.Sales.String, "store_sales_refund_line_fact", "subtotal")
			if err != nil {
				return err
			}
			returned = a
		}
	}
	if bySales {
		s.ProductsBySales = sales
		// The part of Refunds that names no product: the refunds of items less
		// their refund lines on lines that are not gift cards. A KNOWN LIMIT: a
		// refund line naming a line the store has since deleted is not an item
		// return, and the refund's amount for it stays here, in the refunds of
		// items that name no product; once the line is gone the fold cannot
		// tell what kind of line it was. Today's mirror does not produce this.
		without := all.refunds.Sub(returned)
		s.RefundsWithoutItem = &without
	}
	if byUnits {
		s.ProductsByUnits, s.ProductCount = units, &count
	}
	return nil
}

func countOf(n int64) *int64 { return &n }

// newestKey is a text that sorts as (at, the line's version, its title): two
// instants as fixed-width microseconds since the epoch, then the title from
// its 41st character, so max over a product's lines is its newest line.
func newestKey(at string) string {
	return "lpad((extract(epoch FROM " + at + ") * 1000000)::bigint::text, 20, '0') || " +
		"lpad((extract(epoch FROM l.line_version) * 1000000)::bigint::text, 20, '0') || l.title"
}
