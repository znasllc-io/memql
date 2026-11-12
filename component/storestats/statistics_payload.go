package storestats

import (
	"time"

	"github.com/znasllc-io/memql/core/money"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics_payload.go -- the answer of builtin storeStatistics, as Go holds
// it and as the wire carries it.
//
// NOTHING NOT KNOWN IS EVER ZERO. Every figure, count and moment is a pointer,
// and nil is answered as null: a page that shows a dash with its reason for a
// null cannot be told a store sold nothing when nothing was measured. Money is
// a decimal string.
//
// NOTHING PERSONAL. The answer carries the store's name, domain, currency and
// zone, the period's bounds and the recording state; no buyer's email, name or
// key. Task 18's lists carry a product's title and key, and counts.

// Statistics is one answer. readFrame fills its frame, every field above
// Figures; Task 18's readFigures fills Figures and everything after it, which
// is null or empty until then.
type Statistics struct {
	StoreID, SiteID string
	// Destination is "binding", "previewBinding" or "both": the name of the
	// field of the site's row that names the store, or both when the two
	// name the one store.
	Destination string

	StoreName, StoreDomain, Currency *string
	// Timezone is the zone the store recorded, as recorded, whether or not it
	// loads; nil when it recorded none.
	Timezone *string
	// PeriodZone is the zone the period was cut in: the store's zone when it
	// loads, otherwise "UTC"; "" for a store the walk holds no row for. A
	// recorded Timezone with PeriodZone "UTC" is a zone that did not load.
	PeriodZone                                               string
	Development, TaxesIncluded, OlderOrderChangesNotRecorded *bool

	Period statsperiod.Period
	// Bounds is the period cut in Loc; nil for a store the walk holds no row
	// for, whose frame says its ids, its destination and that it is starting.
	Bounds *statsperiod.Bounds
	// Loc is the zone every moment of the answer is written in.
	Loc *time.Location

	HistorySince, LastChangeAt *time.Time
	// AsOf is set only while something stands in the way now
	// (Recording.Blocked): the walk's last clear run, and nil when there was
	// none. Nil otherwise: the figures are current.
	AsOf      *time.Time
	Recording Recording
	// Rebuilding is nil for a store the walk holds no row for.
	Rebuilding *bool

	Figures            Figures
	Previous           *Figures
	Floors             map[string]bool
	Series             []Bucket
	ProductsBySales    []Product
	ProductsByUnits    []Product
	ProductCount       *int64
	RefundsWithoutItem *money.Amount
	IncompleteOrders   *int64
	IncompleteRefunds  *int64
	RefundsNotRead     *int64
	Buyers             *Buyers
	Wholesale          *Wholesale
	OtherCurrencies    []CurrencyCount
}

// Recording is the store's verdict as the walk kept it in store_sales_store.
type Recording struct {
	// State is the verdict's word as stored: recording, starting, paused,
	// needs attention, not connected, or "" when the walk could not read it;
	// or the frame's own, purging or rebuilding, while the store's facts are
	// being deleted or walked again, whatever the verdict says.
	State string
	// Reasons is every reason the verdict gave, those that freeze nothing
	// included, each its code and its sentence.
	Reasons []Reason
	Since   *time.Time
	// Blocked is true while the figures are as of AsOf, or none can be given:
	// recording_blocked, a paused or not-connected store, or a store purging
	// or rebuilding.
	Blocked bool
}

// Reason is one stored reason of the store's verdict.
type Reason struct {
	Code     string `json:"code"`
	Sentence string `json:"sentence"`
}

// Figures is the eight figures of a period.
type Figures struct {
	TotalSales, NetSales, AverageOrderValue, GrossSales, Discounts, Refunds *money.Amount
	Orders, UnitsSold                                                       *int64
}

// Bucket is one bucket of the series: its start, in Loc, and seven figures.
type Bucket struct {
	At                                                   time.Time
	TotalSales, NetSales, GrossSales, Discounts, Refunds *money.Amount
	Orders, UnitsSold                                    *int64
}

// Product is one row of a top list: Amount for the list by sales, Count for
// the list by units.
type Product struct {
	Key, Title string
	Amount     *money.Amount
	Count      *int64
}

// Buyers splits a period's orders by their buyer.
type Buyers struct{ New, Returning, Unknown int64 }

// Wholesale splits a period's orders between the wholesale pack's buyers and
// everybody else.
type Wholesale struct{ Wholesale, Retail Side }

// Side is one side of the wholesale split.
type Side struct {
	Orders   int64
	NetSales *money.Amount
}

// CurrencyCount is the orders of a period in a currency other than the
// store's: counted, never summed.
type CurrencyCount struct {
	Currency string
	Orders   int64
}

// Payload is the answer as the wire carries it: every key present, null for
// what is not known.
func (s Statistics) Payload() map[string]any {
	loc := s.Loc
	if loc == nil {
		loc = time.UTC
	}
	moment := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.In(loc).Format(time.RFC3339)
	}
	var bucket, start, end, prevStart any
	series := make([]any, 0, len(s.Series))
	if b := s.Bounds; b != nil {
		bucket = b.Bucket
		start, end = b.Start.In(loc).Format(time.RFC3339), b.End.In(loc).Format(time.RFC3339)
		if b.HasPrevious {
			prevStart = b.PrevStart.In(loc).Format(time.RFC3339)
		}
	}
	for _, e := range s.Series {
		series = append(series, map[string]any{
			"at": e.At.In(loc).Format(time.RFC3339), "totalSales": amount(e.TotalSales), "netSales": amount(e.NetSales),
			"orders": count(e.Orders), "grossSales": amount(e.GrossSales), "discounts": amount(e.Discounts),
			"refunds": amount(e.Refunds), "unitsSold": count(e.UnitsSold),
		})
	}
	reasons := make([]any, 0, len(s.Recording.Reasons))
	for _, r := range s.Recording.Reasons {
		reasons = append(reasons, map[string]any{"code": r.Code, "sentence": r.Sentence})
	}
	floors := map[string]any{}
	for k, v := range s.Floors {
		if v {
			floors[k] = true
		}
	}
	var previous any
	if s.Previous != nil {
		previous = s.Previous.payload()
	}
	var buyers any
	if s.Buyers != nil {
		buyers = map[string]any{"new": s.Buyers.New, "returning": s.Buyers.Returning, "unknown": s.Buyers.Unknown}
	}
	var wholesale any
	if s.Wholesale != nil {
		side := func(x Side) map[string]any { return map[string]any{"orders": x.Orders, "netSales": amount(x.NetSales)} }
		wholesale = map[string]any{"wholesale": side(s.Wholesale.Wholesale), "retail": side(s.Wholesale.Retail)}
	}
	others := make([]any, 0, len(s.OtherCurrencies))
	for _, c := range s.OtherCurrencies {
		others = append(others, map[string]any{"currency": c.Currency, "orders": c.Orders})
	}
	var periodZone any
	if s.PeriodZone != "" {
		periodZone = s.PeriodZone
	}
	var rebuilding any
	if s.Rebuilding != nil {
		rebuilding = *s.Rebuilding
	}
	return map[string]any{
		"storeId": s.StoreID, "siteId": s.SiteID, "storeName": text(s.StoreName), "storeDomain": text(s.StoreDomain),
		"destination": s.Destination, "currency": text(s.Currency), "timezone": text(s.Timezone),
		"development": flag(s.Development), "taxesIncluded": flag(s.TaxesIncluded),
		"olderOrderChangesNotRecorded": flag(s.OlderOrderChangesNotRecorded),
		"period":                       string(s.Period), "bucket": bucket, "start": start, "end": end, "prevStart": prevStart,
		"historySince": moment(s.HistorySince), "lastChangeAt": moment(s.LastChangeAt), "asOf": moment(s.AsOf),
		"periodZone": periodZone,
		"recording": map[string]any{
			"state": s.Recording.State, "reasons": reasons, "since": moment(s.Recording.Since), "blocked": s.Recording.Blocked,
		},
		"rebuilding": rebuilding,
		"figures":    s.Figures.payload(), "previous": previous, "floors": floors, "series": series,
		"productsBySales": products(s.ProductsBySales), "productsByUnits": products(s.ProductsByUnits),
		"productCount": count(s.ProductCount), "refundsWithoutItem": amount(s.RefundsWithoutItem),
		"incompleteOrders": count(s.IncompleteOrders), "incompleteRefunds": count(s.IncompleteRefunds),
		"refundsNotRead": count(s.RefundsNotRead), "buyers": buyers, "wholesale": wholesale, "otherCurrencies": others,
	}
}

func (f Figures) payload() map[string]any {
	return map[string]any{
		"totalSales": amount(f.TotalSales), "netSales": amount(f.NetSales), "orders": count(f.Orders),
		"averageOrderValue": amount(f.AverageOrderValue), "grossSales": amount(f.GrossSales),
		"discounts": amount(f.Discounts), "refunds": amount(f.Refunds), "unitsSold": count(f.UnitsSold),
	}
}

func products(list []Product) []any {
	out := make([]any, 0, len(list))
	for _, p := range list {
		var value any
		switch {
		case p.Amount != nil:
			value = p.Amount.String()
		case p.Count != nil:
			value = *p.Count
		}
		out = append(out, map[string]any{"key": p.Key, "title": p.Title, "value": value})
	}
	return out
}

func amount(a *money.Amount) any {
	if a == nil {
		return nil
	}
	return a.String()
}

func count(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func text(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func flag(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
