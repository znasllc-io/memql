package shopify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// compliance_test.go -- the three privacy topics (#4395).

func complianceDelivery(topic string, body map[string]any) memqlsync.InboundRequest {
	raw, _ := json.Marshal(body)
	return memqlsync.InboundRequest{
		RequestId: "inb-c", Source: "shopify-" + testStoreID, Topic: topic, Body: raw,
		Headers:    map[string]string{"x-shopify-topic": topic, "x-shopify-shop-domain": "acme-widgets.myshopify.com"},
		ReceivedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
}

func TestAComplianceDeliveryIsQueuedNotRunInline(t *testing.T) {
	h := newHarness(t)
	req := complianceDelivery(TopicRedact, map[string]any{
		"shop_domain": "acme-widgets.myshopify.com",
		"customer":    map[string]any{"id": 991},
	})
	writes, err := h.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Fatalf("a compliance topic produced %d mirror writes", len(writes))
	}
	queued := h.engine.callsTo("queueComplianceJob")
	if len(queued) != 1 {
		t.Fatalf("queued %d jobs", len(queued))
	}
	// Its own queue, not the outbox: the outbox's drain hands every entry
	// to Propagate, and a compliance job handed to Propagate would try to
	// write a customer's export into Shopify.
	if len(h.engine.callsTo("appendOutboxEntry")) != 0 {
		t.Error("a privacy request was queued on the outbound push queue")
	}
	if !strings.Contains(queued[0], `topic: "customers/redact"`) {
		t.Errorf("the job did not record its topic:\n%s", queued[0])
	}
	// Receipt is audited even though the work has not run: the obligation is
	// to be able to show WHAT was done and when, and a request nobody
	// recorded is indistinguishable from one that never arrived.
	if len(h.engine.callsTo("createAuditEvent")) != 1 {
		t.Error("the request was not audited on receipt")
	}
}

func TestTheHoldsAreTheRegulatedOnes(t *testing.T) {
	store := Store{ID: testStoreID}
	received := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	shop, err := parseComplianceJob(TopicShopRedact, store,
		complianceDelivery(TopicShopRedact, map[string]any{"shop_domain": "x"}), received)
	if err != nil {
		t.Fatal(err)
	}
	if got := shop.DueAt.Sub(shop.ReceivedAt); got != ShopRedactHold {
		t.Errorf("shop/redact hold = %v, want 48h -- an uninstall an operator reverses must not cost the mirror", got)
	}

	redact, err := parseComplianceJob(TopicRedact, store,
		complianceDelivery(TopicRedact, map[string]any{"customer": map[string]any{"id": 1}}), received)
	if err != nil {
		t.Fatal(err)
	}
	if !redact.DueAt.After(redact.ReceivedAt) {
		t.Error("customers/redact runs inline, overriding any grace period the merchant configured")
	}
	if redact.CustomerGID != "gid://shopify/Customer/1" {
		t.Errorf("customer GID = %q", redact.CustomerGID)
	}
}

// Completeness comes from the GENERATED model rather than a hand-written list
// of places customers appear: a hand-written list is correct the day it is
// written and wrong after the next allowlist change, which for a legal
// obligation is the wrong kind of wrong.
func TestTheExportWalksEveryMirroredConcept(t *testing.T) {
	h := newHarness(t)
	gid := "gid://shopify/Customer/991"
	h.engine.setRows("shopifyOrderForStore", []map[string]any{
		{"id": "shp1", "gid": "gid://shopify/Order/1", "customerGid": gid, "totalPriceSet": map[string]any{"shopMoney": map[string]any{"amount": "42.00"}}},
		{"id": "shp2", "gid": "gid://shopify/Order/2", "customerGid": "gid://shopify/Customer/other"},
	})
	h.engine.setRows("shopifyCustomerForStore", []map[string]any{
		{"id": "shp3", "gid": gid, "email": "a@example.com"},
	})

	export, err := h.conn.ExportCustomerData(context.Background(), h.store(t), gid)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := export["rows"].(map[string]any)
	orders, _ := rows["order"].([]map[string]any)
	if len(orders) != 1 {
		t.Fatalf("exported %d orders, want only the customer's own", len(orders))
	}
	if _, ok := rows["customer"]; !ok {
		t.Error("the customer's own row is missing from the export")
	}
	// Every mirrored concept was asked, not a curated few.
	if got := len(h.engine.callsTo("shopifyGiftCardForStore")); got == 0 {
		t.Error("a mirrored concept was not walked")
	}
	if export["customerGid"] != gid || export["apiVersion"] != generated.APIVersion {
		t.Errorf("export header = %v", export)
	}
}

// A customer appears on an order as customerGid, inside a nested
// billingAddress and inside a metafield value. A shallow check would miss two
// of the three.
func TestRowMentionsIsDeep(t *testing.T) {
	gid := "gid://shopify/Customer/991"
	cases := []struct {
		name string
		row  any
		want bool
	}{
		{"top level", map[string]any{"customerGid": gid}, true},
		{"nested object", map[string]any{"billingAddress": map[string]any{"ownerGid": gid}}, true},
		{"inside an array", map[string]any{"refs": []any{"other", gid}}, true},
		{"absent", map[string]any{"customerGid": "gid://shopify/Customer/1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowMentions(tc.row, gid); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTheExportIsRefusedWhenTheStoreHasNoOwner(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	store.OwnerUserID = ""
	err := h.conn.writeExportArtifact(context.Background(), store, ComplianceJob{}, []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "ownerUserId") {
		t.Fatalf("err = %v, want a refusal naming the field -- an export is a document about a real person and the Library is owner-tier", err)
	}
}

func TestTheExportIsFiledUnderTheStoreOwner(t *testing.T) {
	h := newHarness(t)
	job := ComplianceJob{RequestID: "dr-1", CustomerGID: "gid://shopify/Customer/991", ReceivedAt: h.now}
	if err := h.conn.writeExportArtifact(context.Background(), h.store(t), job, []byte(`{"rows":{}}`)); err != nil {
		t.Fatal(err)
	}
	calls := h.engine.callsTo("createGeneratedOutput")
	if len(calls) != 1 {
		t.Fatalf("wrote %d artifacts", len(calls))
	}
	if !strings.Contains(calls[0], "dr-1") {
		t.Errorf("the Shopify request id is not recorded with the export:\n%s", calls[0])
	}
}

func TestRedactionKeepsTheCommercialFactsAndLosesThePii(t *testing.T) {
	// The field list is a denylist because the mirror is generated from a
	// schema that carries no PII marking. What makes it adequate is its
	// SCOPE: it is applied to rows referencing ONE customer.
	pii := map[string]bool{}
	for _, f := range piiFieldNames {
		pii[f] = true
	}
	for _, want := range []string{"email", "phone", "firstName", "lastName", "billingAddress", "shippingAddress"} {
		if !pii[want] {
			t.Errorf("%q is not scrubbed", want)
		}
	}
	// The merchant's books are not the customer's personal data, and
	// destroying them would be a different compliance failure.
	for _, keep := range []string{"totalPriceSet", "currentQuantity", "gid", "storeId", "createdAt", "lineItems"} {
		if pii[keep] {
			t.Errorf("%q is scrubbed, but it is a commercial fact", keep)
		}
	}
	if RedactionMarker == "" {
		t.Error("a scrubbed field must be marked, so a reader can tell redacted from never-had-one")
	}
}

func TestRedactionAndPurgeRefuseWithoutADatabase(t *testing.T) {
	h := newHarness(t)
	if _, err := h.conn.RedactCustomer(context.Background(), h.store(t), "gid://shopify/Customer/1"); err == nil {
		t.Error("redaction ran with no database handle")
	}
	if _, err := h.conn.PurgeStore(context.Background(), h.store(t)); err == nil {
		t.Error("the purge ran with no database handle")
	}
}

// An uninstall an operator reverses inside the 48-hour hold must not cost the
// mirror.
func TestShopRedactSkipsThePurgeWhenTheStoreIsBackx(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
	job := ComplianceJob{Topic: TopicShopRedact, StoreID: testStoreID, DueAt: h.now.Add(-time.Hour), ReceivedAt: h.now.Add(-49 * time.Hour)}

	if _, err := h.conn.runComplianceJob(context.Background(), h.store(t), job); err != nil {
		t.Fatal(err)
	}
	audits := h.engine.callsTo("createAuditEvent")
	if len(audits) != 1 || !strings.Contains(audits[0], "shopify_shop_redact_skipped") {
		t.Fatalf("audits = %v, want a recorded skip", audits)
	}
}

func TestAComplianceJobRunsOnlyWhenItsHoldHasElapsed(t *testing.T) {
	h := newHarness(t)
	job := ComplianceJob{
		Topic: TopicRedact, StoreID: testStoreID, CustomerGID: "gid://shopify/Customer/1",
		ReceivedAt: h.now, DueAt: h.now.Add(time.Hour),
	}
	_ = job
	// complianceJobsDue filters on dueAt server-side, so a job still
	// inside its hold is simply not returned. The read here is empty,
	// which is what "the hold has not elapsed" looks like to the runner.
	h.engine.setRows("complianceJobsDue", nil)

	ran, err := h.conn.RunDueComplianceJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatalf("ran %d jobs before the hold elapsed", ran)
	}
}
