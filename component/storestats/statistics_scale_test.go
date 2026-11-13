package storestats

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/money"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics_scale_test.go measures both the 30-day and 90-day page reads.
// The ordinary database lane checks the answers among other stores' facts;
// its wall time is not a correctness assertion on a shared CI runner.
//
// With MEMQL_STORESTATS_SCALE_EVIDENCE_FILE set, the fixture grows to 200,000
// orders and 600,000 lines, among nine stores of 10,000 orders each. Both
// reads must stay below the same 500 ms median bound. The evidence includes
// their timings and the products statement's EXPLAIN ANALYZE plan, collected
// outside the timed requests. Run this evidence lane on a quiet machine:
//
//	MEMQL_STORESTATS_SCALE_EVIDENCE_FILE=/tmp/store-scale.json \
//	  MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... \
//	  go test -count=1 -p=1 github.com/znasllc-io/memql/component/storestats/ -run TestPeriodReadsAtScale
//
// The facts have the derived columns the fold writes and are removed after
// the test. Every run checks Orders and Gross sales against direct aggregates
// of the fixture, including in the ordinary database lane.

const scaleBound = 500 * time.Millisecond

type scaleSize struct {
	Orders, Refunds, OtherStores, OtherOrders int
}

// writeScaleFacts writes a store's facts as of now: orders orders spread
// over the last 120 days, each of three lines; refunds refunds of every
// tenth order, a day after it; and every 50th order deleted, every 70th
// cancelled and every 90th a test.
func writeScaleFacts(t *testing.T, db *bun.DB, storeID string, orders, refunds int, now time.Time) {
	t.Helper()
	ctx := context.Background()
	statements := []struct {
		what string
		sql  string
		args []any
	}{
		{"lines", `INSERT INTO store_sales_line_fact (store_id, line_gid, order_gid, line_version, currency, gift_card, product_key, title,
				quantity, current_quantity, unit_price, allocated_discounts, refunded_quantity, sold_quantity, gross, discounts, order_ordered_at)
			SELECT ?, 'scale-l-' || j, 'scale-o-' || ((j - 1) / 3 + 1), ?::timestamptz, 'EUR', false, 'scale-p-' || (j % 500),
				'Spoke set ' || (j % 500), 1, 1, ((j % 7) + 1) * 1.25, CASE WHEN j % 4 = 0 THEN 0.50 ELSE 0 END, 0, 1,
				((j % 7) + 1) * 1.25, CASE WHEN j % 4 = 0 THEN 0.50 ELSE 0 END,
				?::timestamptz - make_interval(mins => ((((j - 1) / 3 + 1) * 7919) % 172800)::int)
			FROM generate_series(1, ? * 3) AS j`, []any{storeID, now, now, orders}},
		// Each order with what its three lines sum to, as the derivation
		// would write it, in one pass over the lines.
		{"orders", `INSERT INTO store_sales_order_fact (store_id, order_gid, order_version, deleted, recorded_at, ordered_at, currency,
				cancelled, test, taxes_included, buyer_key, wholesale, shipping, tax, duties, expected_quantity, line_count, lines_complete,
				gross, discounts, units)
			SELECT ?, s.order_gid, ?::timestamptz, s.i % 50 = 0, ?::timestamptz,
				?::timestamptz - make_interval(mins => ((s.i * 7919) % 172800)::int), 'EUR', s.i % 70 = 0, s.i % 90 = 0, false,
				CASE WHEN s.i % 3 = 0 THEN '' ELSE 'scale-buyer-' || (s.i % 5000) END, s.i % 9 = 0, 4.95, 2.10, 0, 3, 3, true,
				s.gross, s.discounts, s.units
			FROM (SELECT order_gid, split_part(order_gid, '-', 3)::bigint AS i, sum(gross) AS gross, sum(discounts) AS discounts,
					sum(sold_quantity) AS units
				FROM store_sales_line_fact WHERE store_id = ? GROUP BY order_gid) AS s`, []any{storeID, now, now, now, storeID}},
		{"refunds", `INSERT INTO store_sales_refund_fact (store_id, refund_gid, order_gid, refund_version, recorded_at, refunded_at,
				money_refunded, shipping_refunded, tax_refunded, duties_refunded, detail_fetched, lines_complete,
				gift_card_refunded, item_refunds, units_returned)
			SELECT ?, 'scale-r-' || k, o.order_gid, ?::timestamptz, ?::timestamptz, o.ordered_at + interval '1 day',
				6.05, 0, 1.05, 0, true, true, 0, 5.00, 1
			FROM generate_series(1, ?) AS k
			JOIN store_sales_order_fact o ON o.store_id = ? AND o.order_gid = 'scale-o-' || (k * 10)`, []any{storeID, now, now, refunds, storeID}},
		{"refund lines", `INSERT INTO store_sales_refund_line_fact (store_id, refund_gid, position, order_gid, line_gid, quantity, subtotal, tax)
			SELECT ?, 'scale-r-' || k, 0, 'scale-o-' || (k * 10), 'scale-l-' || ((k * 10 - 1) * 3 + 1), 1, 5.00, 1.05
			FROM generate_series(1, ?) AS k`, []any{storeID, refunds}},
	}
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("write the scale facts of %s (%s): %v", storeID, s.what, err)
		}
	}
}

type scaleEvidence struct {
	Orders, Lines, Refunds, OtherStores, OtherOrders int
	WriteSeconds                                     float64
	BoundMs                                          float64
	Periods                                          []scalePeriodEvidence
}

type scalePeriodEvidence struct {
	Period      string
	RunsMs      []float64
	MedianMs    float64
	ProductPlan json.RawMessage `json:",omitempty"`
}

func TestPeriodReadsAtScale(t *testing.T) {
	size := scaleSize{Orders: 20000, Refunds: 2000, OtherStores: 9, OtherOrders: 1000}
	output := os.Getenv("MEMQL_STORESTATS_SCALE_EVIDENCE_FILE")
	if output != "" {
		size = scaleSize{Orders: 200000, Refunds: 20000, OtherStores: 9, OtherOrders: 10000}
	}
	// Fix the request clock and fixture clock together so a midnight
	// boundary cannot change the expected answer.
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	atNow(t, now)
	storeID, site := salesStore(t, map[string]any{"history_since": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		"last_clear_at": now, "walked_through": now})
	_, db := statsEngine(t)
	began := time.Now()
	writeScaleFacts(t, db, storeID, size.Orders, size.Refunds, now)
	for n := 0; n < size.OtherStores; n++ {
		other := fmt.Sprintf("dbtest-storestats-scale-other-%d-%s", n, uniq())
		t.Cleanup(func() {
			for _, table := range statsTableNames() {
				_, _ = db.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE store_id = ?", other)
			}
		})
		writeScaleFacts(t, db, other, size.OtherOrders, size.OtherOrders/10, now)
	}
	for _, table := range []string{"store_sales_order_fact", "store_sales_line_fact", "store_sales_refund_fact", "store_sales_refund_line_fact"} {
		if _, err := db.ExecContext(context.Background(), "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	evidence := scaleEvidence{Orders: size.Orders, Lines: 3 * size.Orders, Refunds: size.Refunds,
		OtherStores: size.OtherStores, OtherOrders: size.OtherOrders, WriteSeconds: time.Since(began).Seconds(),
		BoundMs: float64(scaleBound.Milliseconds())}

	for _, period := range []struct {
		name string
		kind statsperiod.Period
	}{{"30d", statsperiod.Last30Days}, {"90d", statsperiod.Last90Days}} {
		t.Run(period.name, func(t *testing.T) {
			bounds := statsperiod.ForIn(period.kind, now, madrid)
			var direct struct {
				Orders int64  `bun:"orders"`
				Gross  string `bun:"gross"`
			}
			if err := db.NewSelect().Table("store_sales_order_fact").ColumnExpr("count(*) AS orders, COALESCE(sum(gross), 0)::text AS gross").
				Where("store_id = ? AND NOT deleted AND NOT cancelled AND NOT test AND ordered_at >= ? AND ordered_at < ?", storeID, bounds.Start, bounds.End).
				Scan(context.Background(), &direct); err != nil {
				t.Fatal(err)
			}
			if direct.Orders == 0 {
				t.Fatal("the scale facts hold no order in the period: the fixture is wrong")
			}
			want, err := money.Parse(direct.Gross)
			if err != nil {
				t.Fatal(err)
			}
			queries := storeSalesQueries(t)
			read := scalePeriodEvidence{Period: period.name}
			var productQuery string
			for i := 0; i < 5; i++ {
				start := time.Now()
				answer := mustAsk(t, ownerCtx(), site, storeID, period.name)
				read.RunsMs = append(read.RunsMs, float64(time.Since(start).Microseconds())/1000)
				figures := figuresIn(t, answer, "figures")
				if js(figures["orders"]) != js(direct.Orders) {
					t.Errorf("orders = %s, want %d, counted over what was written", js(figures["orders"]), direct.Orders)
				}
				gross, _ := figures["grossSales"].(string)
				if got, err := money.Parse(gross); err != nil || !got.Equal(want) {
					t.Errorf("grossSales = %q, want %s, summed over what was written", gross, direct.Gross)
				}
				wantValues(t, "the fixture's product lists", answer, map[string]any{
					"productCount": 500, "productSalesCount": 500, "productUnitsCount": 500})
				for _, query := range queries() {
					if strings.Contains(query, "WITH sold AS") {
						productQuery = query
					}
				}
			}
			sorted := append([]float64(nil), read.RunsMs...)
			sort.Float64s(sorted)
			read.MedianMs = sorted[len(sorted)/2]
			t.Logf("%d orders, %d refunds, %d other stores of %d orders; %s in %v ms (median %.1f ms)",
				size.Orders, size.Refunds, size.OtherStores, size.OtherOrders, period.name, read.RunsMs, read.MedianMs)
			if output != "" {
				if read.MedianMs > evidence.BoundMs {
					t.Errorf("the %s read took a median %.1f ms over %v, want under %v", period.name, read.MedianMs, read.RunsMs, scaleBound)
				}
				if productQuery == "" {
					t.Fatal("the products statement was not recorded")
				}
				var plan string
				if err := db.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+productQuery).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				read.ProductPlan = json.RawMessage(plan)
			}
			evidence.Periods = append(evidence.Periods, read)
		})
	}
	if output != "" {
		raw, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, raw, 0o600); err != nil {
			t.Fatalf("write the evidence to %s: %v", output, err)
		}
	}
}
