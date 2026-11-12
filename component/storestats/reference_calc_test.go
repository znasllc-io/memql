package storestats

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// reference_calc_test.go -- a second, independent reading of the figures (the
// 2026-11-11 store statistics design, section 5), computed straight from the
// fixture store's file of orders as Shopify answers them.
//
// IT SHARES NO CODE WITH THE COMPONENT. It imports the standard library only
// and uses no name this package declares in another file: not core/money,
// not core/statsperiod, not a fact, a fold or a figure of the component, and
// none of the other tests' helpers. TestTheReferenceSharesNoCodeWithTheComponent
// holds it to that. Where the two disagree, one of them misreads the record,
// and the disagreement is the finding.
//
// # How it counts
//
// An order counts when the file does not name it `deleted-order` (the one an
// ORDERS_DELETE notice retires after it was read), it has no cancelledAt, and
// it is not a test order, unless the store is a development store. A refund
// counts when its order counts. An order is dated by its processedAt, else its
// createdAt; a refund the same way. A period is [start, end), and a store day
// is a calendar day in the store's zone.
//
// Money is summed over the orders in the store's currency and the refunds of
// those orders; every other order is counted in Orders and in Units sold, and
// listed among the other currencies, and summed nowhere.
//
// # Exact, and how it is written
//
// Every amount is a rational number (math/big). An amount is written with as
// many digits after the point as the most any amount that went into it was
// written with in the file: "35.0" plus "10.00" is "45.00". A figure no amount
// went into is "0". Two amounts are divided, and only two: a line's discount
// shared over the units an edit left, and the average order value. The share
// is rounded half to even at the larger of the digits the discount is written
// with and the two digits of a cent (the file's currencies are the euro and
// the dollar, both in cents), so "1.0" over two of three units is 0.67. The
// average is rounded half to even at two digits. Half to even: a value exactly
// half way between two cents goes to the even one.

// referenceAnswer is what the calculator answers for one period. Amounts are
// written as the rule above says; a count is its decimal digits; a figure that
// has no value is "null".
type referenceAnswer struct {
	// Figures are the eight figures, by the answer's own keys.
	Figures map[string]string
	// Series is every bucket of the period in order, each its start (RFC 3339
	// in the store's zone) and the seven figures but the average.
	Series []referenceBucket
	// ProductsBySales and ProductsByUnits are the two top lists.
	ProductsBySales, ProductsByUnits []referenceProduct
	ProductCount                     string
	// RefundsWithoutItem is the part of Refunds no product holds.
	RefundsWithoutItem string
	// Buyers is new, returning and unknown.
	Buyers map[string]string
	// Wholesale is each side's orders and net sales.
	Wholesale map[string]map[string]string
	// OtherCurrencies is each other currency and its orders, most first.
	OtherCurrencies []referenceCurrency
	// LineDiscounts is D(L) of each line of the period's orders, by its gid.
	LineDiscounts map[string]string
	// Orders and Refunds name what falls in the period: each order by its
	// name, each refund by its order's name and its gid.
	Orders, Refunds []string
}

type referenceBucket struct {
	At      string
	Figures map[string]string
}

type referenceProduct struct{ Key, Title, Value string }

type referenceCurrency struct {
	Currency string
	Orders   string
}

// ---- the file ----

type refMoneySet struct {
	ShopMoney struct {
		Amount       string `json:"amount"`
		CurrencyCode string `json:"currencyCode"`
	} `json:"shopMoney"`
}

type refID struct {
	ID string `json:"id"`
}

type refLineItem struct {
	ID                   string         `json:"id"`
	Title                string         `json:"title"`
	Quantity             int            `json:"quantity"`
	CurrentQuantity      int            `json:"currentQuantity"`
	IsGiftCard           bool           `json:"isGiftCard"`
	Product              *refID         `json:"product"`
	OriginalUnitPriceSet refMoneySet    `json:"originalUnitPriceSet"`
	DiscountAllocations  []refAllocated `json:"discountAllocations"`
}

type refAllocated struct {
	AllocatedAmountSet refMoneySet `json:"allocatedAmountSet"`
}

type refRefund struct {
	ID               string      `json:"id"`
	ProcessedAt      string      `json:"processedAt"`
	CreatedAt        string      `json:"createdAt"`
	TotalRefundedSet refMoneySet `json:"totalRefundedSet"`
	Duties           []struct {
		AmountSet refMoneySet `json:"amountSet"`
	} `json:"duties"`
	RefundLineItems struct {
		Nodes []struct {
			LineItem    refID       `json:"lineItem"`
			Quantity    int         `json:"quantity"`
			SubtotalSet refMoneySet `json:"subtotalSet"`
			TotalTaxSet refMoneySet `json:"totalTaxSet"`
		} `json:"nodes"`
	} `json:"refundLineItems"`
	RefundShippingLines struct {
		Nodes []struct {
			SubtotalAmountSet refMoneySet `json:"subtotalAmountSet"`
			TaxAmountSet      refMoneySet `json:"taxAmountSet"`
		} `json:"nodes"`
	} `json:"refundShippingLines"`
	OrderAdjustments struct {
		Nodes []struct {
			AmountSet    refMoneySet `json:"amountSet"`
			TaxAmountSet refMoneySet `json:"taxAmountSet"`
		} `json:"nodes"`
	} `json:"orderAdjustments"`
}

type refOrder struct {
	ID                     string       `json:"id"`
	Name                   string       `json:"name"`
	Email                  string       `json:"email"`
	Customer               *refID       `json:"customer"`
	CurrencyCode           string       `json:"currencyCode"`
	ProcessedAt            string       `json:"processedAt"`
	CreatedAt              string       `json:"createdAt"`
	CancelledAt            string       `json:"cancelledAt"`
	Test                   bool         `json:"test"`
	TaxesIncluded          bool         `json:"taxesIncluded"`
	TotalShippingPriceSet  *refMoneySet `json:"totalShippingPriceSet"`
	TotalTaxSet            *refMoneySet `json:"totalTaxSet"`
	OriginalTotalDutiesSet *refMoneySet `json:"originalTotalDutiesSet"`
	LineItems              struct {
		Nodes []refLineItem `json:"nodes"`
	} `json:"lineItems"`
	Refunds []refRefund `json:"refunds"`
}

type refFile struct {
	Orders struct {
		Nodes []refOrder `json:"nodes"`
	} `json:"orders"`
}

// refReadOrders reads the file; a file it cannot read is a broken fixture.
func refReadOrders(fixture []byte) []refOrder {
	var f refFile
	if err := json.Unmarshal(fixture, &f); err != nil {
		panic("the fixture store's file cannot be read: " + err.Error())
	}
	return f.Orders.Nodes
}

// ---- exact amounts ----

// refAmount is an exact amount and how many digits after the point it is
// written with.
type refAmount struct {
	value  *big.Rat
	digits int
}

func refZero() refAmount { return refAmount{value: new(big.Rat)} }

// refParse reads an amount as the file writes it: "420.00", "1.0", "-9.00".
func refParse(s string) refAmount {
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("the fixture holds an amount that is not a decimal: " + s)
	}
	digits := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		digits = len(s) - i - 1
	}
	return refAmount{value: v, digits: digits}
}

// refOf is a money set's shop amount, or nothing when the set is absent.
func refOf(m *refMoneySet) refAmount {
	if m == nil {
		return refZero()
	}
	return refParse(m.ShopMoney.Amount)
}

func (a refAmount) plus(b refAmount) refAmount {
	return refAmount{value: new(big.Rat).Add(a.value, b.value), digits: max(a.digits, b.digits)}
}

func (a refAmount) minus(b refAmount) refAmount {
	return refAmount{value: new(big.Rat).Sub(a.value, b.value), digits: max(a.digits, b.digits)}
}

func (a refAmount) times(n int) refAmount {
	return refAmount{value: new(big.Rat).Mul(a.value, new(big.Rat).SetInt64(int64(n))), digits: a.digits}
}

// refPow10 is ten to the n.
func refPow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// refRound rounds v half to even at the digits given.
func refRound(v *big.Rat, digits int) refAmount {
	scaled := new(big.Rat).Mul(v, new(big.Rat).SetInt(refPow10(digits)))
	q, r := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	twice := new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2))
	away := false
	switch twice.Cmp(scaled.Denom()) {
	case 1:
		away = true
	case 0:
		away = q.Bit(0) == 1
	}
	if away {
		if scaled.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return refAmount{value: new(big.Rat).SetFrac(q, refPow10(digits)), digits: digits}
}

// String writes the amount with its digits; an amount that does not fit them
// would be a mistake of this file, never rounded away.
func (a refAmount) String() string {
	scaled := new(big.Rat).Mul(a.value, new(big.Rat).SetInt(refPow10(a.digits)))
	if !scaled.IsInt() {
		panic("an amount does not fit its own digits: " + a.value.RatString())
	}
	n := scaled.Num()
	sign := ""
	if n.Sign() < 0 {
		sign = "-"
		n = new(big.Int).Abs(n)
	}
	text := n.String()
	if a.digits == 0 {
		return sign + text
	}
	for len(text) <= a.digits {
		text = "0" + text
	}
	return sign + text[:len(text)-a.digits] + "." + text[len(text)-a.digits:]
}

// refCentDigits is the digits of the smallest unit of the file's currencies.
func refCentDigits(currency string) int {
	switch currency {
	case "EUR", "USD":
		return 2
	}
	panic("the fixture holds a currency the calculator does not know the cents of: " + currency)
}

// ---- dates and buckets ----

func refInstant(at, fallback string) time.Time {
	s := at
	if s == "" {
		s = fallback
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("the fixture holds a date that is not RFC 3339: " + s)
	}
	return t
}

// refBucketStarts cuts [start, end) into its buckets: hours of elapsed time,
// or calendar days or months in the zone.
func refBucketStarts(zone *time.Location, start, end time.Time, bucket string) []time.Time {
	var out []time.Time
	local := start.In(zone)
	y, m, d := local.Date()
	for k := 0; ; k++ {
		var at time.Time
		switch bucket {
		case "hour":
			at = start.Add(time.Duration(k) * time.Hour)
		case "day":
			at = time.Date(y, m, d+k, 0, 0, 0, 0, zone)
		case "month":
			at = time.Date(y, m+time.Month(k), 1, 0, 0, 0, 0, zone)
		default:
			panic("no such bucket: " + bucket)
		}
		if !at.Before(end) {
			return out
		}
		out = append(out, at)
	}
}

// ---- one line, one order, one refund ----

type refSale struct {
	lineID, productKey, title string
	sold                      int
	gross, discounts          refAmount
}

type refReturn struct {
	productKey, title string
	units             int
	subtotal          refAmount
}

type refCountedOrder struct {
	o                refOrder
	at               time.Time
	inStoreCurrency  bool
	wholesale        bool
	sales            []refSale // the lines that are not gift cards
	gross, discounts refAmount
	units            int
	shipTaxDuties    refAmount
	lineOf           map[string]refLineItem
}

type refCountedRefund struct {
	order                   *refCountedOrder
	id                      string
	at                      time.Time
	items, outShipTaxDuties refAmount // R(F); RS + RT + RU
	units                   int       // RQ(F)
	returns                 []refReturn
}

func refProductKey(l refLineItem) string {
	if l.Product != nil && l.Product.ID != "" {
		return l.Product.ID
	}
	// A line with no product is grouped by its title, under the key the
	// answer gives such a product.
	return "title:" + l.Title
}

func refSameEmail(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// refCount reads every order and refund that counts, with what 5.1 and 5.2
// make of each.
func refCount(orders []refOrder, currency string, development bool, granted []string) ([]*refCountedOrder, []*refCountedRefund) {
	var counted []*refCountedOrder
	var refunds []*refCountedRefund
	for _, o := range orders {
		if o.Name == "deleted-order" || o.CancelledAt != "" || (o.Test && !development) {
			continue
		}
		c := &refCountedOrder{o: o, at: refInstant(o.ProcessedAt, o.CreatedAt), inStoreCurrency: o.CurrencyCode == currency,
			gross: refZero(), discounts: refZero(), lineOf: map[string]refLineItem{}}
		if o.Email != "" {
			for _, g := range granted {
				if refSameEmail(o.Email, g) {
					c.wholesale = true
				}
			}
		}
		refunded := map[string]int{}
		for _, f := range o.Refunds {
			for _, rl := range f.RefundLineItems.Nodes {
				refunded[rl.LineItem.ID] += rl.Quantity
			}
		}
		for _, l := range o.LineItems.Nodes {
			c.lineOf[l.ID] = l
			if l.IsGiftCard {
				continue
			}
			q0, q1, qr := l.Quantity, l.CurrentQuantity, refunded[l.ID]
			qs := min(q0, q1+qr)
			price := refParse(l.OriginalUnitPriceSet.ShopMoney.Amount)
			allocated := refZero()
			for _, a := range l.DiscountAllocations {
				allocated = allocated.plus(refParse(a.AllocatedAmountSet.ShopMoney.Amount))
			}
			var share refAmount
			switch {
			case q0 == 0:
				share = refZero()
			case qs == q0:
				share = allocated
			default:
				part := new(big.Rat).Mul(allocated.value, big.NewRat(int64(qs), int64(q0)))
				share = refRound(part, max(allocated.digits, refCentDigits(l.OriginalUnitPriceSet.ShopMoney.CurrencyCode)))
			}
			s := refSale{lineID: l.ID, productKey: refProductKey(l), title: l.Title, sold: qs, gross: price.times(qs), discounts: share}
			c.sales = append(c.sales, s)
			c.gross, c.discounts, c.units = c.gross.plus(s.gross), c.discounts.plus(s.discounts), c.units+qs
		}
		tax := refOf(o.TotalTaxSet)
		if o.TaxesIncluded {
			tax = refZero()
		}
		c.shipTaxDuties = refOf(o.TotalShippingPriceSet).plus(tax).plus(refOf(o.OriginalTotalDutiesSet))
		counted = append(counted, c)

		for _, f := range o.Refunds {
			r := &refCountedRefund{order: c, id: f.ID, at: refInstant(f.ProcessedAt, f.CreatedAt)}
			money := refParse(f.TotalRefundedSet.ShopMoney.Amount)
			shipping, tax, duties, gifts := refZero(), refZero(), refZero(), refZero()
			for _, rl := range f.RefundLineItems.Nodes {
				line, ok := c.lineOf[rl.LineItem.ID]
				if !ok {
					panic("a refund line of " + o.Name + " names a line the order does not hold")
				}
				tax = tax.plus(refParse(rl.TotalTaxSet.ShopMoney.Amount))
				subtotal := refParse(rl.SubtotalSet.ShopMoney.Amount)
				if line.IsGiftCard {
					gifts = gifts.plus(subtotal)
					continue
				}
				r.units += rl.Quantity
				r.returns = append(r.returns, refReturn{productKey: refProductKey(line), title: line.Title, units: rl.Quantity, subtotal: subtotal})
			}
			for _, sl := range f.RefundShippingLines.Nodes {
				shipping = shipping.plus(refParse(sl.SubtotalAmountSet.ShopMoney.Amount))
				tax = tax.plus(refParse(sl.TaxAmountSet.ShopMoney.Amount))
			}
			for _, adj := range f.OrderAdjustments.Nodes {
				// With the sign the file writes: a positive tax went back.
				tax = tax.plus(refParse(adj.TaxAmountSet.ShopMoney.Amount))
			}
			for _, d := range f.Duties {
				duties = duties.plus(refParse(d.AmountSet.ShopMoney.Amount))
			}
			if o.TaxesIncluded {
				tax = refZero()
			}
			r.items = money.minus(shipping).minus(tax).minus(duties).minus(gifts)
			r.outShipTaxDuties = shipping.plus(tax).plus(duties)
			refunds = append(refunds, r)
		}
	}
	return counted, refunds
}

// ---- a stretch of time ----

// refTotals is the sums over some orders and refunds.
type refTotals struct {
	orders, storeOrders, units, returned int
	gross, discounts, charges            refAmount
	refunds, refundedOut                 refAmount
}

func newRefTotals() refTotals {
	return refTotals{gross: refZero(), discounts: refZero(), charges: refZero(), refunds: refZero(), refundedOut: refZero()}
}

func (t *refTotals) addOrder(c *refCountedOrder) {
	t.orders++
	t.units += c.units
	if c.inStoreCurrency {
		t.storeOrders++
		t.gross, t.discounts, t.charges = t.gross.plus(c.gross), t.discounts.plus(c.discounts), t.charges.plus(c.shipTaxDuties)
	}
}

func (t *refTotals) addRefund(r *refCountedRefund) {
	t.returned += r.units
	if r.order.inStoreCurrency {
		t.refunds, t.refundedOut = t.refunds.plus(r.items), t.refundedOut.plus(r.outShipTaxDuties)
	}
}

func (t refTotals) net() refAmount { return t.gross.minus(t.discounts).minus(t.refunds) }

// figures are the eight figures of the totals; the average only when asked.
func (t refTotals) figures(average bool) map[string]string {
	net := t.net()
	f := map[string]string{
		"orders":     strconv.Itoa(t.orders),
		"grossSales": t.gross.String(),
		"discounts":  t.discounts.String(),
		"refunds":    t.refunds.String(),
		"netSales":   net.String(),
		"totalSales": net.plus(t.charges).minus(t.refundedOut).String(),
		"unitsSold":  strconv.Itoa(t.units - t.returned),
	}
	if average {
		f["averageOrderValue"] = "null"
		if t.storeOrders > 0 {
			sold := t.gross.minus(t.discounts)
			f["averageOrderValue"] = refRound(new(big.Rat).Quo(sold.value, big.NewRat(int64(t.storeOrders), 1)), 2).String()
		}
	}
	return f
}

// referenceFigures is the period [start, end) of the fixture store, cut in
// zone into buckets of the size named ("hour", "day" or "month"), for a store
// in currency, a development store or not, whose granted wholesale buyers are
// the emails given.
func referenceFigures(fixture []byte, zone *time.Location, currency string, development bool, granted []string,
	start, end time.Time, bucket string) referenceAnswer {
	orders, refunds := refCount(refReadOrders(fixture), currency, development, granted)
	in := func(t time.Time) bool { return !t.Before(start) && t.Before(end) }

	starts := refBucketStarts(zone, start, end, bucket)
	bucketOf := func(t time.Time) int {
		i := sort.Search(len(starts), func(i int) bool { return starts[i].After(t) }) - 1
		return i
	}
	all := newRefTotals()
	sides := map[bool]refTotals{true: newRefTotals(), false: newRefTotals()}
	perBucket := make([]refTotals, len(starts))
	for i := range perBucket {
		perBucket[i] = newRefTotals()
	}
	ans := referenceAnswer{LineDiscounts: map[string]string{}}

	type product struct {
		key, title string
		titleAt    time.Time
		sold       bool
		units      int
		sales      refAmount
		priced     bool
	}
	products := map[string]*product{}
	productOf := func(key string) *product {
		p := products[key]
		if p == nil {
			p = &product{key: key, sales: refZero()}
			products[key] = p
		}
		return p
	}
	// A product is titled by its newest line sold in the period; one the
	// period holds only through refunds, by the line of its latest refund.
	name := func(p *product, title string, at time.Time, sold bool) {
		switch {
		case sold && !p.sold, sold == p.sold && (at.After(p.titleAt) || (at.Equal(p.titleAt) && title > p.title)):
			p.title, p.titleAt, p.sold = title, at, sold
		}
	}
	returnedSubtotals := refZero()

	buyersBefore := map[string]bool{}
	periodBuyers := map[string]bool{}
	unknown := 0
	others := map[string]int{}
	for _, c := range orders {
		buyer := ""
		if c.o.Customer != nil {
			buyer = c.o.Customer.ID
		}
		if buyer != "" && c.at.Before(start) {
			buyersBefore[buyer] = true
		}
		if !in(c.at) {
			continue
		}
		ans.Orders = append(ans.Orders, c.o.Name)
		all.addOrder(c)
		s := sides[c.wholesale]
		s.addOrder(c)
		sides[c.wholesale] = s
		perBucket[bucketOf(c.at)].addOrder(c)
		if buyer == "" {
			unknown++
		} else {
			periodBuyers[buyer] = true
		}
		if !c.inStoreCurrency {
			others[c.o.CurrencyCode]++
		}
		for _, sale := range c.sales {
			ans.LineDiscounts[sale.lineID] = sale.discounts.String()
			p := productOf(sale.productKey)
			p.units += sale.sold
			if c.inStoreCurrency {
				p.sales, p.priced = p.sales.plus(sale.gross.minus(sale.discounts)), true
			}
			name(p, sale.title, c.at, true)
		}
	}
	for _, r := range refunds {
		if !in(r.at) {
			continue
		}
		ans.Refunds = append(ans.Refunds, r.order.o.Name+" "+r.id)
		all.addRefund(r)
		s := sides[r.order.wholesale]
		s.addRefund(r)
		sides[r.order.wholesale] = s
		perBucket[bucketOf(r.at)].addRefund(r)
		for _, back := range r.returns {
			p := productOf(back.productKey)
			p.units -= back.units
			if r.order.inStoreCurrency {
				p.sales, p.priced = p.sales.minus(back.subtotal), true
				returnedSubtotals = returnedSubtotals.plus(back.subtotal)
			}
			name(p, back.title, r.at, false)
		}
	}

	ans.Figures = all.figures(true)
	for i, at := range starts {
		ans.Series = append(ans.Series, referenceBucket{At: at.In(zone).Format(time.RFC3339), Figures: perBucket[i].figures(false)})
	}

	var list []*product
	for _, p := range products {
		list = append(list, p)
	}
	ans.ProductCount = strconv.Itoa(len(list))
	// Most first; a tie by title, then by key: an order the record leaves
	// open, chosen here so the lists can be compared row by row.
	tie := func(a, b *product) bool {
		if a.title != b.title {
			return a.title < b.title
		}
		return a.key < b.key
	}
	sort.Slice(list, func(i, j int) bool {
		if c := list[i].sales.value.Cmp(list[j].sales.value); c != 0 {
			return c > 0
		}
		return tie(list[i], list[j])
	})
	ans.ProductsBySales = []referenceProduct{}
	for _, p := range list {
		if p.priced && len(ans.ProductsBySales) < 25 {
			ans.ProductsBySales = append(ans.ProductsBySales, referenceProduct{Key: p.key, Title: p.title, Value: p.sales.String()})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].units != list[j].units {
			return list[i].units > list[j].units
		}
		return tie(list[i], list[j])
	})
	ans.ProductsByUnits = []referenceProduct{}
	for _, p := range list {
		if len(ans.ProductsByUnits) < 25 {
			ans.ProductsByUnits = append(ans.ProductsByUnits, referenceProduct{Key: p.key, Title: p.title, Value: strconv.Itoa(p.units)})
		}
	}
	ans.RefundsWithoutItem = all.refunds.minus(returnedSubtotals).String()

	returning := 0
	for b := range periodBuyers {
		if buyersBefore[b] {
			returning++
		}
	}
	ans.Buyers = map[string]string{
		"new": strconv.Itoa(len(periodBuyers) - returning), "returning": strconv.Itoa(returning), "unknown": strconv.Itoa(unknown),
	}
	ans.Wholesale = map[string]map[string]string{}
	for side, label := range map[bool]string{true: "wholesale", false: "retail"} {
		t := sides[side]
		ans.Wholesale[label] = map[string]string{"orders": strconv.Itoa(t.orders), "netSales": t.net().String()}
	}
	ans.OtherCurrencies = []referenceCurrency{}
	for code, n := range others {
		ans.OtherCurrencies = append(ans.OtherCurrencies, referenceCurrency{Currency: code, Orders: strconv.Itoa(n)})
	}
	sort.Slice(ans.OtherCurrencies, func(i, j int) bool {
		a, b := ans.OtherCurrencies[i], ans.OtherCurrencies[j]
		if a.Orders != b.Orders {
			x, _ := strconv.Atoi(a.Orders)
			y, _ := strconv.Atoi(b.Orders)
			return x > y
		}
		return a.Currency < b.Currency
	})
	return ans
}

// String is the answer in one line per part, for a failure's message.
func (r referenceProduct) String() string { return fmt.Sprintf("%s %q %s", r.Key, r.Title, r.Value) }
