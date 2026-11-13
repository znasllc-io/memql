package storestats

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// erasure_db_test.go -- a customer's erasure, as the mirror itself makes it,
// reaching the facts within one check pass (the 2026-11-11 store statistics
// design, D17 and A17). The erasure rewrites the mirror rows in place and
// keeps each version's write time, so no walk sees it; the check recomputes
// each fact from the row in hand and repairs what differs. The order's money
// stays in every figure. Postgres-gated, through the harness.

const (
	adaEmail    = "ada@example.test"
	adaCustomer = "gid://shopify/Customer/7001"
	adaOrder    = "gid://shopify/Order/7101"
	adaBike     = "gid://shopify/LineItem/7201"
	adaEngrave  = "gid://shopify/LineItem/7202"
	adaEngraved = "Engraving: Ada Example"
)

// erasedWords are what the erasure removes and no fact may hold after the
// check.
var erasedWords = []string{"Ada", adaEmail, "Engraving"}

// eraseCustomer runs the connector's own erasure of the customer and the
// orders, on the engine's maintenance session, as the mirror's privacy job
// runs it.
func eraseCustomer(t *testing.T, storeID, customer string, orders ...string) {
	t.Helper()
	eng, _ := statsEngine(t)
	conn := shopify.NewConnector(eng, quietLogger(), shopify.NewStoreRegistry(eng, eng.ResolveSystemSecret), shopify.NewAdminClient()).
		WithMaintenanceSession(func(ctx context.Context) (*sql.Conn, func(), error) {
			return database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
		})
	res, err := conn.RedactCustomer(context.Background(), storeID, customer, orders)
	if err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if res.Versions == 0 {
		t.Fatalf("the erasure changed no version: %+v", res)
	}
}

// erasureCheck runs the check under its principal, as its sweep does, until the
// store's pass has ended once. Its clock is ahead of the clock, so the rows
// the test wrote a moment ago are before the pass's cut.
func erasureCheck(t *testing.T, i *Integration, storeID string) {
	t.Helper()
	ahead := time.Now().Add(checkAhead)
	i.now = func() time.Time { return ahead }
	defer func() { i.now = nil }()
	ctx := auth.ContextWithInternalOrigin(checkCtx(t))
	for range 10 {
		if _, err := i.handleCheck(ctx, nil, 0); err != nil {
			t.Fatalf("the check failed: %v", err)
		}
		if row, found := checkRowOf(t, storeID); found && row.LastFinishedAt.Valid {
			return
		}
	}
	t.Fatal("the check did not end a pass of the store in 10 calls")
}

// salesTexts is every row of the store in every store_sales_ table, each as
// its JSON text: every column, the text and JSON ones included.
func salesTexts(t *testing.T, storeID string) []string {
	t.Helper()
	_, db := statsEngine(t)
	var out []string
	for _, table := range statsTableNames() {
		rows, err := db.QueryContext(context.Background(), "SELECT to_jsonb(x)::text FROM "+table+" x WHERE store_id = ?", storeID)
		if err != nil {
			t.Fatalf("read the store's %s: %v", table, err)
		}
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				t.Fatal(err)
			}
			out = append(out, table+": "+text)
		}
		rows.Close()
	}
	return out
}

// heldWords answers each row text that holds one of the words.
func heldWords(texts []string, words []string) []string {
	var out []string
	for _, text := range texts {
		for _, w := range words {
			if strings.Contains(text, w) {
				out = append(out, w+" in "+text)
			}
		}
	}
	return out
}

func TestAfterAnErasureAndOneCheckNoFactHoldsWhatTheErasureRemoved(t *testing.T) {
	i, id := wholesaleStore(t)
	answerVerdict(i, verdictReply("recording", time.Now().Add(-24*time.Hour)))
	shopRow(t, id, "EUR", "Europe/Madrid", false)
	settingsFor(t, id, id)
	grant(t, id, id, "ada", adaEmail, "granted")

	// Ada's order: 2 x 40.00 of a city bike, and a custom line with no
	// product, 1 x 10.00; shipping 5.00 and no tax.
	placed := stampOf(time.Now().Add(-2 * time.Hour))
	mirrorOrder(t, id, adaOrder, "processedAt", placed, "sourceCreatedAt", placed, "email", adaEmail, "customerGid", adaCustomer,
		"subtotalLineItemsQuantity", float64(3), "totalShippingPriceSet", set("5.00", "EUR"), "totalTaxSet", set("0.00", "EUR"),
		"currentSubtotalPriceSet", set("90.00", "EUR"), "totalDiscountsSet", set("0.00", "EUR"), "totalRefundedSet", set("0.00", "EUR"))
	mirrorLine(t, id, adaBike, adaOrder, "productGid", "gid://shopify/Product/7301", "title", "A city bike", "quantity", float64(2),
		"currentQuantity", float64(2), "originalUnitPriceSet", set("40.00", "EUR"), "discountAllocations", []any{})
	mirrorLine(t, id, adaEngrave, adaOrder, "productGid", nil, "title", adaEngraved, "quantity", float64(1),
		"currentQuantity", float64(1), "originalUnitPriceSet", set("10.00", "EUR"), "discountAllocations", []any{})
	// Written an hour ago: behind where the walk will stand, as an erased
	// order is, so only the check reads its rows again.
	for _, row := range []struct{ concept, gid string }{
		{orderConcept, adaOrder}, {"v1:shopify:orderLineItem", adaBike}, {"v1:shopify:orderLineItem", adaEngrave},
	} {
		backdate(t, row.concept, id, row.gid, time.Now().Add(-time.Hour))
	}
	settle(t, i, id)

	site := siteFor(t, id, nil)
	period := func(label string) {
		t.Helper()
		got := mustAsk(t, ownerCtx(), site, id, "7d")
		wantValues(t, label, figuresIn(t, got, "figures"), map[string]any{"orders": 1, "grossSales": "90.00", "totalSales": "95.00"})
	}

	// Folded: the order is wholesale and its money is in the figures.
	if got := wholesaleOfFact(t, id, adaOrder); got != "true" {
		t.Fatalf("before the erasure the order is wholesale %s, want true", got)
	}
	period("before the erasure")
	if held := heldWords(salesTexts(t, id), []string{"Engraving"}); len(held) == 0 {
		t.Fatal("before the erasure no fact holds the custom line's title: the fixture proves nothing")
	}

	eraseCustomer(t, id, adaCustomer, adaOrder)
	// The erasure rewrote the rows in place and kept their write times: a run
	// after it does not see it.
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	if got := cell(lineFactsOf(t, id)[adaEngrave], "title"); got != adaEngraved {
		t.Fatalf("a run after the erasure changed the custom line's title to %q: the walk saw the erasure, so this test proves nothing of the check", got)
	}
	erasureCheck(t, i, id)

	for _, held := range heldWords(salesTexts(t, id), erasedWords) {
		t.Errorf("after the erasure and one check a fact still holds %s", held)
	}
	order := orderFactsOf(t, id)[adaOrder]
	if order == nil {
		t.Fatal("the order has no fact")
	}
	if got := cell(order, "wholesale"); got != "false" {
		t.Errorf("the erased buyer's order is wholesale %s, want retail: its email is the erasure's marker", got)
	}
	if got := cell(order, "buyer_key"); got != adaCustomer {
		t.Errorf("the order's buyer_key is %s, want %s: the erasure keeps the customer's reference", got, adaCustomer)
	}
	line := lineFactsOf(t, id)[adaEngrave]
	if line == nil {
		t.Fatal("the custom line has no fact")
	}
	if got := cell(line, "title"); got != shopify.RedactionMarker {
		t.Errorf("the custom line's title is %q, want the erasure's marker %q", got, shopify.RedactionMarker)
	}
	period("after the erasure and one check")
}
