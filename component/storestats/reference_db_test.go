package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// reference_db_test.go -- the figures of a fixture store equal, to the cent,
// an independent calculation from the same file (the 2026-11-11 store
// statistics design, U10 and section 5).
//
// The fixture is an invented bicycle shop's orders as Shopify's ShopifyListOrder
// answers them (testdata/fixture-store; its README says what each order is
// for). They go in through the mirror's own write path: a fake Shopify on
// http.DefaultTransport answers the connector's list of orders and its pages
// of lines from the file, and the connector's ReconcilePartition pages, maps
// and heals them into the mirror. The order an ORDERS_DELETE notice retires is
// then retired by a raw insert, as the mirror writes the tombstone. The rollup
// folds the store, and storeStatistics is asked every period at a fixed clock.
// Every figure, bucket, product row, buyer count and wholesale side must equal,
// as an exact string, what reference_calc_test.go computes from the same file
// with rational arithmetic and none of the component's code.
//
// THE FAKE SHOPIFY IS PROCESS-WIDE (http.DefaultTransport), put back when the
// test ends: no test that calls out may run in parallel with these. None of
// this package's tests is parallel.

const fixturePath = "testdata/fixture-store/orders.json"

// fixtureGranted is the wholesale buyer's email as the pack's entitlement
// holds it; the order writes it with capitals and spaces.
const fixtureGranted = "orders@corner-cycles.example.test"

// fixtureStoreCase is one of the twenty cases the record names, or of the
// three the review added: the orders
// that make it, by name, and what in the file makes it that case.
type fixtureStoreCase struct {
	name   string
	orders []string
	holds  func(orders []map[string]any) bool
}

func fixtureCases() []fixtureStoreCase {
	lines := func(o map[string]any) []map[string]any { return objectsAt(o, "lineItems", "nodes") }
	refunds := func(o map[string]any) []map[string]any { return objectsAt(o, "refunds") }
	refundLines := func(f map[string]any) []map[string]any { return objectsAt(f, "refundLineItems", "nodes") }
	shippingLines := func(f map[string]any) []map[string]any { return objectsAt(f, "refundShippingLines", "nodes") }
	adjustments := func(f map[string]any) []map[string]any { return objectsAt(f, "orderAdjustments", "nodes") }
	one := func(name string, holds func(o map[string]any) bool) fixtureStoreCase {
		return fixtureStoreCase{name: name, orders: []string{name}, holds: func(os []map[string]any) bool { return holds(os[0]) }}
	}
	anyRefund := func(o map[string]any, holds func(f map[string]any) bool) bool {
		return slices.ContainsFunc(refunds(o), holds)
	}
	removed := func(l map[string]any) bool { return numberAt(l, "currentQuantity") < numberAt(l, "quantity") }
	return []fixtureStoreCase{
		one("two-discount-codes", func(o map[string]any) bool {
			codes, _ := o["discountCodes"].([]any)
			return len(codes) == 2 && slices.ContainsFunc(lines(o), func(l map[string]any) bool { return len(objectsAt(l, "discountAllocations")) == 2 })
		}),
		one("order-level-discount", func(o map[string]any) bool {
			return len(lines(o)) >= 2 && !slices.ContainsFunc(lines(o), func(l map[string]any) bool { return len(objectsAt(l, "discountAllocations")) != 1 })
		}),
		one("gift-card", func(o map[string]any) bool {
			gift := ""
			for _, l := range lines(o) {
				if l["isGiftCard"] == true {
					gift, _ = l["id"].(string)
				}
			}
			return gift != "" && anyRefund(o, func(f map[string]any) bool {
				return slices.ContainsFunc(refundLines(f), func(rl map[string]any) bool { return textAt(rl, "lineItem", "id") == gift })
			})
		}),
		one("line-refund-with-and-without-restock", func(o map[string]any) bool {
			withLines := 0
			for _, f := range refunds(o) {
				if len(refundLines(f)) > 0 {
					withLines++
				}
			}
			return withLines == 2 && !slices.ContainsFunc(lines(o), func(l map[string]any) bool { return !removed(l) })
		}),
		one("shipping-refund", func(o map[string]any) bool {
			return anyRefund(o, func(f map[string]any) bool { return len(shippingLines(f)) > 0 && len(refundLines(f)) == 0 })
		}),
		one("amount-only-refund", func(o map[string]any) bool {
			return anyRefund(o, func(f map[string]any) bool {
				return len(refundLines(f)) == 0 && len(shippingLines(f)) == 0 && len(adjustments(f)) == 1
			})
		}),
		one("restocking-fee", func(o map[string]any) bool {
			return anyRefund(o, func(f map[string]any) bool {
				return slices.ContainsFunc(adjustments(f), func(a map[string]any) bool { return a["reason"] == "RESTOCK" })
			})
		}),
		one("refund-in-a-later-period", func(o map[string]any) bool {
			placed := instantAt(o, "processedAt")
			return anyRefund(o, func(f map[string]any) bool { return instantAt(f, "processedAt").Sub(placed) > 7*24*time.Hour })
		}),
		one("edit-removes-a-unit", func(o map[string]any) bool {
			return len(refunds(o)) == 0 && o["edited"] == true && slices.ContainsFunc(lines(o), removed)
		}),
		one("edit-adds-a-line", func(o map[string]any) bool { return o["edited"] == true && len(lines(o)) >= 2 }),
		one("cancelled-with-refund", func(o map[string]any) bool { return o["cancelledAt"] != nil && len(refunds(o)) > 0 }),
		one("test-order", func(o map[string]any) bool { return o["test"] == true }),
		one("order-in-usd", func(o map[string]any) bool { return o["currencyCode"] == "USD" }),
		{name: "three-orders-of-one-buyer", orders: []string{"three-orders-of-one-buyer-1", "three-orders-of-one-buyer-2", "three-orders-of-one-buyer-3"},
			holds: func(os []map[string]any) bool {
				buyer := textAt(os[0], "customer", "id")
				return buyer != "" && textAt(os[1], "customer", "id") == buyer && textAt(os[2], "customer", "id") == buyer
			}},
		one("wholesale-buyer", func(o map[string]any) bool {
			email, _ := o["email"].(string)
			return email != fixtureGranted && strings.EqualFold(strings.TrimSpace(email), fixtureGranted)
		}),
		one("either-side-of-a-local-midnight", func(o map[string]any) bool {
			madrid := mustZone("Europe/Madrid")
			placed := instantAt(o, "processedAt")
			return anyRefund(o, func(f map[string]any) bool {
				back := instantAt(f, "processedAt")
				return placed.UTC().Format(time.DateOnly) == back.UTC().Format(time.DateOnly) &&
					placed.In(madrid).Format(time.DateOnly) != back.In(madrid).Format(time.DateOnly)
			})
		}),
		one("one-hundred-twenty-lines", func(o map[string]any) bool { return len(lines(o)) == 120 }),
		one("discount-written-short", func(o map[string]any) bool {
			return slices.ContainsFunc(lines(o), func(l map[string]any) bool {
				a := objectsAt(l, "discountAllocations")
				return len(a) == 1 && textAt(a[0], "allocatedAmountSet", "shopMoney", "amount") == "1.0" &&
					numberAt(l, "quantity") == 3 && numberAt(l, "currentQuantity") == 2
			})
		}),
		one("tax-inclusive-shipping-refund", func(o map[string]any) bool {
			return o["taxesIncluded"] == true && anyRefund(o, func(f map[string]any) bool { return len(shippingLines(f)) > 0 })
		}),
		one("deleted-order", func(o map[string]any) bool { return len(lines(o)) > 0 }),
		// The three the review added: duties, the date that places an order,
		// and an order that does not count older than every one that does.
		one("duties-with-a-partial-duties-refund", func(o map[string]any) bool {
			charged := textAt(o, "originalTotalDutiesSet", "shopMoney", "amount")
			return charged != "" && charged != "0.00" && anyRefund(o, func(f map[string]any) bool {
				duties := objectsAt(f, "duties")
				return len(duties) == 1 && textAt(duties[0], "amountSet", "shopMoney", "amount") != charged
			})
		}),
		one("processed-after-it-was-created", func(o map[string]any) bool {
			madrid := mustZone("Europe/Madrid")
			return instantAt(o, "processedAt").In(madrid).Format(time.DateOnly) != instantAt(o, "createdAt").In(madrid).Format(time.DateOnly)
		}),
		{name: "cancelled-before-the-first-counted-order", orders: []string{"cancelled-before-the-first-counted-order"},
			holds: func(os []map[string]any) bool { return os[0]["cancelledAt"] != nil }},
	}
}

// ---- reading the file as data ----

func readFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read the fixture store: %v", err)
	}
	return raw
}

// fixtureNodes is the file's orders as plain JSON objects.
func fixtureNodes(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode the fixture store: %v", err)
	}
	return objectsAt(f, "orders", "nodes")
}

func fixtureOrder(t *testing.T, nodes []map[string]any, name string) map[string]any {
	t.Helper()
	for _, o := range nodes {
		if o["name"] == name {
			return o
		}
	}
	t.Fatalf("the fixture holds no order named %s", name)
	return nil
}

func valueAt(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func objectsAt(v any, path ...string) []map[string]any {
	list, _ := valueAt(v, path...).([]any)
	var out []map[string]any
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func textAt(v any, path ...string) string { s, _ := valueAt(v, path...).(string); return s }

func numberAt(v any, path ...string) float64 { n, _ := valueAt(v, path...).(float64); return n }

func instantAt(v any, path ...string) time.Time {
	t, _ := time.Parse(time.RFC3339, textAt(v, path...))
	return t
}

// ---- the fake Shopify ----

// fixtureOrigin is Shopify for one store, answering from the file: the list
// of orders, each with its first page of lines as the document asks for it,
// and the pages of lines after it. Any other request fails the test.
type fixtureOrigin struct {
	t      *testing.T
	host   string
	orders []map[string]any
	mu     sync.Mutex
	asked  map[string]int
}

// linesPage is the page the order documents read lines in.
const linesPage = 100

func (o *fixtureOrigin) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != o.host {
		return nil, fmt.Errorf("the fixture store answers %s only, not %s", o.host, r.URL.Host)
	}
	var body struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.asked[body.OperationName]++
	o.mu.Unlock()
	var data any
	switch body.OperationName {
	case "ShopifyListOrder":
		if !strings.Contains(body.Query, "lineItems(first: "+strconv.Itoa(linesPage)+")") {
			o.t.Errorf("the list of orders no longer reads lines %d at a time: the fixture's pages of lines are cut wrong", linesPage)
		}
		from := 0
		if after, _ := body.Variables["after"].(string); after != "" {
			from, _ = strconv.Atoi(strings.TrimPrefix(after, "orders:"))
		}
		first := int(numberAt(body.Variables, "first"))
		until := min(len(o.orders), from+first)
		nodes := make([]any, 0, until-from)
		for _, order := range o.orders[from:until] {
			nodes = append(nodes, withLinePage(order, 0))
		}
		data = map[string]any{"orders": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": until < len(o.orders), "endCursor": "orders:" + strconv.Itoa(until)},
			"nodes":    nodes,
		}}
	case "ShopifyPageOrderLineItems":
		id, _ := body.Variables["id"].(string)
		after, _ := body.Variables["after"].(string)
		from, err := strconv.Atoi(strings.TrimPrefix(after, "lines:"))
		order := o.order(id)
		if err != nil || order == nil {
			return nil, fmt.Errorf("the fixture store has no page of lines %q of %s", after, id)
		}
		page := withLinePage(order, from)
		data = map[string]any{"node": map[string]any{"__typename": "Order", "lineItems": page["lineItems"]}}
	default:
		o.t.Errorf("the fixture store was asked %s, which it does not answer", body.OperationName)
		data = nil
	}
	raw, err := json.Marshal(map[string]any{
		"data": data,
		"extensions": map[string]any{"cost": map[string]any{
			"actualQueryCost": 10,
			"throttleStatus":  map[string]any{"maximumAvailable": 2000, "currentlyAvailable": 1800, "restoreRate": 100},
		}},
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
}

func (o *fixtureOrigin) order(id string) map[string]any {
	for _, order := range o.orders {
		if order["id"] == id {
			return order
		}
	}
	return nil
}

// withLinePage is the order with the page of its lines that starts at from,
// as Shopify answers a connection read linesPage at a time.
func withLinePage(order map[string]any, from int) map[string]any {
	out := map[string]any{}
	for k, v := range order {
		out[k] = v
	}
	all := objectsAt(order, "lineItems", "nodes")
	until := min(len(all), from+linesPage)
	page := make([]any, 0, until-from)
	for _, l := range all[from:until] {
		page = append(page, l)
	}
	var end any
	if until < len(all) {
		end = "lines:" + strconv.Itoa(until)
	}
	out["lineItems"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": until < len(all), "endCursor": end}, "nodes": page}
	return out
}

// ---- the store ----

type fixtureStore struct {
	i             *Integration
	storeID, site string
	fixture       []byte
	nodes         []map[string]any
	zone          *time.Location
	origin        *fixtureOrigin
}

// newFixtureStore writes the fixture store through the mirror's own path and
// folds it: the shop row and the wholesale pack's rows, the orders through
// the connector's sweep, the deleted order's tombstone, then the rollup until
// it has caught up.
func newFixtureStore(t *testing.T) fixtureStore {
	t.Helper()
	i, id := wholesaleStore(t)
	eng, db := statsEngine(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = ?`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = ?`,
		} {
			_, _ = db.ExecContext(ctx, q, id)
		}
	})
	s := fixtureStore{i: i, storeID: id, fixture: readFixture(t), zone: mustZone("Europe/Madrid")}
	s.nodes = fixtureNodes(t, s.fixture)

	answerVerdict(i, verdictReply("recording", time.Now().Add(-24*time.Hour)))
	writeShopRow(t, id, map[string]any{
		"currencyCode": "EUR", "ianaTimezone": "Europe/Madrid", "taxesIncluded": false,
		"plan": map[string]any{"partnerDevelopment": false},
	})
	settingsFor(t, id, id)
	grant(t, id, id, "corner", fixtureGranted, "granted")

	// The orders, read from the fake Shopify by the connector's own sweep.
	s.origin = &fixtureOrigin{t: t, host: id + ".myshopify.com", orders: s.nodes, asked: map[string]int{}}
	previous := http.DefaultTransport
	http.DefaultTransport = s.origin
	t.Cleanup(func() { http.DefaultTransport = previous })
	stores := shopify.NewStoreRegistry(eng, func(context.Context, string) (string, error) { return "shpat_fixture", nil })
	conn := shopify.NewConnector(eng, quietLogger(), stores, shopify.NewAdminClient()).WithDatabase(func() *sql.DB { return db.DB })
	report, err := conn.ReconcilePartition(context.Background(), id, generated.ConceptID("order"), time.Time{})
	if err != nil {
		t.Fatalf("the sweep of the fixture's orders: %v", err)
	}
	if report.Checked < len(s.nodes) {
		t.Fatalf("the sweep checked %d orders, want the fixture's %d: %+v", report.Checked, len(s.nodes), report)
	}
	if s.origin.asked["ShopifyPageOrderLineItems"] == 0 {
		t.Fatal("the sweep never read a second page of lines: the order of 120 lines proves nothing")
	}
	settle(t, i, id)

	// ORDERS_DELETE for an order already read: the tombstone as the mirror
	// writes it for an announced deletion, the order's identity and nothing
	// else of it, marked deleted.
	gone := textAt(fixtureOrder(t, s.nodes, "deleted-order"), "id")
	if fact := orderFactsOf(t, id)[gone]; fact == nil || cell(fact, "deleted") != "false" {
		t.Fatalf("the order to be deleted was not folded live first: %v", fact)
	}
	at := time.Now().UTC()
	mirror(t, orderConcept, id, gone, map[string]any{
		"storeId": id, "gid": gone, "deleted": true, "updatedAt": at.Format(time.RFC3339),
		"versionSource": memqlsync.VersionFromAnnouncement, "syncedAt": memqlsync.ReadTime(at), "versionReadAt": memqlsync.ReadTime(at),
	})

	caughtUp := func() bool {
		k := keptRow(t, id)
		row := storeRow(t, id)
		fact := orderFactsOf(t, id)[gone]
		return !k.rebuildingSince.Valid && k.walkedThrough.Valid && row["wholesale_enabled"] == true &&
			row["wholesale_cursor"] == "" && fact != nil && cell(fact, "deleted") == "true"
	}
	for range 6 {
		if caughtUp() {
			break
		}
		runOK(t, i)
	}
	if !caughtUp() {
		t.Fatalf("the rollup did not catch up with the fixture store in 6 runs: %v", storeRow(t, id))
	}
	s.site = siteFor(t, id, nil)
	return s
}

// fixtureClock is the moment every period is asked at: a Monday lunchtime in
// Madrid, after the last of the fixture's refunds.
func fixtureClock(zone *time.Location) time.Time { return time.Date(2025, 11, 10, 12, 30, 0, 0, zone) }

// fixturePeriod is a period's bounds, by plain date arithmetic on the clock in
// the store's zone: 24 hours of elapsed time ending with the hour under way,
// n store days ending today, or this month and the twelve before it. Madrid's
// midnight always exists, so time.Date answers every bound.
type fixturePeriod struct {
	name                 string
	start, end, prevFrom time.Time
	bucket               string
	hasPrevious          bool
}

func fixturePeriods(now time.Time) []fixturePeriod {
	loc := now.Location()
	y, m, d := now.Date()
	hourEnd := time.Date(y, m, d, now.Hour(), 0, 0, 0, loc).Add(time.Hour)
	days := func(name string, n int) fixturePeriod {
		return fixturePeriod{name: name, start: time.Date(y, m, d+1-n, 0, 0, 0, 0, loc), end: time.Date(y, m, d+1, 0, 0, 0, 0, loc),
			prevFrom: time.Date(y, m, d+1-2*n, 0, 0, 0, 0, loc), bucket: "day", hasPrevious: true}
	}
	return []fixturePeriod{
		{name: "24h", start: hourEnd.Add(-24 * time.Hour), end: hourEnd, prevFrom: hourEnd.Add(-48 * time.Hour), bucket: "hour", hasPrevious: true},
		days("7d", 7), days("30d", 30), days("90d", 90),
		{name: "12m", start: time.Date(y, m-12, 1, 0, 0, 0, 0, loc), end: time.Date(y, m+1, 1, 0, 0, 0, 0, loc), bucket: "month"},
	}
}

// historyFrom is the store-local midnight of the day of the fixture's oldest
// order that was not deleted, cancelled or not: where the store's recorded
// history begins.
func historyFrom(t *testing.T, nodes []map[string]any, zone *time.Location) time.Time {
	t.Helper()
	var oldest time.Time
	for _, o := range nodes {
		if o["name"] == "deleted-order" {
			continue
		}
		if at := instantAt(o, "processedAt"); oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	y, m, d := oldest.In(zone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, zone)
}

// answerText is a value of the answer as the calculator writes one: a string
// as it is, a count in its digits, null as "null".
func answerText(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return fmt.Sprintf("%v", v)
}

// differences compares the answer's eight figures, or seven of a bucket, with
// the calculator's.
func differences(where string, got map[string]any, want map[string]string, in string) []string {
	var out []string
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if g := answerText(got[k]); g != want[k] {
			out = append(out, fmt.Sprintf("%s %s: the component answers %s, the calculator %s (%s)", where, k, g, want[k], in))
		}
	}
	return out
}

// listText is a list of the answer as one text: "null" when the answer
// gives null, "[]" when it gives an empty list, each row by row otherwise, so
// a list measured and empty is never taken for one not measured.
func listText(v any, row func(m map[string]any) string) string {
	if v == nil {
		return "null"
	}
	list, ok := v.([]any)
	if !ok {
		return fmt.Sprintf("not a list: %v", v)
	}
	var rows []string
	for _, x := range list {
		m, _ := x.(map[string]any)
		rows = append(rows, row(m))
	}
	return "[" + strings.Join(rows, "; ") + "]"
}

func productText(p map[string]any) string {
	return fmt.Sprintf("%s %q %s", answerText(p["key"]), answerText(p["title"]), answerText(p["value"]))
}

func (s fixtureStore) ask(t *testing.T, period string) map[string]any {
	t.Helper()
	return mustAsk(t, ownerCtx(), s.site, s.storeID, period)
}

// THE FIGURES EQUAL AN INDEPENDENT CALCULATION TO THE CENT, in every period:
// the eight figures, the previous period's, every bucket of the series, both
// top lists, the products' count and what names no product, the buyers, the
// wholesale split and the other currencies. A difference names the period,
// the figure, both values and the orders and refunds that fall in it.
func TestTheFiguresEqualAnIndependentCalculationToTheCent(t *testing.T) {
	s := newFixtureStore(t)
	history := historyFrom(t, s.nodes, s.zone)
	granted := []string{fixtureGranted}
	stamp := func(at time.Time) string { return at.In(s.zone).Format(time.RFC3339) }

	// Every period at the fixture's clock, and the 24 hours of a quiet day
	// after it, which hold no order and no refund: its lists are measured and
	// empty, never null.
	type asked struct {
		now time.Time
		fixturePeriod
	}
	var periods []asked
	for _, p := range fixturePeriods(fixtureClock(s.zone)) {
		periods = append(periods, asked{fixtureClock(s.zone), p})
	}
	quiet := fixtureClock(s.zone).AddDate(0, 0, 2)
	periods = append(periods, asked{quiet, fixturePeriods(quiet)[0]})
	if periods[len(periods)-1].name != "24h" {
		t.Fatal("the quiet day's period is not the 24 hours")
	}

	for _, p := range periods {
		atNow(t, p.now)
		got := s.ask(t, p.name)
		if p.now.Equal(quiet) {
			p.name = "24h on a quiet day"
		}
		want := referenceFigures(s.fixture, s.zone, "EUR", false, granted, p.start, p.end, p.bucket)
		in := "orders: " + strings.Join(want.Orders, ", ") + "; refunds: " + strings.Join(want.Refunds, ", ")
		var diffs []string
		note := func(what, gotText, wantText string) {
			if gotText != wantText {
				diffs = append(diffs, fmt.Sprintf("%s %s: the component answers %s, the calculator %s (%s)", p.name, what, gotText, wantText, in))
			}
		}

		// The frame the figures are read in, and that nothing is left out.
		note("start", answerText(got["start"]), stamp(p.start))
		note("end", answerText(got["end"]), stamp(p.end))
		note("bucket", answerText(got["bucket"]), p.bucket)
		note("historySince", answerText(got["historySince"]), stamp(history))
		note("recording.reasons", fmt.Sprint(valueAt(got, "recording", "reasons")), "[]")
		note("recording.blocked", answerText(valueAt(got, "recording", "blocked")), "false")
		note("floors", fmt.Sprint(got["floors"]), "map[]")
		for _, k := range []string{"incompleteOrders", "incompleteRefunds", "refundsNotRead"} {
			note(k, answerText(got[k]), "0")
		}

		figures, _ := got["figures"].(map[string]any)
		diffs = append(diffs, differences(p.name+" figures", figures, want.Figures, in)...)

		// The previous period: the same figures, where history covers it.
		previous, _ := got["previous"].(map[string]any)
		switch {
		case !p.hasPrevious || p.prevFrom.Before(history):
			note("previous", answerText(got["previous"]), "null")
		case previous == nil:
			note("previous", "null", "the previous period's figures")
		default:
			prev := referenceFigures(s.fixture, s.zone, "EUR", false, granted, p.prevFrom, p.start, p.bucket)
			note("prevStart", answerText(got["prevStart"]), stamp(p.prevFrom))
			diffs = append(diffs, differences(p.name+" previous", previous, prev.Figures,
				"orders: "+strings.Join(prev.Orders, ", ")+"; refunds: "+strings.Join(prev.Refunds, ", "))...)
		}

		series := objectsAt(got, "series")
		note("series length", strconv.Itoa(len(series)), strconv.Itoa(len(want.Series)))
		for i, b := range want.Series {
			if i >= len(series) {
				break
			}
			note(fmt.Sprintf("series[%d].at", i), answerText(series[i]["at"]), b.At)
			// A bucket that ends before the recorded history begins is not
			// measured, and answered null: the read's rule, not a figure's.
			bucketEnd := p.end
			if i+1 < len(want.Series) {
				bucketEnd, _ = time.Parse(time.RFC3339, want.Series[i+1].At)
			}
			if !bucketEnd.After(history) {
				for k := range b.Figures {
					b.Figures[k] = "null"
				}
			}
			diffs = append(diffs, differences(p.name+" bucket "+b.At, series[i], b.Figures, in)...)
		}

		var bySales, byUnits []string
		for _, r := range want.ProductsBySales {
			bySales = append(bySales, r.String())
		}
		for _, r := range want.ProductsByUnits {
			byUnits = append(byUnits, r.String())
		}
		note("productsBySales", listText(got["productsBySales"], productText), "["+strings.Join(bySales, "; ")+"]")
		note("productsByUnits", listText(got["productsByUnits"], productText), "["+strings.Join(byUnits, "; ")+"]")
		note("productCount", answerText(got["productCount"]), want.ProductCount)
		note("refundsWithoutItem", answerText(got["refundsWithoutItem"]), want.RefundsWithoutItem)

		buyers, _ := got["buyers"].(map[string]any)
		diffs = append(diffs, differences(p.name+" buyers", buyers, want.Buyers, in)...)
		for _, side := range []string{"wholesale", "retail"} {
			split, _ := valueAt(got, "wholesale", side).(map[string]any)
			diffs = append(diffs, differences(p.name+" wholesale."+side, split, want.Wholesale[side], in)...)
		}
		var wantOthers []string
		for _, c := range want.OtherCurrencies {
			wantOthers = append(wantOthers, c.Currency+" "+c.Orders)
		}
		note("otherCurrencies", listText(got["otherCurrencies"], func(c map[string]any) string {
			return answerText(c["currency"]) + " " + answerText(c["orders"])
		}), "["+strings.Join(wantOthers, "; ")+"]")

		for _, d := range diffs {
			t.Error(d)
		}
	}
}

// THE SHORT-WRITTEN DISCOUNT IS SHARED IN CENTS BY BOTH: "1.0" over the two
// of three units an edit left is 0.67, in the component's line fact and in
// the calculator alike, not 0.7.
func TestTheShortWrittenDiscountIsSharedInCentsByBoth(t *testing.T) {
	s := newFixtureStore(t)
	order := fixtureOrder(t, s.nodes, "discount-written-short")
	line := textAt(objectsAt(order, "lineItems", "nodes")[0], "id")

	fact := lineFactsOf(t, s.storeID)[line]
	if fact == nil {
		t.Fatalf("the line %s has no fact", line)
	}
	if got := cell(fact, "discounts"); got != "0.67" {
		t.Errorf("the component shares the discount as %s, want 0.67", got)
	}
	placed := instantAt(order, "processedAt").In(s.zone)
	y, m, d := placed.Date()
	ref := referenceFigures(s.fixture, s.zone, "EUR", false, []string{fixtureGranted},
		time.Date(y, m, d, 0, 0, 0, 0, s.zone), time.Date(y, m, d+1, 0, 0, 0, 0, s.zone), "day")
	if got := ref.LineDiscounts[line]; got != "0.67" {
		t.Errorf("the calculator shares the discount as %s, want 0.67", got)
	}
}

// THE FIXTURE HOLDS EVERY CASE THE RECORD NAMES: twenty cases and the three
// the review added, each found by
// its orders' names in the file and holding what makes it that case; and
// every key the file holds is a field the mirror's order documents select.
func TestTheFixtureHoldsEveryCaseTheRecordNames(t *testing.T) {
	nodes := fixtureNodes(t, readFixture(t))
	cases := fixtureCases()
	if len(cases) != 23 {
		t.Fatalf("%d cases, want the twenty the record names and the three the review added", len(cases))
	}
	named := map[string]bool{}
	for _, c := range cases {
		var orders []map[string]any
		for _, name := range c.orders {
			named[name] = true
			found := false
			for _, o := range nodes {
				if o["name"] == name {
					orders, found = append(orders, o), true
				}
			}
			if !found {
				t.Errorf("case %s: the fixture holds no order named %s", c.name, name)
			}
		}
		if len(orders) == len(c.orders) && !c.holds(orders) {
			t.Errorf("case %s: its orders do not hold what makes the case", c.name)
		}
	}
	for _, o := range nodes {
		if name, _ := o["name"].(string); !named[name] {
			t.Errorf("the fixture holds an order, %q, that is no case", name)
		}
	}

	document := generated.Types["order"].FetchDocument
	word := map[string]bool{}
	for _, w := range regexp.MustCompile(`[A-Za-z_]+`).FindAllString(document, -1) {
		word[w] = true
	}
	var keys func(v any, path string)
	keys = func(v any, path string) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if !word[k] {
					t.Errorf("the fixture holds %s.%s, which no order document selects", path, k)
				}
				keys(child, path+"."+k)
			}
		case []any:
			for _, child := range x {
				keys(child, path+"[]")
			}
		}
	}
	for _, o := range nodes {
		keys(o, textAt(o, "name"))
	}
}

// THE FIXTURE'S REFUNDS HOLD ONLY THE FIELDS THE MIRROR KEEPS of a refund's
// three connections (Task 2's allowlist): no shippingLine, no phone, nothing a
// live refund carries that the mirror would drop.
func TestTheFixturesRefundsHoldOnlyTheFieldsTheMirrorKeeps(t *testing.T) {
	kept := map[string][]string{
		"refundLineItems":     {"lineItem", "quantity", "subtotalSet", "totalTaxSet"},
		"refundShippingLines": {"subtotalAmountSet", "taxAmountSet"},
		"orderAdjustments":    {"amountSet", "taxAmountSet", "reason"},
	}
	// Each kept key is what the list of orders selects there.
	document := generated.Types["order"].FetchDocument
	list := document[strings.Index(document, "query ShopifyListOrder"):]
	for connection, fields := range kept {
		at := strings.Index(list, connection+"(")
		if at < 0 {
			t.Fatalf("the list of orders selects no %s", connection)
		}
		selection := list[at : at+strings.Index(list[at:], "\n")]
		for _, f := range fields {
			if !strings.Contains(selection, " "+f+" ") {
				t.Errorf("the list of orders selects no %s in %s: the mirror would not keep it", f, connection)
			}
		}
	}
	refunds := 0
	for _, o := range fixtureNodes(t, readFixture(t)) {
		for _, f := range objectsAt(o, "refunds") {
			refunds++
			for connection, fields := range kept {
				for _, n := range objectsAt(f, connection, "nodes") {
					for k := range n {
						if !slices.Contains(fields, k) {
							t.Errorf("order %s, refund %s: %s holds %s, which the mirror does not keep", textAt(o, "name"), textAt(f, "id"), connection, k)
						}
					}
				}
			}
		}
	}
	if refunds == 0 {
		t.Fatal("the fixture holds no refund: this test proves nothing")
	}
}

// THE REFERENCE SHARES NO CODE WITH THE COMPONENT: reference_calc_test.go
// imports the standard library only, and it type-checks ALONE, as a package of
// its one file: every name it uses is declared in it, a Go builtin, or a
// package it imports. A name of the component's anywhere -- a call, a type, a
// map or array key in a composite literal -- is undefined there. A builtin's
// name another file of the package declares again would be that file's in the
// real package, so none may be used.
func TestTheReferenceSharesNoCodeWithTheComponent(t *testing.T) {
	others := packageNames(t, "reference_calc_test.go")
	source, err := os.ReadFile("reference_calc_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range sharedCode(t, string(source), others) {
		t.Errorf("the calculator: %s", problem)
	}

	// The check can still say otherwise: a component's name as a map key and
	// as an array index key, which the parser alone leaves unseen, a call of a
	// test helper, and an import of the component's money.
	for _, planted := range []string{
		"package storestats\n\nvar _ = map[int]string{topRows: \"rows\"}\n",
		"package storestats\n\nvar _ = [30]string{topRows: \"rows\"}\n",
		"package storestats\n\nvar _ = ptr(1)\n",
		"package storestats\n\nimport \"github.com/znasllc-io/memql/core/money\"\n\nvar _ money.Amount\n",
	} {
		if len(sharedCode(t, planted, others)) == 0 {
			t.Errorf("the check passes a source that uses the component's code:\n%s", planted)
		}
	}
}

// packageNames is every top-level name the package's files but skip declare.
func packageNames(t *testing.T, skip string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if path == skip {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					names[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						names[spec.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range spec.Names {
							names[n.Name] = true
						}
					}
				}
			}
		}
	}
	return names
}

// sharedCode is every way source reaches past itself and the standard
// library: an import that is not the standard library's, anything the type
// checker finds undefined when the file is its own package, and a builtin
// another file of the package declares again.
func sharedCode(t *testing.T, source string, others map[string]bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "calculator.go", source, 0)
	if err != nil {
		return []string{"does not parse: " + err.Error()}
	}
	var problems []string
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
			problems = append(problems, "imports "+path+", which is not the standard library")
		}
		if spec.Name != nil && (spec.Name.Name == "_" || spec.Name.Name == ".") {
			problems = append(problems, "imports "+path+" as "+spec.Name.Name)
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "source", nil),
		Error:    func(err error) { problems = append(problems, err.Error()) },
	}
	_, _ = conf.Check("storestats", fset, []*ast.File{file}, info)
	for id, obj := range info.Uses {
		if obj.Parent() == types.Universe && others[id.Name] {
			problems = append(problems, fmt.Sprintf("uses %s at %s, a builtin another file of the package declares again",
				id.Name, fset.Position(id.Pos())))
		}
	}
	return problems
}
