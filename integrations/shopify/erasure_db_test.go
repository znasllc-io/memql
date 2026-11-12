package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// erasure_db_test.go -- a customer's erasure against a real engine and a real
// Postgres (rulings 78, 81 and 83).
//
// # Why none of this can be faked
//
// Three claims, and each is about something a fake does not have. WHAT IS
// REACHED is decided in the rows: a gid that is the whole value of a field, a
// parent three levels up, a version from last month that still names the
// customer. WHAT IS LEFT has to be a row the engine accepts: the mirror's
// write merges the stored row under what it writes and validates the result,
// so a scrub value of the wrong type makes the row unwritable, and only the
// engine says so. And EVERY VERSION means the table's rows, not a row's
// newest state.
//
// The rows are written the way the mirror writes them: a fetched object
// through the mapper (mapObject) and the connector's own write (writeMirror).
// The fixtures are invented: a bicycle shop, Ada Example, example.com.

const (
	adaEmail = "ada@example.com"
	adaPhone = "+15550100"
)

// adaWords are strings that appear in Ada's fixtures and nowhere else. After
// her erasure no version of a row in scope may hold any of them.
var adaWords = []string{
	adaEmail, adaPhone, "Ada", "1 Example Street", "Testville", "203.0.113.7", "club-rider", "54cm",
	"call before delivery", "ada.png", "neighbour at number 4", "engraving", "XXXX-4242", "EX123456789",
	"returned the saddle bag", "changed her mind",
}

type erasureFixture struct {
	t     *testing.T
	eng   *memql.MemQLEngine
	db    *sql.DB
	conn  *Connector
	store string
}

// newErasureFixture boots a real engine and a connector with the database
// handle the privacy jobs need, on a store id of the test's own: an erasure
// reads every row of its store, and the database is shared.
func newErasureFixture(t *testing.T) *erasureFixture {
	t.Helper()
	eng, db := authorityEngine(t)
	if eng == nil {
		return nil
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	f := &erasureFixture{
		t: t, eng: eng, db: db, store: fmt.Sprintf("dbtest-era-%08x", h.Sum32()),
		conn: NewConnector(eng, nil, NewStoreRegistry(eng, eng.ResolveSystemSecret), NewAdminClient()).
			WithDatabase(func() *sql.DB { return db }).WithMaintenanceSession(privacySessionOf(db)),
	}
	clean := func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = $1`, f.store)
	}
	clean()
	t.Cleanup(clean)
	return f
}

// write maps one fetched object, with the children it carries, and writes
// every row through the engine as the connector does.
func (f *erasureFixture) write(concept string, obj map[string]any, at time.Time) {
	f.t.Helper()
	writes := mapObject(generated.Types[concept], f.store, obj, "", at)
	if len(writes) == 0 {
		f.t.Fatalf("the mapper made no row of this %s: %v", concept, obj)
	}
	for _, w := range writes {
		if err := f.conn.writeMirror(context.Background(), w); err != nil {
			f.t.Fatalf("write %s: %v", w.Concept, err)
		}
	}
}

// texts is every stored version of one row, oldest first, as the table holds
// it.
func (f *erasureFixture) texts(concept, gid string) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT payload::text FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2 AND payload->>'gid' = $3 ORDER BY "createdAt"`,
		generated.ConceptID(concept), f.store, gid)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, text)
	}
	return out
}

func (f *erasureFixture) versions(concept, gid string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, text := range f.texts(concept, gid) {
		payload := map[string]any{}
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, payload)
	}
	return out
}

// newest is the row's newest version, which has to exist.
func (f *erasureFixture) newest(concept, gid string) map[string]any {
	f.t.Helper()
	v := f.versions(concept, gid)
	if len(v) == 0 {
		f.t.Fatalf("the mirror holds no %s %s", concept, gid)
	}
	return v[len(v)-1]
}

// read is the typed read of one row, as a cluster owner makes it.
func (f *erasureFixture) read(concept, gid string) map[string]any {
	f.t.Helper()
	spec := generated.Types[concept]
	res, err := f.eng.Execute(memql.ContextWithFreshRead(ownerCtx()), renderCall(spec.ByGidFn, map[string]any{"storeId": f.store, "gid": gid}))
	if err != nil {
		f.t.Fatalf("the typed read of %s %s failed: %v", concept, gid, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		f.t.Fatalf("the typed read of %s %s answered %d rows", concept, gid, len(rows))
	}
	return rows[0]
}

func (f *erasureFixture) erase(customer string, orders ...string) ErasureResult {
	f.t.Helper()
	res, err := f.conn.RedactCustomer(context.Background(), f.store, customer, orders)
	if err != nil {
		f.t.Fatalf("the erasure failed: %v", err)
	}
	return res
}

// holdsNoWordOf fails for every version of the row that still holds one of
// the words.
func (f *erasureFixture) holdsNoWordOf(concept, gid string, words []string) {
	f.t.Helper()
	texts := f.texts(concept, gid)
	if len(texts) == 0 {
		f.t.Errorf("the mirror holds no %s %s: an erasure rewrites a row, it does not delete it", concept, gid)
	}
	for i, text := range texts {
		for _, word := range words {
			if strings.Contains(text, word) {
				f.t.Errorf("version %d of %s %s still holds %q: %s", i, concept, gid, word, text)
			}
		}
	}
}

func eraCustomerGID(n string) string { return "gid://shopify/Customer/" + n }
func eraOrderGID(n string) string    { return "gid://shopify/Order/" + n }

func euros(amount string) map[string]any {
	one := map[string]any{"amount": amount, "currencyCode": "EUR"}
	return map[string]any{"shopMoney": one, "presentmentMoney": one}
}

func adaAddress() map[string]any {
	return map[string]any{"firstName": "Ada", "lastName": "Example", "address1": "1 Example Street", "city": "Testville", "zip": "00000", "phone": adaPhone}
}

// adaCustomer is the customer as the Admin API answers her.
func adaCustomer(n, updatedAt string) map[string]any {
	return map[string]any{
		"id": eraCustomerGID(n), "updatedAt": updatedAt, "createdAt": "2025-01-05T09:00:00Z",
		"firstName": "Ada", "lastName": "Example", "displayName": "Ada Example",
		"defaultEmailAddress": map[string]any{"emailAddress": adaEmail, "marketingState": "SUBSCRIBED"},
		"defaultPhoneNumber":  map[string]any{"phoneNumber": adaPhone},
		"defaultAddress":      adaAddress(),
		"image":               map[string]any{"url": "https://cdn.example.com/ada.png", "altText": "Ada Example"},
		"tags":                []any{"club-rider", "Ada"},
		"metafields": map[string]any{"nodes": []any{
			map[string]any{"namespace": "custom", "key": "frame_size", "type": "single_line_text_field", "value": "54cm, Ada's own"},
		}},
		"note":           "Ada would like a call before delivery",
		"numberOfOrders": "3", "legacyResourceId": n,
		"amountSpent":   map[string]any{"amount": "1840.00", "currencyCode": "EUR"},
		"verifiedEmail": true, "taxExempt": false, "state": "ENABLED",
		"taxExemptions": []any{"EU_REVERSE_CHARGE_EXEMPTION_RULE"},
		"lastOrder":     map[string]any{"id": eraOrderGID(n + "01")},
		"orders":        map[string]any{"nodes": []any{map[string]any{"id": eraOrderGID(n + "01")}}},
	}
}

// bikeOrder is an order of the bicycle shop with one line, one transaction,
// one fulfilment and one refund, as the Admin API answers it. customer is the
// buyer's gid, or "" for an order with no customer on it.
func bikeOrder(n, customer, updatedAt string) map[string]any {
	gid := eraOrderGID(n)
	order := map[string]any{
		"id": gid, "updatedAt": updatedAt, "createdAt": "2026-09-01T08:00:00Z", "processedAt": "2026-09-01T08:00:05Z",
		"name": "#" + n, "email": adaEmail, "phone": adaPhone, "clientIp": "203.0.113.7",
		"note":         "leave with the neighbour at number 4",
		"currencyCode": "EUR", "displayFinancialStatus": "PAID", "taxExempt": false, "customerAcceptsMarketing": true,
		"currentSubtotalLineItemsQuantity": 1,
		"billingAddress":                   adaAddress(),
		"shippingAddress":                  adaAddress(),
		"customAttributes":                 []any{map[string]any{"key": "engraving", "value": "Ada"}},
		"tags":                             []any{"club-rider"},
		"totalPriceSet":                    euros("1299.00"),
		"subtotalPriceSet":                 euros("1199.00"),
		"taxLines":                         []any{map[string]any{"title": "VAT", "rate": 0.21, "priceSet": euros("210.00")}},
		"paymentGatewayNames":              []any{"bogus"},
		"lineItems": map[string]any{"nodes": []any{
			// A line of a product.
			map[string]any{
				"id": "gid://shopify/LineItem/" + n + "01", "title": "Road bicycle", "variantTitle": "54 cm",
				"name": "Road bicycle - 54 cm", "sku": "BIKE-ROAD-54", "quantity": 1,
				"product": map[string]any{"id": "gid://shopify/Product/41"}, "variant": map[string]any{"id": "gid://shopify/ProductVariant/4154"},
				"originalUnitPriceSet": euros("1199.00"),
				"customAttributes":     []any{map[string]any{"key": "engraving", "value": "Ada Example"}},
			},
			// A custom line: no product, and a title somebody typed.
			map[string]any{
				"id": "gid://shopify/LineItem/" + n + "02", "title": "Engraving: Ada", "variantTitle": "for Ada",
				"name": "Engraving: Ada - for Ada", "sku": "ENGRAVE-ADA", "quantity": 2,
				"originalUnitPriceSet": euros("20.00"),
			},
		}},
		"transactions": []any{map[string]any{
			"id": "gid://shopify/OrderTransaction/" + n + "01", "kind": "SALE", "status": "SUCCESS", "gateway": "bogus",
			"amountSet": euros("1299.00"), "accountNumber": "XXXX-4242", "processedAt": "2026-09-01T08:00:06Z",
			"paymentDetails": map[string]any{"__typename": "CardPaymentDetails", "company": "Example Card", "number": "XXXX-4242", "name": "Ada Example"},
			"receiptJson":    map[string]any{"payer": "Ada Example"},
			"order":          map[string]any{"id": gid},
		}},
		"fulfillments": []any{map[string]any{
			"id": "gid://shopify/Fulfillment/" + n + "01", "status": "SUCCESS", "name": "#" + n + "-F1", "totalQuantity": 1,
			"trackingInfo": []any{map[string]any{"company": "Example Post", "number": "EX123456789", "url": "https://tracking.example.com/EX123456789"}},
			"order":        map[string]any{"id": gid},
		}},
		"refunds": []any{map[string]any{
			"id": "gid://shopify/Refund/" + n + "01", "note": "Ada returned the saddle bag",
			"totalRefundedSet": euros("49.00"), "order": map[string]any{"id": gid},
		}},
	}
	if customer != "" {
		order["customer"] = map[string]any{"id": customer}
	}
	return order
}

func childGID(kind, n string) string { return "gid://shopify/" + kind + "/" + n + "01" }

var seen = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// A customer with an email address, a phone number, an image, tags,
// metafields and a note is erased: no version of the row holds any of them,
// and the row is still the row it was. The generated customer keeps the
// address and the number in defaultEmailAddress and defaultPhoneNumber, which
// the old field list did not name.
func TestAnErasedCustomersRowKeepsNothingButItsStructure(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	gid := eraCustomerGID("123")
	f.write("customer", adaCustomer("123", "2026-10-01T10:00:00Z"), seen)
	later := adaCustomer("123", "2026-10-02T10:00:00Z")
	later["note"] = "Ada would like a call before delivery, after six"
	f.write("customer", later, seen.Add(24*time.Hour))
	before := f.versions("customer", gid)
	if len(before) != 2 || !strings.Contains(fmt.Sprint(before[0]["defaultEmailAddress"]), adaEmail) {
		t.Fatalf("the fixture is %d version(s) %v, want two that hold her address", len(before), before)
	}

	res := f.erase(gid)

	after := f.versions("customer", gid)
	if len(after) != 2 {
		t.Fatalf("the erasure left %d version(s) of the row, want the two there were: it rewrites history, it does not delete it", len(after))
	}
	if res.Versions != 2 {
		t.Errorf("the erasure says it changed %d row version(s), want 2", res.Versions)
	}
	f.holdsNoWordOf("customer", gid, adaWords)
	for i, v := range after {
		for _, key := range []string{"storeId", "gid", "updatedAt", "versionSource", "syncedAt", "versionReadAt", "deleted"} {
			if fmt.Sprint(v[key]) != fmt.Sprint(before[i][key]) {
				t.Errorf("version %d: structural field %s was %v and is %v", i, key, before[i][key], v[key])
			}
		}
		for _, gone := range []string{"defaultEmailAddress", "defaultPhoneNumber", "defaultAddress", "image", "metafields", "amountSpent", "verifiedEmail", "taxExempt", "state"} {
			if value, present := v[gone]; present {
				t.Errorf("version %d keeps %s = %v; an object, a number, a boolean or an enum of her own row has its key removed", i, gone, value)
			}
		}
		// A string that is not free text loses its key too: a count the API
		// writes as a string, a date, a reference. A reader comparing dates
		// must never meet the marker.
		for _, gone := range []string{"numberOfOrders", "legacyResourceId", "lastOrderGid", "sourceCreatedAt"} {
			if value, present := v[gone]; present {
				t.Errorf("version %d keeps %s = %v; a date, a number or a reference of her own row has its key removed", i, gone, value)
			}
		}
		for _, marked := range []string{"firstName", "lastName", "displayName", "note"} {
			if v[marked] != RedactionMarker {
				t.Errorf("version %d: %s = %v, want %q", i, marked, v[marked], RedactionMarker)
			}
		}
		for _, emptied := range []string{"tags", "taxExemptions", "ordersGids"} {
			if list, ok := v[emptied].([]any); !ok || len(list) != 0 {
				t.Errorf("version %d: %s = %v, want the empty list", i, emptied, v[emptied])
			}
		}
	}
	// The row still reads through the engine, as the typed read it is.
	row := f.read("customer", gid)
	if row["gid"] != gid || row["defaultEmailAddress"] != nil || row["firstName"] != RedactionMarker {
		t.Errorf("the typed read answers %v", row)
	}
}

// Every mirror row records the account it belongs to, its store's, in
// accountId. The engine fills it in and the connector never states it, so it
// is in no write the mapper makes; it says whose row this is and nothing about
// a person. An erasure rebuilds each version from the fields its plan names,
// and a plan that named only what the connector writes would take every row
// it touched out of its account. The account is set here as the engine's own
// assignment of existing rows sets it: in place, on the stored row.
func TestAnErasureKeepsTheAccountARowBelongsTo(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("151"), eraOrderGID("5151")
	f.write("customer", adaCustomer("151", "2026-10-01T10:00:00Z"), seen)
	f.write("customer", adaCustomer("151", "2026-10-02T10:00:00Z"), seen.Add(24*time.Hour))
	f.write("order", bikeOrder("5151", customer, "2026-10-01T10:00:00Z"), seen)
	const account = "account-of-the-erasure-test"
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE "MemoryNodes" SET payload = payload || jsonb_build_object('accountId', $2::text) WHERE payload->>'storeId' = $1`,
		f.store, account); err != nil {
		t.Fatal(err)
	}

	res := f.erase(customer)

	if res.Versions == 0 {
		t.Fatalf("the erasure changed nothing: %+v", res)
	}
	f.holdsNoWordOf("customer", customer, adaWords)
	for _, row := range []struct{ concept, gid string }{{"customer", customer}, {"order", order}} {
		versions := f.versions(row.concept, row.gid)
		if len(versions) == 0 {
			t.Fatalf("the mirror holds no %s %s", row.concept, row.gid)
		}
		for i, v := range versions {
			if v["accountId"] != account {
				t.Errorf("%s version %d: accountId = %v after the erasure, want %q: the row left its account", row.concept, i, v["accountId"], account)
			}
		}
	}
}

// Customer 123 is erased. Customer 1234, whose id only begins the same, and an
// order of customer 1234, are untouched byte for byte: the old match was a
// substring of the row's text.
func TestErasingACustomerLeavesAnotherWhoseIdOnlyBeginsTheSame(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	f.write("customer", adaCustomer("123", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("5123", eraCustomerGID("123"), "2026-10-01T10:00:00Z"), seen)
	f.write("customer", adaCustomer("1234", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("51234", eraCustomerGID("1234"), "2026-10-01T10:00:00Z"), seen)

	others := map[string][2]string{
		"the other customer":      {"customer", eraCustomerGID("1234")},
		"the other's order":       {"order", eraOrderGID("51234")},
		"the other's line":        {"orderLineItem", childGID("LineItem", "51234")},
		"the other's transaction": {"orderTransaction", childGID("OrderTransaction", "51234")},
		"the other's fulfilment":  {"fulfillment", childGID("Fulfillment", "51234")},
		"the other's refund":      {"refund", childGID("Refund", "51234")},
	}
	before := map[string]string{}
	for name, row := range others {
		before[name] = strings.Join(f.texts(row[0], row[1]), "\n")
		if before[name] == "" {
			t.Fatalf("the fixture holds no row for %s", name)
		}
	}

	f.erase(eraCustomerGID("123"))

	for name, row := range others {
		if got := strings.Join(f.texts(row[0], row[1]), "\n"); got != before[name] {
			t.Errorf("%s was changed by the erasure of customer 123:\n  before: %s\n  after:  %s", name, before[name], got)
		}
	}
	// And the erasure did run: the erased customer's own order lost her.
	f.holdsNoWordOf("order", eraOrderGID("5123"), adaWords)
}

// An order of the erased customer keeps the sale and loses the person, and so
// does everything that hangs from it: the old scrub reached only rows that
// named the customer, and an order's transactions, fulfilments and refunds
// name the order.
func TestAnErasedCustomersOrderKeepsTheSaleAndLosesThePerson(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("201"), eraOrderGID("5201")
	f.write("customer", adaCustomer("201", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("5201", customer, "2026-10-01T10:00:00Z"), seen)

	f.erase(customer)

	o := f.newest("order", order)
	if total, _ := json.Marshal(o["totalPriceSet"]); !strings.Contains(string(total), `"1299.00"`) {
		t.Errorf("the order lost its total: %s", total)
	}
	if _, present := o["customerAcceptsMarketing"]; present {
		t.Errorf("the order keeps her consent to marketing: %v", o["customerAcceptsMarketing"])
	}
	if o["currencyCode"] != "EUR" || o["name"] != "#5201" || o["displayFinancialStatus"] != "PAID" || o["processedAt"] != "2026-09-01T08:00:05Z" || o["customerGid"] != customer {
		t.Errorf("the order lost a fact of the sale: currency %v, number %v, status %v, processed %v, customer %v",
			o["currencyCode"], o["name"], o["displayFinancialStatus"], o["processedAt"], o["customerGid"])
	}
	if taxes, _ := json.Marshal(o["taxLines"]); !strings.Contains(string(taxes), "VAT") {
		t.Errorf("the order lost its tax lines: %s", taxes)
	}
	for _, marked := range []string{"email", "phone", "note", "clientIp"} {
		if o[marked] != RedactionMarker {
			t.Errorf("the order keeps %s = %v", marked, o[marked])
		}
	}
	for _, gone := range []string{"billingAddress", "shippingAddress"} {
		if value, present := o[gone]; present {
			t.Errorf("the order keeps %s = %v", gone, value)
		}
	}
	if attrs, ok := o["customAttributes"].([]any); !ok || len(attrs) != 0 {
		t.Errorf("the order keeps its custom attributes: %v", o["customAttributes"])
	}

	line := f.newest("orderLineItem", childGID("LineItem", "5201"))
	if price, _ := json.Marshal(line["originalUnitPriceSet"]); line["title"] != "Road bicycle" || line["variantTitle"] != "54 cm" || line["name"] != "Road bicycle - 54 cm" ||
		line["sku"] != "BIKE-ROAD-54" || fmt.Sprint(line["quantity"]) != "1" || !strings.Contains(string(price), `"1199.00"`) {
		t.Errorf("the line of a product lost its title, quantity or price: %v", line)
	}
	// A custom line has no product, and its title is what somebody typed
	// (ruling 89): the three titles go, and the sale's figures stay.
	const customLine = "gid://shopify/LineItem/520102"
	custom := f.newest("orderLineItem", customLine)
	if custom["title"] != RedactionMarker || custom["variantTitle"] != RedactionMarker || custom["name"] != RedactionMarker || custom["sku"] != RedactionMarker {
		t.Errorf("the custom line keeps a title or its SKU: %v, %v, %v, %v", custom["title"], custom["variantTitle"], custom["name"], custom["sku"])
	}
	if price, _ := json.Marshal(custom["originalUnitPriceSet"]); fmt.Sprint(custom["quantity"]) != "2" || !strings.Contains(string(price), `"20.00"`) {
		t.Errorf("the custom line lost its quantity or price: %v", custom)
	}
	f.holdsNoWordOf("orderLineItem", customLine, adaWords)

	tx := f.newest("orderTransaction", childGID("OrderTransaction", "5201"))
	if amount, _ := json.Marshal(tx["amountSet"]); tx["kind"] != "SALE" || tx["status"] != "SUCCESS" || tx["gateway"] != "bogus" || !strings.Contains(string(amount), `"1299.00"`) {
		t.Errorf("the transaction lost its amount, kind, status or gateway: %v", tx)
	}
	if _, present := tx["paymentDetails"]; present || tx["accountNumber"] != RedactionMarker {
		t.Errorf("the transaction keeps its payment details or its account number: %v", tx)
	}
	if _, present := tx["receiptJson"]; present {
		t.Errorf("the transaction keeps its receipt: %v", tx["receiptJson"])
	}

	ful := f.newest("fulfillment", childGID("Fulfillment", "5201"))
	if tracking, ok := ful["trackingInfo"].([]any); !ok || len(tracking) != 0 || ful["status"] != "SUCCESS" {
		t.Errorf("the fulfilment keeps its tracking, or lost its status: %v", ful)
	}

	refund := f.newest("refund", childGID("Refund", "5201"))
	if amount, _ := json.Marshal(refund["totalRefundedSet"]); refund["note"] != RedactionMarker || !strings.Contains(string(amount), `"49.00"`) {
		t.Errorf("the refund keeps its note, or lost its amount: %v", refund)
	}

	for concept, gid := range map[string]string{
		"order": order, "orderLineItem": childGID("LineItem", "5201"), "orderTransaction": childGID("OrderTransaction", "5201"),
		"fulfillment": childGID("Fulfillment", "5201"), "refund": childGID("Refund", "5201"),
	} {
		f.holdsNoWordOf(concept, gid, adaWords)
	}
}

// An order named only by the request, with no customer on it, is scrubbed
// with its children; and so are the rows that name the order without hanging
// from it (ruling 83): a fulfilment order holds the delivery's name and
// address, a tender transaction the card.
func TestAnOrderNamedOnlyByTheRequestIsScrubbedWithEverythingOfIt(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	order := eraOrderGID("5301")
	f.write("order", bikeOrder("5301", "", "2026-10-01T10:00:00Z"), seen)
	const fulfilmentOrder, tender = "gid://shopify/FulfillmentOrder/7301", "gid://shopify/TenderTransaction/7302"
	f.write("fulfillmentOrder", map[string]any{
		"id": fulfilmentOrder, "updatedAt": "2026-10-01T10:00:00Z", "createdAt": "2026-09-01T08:00:10Z",
		"order": map[string]any{"id": order}, "orderName": "#5301", "status": "OPEN",
		"destination": map[string]any{"firstName": "Ada", "lastName": "Example", "address1": "1 Example Street", "city": "Testville", "email": adaEmail, "phone": adaPhone},
	}, seen)
	f.write("tenderTransaction", map[string]any{
		"id": tender, "processedAt": "2026-09-01T08:00:06Z", "order": map[string]any{"id": order},
		"amount": map[string]any{"amount": "1299.00", "currencyCode": "EUR"}, "paymentMethod": "credit_card", "remoteReference": "ch_Ada_1",
		"transactionDetails": map[string]any{"__typename": "TenderTransactionCreditCardDetails", "creditCardCompany": "Example Card", "creditCardNumber": "XXXX-4242"},
	}, seen)
	if o := f.newest("order", order); o["customerGid"] != "" {
		t.Fatalf("the fixture's order names a customer: %v", o["customerGid"])
	}

	// A customer the mirror holds nothing of: the request's own list of
	// orders is all that brings these rows in.
	f.erase(eraCustomerGID("309"), order)

	for concept, gid := range map[string]string{
		"order": order, "orderLineItem": childGID("LineItem", "5301"), "orderTransaction": childGID("OrderTransaction", "5301"),
		"fulfillment": childGID("Fulfillment", "5301"), "refund": childGID("Refund", "5301"),
		"fulfillmentOrder": fulfilmentOrder, "tenderTransaction": tender,
	} {
		f.holdsNoWordOf(concept, gid, adaWords)
	}
	fo := f.newest("fulfillmentOrder", fulfilmentOrder)
	if _, present := fo["destination"]; present || fo["orderName"] != "#5301" || fo["status"] != "OPEN" || fo["orderGid"] != order {
		t.Errorf("the fulfilment order keeps its destination, or lost the order it is of: %v", fo)
	}
	tt := f.newest("tenderTransaction", tender)
	if amount, _ := json.Marshal(tt["amount"]); tt["remoteReference"] != RedactionMarker || tt["paymentMethod"] != "credit_card" || !strings.Contains(string(amount), `"1299.00"`) {
		t.Errorf("the tender transaction keeps its reference, or lost its amount: %v", tt)
	}
	if _, present := tt["transactionDetails"]; present {
		t.Errorf("the tender transaction keeps the card: %v", tt["transactionDetails"])
	}
}

// After the erasure the mirror applies a later change to the same rows through
// the engine, and they read back as the rows they are. The value a scrubbed
// field is left with is what decides it: a null, or the string marker, in a
// field that is not a string made the engine refuse every later write.
func TestAfterAnErasureTheMirrorWritesTheRowsAgain(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("401"), eraOrderGID("5401")
	f.write("customer", adaCustomer("401", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("5401", customer, "2026-10-01T10:00:00Z"), seen)

	f.erase(customer)

	// The origin has erased her too. What it answers now is the sale, with
	// nothing where the person was: null for every field the mapper then
	// leaves out of its write (an object, a number, a boolean, an enum),
	// which is where the stored value is carried into the merge.
	afterOrder := map[string]any{
		"id": order, "updatedAt": "2026-11-01T10:00:00Z", "createdAt": "2026-09-01T08:00:00Z", "processedAt": "2026-09-01T08:00:05Z",
		"name": "#5401", "currencyCode": "EUR", "displayFinancialStatus": "REFUNDED",
		"totalPriceSet": euros("1299.00"), "customAttributes": []any{}, "tags": []any{},
		"transactions": []any{map[string]any{
			"id": childGID("OrderTransaction", "5401"), "kind": "SALE", "status": "SUCCESS", "gateway": "bogus",
			"amountSet": euros("1299.00"), "processedAt": "2026-09-01T08:00:06Z", "order": map[string]any{"id": order},
		}},
		"fulfillments": []any{map[string]any{
			"id": childGID("Fulfillment", "5401"), "status": "CANCELLED", "name": "#5401-F1", "order": map[string]any{"id": order},
		}},
	}
	afterCustomer := map[string]any{"id": customer, "updatedAt": "2026-11-01T10:00:00Z", "createdAt": "2025-01-05T09:00:00Z", "state": "DISABLED", "tags": []any{}}
	at := seen.Add(31 * 24 * time.Hour)
	for concept, obj := range map[string]map[string]any{"order": afterOrder, "customer": afterCustomer} {
		writes := mapObject(generated.Types[concept], f.store, obj, "", at)
		healed, failed := f.conn.heal(context.Background(), writes)
		if failed != 0 || healed != len(writes) {
			for _, w := range writes {
				if err := f.conn.writeMirror(context.Background(), w); err != nil {
					t.Errorf("after the erasure the mirror cannot write %s again: %v", w.Concept, err)
				}
			}
			t.Fatalf("after the erasure a later change to the %s healed %d of %d rows and failed %d", concept, healed, len(writes), failed)
		}
	}

	o := f.read("order", order)
	if o["displayFinancialStatus"] != "REFUNDED" || o["updatedAt"] != "2026-11-01T10:00:00Z" || o["billingAddress"] != nil || o["email"] != "" {
		t.Errorf("the order reads back as %v, want the later change applied and nothing of the person", o)
	}
	if ful := f.read("fulfillment", childGID("Fulfillment", "5401")); ful["status"] != "CANCELLED" {
		t.Errorf("the fulfilment reads back as %v, want the later change applied", ful)
	}
	if tx := f.read("orderTransaction", childGID("OrderTransaction", "5401")); tx["paymentDetails"] != nil || tx["kind"] != "SALE" {
		t.Errorf("the transaction reads back as %v", tx)
	}
	if c := f.read("customer", customer); c["state"] != "DISABLED" || c["defaultEmailAddress"] != nil || c["verifiedEmail"] != nil {
		t.Errorf("the customer reads back as %v, want the later change applied and nothing of the person", c)
	}
	for concept, gid := range map[string]string{"order": order, "customer": customer} {
		f.holdsNoWordOf(concept, gid, adaWords)
	}
}

// A row the OLD erasure touched holds the string marker in fields that are not
// strings, and the engine refuses every later write of it. The new erasure
// treats what the old one left as a value to scrub like any other, so the row
// is writable after it; and it takes what the old one missed.
func TestARowTheOldErasureTouchedIsWritableAfterTheNewOne(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("501"), eraOrderGID("5501")
	f.write("customer", adaCustomer("501", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("5501", customer, "2026-10-01T10:00:00Z"), seen)
	f.leftByTheOldErasure(customer)

	o := f.newest("order", order)
	if o["billingAddress"] != RedactionMarker || o["name"] != RedactionMarker {
		t.Fatalf("the fixture is not what the old erasure left: %v", o)
	}
	c := f.newest("customer", customer)
	if c["verifiedEmail"] != RedactionMarker || !strings.Contains(fmt.Sprint(c["defaultEmailAddress"]), adaEmail) {
		t.Fatalf("the fixture is not what the old erasure left: a marker in a boolean, and her address still in place: %v", c)
	}
	later := map[string]any{"id": order, "updatedAt": "2026-11-01T10:00:00Z", "name": "#5501", "displayFinancialStatus": "REFUNDED", "totalPriceSet": euros("1299.00")}
	laterWrites := mapObject(generated.Types["order"], f.store, later, "", seen.Add(31*24*time.Hour))
	if err := f.conn.writeMirror(context.Background(), laterWrites[0]); err == nil || !strings.Contains(err.Error(), "expected object, but got string") {
		t.Fatalf("the premise does not hold: a write over what the old erasure left answered %v, want the engine's refusal of the marker in an object", err)
	}

	f.erase(customer)

	if err := f.conn.writeMirror(context.Background(), laterWrites[0]); err != nil {
		t.Fatalf("after the new erasure the mirror still cannot write the order: %v", err)
	}
	if o := f.read("order", order); o["displayFinancialStatus"] != "REFUNDED" || o["billingAddress"] != nil {
		t.Errorf("the order reads back as %v", o)
	}
	laterCustomer := mapObject(generated.Types["customer"], f.store, map[string]any{"id": customer, "updatedAt": "2026-11-01T10:00:00Z", "state": "DISABLED"}, "", seen.Add(31*24*time.Hour))
	if err := f.conn.writeMirror(context.Background(), laterCustomer[0]); err != nil {
		t.Fatalf("after the new erasure the mirror still cannot write the customer: %v", err)
	}
	f.holdsNoWordOf("customer", customer, adaWords)
	f.holdsNoWordOf("order", order, adaWords)
}

// leftByTheOldErasure rewrites every version of every row that mentions the
// customer the way the erasure before this one did: the string marker in each
// field of its hand-written list, whatever the field's type, and nothing else.
func (f *erasureFixture) leftByTheOldErasure(customer string) {
	f.t.Helper()
	old := []string{
		"email", "phone", "firstName", "lastName", "displayName", "name", "note", "billingAddress", "shippingAddress",
		"defaultAddress", "addresses", "addressesV2", "customerLocale", "clientIp", "emailMarketingConsent",
		"smsMarketingConsent", "whatsAppMarketingConsent", "contactEmail", "verifiedEmail", "multipassIdentifier",
		"taxExemptions", "unsubscribeUrl", "customerJourneySummary",
	}
	expr := "payload"
	for _, field := range old {
		expr = fmt.Sprintf(`jsonb_set(%s, '{%s}', '"%s"'::jsonb, false)`, expr, field, RedactionMarker)
	}
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE "MemoryNodes" SET payload = `+expr+` WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = $1 AND payload::text LIKE '%' || $2 || '%'`,
		f.store, customer); err != nil {
		f.t.Fatal(err)
	}
}

// Every version of a row in scope is scrubbed: MemoryNodes is keyed (id,
// createdAt), and the versions before the newest are what a point-in-time
// read answers.
func TestEveryVersionOfARowInScopeIsScrubbed(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("601"), eraOrderGID("5601")
	f.write("order", bikeOrder("5601", customer, "2026-10-01T10:00:00Z"), seen)
	second := bikeOrder("5601", customer, "2026-10-02T10:00:00Z")
	second["note"] = "Ada asked for the afternoon"
	f.write("order", second, seen.Add(24*time.Hour))
	f.write("order", bikeOrder("5601", customer, "2026-10-03T10:00:00Z"), seen.Add(48*time.Hour))
	if before := f.versions("order", order); len(before) != 3 || before[2]["customerGid"] != customer {
		t.Fatalf("the fixture is %d version(s), want three that name her", len(before))
	}

	res := f.erase(customer)

	if after := f.versions("order", order); len(after) != 3 {
		t.Fatalf("the erasure left %d version(s), want the three there were", len(after))
	}
	f.holdsNoWordOf("order", order, adaWords)
	f.holdsNoWordOf("orderTransaction", childGID("OrderTransaction", "5601"), adaWords)
	if res.Versions < 3 {
		t.Errorf("the erasure says it changed %d row version(s), want at least the order's three", res.Versions)
	}
	// Made again, it changes nothing: every erasure is run twice.
	if again := f.erase(customer); again.Versions != 0 || again.Rows != res.Rows {
		t.Errorf("the erasure made again changed %d version(s) of %d row(s), want none of the same %d", again.Versions, again.Rows, res.Rows)
	}
}

// A row is decided on what it is NOW (ruling 94). An order that was the
// erased customer's and has since been moved to another person is that
// person's order: its newest version, and everything that hangs from it, is
// theirs and is left byte for byte. What was hers is the version from when it
// named her, and that version is scrubbed by itself. And her export does not
// hold the order: exported at its newest version, it would hand the other
// person's order to her.
func TestAnOrderThatMovedToAnotherPersonKeepsTheirStateAndLosesHers(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a, b, order := eraCustomerGID("611"), eraCustomerGID("612"), eraOrderGID("5611")
	f.write("customer", adaCustomer("611", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("5611", a, "2026-10-01T10:00:00Z"), seen)
	// The merchant moves the order to Bob.
	bobs := bikeOrder("5611", b, "2026-10-05T10:00:00Z")
	bobs["email"], bobs["note"] = "bob@example.com", "Bob collects it on Saturday"
	f.write("order", bobs, seen.Add(96*time.Hour))
	// An order the origin took her off and gave to nobody is decided the same
	// way: it names no customer now.
	f.write("order", bikeOrder("5612", a, "2026-10-01T10:00:00Z"), seen)
	guest := bikeOrder("5612", "", "2026-10-05T10:00:00Z")
	guest["customer"] = nil
	f.write("order", guest, seen.Add(96*time.Hour))

	versions := f.texts("order", order)
	if len(versions) != 2 || !strings.Contains(versions[0], a) || !strings.Contains(versions[1], "bob@example.com") || strings.Contains(versions[1], a) {
		t.Fatalf("the fixture's order is %d version(s), want the first hers and the second Bob's", len(versions))
	}
	before := f.allTexts()
	transaction := "orderTransaction " + childGID("OrderTransaction", "5611")
	if before[transaction] == "" {
		t.Fatal("the fixture's order has no transaction")
	}
	export, err := f.conn.ExportCustomerData(context.Background(), Store{ID: f.store, Domain: "bicycles.example.com"}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(export["rows"]); strings.Contains(string(raw), order) || strings.Contains(string(raw), "bob@example.com") || strings.Contains(string(raw), eraOrderGID("5612")) {
		t.Errorf("her export holds an order that is no longer hers: %s", raw)
	} else if !strings.Contains(string(raw), adaEmail) {
		t.Fatalf("her export does not hold her own row: %s", raw)
	}

	res := f.erase(a)

	after := f.texts("order", order)
	if after[1] != versions[1] {
		t.Errorf("the order's newest version is Bob's and was changed by her erasure:\n  before: %s\n  after:  %s", versions[1], after[1])
	}
	earlier := f.versions("order", order)[0]
	if earlier["email"] != RedactionMarker || earlier["note"] != RedactionMarker || earlier["customerGid"] != a || earlier["currencyCode"] != "EUR" {
		t.Errorf("the version from when the order was hers is %v, want it scrubbed as an order of hers is: the person gone, the sale kept", earlier)
	}
	if _, present := earlier["billingAddress"]; present {
		t.Errorf("the version from when the order was hers keeps her billing address: %v", earlier["billingAddress"])
	}
	for key, text := range f.allTexts() {
		if strings.HasPrefix(key, "order ") || strings.HasPrefix(key, "customer ") {
			continue
		}
		if text != before[key] {
			t.Errorf("%s hangs from an order that is no longer hers and was changed:\n  before: %s\n  after:  %s", key, before[key], text)
		}
	}
	if res.Earlier != 2 {
		t.Errorf("the erasure says it scrubbed %d earlier version(s) of rows no longer hers, want the two orders' one each", res.Earlier)
	}
	// The order nobody holds now: its newest version is left, its older one scrubbed.
	nobodys := f.versions("order", eraOrderGID("5612"))
	if nobodys[0]["email"] != RedactionMarker || nobodys[1]["email"] != adaEmail {
		t.Errorf("an order the origin took her off reads %v then %v, want its older version scrubbed and its newest left", nobodys[0]["email"], nobodys[1]["email"])
	}
	// Made again, it changes nothing.
	if again := f.erase(a); again.Versions != 0 || again.Earlier != 2 {
		t.Errorf("the erasure made again changed %d version(s) and found %d earlier one(s), want none changed of the same two", again.Versions, again.Earlier)
	}
}

// The data request exports exactly the rows the erasure would scrub: one
// function decides both, so an export and an erasure cannot disagree about
// what MemQL holds on a person.
func TestTheDataRequestExportsExactlyTheRowsTheErasureScrubs(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("701"), eraOrderGID("5701")
	f.write("customer", adaCustomer("701", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("5701", customer, "2026-10-01T10:00:00Z"), seen)
	f.write("fulfillmentOrder", map[string]any{
		"id": "gid://shopify/FulfillmentOrder/7701", "updatedAt": "2026-10-01T10:00:00Z", "order": map[string]any{"id": order}, "status": "OPEN",
		"destination": map[string]any{"firstName": "Ada", "email": adaEmail},
	}, seen)
	// Somebody else, and somebody else's order.
	f.write("customer", adaCustomer("7019", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("57019", eraCustomerGID("7019"), "2026-10-01T10:00:00Z"), seen)

	all := f.allTexts()
	export, err := f.conn.ExportCustomerData(context.Background(), Store{ID: f.store, Domain: "bicycles.example.com"}, customer, nil)
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	rows, _ := export["rows"].(map[string]any)
	for concept, list := range rows {
		items, _ := list.([]map[string]any)
		for _, row := range items {
			exported[concept+" "+fmt.Sprint(row["gid"])] = true
		}
	}
	want := []string{
		"customer " + customer, "order " + order, "orderLineItem " + childGID("LineItem", "5701"), "orderLineItem gid://shopify/LineItem/570102",
		"orderTransaction " + childGID("OrderTransaction", "5701"), "fulfillment " + childGID("Fulfillment", "5701"),
		"refund " + childGID("Refund", "5701"), "fulfillmentOrder gid://shopify/FulfillmentOrder/7701",
	}
	if got := sortedSet(exported); strings.Join(got, "\n") != strings.Join(sortedStrings(want), "\n") {
		t.Fatalf("the export holds\n  %s\nwant her row, her order and everything of it, and nobody else's:\n  %s", strings.Join(got, "\n  "), strings.Join(sortedStrings(want), "\n  "))
	}
	if export["customerGid"] != customer || export["apiVersion"] != generated.APIVersion || export["store"] != "bicycles.example.com" {
		t.Errorf("the export's header is %v", export)
	}
	// The export is what MemQL holds, not what an erasure would leave.
	if raw, _ := json.Marshal(rows); !strings.Contains(string(raw), adaEmail) || !strings.Contains(string(raw), "EX123456789") {
		t.Errorf("the export does not hold what the rows hold: %s", raw)
	}

	// And those are the rows the erasure changes: no more, no fewer.
	f.erase(customer)
	changed := map[string]bool{}
	for key, text := range f.allTexts() {
		if text != all[key] {
			changed[key] = true
		}
	}
	if got := sortedSet(changed); strings.Join(got, "\n") != strings.Join(sortedSet(exported), "\n") {
		t.Errorf("the erasure changed\n  %s\nand the export held\n  %s", strings.Join(got, "\n  "), strings.Join(sortedSet(exported), "\n  "))
	}
}

// allTexts is every mirror row of the fixture's store, as "concept gid", with
// all its versions' text. (A privacy job of the store is a row of the store
// too, and is not a mirror row: it carries no gid.)
func (f *erasureFixture) allTexts() map[string]string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT concept, payload->>'gid', payload::text FROM "MemoryNodes"
		  WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = $1 AND payload->>'gid' IS NOT NULL
		  ORDER BY concept, id, "createdAt"`, f.store)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var concept, gid, text string
		if err := rows.Scan(&concept, &gid, &text); err != nil {
			f.t.Fatal(err)
		}
		out[generated.ConceptFromID(concept)+" "+gid] += text + "\n"
	}
	return out
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// The scope grows by three links and no other (ruling 83).
func TestTheScopeOfAnErasureGrowsByItsThreeLinksAndNoOther(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer, order := eraCustomerGID("801"), eraOrderGID("5801")
	// 1. A parent link, at every depth: the order's return, the return's
	// reverse fulfilment order, and that one's delivery.
	withReturn := bikeOrder("5801", customer, "2026-10-01T10:00:00Z")
	withReturn["returns"] = map[string]any{"nodes": []any{map[string]any{
		"id": "gid://shopify/Return/8101", "createdAt": "2026-09-10T08:00:00Z", "name": "#5801-R1", "status": "OPEN",
		"order":   map[string]any{"id": order},
		"decline": map[string]any{"note": "Ada changed her mind", "reason": "OTHER"},
		"reverseFulfillmentOrders": map[string]any{"nodes": []any{map[string]any{
			"id": "gid://shopify/ReverseFulfillmentOrder/8102", "status": "OPEN",
			"thirdPartyConfirmation": map[string]any{"status": "ACCEPTED", "note": "Ada"},
			"reverseDeliveries": map[string]any{"nodes": []any{map[string]any{
				"id": "gid://shopify/ReverseDelivery/8103", "deliverable": map[string]any{"__typename": "ReverseDeliveryShippingDeliverable", "label": "Ada"},
			}}},
		}}},
	}}}
	f.write("order", withReturn, seen)
	// 2. The customer's gid inside an object: a draft order that names her
	// only as its purchasing entity.
	const draft = "gid://shopify/DraftOrder/8201"
	f.write("draftOrder", map[string]any{
		"id": draft, "updatedAt": "2026-10-01T10:00:00Z", "name": "#D8201", "email": adaEmail, "status": "OPEN",
		"purchasingEntity": map[string]any{"__typename": "Customer", "id": customer},
		"totalPriceSet":    euros("15.00"),
	}, seen)
	// NOT a link either: an object that only REFERS to her. A metaobject one
	// of whose fields names her is the merchant's; the entry goes and the
	// object stays.
	const testimonial = "gid://shopify/Metaobject/8251"
	f.write("metaobject", map[string]any{
		"id": testimonial, "updatedAt": "2026-10-01T10:00:00Z", "displayName": "A rider's word, by Ada", "handle": "a-riders-word", "type": "testimonial",
		"fields": []any{
			map[string]any{"key": "stars", "value": "5"},
			map[string]any{"key": "rider", "value": customer, "reference": map[string]any{"__typename": "Customer", "id": customer}},
		},
		"thumbnailField": map[string]any{"key": "rider", "value": customer, "reference": map[string]any{"__typename": "Customer", "id": customer}},
	}, seen)
	// A metaobject that names somebody else is nobody's business here.
	const another = "gid://shopify/Metaobject/8252"
	f.write("metaobject", map[string]any{
		"id": another, "updatedAt": "2026-10-01T10:00:00Z", "displayName": "A rider's word, by Bob", "handle": "another-riders-word", "type": "testimonial",
		"fields":         []any{map[string]any{"key": "rider", "value": eraCustomerGID("8209"), "reference": map[string]any{"__typename": "Customer", "id": eraCustomerGID("8209")}}},
		"thumbnailField": map[string]any{"key": "rider", "value": eraCustomerGID("8209")},
	}, seen)
	anotherBefore := f.allTexts()["metaobject "+another]
	// 3. NOT a link: a list of references. A row that lists her ORDER among
	// its references is left alone.
	const clubOfOrders = "gid://shopify/Company/8302"
	f.write("company", map[string]any{
		"id": clubOfOrders, "updatedAt": "2026-10-01T10:00:00Z", "name": "Example Touring Club",
		"orders": map[string]any{"nodes": []any{map[string]any{"id": order}}},
	}, seen)
	// 4. NOT a link: a metafield that names her. It is a reference a merchant
	// wrote on a product, and the product is not about her.
	const product = "gid://shopify/Product/8401"
	f.write("product", map[string]any{
		"id": product, "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle",
		"metafields": map[string]any{"nodes": []any{map[string]any{"namespace": "custom", "key": "first_buyer", "type": "customer_reference", "value": customer}}},
	}, seen)
	untouched := map[string]string{"company " + clubOfOrders: "", "product " + product: ""}
	all := f.allTexts()
	for key := range untouched {
		untouched[key] = all[key]
	}

	res := f.erase(customer)

	ret := f.newest("shopifyReturn", "gid://shopify/Return/8101")
	if _, present := ret["decline"]; present || ret["name"] != "#5801-R1" {
		t.Errorf("the return keeps why it was declined, or lost its number: %v", ret)
	}
	if rfo := f.newest("reverseFulfillmentOrder", "gid://shopify/ReverseFulfillmentOrder/8102"); rfo["thirdPartyConfirmation"] != nil || rfo["status"] != "OPEN" {
		t.Errorf("two levels under the order, the reverse fulfilment order is %v", rfo)
	}
	if rd := f.newest("reverseDelivery", "gid://shopify/ReverseDelivery/8103"); rd["deliverable"] != nil {
		t.Errorf("three levels under the order, the reverse delivery is %v", rd)
	}
	if d := f.newest("draftOrder", draft); d["email"] != RedactionMarker || d["purchasingEntity"] != nil || d["name"] != "#D8201" {
		t.Errorf("the draft order that names her inside an object is %v", d)
	}
	m := f.newest("metaobject", testimonial)
	if fields, _ := json.Marshal(m["fields"]); m["displayName"] != "A rider's word, by Ada" || m["handle"] != "a-riders-word" ||
		string(fields) != `[{"key": "stars", "value": "5"}]` && string(fields) != `[{"key":"stars","value":"5"}]` {
		t.Errorf("the metaobject that refers to her is %v, want the entry that names her taken out and nothing else changed", m)
	}
	if value, present := m["thumbnailField"]; present {
		t.Errorf("the metaobject still shows her as its thumbnail: %v", value)
	}
	if res.Unlisted != 1 {
		t.Errorf("the erasure says it took her out of %d row version(s), want the metaobject's one", res.Unlisted)
	}
	if after := f.allTexts()["metaobject "+another]; after != anotherBefore || anotherBefore == "" {
		t.Errorf("a metaobject that names another customer was changed:\n  before: %s\n  after:  %s", anotherBefore, after)
	}
	now := f.allTexts()
	for key, before := range untouched {
		if now[key] != before {
			t.Errorf("%s is not in scope and was changed:\n  before: %s\n  after:  %s", key, before, now[key])
		}
	}
}

// A company is not scrubbed because one of its contacts was (ruling 91). The
// company names its main contact, and its contacts and locations name the
// company; none of those is a link. The erased person's own contact row is
// hers and is scrubbed. The company, her colleagues and the company's
// locations are untouched byte for byte.
func TestTheErasureOfACompanysMainContactLeavesTheCompany(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	customer := eraCustomerGID("901")
	const company, hers, colleague, location = "gid://shopify/Company/9100", "gid://shopify/CompanyContact/9101", "gid://shopify/CompanyContact/9102", "gid://shopify/CompanyLocation/9103"
	f.write("company", map[string]any{
		"id": company, "updatedAt": "2026-10-01T10:00:00Z", "name": "Example Cycling Club", "mainContact": map[string]any{"id": hers},
		"contacts": map[string]any{"nodes": []any{
			map[string]any{"id": hers, "updatedAt": "2026-10-01T10:00:00Z", "title": "Club secretary", "customer": map[string]any{"id": customer}, "company": map[string]any{"id": company}},
			map[string]any{"id": colleague, "updatedAt": "2026-10-01T10:00:00Z", "title": "Treasurer", "customer": map[string]any{"id": eraCustomerGID("902")}, "company": map[string]any{"id": company}},
		}},
		"locations": map[string]any{"nodes": []any{
			map[string]any{"id": location, "updatedAt": "2026-10-01T10:00:00Z", "name": "The clubhouse", "company": map[string]any{"id": company}},
		}},
	}, seen)
	before := f.allTexts()
	for _, key := range []string{"company " + company, "companyContact " + colleague, "companyLocation " + location} {
		if before[key] == "" {
			t.Fatalf("the fixture holds no row %s", key)
		}
	}

	f.erase(customer)

	if got := f.newest("companyContact", hers); got["title"] != RedactionMarker || got["customerGid"] != customer {
		t.Errorf("her own contact row is %v, want it scrubbed", got)
	}
	after := f.allTexts()
	for _, key := range []string{"company " + company, "companyContact " + colleague, "companyLocation " + location} {
		if after[key] != before[key] {
			t.Errorf("%s was changed by the erasure of the company's main contact:\n  before: %s\n  after:  %s", key, before[key], after[key])
		}
	}
}

// What the erasure of A must never reach: another person's rows (ruling 91).
// With every gid field a link, customer B, whose row once named A's order as
// his last order, was scrubbed with her and exported to her; with her gid
// matched by value anywhere, an order of customer C that carried it in a
// custom attribute was scrubbed with its transaction, and exported to her.
// One person's data handed to another is the worst thing this code can do.
func TestAnErasureNeverReachesAnotherPersonsRows(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a, orderA := eraCustomerGID("9001"), eraOrderGID("59001")
	b := eraCustomerGID("9002")
	c, orderC := eraCustomerGID("9003"), eraOrderGID("59003")
	f.write("customer", adaCustomer("9001", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("59001", a, "2026-10-01T10:00:00Z"), seen)

	sample := func(n, first, updatedAt string) map[string]any {
		row := adaCustomer(n, updatedAt)
		row["firstName"], row["lastName"], row["displayName"] = first, "Sample", first+" Sample"
		row["defaultEmailAddress"] = map[string]any{"emailAddress": strings.ToLower(first) + "@example.com"}
		row["lastOrder"], row["orders"] = nil, nil
		return row
	}
	// B: a live customer whose row named A's order as his last order (the
	// order was once his and was moved to her), and no longer does.
	bob := sample("9002", "Bob", "2026-10-01T10:00:00Z")
	bob["lastOrder"] = map[string]any{"id": orderA}
	f.write("customer", bob, seen)
	f.write("customer", sample("9002", "Bob", "2026-10-05T10:00:00Z"), seen.Add(96*time.Hour))
	// C: her order carries A's gid as the VALUE of a custom attribute.
	f.write("customer", sample("9003", "Cara", "2026-10-01T10:00:00Z"), seen)
	carasOrder := bikeOrder("59003", c, "2026-10-01T10:00:00Z")
	carasOrder["email"] = "cara@example.com"
	carasOrder["customAttributes"] = []any{map[string]any{"key": "referred_by", "value": a}}
	f.write("order", carasOrder, seen)
	// A gift card A bought (on her order, on her line) that C holds.
	const card = "gid://shopify/GiftCard/9004"
	f.write("giftCard", map[string]any{
		"id": card, "updatedAt": "2026-10-01T10:00:00Z", "note": "a present for Cara, from Ada", "lastCharacters": "c4r4",
		"customer": map[string]any{"id": c}, "order": map[string]any{"id": orderA}, "lineItem": map[string]any{"id": childGID("LineItem", "59001")},
		"recipientAttributes": map[string]any{"message": "ride well", "recipient": map[string]any{"id": c}},
		"balance":             map[string]any{"amount": "50.00", "currencyCode": "EUR"},
	}, seen)

	theirs := []string{
		"customer " + b, "customer " + c, "order " + orderC, "orderLineItem " + childGID("LineItem", "59003"),
		"orderTransaction " + childGID("OrderTransaction", "59003"), "fulfillment " + childGID("Fulfillment", "59003"),
		"refund " + childGID("Refund", "59003"), "giftCard " + card,
	}
	before := f.allTexts()
	for _, key := range theirs {
		if before[key] == "" {
			t.Fatalf("the fixture holds no row %s", key)
		}
	}

	// Her export holds nothing of theirs.
	export, err := f.conn.ExportCustomerData(context.Background(), Store{ID: f.store, Domain: "bicycles.example.com"}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := export["rows"].(map[string]any)
	raw, _ := json.Marshal(rows)
	for _, whose := range []string{"bob@example.com", "cara@example.com", "Bob Sample", "Cara Sample", "ride well", b, orderC, card} {
		if strings.Contains(string(raw), whose) {
			t.Errorf("A's export holds %q, which is another person's", whose)
		}
	}
	if !strings.Contains(string(raw), adaEmail) || !strings.Contains(string(raw), orderA) {
		t.Fatalf("A's export does not hold her own rows: %s", raw)
	}

	f.erase(a)

	after := f.allTexts()
	for _, key := range theirs {
		if after[key] != before[key] {
			t.Errorf("%s is another person's and was changed by A's erasure:\n  before: %s\n  after:  %s", key, before[key], after[key])
		}
	}
	// And A's own were erased.
	f.holdsNoWordOf("customer", a, adaWords)
	f.holdsNoWordOf("order", orderA, adaWords)
}

// The customer's gid inside an object makes the row hers only where the object
// says who the row is FOR, and only as an id: an order's purchasing entity, a
// gift card's recipient. A value that happens to spell her gid, in the same
// object, does not: a message is words.
func TestTheCustomerInsideAnObjectIsALinkOnlyAsTheOneTheRowIsFor(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a, c := eraCustomerGID("9301"), eraCustomerGID("9303")
	f.write("customer", adaCustomer("9301", "2026-10-01T10:00:00Z"), seen)
	// An order with no customer on it, bought by her as its purchasing entity.
	bought := bikeOrder("59301", "", "2026-10-01T10:00:00Z")
	bought["customer"] = nil
	bought["purchasingEntity"] = map[string]any{"__typename": "Customer", "id": a}
	f.write("order", bought, seen)
	// A gift card sent to her, which no customer field names her on.
	const sentToHer, namesHerInWords = "gid://shopify/GiftCard/9305", "gid://shopify/GiftCard/9306"
	f.write("giftCard", map[string]any{
		"id": sentToHer, "updatedAt": "2026-10-01T10:00:00Z", "note": "for Ada", "lastCharacters": "ad4a",
		"recipientAttributes": map[string]any{"message": "happy birthday, Ada", "preferredName": "Ada", "recipient": map[string]any{"id": a}},
		"balance":             map[string]any{"amount": "50.00", "currencyCode": "EUR"},
	}, seen)
	// A gift card that is Cara's, whose message is the erased customer's gid
	// and nothing else: a free value, in the very object a link is read from.
	f.write("giftCard", map[string]any{
		"id": namesHerInWords, "updatedAt": "2026-10-01T10:00:00Z", "note": "for Cara", "lastCharacters": "c4r4",
		"customer":            map[string]any{"id": c},
		"recipientAttributes": map[string]any{"message": a, "preferredName": "Cara", "recipient": map[string]any{"id": c}},
		"balance":             map[string]any{"amount": "25.00", "currencyCode": "EUR"},
	}, seen)
	before := f.allTexts()["giftCard "+namesHerInWords]
	if !strings.Contains(before, `"message": "`+a+`"`) {
		t.Fatalf("the fixture's card does not carry her gid as its message: %s", before)
	}

	f.erase(a)

	f.holdsNoWordOf("order", eraOrderGID("59301"), adaWords)
	if o := f.newest("order", eraOrderGID("59301")); o["currencyCode"] != "EUR" {
		t.Errorf("the order she bought as its purchasing entity lost the facts of the sale: %v", o)
	}
	f.holdsNoWordOf("giftCard", sentToHer, []string{"Ada", "happy birthday"})
	if card := f.newest("giftCard", sentToHer); card["note"] != RedactionMarker {
		t.Errorf("the card sent to her is %v, want it scrubbed", card)
	}
	if after := f.allTexts()["giftCard "+namesHerInWords]; after != before {
		t.Errorf("a card that is another person's, whose message spells the erased customer's gid, was changed:\n  before: %s\n  after:  %s", before, after)
	}
}

// A customer merged into the erased one is the same person's earlier record:
// the origin deleted it and moved its orders to her. Its row is a tombstone
// whose last order is in scope, and that is the one case in which a
// customer's last order is a link. A LIVE customer who names the same order
// is another person (the test above).
func TestACustomerMergedIntoTheErasedOneIsErasedWithHer(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a, order := eraCustomerGID("9101"), eraOrderGID("59101")
	earlier := eraCustomerGID("9102")
	f.write("customer", adaCustomer("9101", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("59101", a, "2026-10-01T10:00:00Z"), seen)
	// Her earlier record: its last order is the order that is hers now, and
	// the origin deleted it when the two were merged.
	old := adaCustomer("9102", "2026-09-01T10:00:00Z")
	old["lastOrder"] = map[string]any{"id": order}
	old["orders"] = nil
	f.write("customer", old, seen.Add(-24*time.Hour))
	if err := f.conn.writeMirror(context.Background(), tombstone("customer", f.store, earlier, seen.Add(time.Hour))); err != nil {
		t.Fatalf("retire the merged customer: %v", err)
	}
	if v := f.newest("customer", earlier); v["deleted"] != true || v["lastOrderGid"] != order || !strings.Contains(fmt.Sprint(v["defaultEmailAddress"]), adaEmail) {
		t.Fatalf("the fixture's merged customer is %v, want a tombstone that still holds her and names the order", v)
	}

	f.erase(a)

	f.holdsNoWordOf("customer", earlier, adaWords)
	for i, v := range f.versions("customer", earlier) {
		if _, present := v["defaultEmailAddress"]; present {
			t.Errorf("version %d of the merged record keeps her address", i)
		}
		if _, present := v["numberOfOrders"]; present {
			t.Errorf("version %d of the merged record keeps her count of orders: it is the person's own row and keeps nothing", i)
		}
	}
	if v := f.newest("customer", earlier); v["deleted"] != true || v["gid"] != earlier {
		t.Errorf("the merged record lost its structure: %v", v)
	}
}

// The same gid in another store is another store's row. A row's id is made of
// its store and its gid, and every statement of the scope names the store.
func TestAnErasureIsOfOneStore(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	other := &erasureFixture{t: t, eng: f.eng, db: f.db, conn: f.conn, store: f.store + "-b"}
	clean := func() {
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = $1`, other.store)
	}
	clean()
	t.Cleanup(clean)
	customer := eraCustomerGID("9201")
	for _, fx := range []*erasureFixture{f, other} {
		fx.write("customer", adaCustomer("9201", "2026-10-01T10:00:00Z"), seen)
		fx.write("order", bikeOrder("59201", customer, "2026-10-01T10:00:00Z"), seen)
	}
	before := other.allTexts()
	if len(before) < 2 {
		t.Fatalf("the other store's fixture holds %d rows", len(before))
	}

	f.erase(customer, eraOrderGID("59201"))

	f.holdsNoWordOf("customer", customer, adaWords)
	for key, text := range other.allTexts() {
		if text != before[key] {
			t.Errorf("%s of the other store was changed by this store's erasure", key)
		}
	}
}

// An order the request names is hers to erase only if it is hers, or
// nobody's, NOW (ruling 98). A request is compiled when its orders were the
// customer's, and a job keeps the orders of every request it was ever asked;
// one of them since moved to another person is that person's order. Named, it
// used to be a seed whoever it named: it was scrubbed whole with its
// children, and the other person's email and street were in her export.
func TestAnOrderTheRequestNamesThatIsAnotherPersonsNowIsNotHers(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a, b, order := eraCustomerGID("9811"), eraCustomerGID("9812"), eraOrderGID("59811")
	f.write("customer", adaCustomer("9811", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("59811", a, "2026-10-01T10:00:00Z"), seen)
	bobs := bikeOrder("59811", b, "2026-10-05T10:00:00Z")
	bobs["email"], bobs["note"] = "bob@example.com", "Bob collects it on Saturday"
	bobs["billingAddress"] = map[string]any{"firstName": "Bob", "lastName": "Sample", "address1": "2 Sample Road", "city": "Sampleton"}
	f.write("order", bobs, seen.Add(96*time.Hour))
	// An order the request names that names nobody now is still a seed.
	guest := bikeOrder("59812", "", "2026-10-01T10:00:00Z")
	guest["customer"] = nil
	f.write("order", guest, seen)
	versions := f.texts("order", order)
	before := f.allTexts()

	export, err := f.conn.ExportCustomerData(context.Background(), Store{ID: f.store, Domain: "bicycles.example.com"}, a, []string{order, eraOrderGID("59812")})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(export["rows"])
	for _, theirs := range []string{"bob@example.com", "2 Sample Road", order} {
		if strings.Contains(string(raw), theirs) {
			t.Errorf("her export holds %q: the request names an order that is another person's now", theirs)
		}
	}
	if !strings.Contains(string(raw), eraOrderGID("59812")) {
		t.Errorf("her export does not hold the order the request names that names nobody now")
	}

	f.erase(a, order, eraOrderGID("59812"))

	after := f.texts("order", order)
	if after[len(after)-1] != versions[len(versions)-1] {
		t.Errorf("the named order's newest version is Bob's and was changed:\n  before: %s\n  after:  %s", versions[len(versions)-1], after[len(after)-1])
	}
	if earlier := f.versions("order", order)[0]; earlier["email"] != RedactionMarker || earlier["customerGid"] != a {
		t.Errorf("the version from when the named order was hers is %v, want it scrubbed by itself", earlier)
	}
	for key, text := range f.allTexts() {
		if strings.HasPrefix(key, "customer ") || key == "order "+order || strings.Contains(key, "59812") {
			continue
		}
		if text != before[key] {
			t.Errorf("%s hangs from an order that is another person's now and was changed", key)
		}
	}
	f.holdsNoWordOf("order", eraOrderGID("59812"), adaWords)
}

// A deleted customer whose last order is in scope is scrubbed BY ITSELF
// (ruling 98). The link cannot tell a record merged into her from another
// person the merchant deleted, whose row still points at an order that has
// since become hers. So the row is scrubbed, and nothing is reached from it:
// not his orders, not their children, not a row that names him; and it is
// never in her export. He, his own order with its line items, transaction,
// fulfilment and refund used to be scrubbed, and all of it exported to her.
func TestADeletedCustomerWhoseLastOrderIsHersIsScrubbedByItself(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a, order := eraCustomerGID("9821"), eraOrderGID("59821")
	b, c := eraCustomerGID("9822"), eraCustomerGID("9823")
	f.write("customer", adaCustomer("9821", "2026-10-01T10:00:00Z"), seen)
	f.write("order", bikeOrder("59821", a, "2026-10-01T10:00:00Z"), seen)
	sample := func(n, first, last string) map[string]any {
		row := adaCustomer(n, "2026-09-01T10:00:00Z")
		row["firstName"], row["lastName"], row["displayName"] = first, "Sample", first+" Sample"
		row["defaultEmailAddress"] = map[string]any{"emailAddress": strings.ToLower(first) + "@example.com"}
		row["orders"], row["lastOrder"] = nil, map[string]any{"id": last}
		return row
	}
	// Bob: the order was his and was moved to her; his row was never read
	// again, and the merchant then deleted him.
	f.write("customer", sample("9822", "Bob", order), seen.Add(-24*time.Hour))
	if err := f.conn.writeMirror(context.Background(), tombstone("customer", f.store, b, seen.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	// An order the mirror still holds as Bob's own.
	bobsOwn := bikeOrder("59850", b, "2026-09-01T10:00:00Z")
	bobsOwn["email"], bobsOwn["note"] = "bob@example.com", "Bob's own order"
	f.write("order", bobsOwn, seen.Add(-24*time.Hour))
	// Cara: deleted, and her last order is somebody else's.
	f.write("customer", sample("9823", "Cara", eraOrderGID("59899")), seen.Add(-24*time.Hour))
	if err := f.conn.writeMirror(context.Background(), tombstone("customer", f.store, c, seen.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	before := f.allTexts()

	export, err := f.conn.ExportCustomerData(context.Background(), Store{ID: f.store, Domain: "bicycles.example.com"}, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(export["rows"])
	for _, his := range []string{"bob@example.com", "Bob Sample", b, eraOrderGID("59850"), "Bob's own order"} {
		if strings.Contains(string(raw), his) {
			t.Errorf("her export holds %q, which is a deleted customer's whose last order is hers now", his)
		}
	}

	res := f.erase(a)

	// His row is scrubbed, by itself.
	f.holdsNoWordOf("customer", b, []string{"bob@example.com", "Bob", "Sample"})
	if v := f.newest("customer", b); v["deleted"] != true || v["gid"] != b {
		t.Errorf("the deleted customer's row lost its structure: %v", v)
	}
	if res.Alone != 1 {
		t.Errorf("the erasure says it scrubbed %d deleted customer record(s) by themselves, want the one", res.Alone)
	}
	// Nothing is reached from it, and nobody else is touched.
	for key, text := range f.allTexts() {
		if key == "customer "+a || key == "customer "+b || strings.Contains(key, "59821") {
			continue
		}
		if text != before[key] {
			t.Errorf("%s was changed by her erasure: it is reached only through a deleted customer whose last order is hers", key)
		}
	}
}

// A metaobject keeps what its fields hold a second time, in one value by
// key (values). An entry that names her is taken out there as well, and the
// other keys stay; and an entry that names her in an EARLIER version of the
// metaobject only is taken out of that version.
func TestAMetaobjectLosesHerInItsValuesAsWell(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	a := eraCustomerGID("9831")
	f.write("customer", adaCustomer("9831", "2026-10-01T10:00:00Z"), seen)
	const earlier, both = "gid://shopify/Metaobject/9841", "gid://shopify/Metaobject/9842"
	entry := map[string]any{"key": "rider", "value": a, "jsonValue": a, "reference": map[string]any{"__typename": "Customer", "id": a}}
	f.write("metaobject", map[string]any{
		"id": earlier, "updatedAt": "2026-10-01T10:00:00Z", "displayName": "A rider's word", "handle": "earlier", "type": "testimonial",
		"fields": []any{map[string]any{"key": "stars", "value": "5"}, entry},
	}, seen)
	f.write("metaobject", map[string]any{
		"id": earlier, "updatedAt": "2026-10-05T10:00:00Z", "displayName": "A rider's word", "handle": "earlier", "type": "testimonial",
		"fields": []any{map[string]any{"key": "stars", "value": "4"}},
	}, seen.Add(96*time.Hour))
	f.write("metaobject", map[string]any{
		"id": both, "updatedAt": "2026-10-01T10:00:00Z", "displayName": "Another word", "handle": "both", "type": "testimonial",
		"fields": []any{map[string]any{"key": "stars", "value": "5"}, entry},
		"values": map[string]any{"stars": "5", "rider": a, "riders": []any{a, eraCustomerGID("9839")}},
	}, seen)
	if v := f.newest("metaobject", both); !strings.Contains(fmt.Sprint(v["values"]), a) {
		t.Fatalf("the fixture's metaobject does not hold her in its values: %v", v["values"])
	}

	f.erase(a)

	for _, gid := range []string{earlier, both} {
		for i, text := range f.texts("metaobject", gid) {
			if strings.Contains(text, a) {
				t.Errorf("version %d of the metaobject %s still names her: %s", i, gid, text)
			}
		}
	}
	v := f.newest("metaobject", both)
	values, _ := v["values"].(map[string]any)
	if values["stars"] != "5" || len(values) != 2 || fmt.Sprint(values["riders"]) != "["+eraCustomerGID("9839")+"]" || v["displayName"] != "Another word" {
		t.Errorf("the metaobject's values are %v, want the key that named her gone, her gid out of the list, and the rest as it was", v["values"])
	}
}

// The orders a request names travel on its job's row: queued by the delivery,
// read back by the runner, through the engine and the job's own concept.
func TestAJobKeepsTheOrdersItsRequestNames(t *testing.T) {
	f := newErasureFixture(t)
	if f == nil {
		return
	}
	store := Store{ID: f.store, Domain: "bicycles.example.com"}
	body, _ := json.Marshal(map[string]any{
		"shop_domain": store.Domain, "customer": map[string]any{"id": 1001}, "orders_to_redact": []any{5901, 9007199254740993},
	})
	req := complianceDelivery(TopicRedact, nil)
	req.Body = body
	if err := f.conn.enqueueComplianceJob(context.Background(), store, TopicRedact, req); err != nil {
		t.Fatalf("queue the erasure: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, f.store)
	})
	res, err := f.eng.Execute(memql.ContextWithFreshRead(operatorContext(context.Background())), renderCall("complianceJobsForStore", map[string]any{"storeId": f.store}))
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("the store has %d privacy job(s), want the one queued", len(rows))
	}
	job := complianceJobOf(rows[0])
	if got := strings.Join(job.OrderGIDs, ","); got != "gid://shopify/Order/5901,gid://shopify/Order/9007199254740993" {
		t.Errorf("the job's row names the orders %q, want the two its request named", got)
	}
	if job.CustomerGID != eraCustomerGID("1001") || job.Topic != TopicRedact || job.StoreID != f.store {
		t.Errorf("the job's row is %+v", job)
	}
}
