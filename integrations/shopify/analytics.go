package shopify

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// analytics.go -- the four commerce questions, answered from the mirror.
//
// MemQL aggregates COUNT and nothing else, so the named reads in
// dsl/shopify/overlay/queries.memql are windowed ROW reads and the grouping
// and arithmetic happen here. That split is the honest one: a query called
// soldByProduct that returned rows a caller had to add up would be worse than
// one that says what it returns.
//
// Everything here reads the MIRROR. No Admin call, no cost points, no rate
// limit -- which is the whole reason the mirror exists, and the reason these
// are worth being tools an agent can call in a conversation rather than a
// report somebody runs.

// windowDays bounds a question that arrives with no window. Thirty days is
// the shape of most merchandising questions, and an unbounded default would
// walk an entire order history to answer "what sold".
const windowDays = 30

// analyticsPageCap bounds a walk. Each read pages at 250; ten pages is 2,500
// orders, which answers a month for most stores. A question that hits the cap
// SAYS SO in its answer rather than returning a smaller number and letting
// the caller believe it.
const analyticsPageCap = 10

// SoldLine is one product or variant's contribution to a window.
type SoldLine struct {
	GID      string `json:"gid"`
	Title    string `json:"title"`
	SKU      string `json:"sku,omitempty"`
	Quantity int    `json:"quantity"`
	Orders   int    `json:"orders"`
}

// SoldReport answers "what sold".
type SoldReport struct {
	StoreID   string     `json:"storeId"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	GroupBy   string     `json:"groupBy"`
	Orders    int        `json:"orders"`
	Units     int        `json:"units"`
	Lines     []SoldLine `json:"lines"`
	Truncated bool       `json:"truncated"`
}

// CommerceSold groups the window's line items by product or by variant.
func (c *Connector) CommerceSold(ctx context.Context, store Store, from, to time.Time, groupBy string) (SoldReport, error) {
	if groupBy != "variant" {
		groupBy = "product"
	}
	report := SoldReport{
		StoreID: store.ID, GroupBy: groupBy,
		From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339),
	}
	orders, truncated, err := c.walk(ctx, "ordersInWindow", map[string]any{
		"storeId": store.ID, "from": report.From, "to": report.To,
	})
	if err != nil {
		return report, err
	}
	report.Truncated = truncated
	report.Orders = len(orders)

	lines := map[string]*SoldLine{}
	for _, order := range orders {
		gid := mapString(order, "gid")
		if gid == "" {
			continue
		}
		items, _, err := c.walk(ctx, "lineItemsForOrder", map[string]any{"storeId": store.ID, "orderGid": gid})
		if err != nil {
			return report, err
		}
		for _, item := range items {
			key := mapString(item, "productGid")
			if groupBy == "variant" {
				key = mapString(item, "variantGid")
			}
			if key == "" {
				continue
			}
			line, ok := lines[key]
			if !ok {
				line = &SoldLine{GID: key, Title: mapString(item, "title"), SKU: mapString(item, "sku")}
				lines[key] = line
			}
			// currentQuantity, not quantity: a refunded or removed line
			// still carries its original quantity, and counting it would
			// report units that were sold and then were not.
			qty := mapInt(item, "currentQuantity")
			if qty == 0 {
				qty = mapInt(item, "quantity")
			}
			line.Quantity += qty
			line.Orders++
			report.Units += qty
		}
	}
	for _, line := range lines {
		report.Lines = append(report.Lines, *line)
	}
	sort.Slice(report.Lines, func(i, j int) bool {
		if report.Lines[i].Quantity != report.Lines[j].Quantity {
			return report.Lines[i].Quantity > report.Lines[j].Quantity
		}
		return report.Lines[i].GID < report.Lines[j].GID
	})
	return report, nil
}

// StockLine is one inventory level below the threshold.
type StockLine struct {
	GID         string `json:"gid"`
	ItemGID     string `json:"itemGid"`
	LocationGID string `json:"locationGid"`
	Available   int    `json:"available"`
}

// StockReport answers "what is running out".
type StockReport struct {
	StoreID     string      `json:"storeId"`
	LocationGID string      `json:"locationGid"`
	Threshold   int         `json:"threshold"`
	Below       []StockLine `json:"below"`
	Checked     int         `json:"checked"`
	Truncated   bool        `json:"truncated"`
}

// CommerceStock reports the inventory levels below a threshold at a location.
//
// The threshold is applied HERE because a level's quantities are a nested
// object keyed by name (available, committed, on_hand, ...), which a filter
// cannot compare against a number.
func (c *Connector) CommerceStock(ctx context.Context, store Store, locationGID string, threshold int) (StockReport, error) {
	report := StockReport{StoreID: store.ID, LocationGID: locationGID, Threshold: threshold}
	levels, truncated, err := c.walk(ctx, "stockBelow", map[string]any{
		"storeId": store.ID, "locationGid": locationGID,
	})
	if err != nil {
		return report, err
	}
	report.Truncated = truncated
	report.Checked = len(levels)
	for _, level := range levels {
		available, known := availableQuantity(level)
		if !known || available >= threshold {
			continue
		}
		report.Below = append(report.Below, StockLine{
			GID:         mapString(level, "gid"),
			ItemGID:     mapString(level, "itemGid"),
			LocationGID: mapString(level, "locationGid"),
			Available:   available,
		})
	}
	sort.Slice(report.Below, func(i, j int) bool { return report.Below[i].Available < report.Below[j].Available })
	return report, nil
}

// availableQuantity digs the `available` count out of a level's quantities.
//
// Shopify models inventory as NAMED quantities rather than one number, and
// "available" is the one a merchant means by "in stock". Reporting a level
// with no available quantity as zero would invent a stockout, so an unknown
// reads as unknown and the level is skipped.
func availableQuantity(level map[string]any) (int, bool) {
	raw := rowValue(level, "quantities")
	items, ok := raw.([]any)
	if !ok {
		return 0, false
	}
	for _, item := range items {
		q, ok := item.(map[string]any)
		if !ok || fmt.Sprintf("%v", q["name"]) != "available" {
			continue
		}
		switch v := q["quantity"].(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		case string:
			n, err := strconv.Atoi(v)
			return n, err == nil
		}
	}
	return 0, false
}

// CustomerReport answers "who is coming back, and how much is going back".
type CustomerReport struct {
	StoreID         string  `json:"storeId"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	Orders          int     `json:"orders"`
	Customers       int     `json:"customers"`
	RepeatCustomers int     `json:"repeatCustomers"`
	RepeatRate      float64 `json:"repeatRate"`
	Refunds         int     `json:"refunds"`
	RefundRate      float64 `json:"refundRate"`
	Truncated       bool    `json:"truncated"`
}

// CommerceCustomers computes the repeat rate and the refund rate.
func (c *Connector) CommerceCustomers(ctx context.Context, store Store, from, to time.Time) (CustomerReport, error) {
	report := CustomerReport{
		StoreID: store.ID,
		From:    from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339),
	}
	orders, truncated, err := c.walk(ctx, "repeatCustomers", map[string]any{
		"storeId": store.ID, "from": report.From, "to": report.To,
	})
	if err != nil {
		return report, err
	}
	report.Truncated = truncated
	report.Orders = len(orders)
	counts := map[string]int{}
	for _, order := range orders {
		if gid := mapString(order, "customerGid"); gid != "" {
			counts[gid]++
		}
	}
	report.Customers = len(counts)
	for _, n := range counts {
		if n > 1 {
			report.RepeatCustomers++
		}
	}
	if report.Customers > 0 {
		report.RepeatRate = float64(report.RepeatCustomers) / float64(report.Customers)
	}

	refunds, refundsTruncated, err := c.walk(ctx, "refundRate", map[string]any{
		"storeId": store.ID, "from": report.From, "to": report.To,
	})
	if err != nil {
		return report, err
	}
	report.Truncated = report.Truncated || refundsTruncated
	report.Refunds = len(refunds)
	if report.Orders > 0 {
		report.RefundRate = float64(report.Refunds) / float64(report.Orders)
	}
	return report, nil
}

// CompanyReport answers "how is this B2B account doing".
type CompanyReport struct {
	StoreID     string           `json:"storeId"`
	CompanyGID  string           `json:"companyGid"`
	From        string           `json:"from"`
	To          string           `json:"to"`
	Orders      int              `json:"orders"`
	Outstanding int              `json:"outstandingOrders"`
	CreditLimit map[string]any   `json:"creditLimit,omitempty"`
	Recent      []map[string]any `json:"recentOrders,omitempty"`
	Truncated   bool             `json:"truncated"`
}

// CommerceCompany reports one company's orders, its outstanding payment
// terms and its MemQL-owned credit limit.
//
// The credit limit is the one field here MemQL is the ORIGIN of, and mixing
// it into the same answer is the point: the question a rep asks is "can this
// account order", which needs both halves and neither system has both.
func (c *Connector) CommerceCompany(ctx context.Context, store Store, companyGID string, from, to time.Time) (CompanyReport, error) {
	report := CompanyReport{
		StoreID: store.ID, CompanyGID: companyGID,
		From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339),
	}
	orders, truncated, err := c.walk(ctx, "ordersByCompany", map[string]any{
		"storeId": store.ID, "companyGid": companyGID, "from": report.From, "to": report.To,
	})
	if err != nil {
		return report, err
	}
	report.Truncated = truncated
	report.Orders = len(orders)
	for i, order := range orders {
		if i >= 10 {
			break
		}
		report.Recent = append(report.Recent, map[string]any{
			"gid":       mapString(order, "gid"),
			"name":      mapString(order, "name"),
			"createdAt": mapString(order, "sourceCreatedAt"),
			"total":     rowValue(order, "totalPriceSet"),
			"fullyPaid": rowValue(order, "fullyPaid"),
		})
	}

	outstanding, _, err := c.walk(ctx, "paymentTermsOutstanding", map[string]any{"storeId": store.ID})
	if err != nil {
		return report, err
	}
	for _, order := range outstanding {
		if strings.Contains(fmt.Sprintf("%v", rowValue(order, "purchasingEntity")), companyGID) {
			report.Outstanding++
		}
	}

	limits, _, err := c.walk(ctx, "creditLimitsForCompany", map[string]any{"storeId": store.ID, "companyGid": companyGID})
	if err != nil {
		// A store with no commerce rows is the normal case before the
		// wholesale application exists. Not an error, just no limit.
		return report, nil
	}
	if len(limits) > 0 {
		report.CreditLimit = map[string]any{
			"amount":      mapString(limits[0], "limitAmount"),
			"currency":    mapString(limits[0], "currencyCode"),
			"outstanding": mapString(limits[0], "outstandingAmount"),
			"status":      mapString(limits[0], "status"),
		}
	}
	return report, nil
}

// walk pages a named read to the cap, reporting whether it stopped early.
//
// The TRUNCATION FLAG is not decoration. An analytics answer that silently
// stopped at 2,500 orders reads as a complete month, and a merchant acts on
// it -- so a capped walk says so in the payload the agent reads back.
func (c *Connector) walk(ctx context.Context, fn string, args map[string]any) ([]map[string]any, bool, error) {
	base := connectorContext(ctx)
	call := renderCall(fn, args)
	var out []map[string]any
	cursor := ""
	for page := 0; page < analyticsPageCap; page++ {
		pageCtx := base
		if cursor != "" {
			// The cursor rides the CONTEXT, not the query text. That is the
			// engine's own contract: `paginate` declares a page size and the
			// continuation is a runtime value the executor compiles into a
			// keyset predicate. A cursor spelled into the call would be a
			// caller-supplied scan position on a cluster-owner read.
			pageCtx = memql.ContextWithCursor(base, cursor)
		}
		res, err := c.engine.Execute(pageCtx, call)
		if err != nil {
			return nil, false, fmt.Errorf("shopify: %s: %w", fn, err)
		}
		rows := memql.MaterializeRows(res)
		out = append(out, rows...)
		next := ""
		if res != nil {
			if meta := res.GetMeta(); meta != nil {
				next = strings.TrimSpace(meta.Cursor)
			}
		}
		// No continuation is an exhausted walk, which is the safe reading:
		// it ends rather than continuing from a position nobody supplied.
		if next == "" || next == cursor || len(rows) == 0 {
			return out, false, nil
		}
		cursor = next
	}
	return out, true, nil
}

// AnalyticsWindow resolves a from/to pair from optional arguments.
func (c *Connector) AnalyticsWindow(from, to string) (time.Time, time.Time) {
	end := c.now().UTC()
	if t, err := time.Parse(time.RFC3339, to); err == nil {
		end = t.UTC()
	}
	start := end.AddDate(0, 0, -windowDays)
	if t, err := time.Parse(time.RFC3339, from); err == nil {
		start = t.UTC()
	}
	return start, end
}
