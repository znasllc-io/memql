package shopify

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// propagate_test.go -- the push channel (#4396).

func contentEntry(fields map[string]any) memqlsync.OutboxEntry {
	// storeId rides the PAYLOAD, because the contract's OutboxEntry has no
	// tenant field -- the runtime appends the row's own fields, and a
	// multi-tenant connector reads its scope from them.
	payload := map[string]any{"productGid": "gid://shopify/Product/1", "storeId": testStoreID}
	for k, v := range fields {
		payload[k] = v
	}
	return memqlsync.OutboxEntry{
		Id: "obx1", Concept: "v1:commerce:productContent", RowId: "pc1",
		Action: memqlsync.OutboxUpsert, Payload: payload,
		IdempotencyKey: "v1:commerce:productContent|pc1|1|shopify",
	}
}

func okDefinitions(h *testHarness) {
	h.admin.reply("ShopifyMetafieldDefinitionCreate", map[string]any{
		"metafieldDefinitionCreate": map[string]any{"userErrors": []any{}},
	})
}

func TestOneEntryBecomesOneMetafieldsSet(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	h.admin.reply("ShopifyMetafieldsSet", map[string]any{
		"metafieldsSet": map[string]any{
			"metafields": []any{map[string]any{"id": "gid://shopify/Metafield/1"}},
			"userErrors":  []any{},
		},
	})

	if _, err := h.conn.Propagate(context.Background(), contentEntry(map[string]any{"summary": "A linen shirt"})); err != nil {
		t.Fatal(err)
	}
	if h.admin.countOp("ShopifyMetafieldsSet") != 1 {
		t.Fatalf("metafieldsSet ran %d times", h.admin.countOp("ShopifyMetafieldsSet"))
	}
	var set *adminRequest
	for i, r := range h.admin.seen() {
		if r.Operation == "ShopifyMetafieldsSet" {
			set = &h.admin.seen()[i]
		}
	}
	raw, _ := json.Marshal(set.Variables["metafields"])
	body := string(raw)
	// The namespace is OURS, so nothing this connector writes can collide
	// with the merchant's own metafields or another app's.
	if !strings.Contains(body, `"namespace":"memql"`) {
		t.Errorf("namespace is not memql:\n%s", body)
	}
	if !strings.Contains(body, `"key":"summary"`) || !strings.Contains(body, `"ownerId":"gid://shopify/Product/1"`) {
		t.Errorf("input = %s", body)
	}
}

// A Shopify userError arrives inside a 200 and will fail identically forever.
// Retrying it is how a queue stops draining while every attempt looks
// transient.
func TestAUserErrorDeadLettersRatherThanRetrying(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	h.admin.userError("ShopifyMetafieldsSet", "metafieldsSet", "Value is invalid for type json")

	_, err := h.conn.Propagate(context.Background(), contentEntry(map[string]any{"summary": "x"}))
	if err == nil {
		t.Fatal("a userError inside a 200 was reported as success -- the trap of this API")
	}
	if !memqlsync.IsPermanent(err) {
		t.Fatalf("err = %v, want a PERMANENT failure -- the drain would otherwise spend its whole attempt budget on a request that cannot succeed", err)
	}
	if !strings.Contains(err.Error(), "Value is invalid") {
		t.Errorf("err = %v, want the vendor's own message", err)
	}
}

// A throttle or a 5xx WILL succeed later. Dead-lettering it would lose a
// legitimate change to a blip.
func TestATransportFailureRetriesRatherThanDeadLettering(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	h.admin.httpStatus("ShopifyMetafieldsSet", http.StatusBadGateway)
	h.conn.admin.MaxRetries = 0

	_, err := h.conn.Propagate(context.Background(), contentEntry(map[string]any{"summary": "x"}))
	if err == nil {
		t.Fatal("a 502 was reported as success")
	}
	if memqlsync.IsPermanent(err) {
		t.Fatalf("a 502 was marked permanent: %v -- it would dead-letter a change that will deliver on the next try", err)
	}
}

func TestAnEntryWithNothingToPushIsDeliveredNotDeadLettered(t *testing.T) {
	h := newHarness(t)
	result, err := h.conn.Propagate(context.Background(), contentEntry(nil))
	if err != nil {
		t.Fatalf("an empty row is not a failure: %v", err)
	}
	if !result.AlreadyDelivered {
		t.Errorf("result = %+v -- nothing to push is already delivered, not a retry", result)
	}
	if len(h.admin.seen()) != 0 {
		t.Error("an empty row still called Shopify")
	}
}

func TestAConceptWithNoProjectionDeadLetters(t *testing.T) {
	h := newHarness(t)
	_, err := h.conn.Propagate(context.Background(), memqlsync.OutboxEntry{
		Id: "x", Concept: "v1:commerce:quote", Payload: map[string]any{"storeId": testStoreID},
	})
	if !memqlsync.IsPermanent(err) {
		t.Fatalf("err = %v -- an undeclared projection cannot be retried into existence", err)
	}
}

// Above the threshold the batch is staged and run as one bulk mutation:
// hundreds of metafieldsSet calls against a cost bucket a backfill may
// already be using is the failure this avoids.
func TestALargeBatchStagesABulkMutation(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	h.admin.reply("ShopifyStagedUploadsCreate", map[string]any{
		"stagedUploadsCreate": map[string]any{
			"stagedTargets": []any{map[string]any{
				"url":         h.admin.server.URL + "/staged",
				"resourceUrl": h.admin.server.URL + "/staged",
				"parameters":  []any{map[string]any{"name": "key", "value": "tmp/metafields.jsonl"}},
			}},
			"userErrors": []any{},
		},
	})
	h.admin.reply("ShopifyBulkMutationRun", map[string]any{
		"bulkOperationRunMutation": map[string]any{
			"bulkOperation": map[string]any{"id": "gid://shopify/BulkOperation/7", "status": "CREATED"},
			"userErrors":    []any{},
		},
	})

	// keywords is a list metafield: one entry with enough of them clears the
	// threshold without inventing a second concept.
	var inputs []metafieldInput
	for i := 0; i < bulkMutationThreshold+1; i++ {
		inputs = append(inputs, metafieldInput{OwnerID: "gid://shopify/Product/1", Namespace: "memql", Key: "k", Type: "single_line_text_field", Value: "v"})
	}
	result, err := h.conn.propagateBulk(context.Background(), h.store(t), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExternalId != "gid://shopify/BulkOperation/7" {
		t.Errorf("externalId = %q, want the operation id", result.ExternalId)
	}
	if h.admin.countOp("ShopifyMetafieldsSet") != 0 {
		t.Error("the direct path ran as well as the bulk one")
	}
}

// Shopify takes every metafield value as a STRING and validates it against
// the declared type, so getting this wrong is a userError -- which dead-
// letters, which is why it is checked before the call.
func TestMetafieldValueRendering(t *testing.T) {
	cases := []struct {
		name, mfType string
		raw          any
		want         string
		present      bool
	}{
		{"text", "single_line_text_field", "hello", "hello", true},
		{"empty text is omitted", "single_line_text_field", "", "", false},
		{"nil is omitted", "single_line_text_field", nil, "", false},
		{"list is JSON", "list.single_line_text_field", []any{"a", "b"}, `["a","b"]`, true},
		{"empty list is omitted", "list.single_line_text_field", []any{}, "", false},
		{"json object", "json", map[string]any{"a": 1}, `{"a":1}`, true},
		{"empty object is omitted", "json", map[string]any{}, "", false},
		{"money is an object", "money", "12.50", `{"amount":"12.50","currency_code":"USD"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := metafieldValue(tc.mfType, tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if present != tc.present || got != tc.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, present, tc.want, tc.present)
			}
		})
	}
}

// productContent is storefront-readable; a note about a customer is not.
// Making an internal note public would put it one query away from anybody
// holding the public Storefront token.
func TestOnlyStorefrontFacingProjectionsArePublicRead(t *testing.T) {
	for _, p := range projections["v1:commerce:productContent"] {
		if p.StorefrontAccess != "PUBLIC_READ" {
			t.Errorf("productContent.%s is not storefront-readable, but the storefront renders it", p.Key)
		}
	}
	for _, concept := range []string{"v1:commerce:customerNote", "v1:commerce:companyLocationNote", "v1:commerce:creditLimit"} {
		for _, p := range projections[concept] {
			if p.StorefrontAccess != "" {
				t.Errorf("%s.%s is storefront-readable; it is internal", concept, p.Key)
			}
		}
	}
}

// priceOverride is what makes a quote a quote: without it Shopify
// recalculates from the catalog and the buyer is charged today's price for
// something they accepted last month.
func TestAnAcceptedQuoteLocksItsPrices(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyDraftOrderCreate", map[string]any{
		"draftOrderCreate": map[string]any{
			"draftOrder": map[string]any{"id": "gid://shopify/DraftOrder/5"},
			"userErrors":  []any{},
		},
	})
	gid, err := h.conn.CreateDraftOrderFromQuote(context.Background(), h.store(t), QuoteInput{
		CompanyGID: "gid://shopify/Company/1", CompanyLocationGID: "gid://shopify/CompanyLocation/2",
		CompanyContactGID: "gid://shopify/CompanyContact/3", PONumber: "PO-77",
		PaymentTermsTemplateGID: "gid://shopify/PaymentTermsTemplate/4", CurrencyCode: "USD",
		Lines: []QuoteLine{{VariantGID: "gid://shopify/ProductVariant/9", Quantity: 12, UnitAmount: "8.25"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gid != "gid://shopify/DraftOrder/5" {
		t.Fatalf("draft order = %q", gid)
	}
	raw, _ := json.Marshal(h.admin.seen()[0].Variables["input"])
	body := string(raw)
	for _, want := range []string{`"priceOverride"`, `"8.25"`, `"poNumber":"PO-77"`, `"purchasingCompany"`, `"paymentTermsTemplateId"`} {
		if !strings.Contains(body, want) {
			t.Errorf("draft order input is missing %s:\n%s", want, body)
		}
	}
}
