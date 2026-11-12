package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/core/money"
)

// fold.go -- a store's walk, and the fold: each page of walked rows becomes
// facts in the store_sales_ tables, in the one transaction that also keeps
// where the walk stands. What each touched order's facts sum to is derive.go's.
//
// # One store's walk
//
// walkOneStore is what walkStore is set to. A store whose purging is set is
// not walked: its facts are being deleted, and a fact written now would be
// deleted with them or outlive the purge. Otherwise the store's floor is found
// (ensureFloor), and each of its three sources -- order, line and refund, the
// first three of walkedConcepts, each with its read in walkedSources -- is
// walked with walkSource from its store_sales_watermark row, at most
// maxPagesPerRun pages. A source that fails is the store's error, and the
// other sources are still walked; a store rebuilt under its walk is left at
// once, for the next run. Last, the store's walked_through is set to the
// least of its three sources' walked_through, and null while any is null.
//
// # One transaction a page
//
// Each page, and each window that held no row, is one writeTx under the
// generation the run read before the store's first page was asked: the page's
// facts (foldPage), the source's watermark and the store's last_change_at
// (keepWatermark). So the facts and the watermark commit together or not at
// all: a crash between them can neither lose a page (a watermark moved past
// facts never written) nor repeat one. A rebuild that lands after the page was
// read raises the generation, so the page writes nothing, the watermark does
// not move, and the store is left for the next run, which starts where the
// rebuild left it.
//
// NOTHING OF THE ENGINE AND NO FUNCTION VALUE INSIDE. Everything a page's
// transaction needs is read before it begins: the rows are the page's, and the
// wholesale judgement is a value, wholesaleState, whose method is called.
//
// # A row can arrive twice, and out of order
//
// The walk reads an overlap again on every run, a row rewritten mid-walk comes
// back on a later page, and another node or the check may fold an older
// version after a newer one. So every write of a fact is guarded by its
// version: an upsert that updates only when the version it carries is at
// least the one held. The same version folded again writes what is there; an
// older one changes nothing. Within one page a row is kept once, at its newest
// version.
//
// A TOMBSTONE IS KEPT AS A FACT MARKED DELETED, under the same guard, with its
// version and recorded_at; a fact that exists keeps its other columns, and one
// that does not is inserted with zeros, empty strings and its version as its
// dates, none of which is ever read. It then refuses every older version: the
// live one a later window of a first fill reads again, or one another node
// folds after it. A row that comes back after its tombstone is a live row with
// a newer version, and is written with deleted false. A deleted refund's lines
// go with it: they have no version of their own, and are only ever written
// with their refund, replaced whole when the refund's own write took effect.
//
// # An unreadable row
//
// A row the rules cannot read writes no fact. It is counted in the run's
// report (RollupReport.Unreadable) and logged once, with the store, the source
// and the rules' error, which names the row's gid and the field and never a
// value. What depends on it shows that it is missing: an order whose line could
// not be read has its recorded quantities short of subtotalLineItemsQuantity,
// so lines_complete is false (derive.go).
//
// # Errors
//
// An error names the store and the source with a fixed sentence. A statement
// the database refuses is reported by its SQLSTATE alone: the server's own
// message can quote a value of the row.

// pageResult is what one page's fold did.
type pageResult struct {
	Facts, Unreadable int
	Orders            []string // the order gids the page touched
	LastRecorded      time.Time

	unreadable []error // the rules' errors: each a gid and a field, never a value
}

// walkedSource is one source of a store's walk: its name, the concept its
// rows are of, and the read that walks it.
type walkedSource struct{ source, concept, query string }

// walkedSources are the three sources a store is walked by, in the order
// they are walked: the first three entries of walkedConcepts.
var walkedSources = []walkedSource{
	{sourceOrder, walkedConcepts[0], "storeStatisticsOrdersChanged"},
	{sourceLine, walkedConcepts[1], "storeStatisticsLinesChanged"},
	{sourceRefund, walkedConcepts[2], "storeStatisticsRefundsChanged"},
}

// walkOneStore is what walkStore is set to. See "One store's walk".
func (i *Integration) walkOneStore(ctx context.Context, store map[string]any, generation int64, now time.Time, wholesale wholesaleState, report *RollupReport) error {
	id, _ := store["id"].(string)
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("walk a store: %w", errNoStore)
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return errors.New("this node has no database handle, so the store statistics cannot be written")
	}
	if report.Rows == nil {
		report.Rows = map[string]int{}
	}
	var purging bool
	if err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		purging, err = storePurging(ctx, tx, id)
		return err
	}); err != nil {
		return err
	}
	if purging {
		return nil
	}
	floor, err := ensureFloor(ctx, i, id, now)
	if err != nil {
		return err
	}
	clock := time.Now
	if i.now != nil {
		clock = i.now
	}
	var failures []error
	for _, s := range walkedSources {
		err := i.walkOneSource(ctx, db, id, s, floor, generation, clock, wholesale, report)
		if errors.Is(err, errRebuilt) {
			return err
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	if err := writeTx(ctx, db, id, generation, func(ctx context.Context, tx bun.Tx) error {
		return keepWalked(ctx, tx, id)
	}); err != nil {
		if errors.Is(err, errRebuilt) {
			return err
		}
		failures = append(failures, fmt.Errorf("store %s: keep how far the store is walked: %w", id, sanitized(err)))
	}
	return errors.Join(failures...)
}

// walkOneSource walks one source of the store from its watermark.
func (i *Integration) walkOneSource(ctx context.Context, db *bun.DB, storeID string, s walkedSource, floor time.Time, generation int64,
	clock func() time.Time, wholesale wholesaleState, report *RollupReport) error {
	w, err := watermarkOf(ctx, db, storeID, s.source)
	if err != nil {
		return err
	}
	kept := w
	next, caughtUp, err := walkSource(ctx, i.engine, s.query, storeID, floor, w, clock, maxPagesPerRun, func(page walkPage) error {
		res, err := keepPage(ctx, db, storeID, s.source, generation, page, wholesale)
		if err != nil {
			return err
		}
		kept = walkWindow{Through: page.Through, Width: page.Width}
		report.Rows[s.source] += len(page.Rows)
		report.Facts += res.Facts
		report.Unreadable += res.Unreadable
		for _, e := range res.unreadable {
			i.logger.Warn("Store statistics: a mirrored row could not be read; it was counted and no fact was written",
				"component", "storestats", "storeId", storeID, "source", s.source, "error", e.Error())
		}
		return nil
	})
	if errors.Is(err, errRebuilt) {
		return err
	}
	// A width the walk changed with no page handed on -- a page that
	// outlasted a read halves it -- is kept for the next run.
	if next != kept && !next.Through.IsZero() {
		if kerr := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			return keepWatermark(ctx, tx, storeID, s.source, next, time.Time{}, time.Time{})
		}); kerr != nil {
			if errors.Is(kerr, errRebuilt) {
				return kerr
			}
			err = errors.Join(err, fmt.Errorf("store %s, %s: keep the walk's window: %w", storeID, s.source, sanitized(kerr)))
		}
	}
	if err != nil {
		return err
	}
	if caughtUp.IsZero() {
		report.Behind = append(report.Behind, storeID+"/"+s.source)
	}
	return nil
}

// watermarkOf reads where the source's walk stands: the zero window when it
// has not been walked.
func watermarkOf(ctx context.Context, db *bun.DB, storeID, source string) (walkWindow, error) {
	var through time.Time
	var seconds int64
	err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		return tx.NewSelect().Table("store_sales_watermark").Column("through", "window_seconds").
			Where("store_id = ? AND source = ?", storeID, source).Scan(ctx, &through, &seconds)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return walkWindow{}, nil
	}
	if err != nil {
		return walkWindow{}, fmt.Errorf("store %s, %s: read the watermark: %w", storeID, source, sanitized(err))
	}
	return walkWindow{Through: through.UTC(), Width: time.Duration(seconds) * time.Second}, nil
}

// keepPage folds one walked page and keeps its watermark, in one writeTx. See
// "One transaction a page".
func keepPage(ctx context.Context, db *bun.DB, storeID, source string, generation int64, page walkPage, wholesale wholesaleState) (pageResult, error) {
	var res pageResult
	err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
		var err error
		res, err = foldPage(ctx, tx, storeID, source, page.Rows, wholesale)
		if err != nil {
			return err
		}
		return keepWatermark(ctx, tx, storeID, source, walkWindow{Through: page.Through, Width: page.Width}, page.CaughtUp, res.LastRecorded)
	})
	if errors.Is(err, errRebuilt) {
		return pageResult{}, err
	}
	if err != nil {
		return pageResult{}, fmt.Errorf("store %s, %s: a page could not be folded: %w", storeID, source, sanitized(err))
	}
	return res, nil
}

// sanitized is an error as this file passes it on: a statement the database
// refused is its SQLSTATE alone, since the server's message can quote a value
// of the row; any other error is kept.
func sanitized(err error) error {
	var pgerr pgdriver.Error
	if errors.As(err, &pgerr) {
		return fmt.Errorf("the database refused the statement (SQLSTATE %s)", pgerr.Field('C'))
	}
	return err
}

// ---- reading a page ----

type foldedOrder struct {
	fact      OrderFact
	gone      bool
	wholesale bool
}

type foldedLine struct {
	fact LineFact
	gone bool
}

type foldedRefund struct {
	fact RefundFact
	gone bool
}

// pageFacts is a page as the fold writes it: each readable row once, at its
// newest version, and the rules' error for each row that could not be read.
type pageFacts struct {
	source       string
	orders       []foldedOrder
	lines        []foldedLine
	refunds      []foldedRefund
	unreadable   []error
	lastRecorded time.Time
}

// pageOf is a page's rows as the rules read them.
func pageOf(rows []map[string]any) []fields {
	out := make([]fields, len(rows))
	for n, r := range rows {
		out[n] = mapFields(r)
	}
	return out
}

// errAnotherStore is a row of another store than the one walked: no fact.
func errAnotherStore(concept, gid string) error {
	return fmt.Errorf("storestats: %s %s: storeId is not the store walked", concept, gid)
}

// readPage reads a page of a source with the rules. Pure: called inside the
// page's transaction, it reads the rows it is handed and nothing else.
func readPage(storeID, source string, rows []fields, wholesale wholesaleState) (pageFacts, error) {
	p := pageFacts{source: source}
	seen := map[string]int{}
	linesPage := refundLinesPage()
	for _, row := range rows {
		switch source {
		case sourceOrder:
			f, kind, err := orderFactFrom(row)
			switch {
			case kind == rowUnreadable:
				p.unreadable = append(p.unreadable, err)
				continue
			case f.StoreID != storeID:
				p.unreadable = append(p.unreadable, errAnotherStore("order", f.OrderGID))
				continue
			}
			o := foldedOrder{fact: f, gone: kind == rowGone}
			if !o.gone {
				o.wholesale = wholesale.isWholesale(f.Email)
			}
			p.lastRecorded = later(p.lastRecorded, f.RecordedAt)
			if n, ok := seen[f.OrderGID]; !ok {
				seen[f.OrderGID] = len(p.orders)
				p.orders = append(p.orders, o)
			} else if f.Version.After(p.orders[n].fact.Version) {
				p.orders[n] = o
			}
		case sourceLine:
			f, kind, err := lineFactFrom(row)
			switch {
			case kind == rowUnreadable:
				p.unreadable = append(p.unreadable, err)
				continue
			case f.StoreID != storeID:
				p.unreadable = append(p.unreadable, errAnotherStore("orderLineItem", f.LineGID))
				continue
			}
			l := foldedLine{fact: f, gone: kind == rowGone}
			// A line carries no syncedAt: its write time stands for it.
			p.lastRecorded = later(p.lastRecorded, f.Version)
			if n, ok := seen[f.LineGID]; !ok {
				seen[f.LineGID] = len(p.lines)
				p.lines = append(p.lines, l)
			} else if f.Version.After(p.lines[n].fact.Version) {
				p.lines[n] = l
			}
		case sourceRefund:
			f, kind, err := refundFactFrom(row, linesPage)
			switch {
			case kind == rowUnreadable:
				p.unreadable = append(p.unreadable, err)
				continue
			case f.StoreID != storeID:
				p.unreadable = append(p.unreadable, errAnotherStore("refund", f.RefundGID))
				continue
			}
			r := foldedRefund{fact: f, gone: kind == rowGone}
			p.lastRecorded = later(p.lastRecorded, f.RecordedAt)
			if n, ok := seen[f.RefundGID]; !ok {
				seen[f.RefundGID] = len(p.refunds)
				p.refunds = append(p.refunds, r)
			} else if f.Version.After(p.refunds[n].fact.Version) {
				p.refunds[n] = r
			}
		default:
			return pageFacts{}, errors.New("storestats: a page of a source the walk does not have")
		}
	}
	return p, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// ---- the rows written ----

type orderFactRow struct {
	bun.BaseModel `bun:"table:store_sales_order_fact,alias:f"`

	StoreID                string    `bun:"store_id"`
	OrderGID               string    `bun:"order_gid"`
	OrderVersion           time.Time `bun:"order_version"`
	Deleted                bool      `bun:"deleted"`
	RecordedAt             time.Time `bun:"recorded_at"`
	OrderedAt              time.Time `bun:"ordered_at"`
	Currency               string    `bun:"currency"`
	Cancelled              bool      `bun:"cancelled"`
	Test                   bool      `bun:"test"`
	TaxesIncluded          bool      `bun:"taxes_included"`
	BuyerKey               string    `bun:"buyer_key"`
	Wholesale              bool      `bun:"wholesale"`
	Shipping               string    `bun:"shipping"`
	Tax                    string    `bun:"tax"`
	Duties                 string    `bun:"duties"`
	ExpectedQuantity       int       `bun:"expected_quantity"`
	ShopifyCurrentSubtotal *string   `bun:"shopify_current_subtotal"`
	ShopifyTotalDiscounts  *string   `bun:"shopify_total_discounts"`
	ShopifyTotalRefunded   *string   `bun:"shopify_total_refunded"`
}

type lineFactRow struct {
	bun.BaseModel `bun:"table:store_sales_line_fact,alias:f"`

	StoreID            string    `bun:"store_id"`
	LineGID            string    `bun:"line_gid"`
	OrderGID           string    `bun:"order_gid"`
	LineVersion        time.Time `bun:"line_version"`
	Deleted            bool      `bun:"deleted"`
	Currency           string    `bun:"currency"`
	GiftCard           bool      `bun:"gift_card"`
	ProductKey         string    `bun:"product_key"`
	Title              string    `bun:"title"`
	Quantity           int       `bun:"quantity"`
	CurrentQuantity    int       `bun:"current_quantity"`
	UnitPrice          string    `bun:"unit_price"`
	AllocatedDiscounts string    `bun:"allocated_discounts"`
}

type refundFactRow struct {
	bun.BaseModel `bun:"table:store_sales_refund_fact,alias:f"`

	StoreID          string    `bun:"store_id"`
	RefundGID        string    `bun:"refund_gid"`
	OrderGID         string    `bun:"order_gid"`
	RefundVersion    time.Time `bun:"refund_version"`
	Deleted          bool      `bun:"deleted"`
	RecordedAt       time.Time `bun:"recorded_at"`
	RefundedAt       time.Time `bun:"refunded_at"`
	MoneyRefunded    string    `bun:"money_refunded"`
	ShippingRefunded *string   `bun:"shipping_refunded"`
	TaxRefunded      *string   `bun:"tax_refunded"`
	DutiesRefunded   *string   `bun:"duties_refunded"`
	DetailFetched    bool      `bun:"detail_fetched"`
	LinesComplete    bool      `bun:"lines_complete"`
}

type refundLineFactRow struct {
	bun.BaseModel `bun:"table:store_sales_refund_line_fact,alias:f"`

	StoreID   string `bun:"store_id"`
	RefundGID string `bun:"refund_gid"`
	Position  int    `bun:"position"`
	OrderGID  string `bun:"order_gid"`
	LineGID   string `bun:"line_gid"`
	Quantity  int    `bun:"quantity"`
	Subtotal  string `bun:"subtotal"`
	Tax       string `bun:"tax"`
}

// A NUMERIC column is written from, and read back to, an Amount's decimal
// text: never through a float.
func amountText(a money.Amount) string { return a.String() }

func optionalText(a *money.Amount) *string {
	if a == nil {
		return nil
	}
	s := a.String()
	return &s
}

// recordedOr is a fact's recorded_at: the row's syncedAt, else its version.
func recordedOr(recorded, version time.Time) time.Time {
	if recorded.IsZero() {
		return version
	}
	return recorded
}

func orderRowOf(storeID string, o foldedOrder) orderFactRow {
	f := o.fact
	if o.gone {
		return orderFactRow{StoreID: storeID, OrderGID: f.OrderGID, OrderVersion: f.Version, Deleted: true,
			RecordedAt: recordedOr(f.RecordedAt, f.Version), OrderedAt: f.Version, Shipping: "0", Tax: "0", Duties: "0"}
	}
	return orderFactRow{
		StoreID: storeID, OrderGID: f.OrderGID, OrderVersion: f.Version,
		RecordedAt: recordedOr(f.RecordedAt, f.Version), OrderedAt: f.OrderedAt,
		Currency: f.Currency, Cancelled: f.Cancelled, Test: f.Test, TaxesIncluded: f.TaxIncluded,
		BuyerKey: f.BuyerKey, Wholesale: o.wholesale,
		Shipping: amountText(f.Shipping), Tax: amountText(f.Tax), Duties: amountText(f.Duties),
		ExpectedQuantity:       f.ExpectedQuantity,
		ShopifyCurrentSubtotal: optionalText(f.ShopifyCurrentSubtotal),
		ShopifyTotalDiscounts:  optionalText(f.ShopifyTotalDiscounts),
		ShopifyTotalRefunded:   optionalText(f.ShopifyTotalRefunded),
	}
}

func lineRowOf(storeID string, l foldedLine) lineFactRow {
	f := l.fact
	if l.gone {
		return lineFactRow{StoreID: storeID, LineGID: f.LineGID, OrderGID: f.OrderGID, LineVersion: f.Version, Deleted: true,
			UnitPrice: "0", AllocatedDiscounts: "0"}
	}
	return lineFactRow{
		StoreID: storeID, LineGID: f.LineGID, OrderGID: f.OrderGID, LineVersion: f.Version,
		Currency: f.Currency, GiftCard: f.GiftCard, ProductKey: f.ProductKey, Title: f.Title,
		Quantity: f.Quantity, CurrentQuantity: f.CurrentQuantity,
		UnitPrice: amountText(f.UnitPrice), AllocatedDiscounts: amountText(f.AllocatedDiscounts),
	}
}

func refundRowOf(storeID string, r foldedRefund) refundFactRow {
	f := r.fact
	if r.gone {
		return refundFactRow{StoreID: storeID, RefundGID: f.RefundGID, OrderGID: f.OrderGID, RefundVersion: f.Version, Deleted: true,
			RecordedAt: recordedOr(f.RecordedAt, f.Version), RefundedAt: f.Version, MoneyRefunded: "0"}
	}
	return refundFactRow{
		StoreID: storeID, RefundGID: f.RefundGID, OrderGID: f.OrderGID, RefundVersion: f.Version,
		RecordedAt: recordedOr(f.RecordedAt, f.Version), RefundedAt: f.RefundedAt,
		MoneyRefunded:    amountText(f.MoneyRefunded),
		ShippingRefunded: optionalText(f.ShippingRefunded), TaxRefunded: optionalText(f.TaxRefunded), DutiesRefunded: optionalText(f.DutiesRefunded),
		DetailFetched: f.DetailFetched, LinesComplete: f.LinesComplete,
	}
}

// ---- the fold ----

// foldPage writes one page's facts inside tx: guarded upserts, a retired row
// kept as a fact marked deleted, a refund's lines replaced with it, then
// deriveOrders over the orders the page touched. It never writes a derived
// column or checked_at. See the file's comment.
func foldPage(ctx context.Context, tx bun.Tx, storeID, source string, rows []map[string]any, wholesale wholesaleState) (pageResult, error) {
	page, err := readPage(storeID, source, pageOf(rows), wholesale)
	if err != nil {
		return pageResult{}, err
	}
	res := pageResult{
		Facts:        len(page.orders) + len(page.lines) + len(page.refunds),
		Unreadable:   len(page.unreadable),
		LastRecorded: page.lastRecorded,
		unreadable:   page.unreadable,
	}
	touched := map[string]bool{}

	// Orders.
	var liveOrders, goneOrders []orderFactRow
	for _, o := range page.orders {
		touched[o.fact.OrderGID] = true
		if o.gone {
			goneOrders = append(goneOrders, orderRowOf(storeID, o))
		} else {
			liveOrders = append(liveOrders, orderRowOf(storeID, o))
		}
	}
	if len(liveOrders) > 0 {
		if _, err := tx.NewInsert().Model(&liveOrders).
			On("CONFLICT (store_id, order_gid) DO UPDATE").
			Where("EXCLUDED.order_version >= f.order_version").
			Exec(ctx); err != nil {
			return pageResult{}, fmt.Errorf("write the order facts: %w", err)
		}
	}
	if len(goneOrders) > 0 {
		if _, err := tx.NewInsert().Model(&goneOrders).
			On("CONFLICT (store_id, order_gid) DO UPDATE").
			Set("order_version = EXCLUDED.order_version").Set("deleted = true").Set("recorded_at = EXCLUDED.recorded_at").
			Where("EXCLUDED.order_version >= f.order_version").
			Exec(ctx); err != nil {
			return pageResult{}, fmt.Errorf("mark the retired orders' facts deleted: %w", err)
		}
	}

	// Lines.
	var liveLines, goneLines []lineFactRow
	for _, l := range page.lines {
		if l.fact.OrderGID != "" {
			touched[l.fact.OrderGID] = true
		}
		if l.gone {
			goneLines = append(goneLines, lineRowOf(storeID, l))
		} else {
			liveLines = append(liveLines, lineRowOf(storeID, l))
		}
	}
	if len(liveLines) > 0 {
		if _, err := tx.NewInsert().Model(&liveLines).
			On("CONFLICT (store_id, line_gid) DO UPDATE").
			Where("EXCLUDED.line_version >= f.line_version").
			Exec(ctx); err != nil {
			return pageResult{}, fmt.Errorf("write the line facts: %w", err)
		}
	}
	if len(goneLines) > 0 {
		// The order a deleted line named is touched: the fact's, which its
		// tombstone may not carry.
		var orders []string
		if err := tx.NewInsert().Model(&goneLines).
			On("CONFLICT (store_id, line_gid) DO UPDATE").
			Set("line_version = EXCLUDED.line_version").Set("deleted = true").
			Where("EXCLUDED.line_version >= f.line_version").
			Returning("order_gid").
			Scan(ctx, &orders); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return pageResult{}, fmt.Errorf("mark the retired lines' facts deleted: %w", err)
		}
		for _, o := range orders {
			if o != "" {
				touched[o] = true
			}
		}
	}

	// Refunds, and their lines with them.
	var liveRefunds, goneRefunds []refundFactRow
	linesOf := map[string][]RefundLineFact{}
	orderOf := map[string]string{}
	for _, r := range page.refunds {
		if r.fact.OrderGID != "" {
			touched[r.fact.OrderGID] = true
		}
		if r.gone {
			goneRefunds = append(goneRefunds, refundRowOf(storeID, r))
		} else {
			liveRefunds = append(liveRefunds, refundRowOf(storeID, r))
			linesOf[r.fact.RefundGID] = r.fact.Lines
			orderOf[r.fact.RefundGID] = r.fact.OrderGID
		}
	}
	var replaced, removed, removedOrders []string
	if len(liveRefunds) > 0 {
		if err := tx.NewInsert().Model(&liveRefunds).
			On("CONFLICT (store_id, refund_gid) DO UPDATE").
			Where("EXCLUDED.refund_version >= f.refund_version").
			Returning("refund_gid").
			Scan(ctx, &replaced); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return pageResult{}, fmt.Errorf("write the refund facts: %w", err)
		}
	}
	if len(goneRefunds) > 0 {
		if err := tx.NewInsert().Model(&goneRefunds).
			On("CONFLICT (store_id, refund_gid) DO UPDATE").
			Set("refund_version = EXCLUDED.refund_version").Set("deleted = true").Set("recorded_at = EXCLUDED.recorded_at").
			Where("EXCLUDED.refund_version >= f.refund_version").
			Returning("refund_gid, order_gid").
			Scan(ctx, &removed, &removedOrders); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return pageResult{}, fmt.Errorf("mark the retired refunds' facts deleted: %w", err)
		}
		for _, o := range removedOrders {
			if o != "" {
				touched[o] = true
			}
		}
	}
	if gone := append(append([]string(nil), replaced...), removed...); len(gone) > 0 {
		if _, err := tx.NewDelete().TableExpr("store_sales_refund_line_fact").
			Where("store_id = ? AND refund_gid IN (?)", storeID, bun.In(gone)).
			Exec(ctx); err != nil {
			return pageResult{}, fmt.Errorf("remove the refunds' old lines: %w", err)
		}
	}
	var refundLines []refundLineFactRow
	sort.Strings(replaced)
	for _, gid := range replaced {
		for _, l := range linesOf[gid] {
			refundLines = append(refundLines, refundLineFactRow{StoreID: storeID, RefundGID: gid, Position: l.Position,
				OrderGID: orderOf[gid], LineGID: l.LineGID, Quantity: l.Quantity, Subtotal: amountText(l.Subtotal), Tax: amountText(l.Tax)})
		}
	}
	if len(refundLines) > 0 {
		if _, err := tx.NewInsert().Model(&refundLines).Exec(ctx); err != nil {
			return pageResult{}, fmt.Errorf("write the refunds' lines: %w", err)
		}
	}

	for gid := range touched {
		res.Orders = append(res.Orders, gid)
	}
	sort.Strings(res.Orders)
	if err := deriveOrders(ctx, tx, storeID, res.Orders); err != nil {
		return pageResult{}, err
	}
	return res, nil
}

// keepWatermark keeps where the source's walk stands -- through by GREATEST,
// the window's width, walked_through by GREATEST when caughtUp is set -- and
// the store's last_change_at by GREATEST with lastRecorded when it is set.
func keepWatermark(ctx context.Context, tx bun.Tx, storeID, source string, w walkWindow, caughtUp, lastRecorded time.Time) error {
	var walked *time.Time
	if !caughtUp.IsZero() {
		c := caughtUp.UTC()
		walked = &c
	}
	seconds := int64(windowWidth(w.Width) / time.Second)
	if _, err := tx.ExecContext(ctx, `INSERT INTO store_sales_watermark AS m (store_id, source, through, window_seconds, walked_through)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (store_id, source) DO UPDATE SET
			through = GREATEST(m.through, EXCLUDED.through),
			window_seconds = EXCLUDED.window_seconds,
			walked_through = GREATEST(m.walked_through, EXCLUDED.walked_through)`,
		storeID, source, w.Through.UTC(), seconds, walked); err != nil {
		return fmt.Errorf("keep the watermark: %w", err)
	}
	if !lastRecorded.IsZero() {
		if _, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET last_change_at = GREATEST(last_change_at, ?) WHERE store_id = ?`,
			lastRecorded.UTC(), storeID); err != nil {
			return fmt.Errorf("keep when the store last changed: %w", err)
		}
	}
	return nil
}

// keepWalked sets the store's walked_through to the least of its three
// sources' walked_through, and to null while any of them is null or has no
// watermark.
func keepWalked(ctx context.Context, tx bun.Tx, storeID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET walked_through = (
			SELECT CASE WHEN count(walked_through) = 3 THEN min(walked_through) END
			FROM store_sales_watermark WHERE store_id = ? AND source IN (?, ?, ?))
		WHERE store_id = ?`, storeID, sourceOrder, sourceLine, sourceRefund, storeID); err != nil {
		return fmt.Errorf("keep how far the store is walked: %w", err)
	}
	return nil
}
