package shopify

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

func TestBulkRefusalRetainsTheProviderCause(t *testing.T) {
	t.Run("unrequested feature user error", func(t *testing.T) {
		h := newHarness(t)
		message := "Access denied for inventoryShipments field. Required access: `read_inventory_shipments` access scope."
		h.admin.userError("ShopifyBulkRun", "bulkOperationRunQuery", message)
		page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("inventoryShipment"), "")
		var cause *UserError
		if !memqlsync.IsNotRequested(err) || !errors.As(err, &cause) || !strings.Contains(err.Error(), message) {
			t.Fatalf("refusal lost its classification or provider cause: %v", err)
		}
		if page.Done || len(page.Writes) != 0 || page.NextCursor != "" || h.admin.countOp("ShopifyBulkRun") != 1 {
			t.Fatalf("refused export advanced or retried: %+v", page)
		}
	})
	t.Run("requested scope GraphQL error", func(t *testing.T) {
		h := newHarness(t)
		message := "Access denied for customers field. Required access: `read_customers` access scope."
		h.admin.graphQLError("ShopifyBulkRun", "ACCESS_DENIED", message)
		page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("customer"), "")
		var cause *AdminError
		if !memqlsync.IsNotGranted(err) || !errors.As(err, &cause) || !strings.Contains(err.Error(), message) {
			t.Fatalf("refusal lost its classification or provider cause: %v", err)
		}
		if page.Done || len(page.Writes) != 0 || page.NextCursor != "" || h.admin.countOp("ShopifyBulkRun") != 1 {
			t.Fatalf("refused export advanced or retried: %+v", page)
		}
	})
}

func TestBulkOrderUsesAnAlreadyLearnedOptionalFieldDenial(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	h.conn.noteDeniedFields(store, generated.Types["order"], []string{"staffMember"})
	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(context.Background(), store, generated.ConceptID("order"), "")
	if err != nil || page.Done || page.NextCursor == "" || len(page.Writes) != 0 {
		t.Fatalf("start = %+v, %v", page, err)
	}
	seen := h.admin.seen()
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want one already-pruned start", len(seen))
	}
	document := seen[0].Variables["query"].(string)
	if strings.Contains(document, "staffMember") {
		t.Fatal("the nested bulk query still selects the field already denied to ordinary reads")
	}
	for _, required := range []string{"orders(", "id", "updatedAt", "createdAt", "lineItems", "refunds", "quantity", "originalUnitPriceSet"} {
		if !strings.Contains(document, required) {
			t.Errorf("the bulk query lost required selection %q", required)
		}
	}
}

func TestBulkOrderLearnsAnOptionalFieldFromItsUserError(t *testing.T) {
	h := newHarness(t)
	h.admin.replyWith("ShopifyBulkRun", func(vars map[string]any) any {
		if strings.Contains(vars["query"].(string), "staffMember") {
			return map[string]any{"bulkOperationRunQuery": map[string]any{"userErrors": []any{map[string]any{
				"field": []string{"query"}, "code": "ACCESS_DENIED",
				"message": "Access denied for staffMember field. Required access: `read_users` access scope.",
			}}}}
		}
		return startedOperation("gid://shopify/BulkOperation/1")
	})
	page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("order"), "")
	if err != nil || page.Done || page.NextCursor == "" || len(page.Writes) != 0 {
		t.Fatalf("optional field refusal prevented a bounded export start: %+v, %v", page, err)
	}
	if got := h.admin.countOp("ShopifyBulkRun"); got != 2 {
		t.Fatalf("bulk starts = %d, want one refused and one reduced selection", got)
	}
	if got := h.conn.FieldsDenied(testStoreID)[generated.ConceptID("order")]; len(got) != 1 || got[0] != "staffMember" {
		t.Fatalf("learned denials = %v", got)
	}
}

func TestBulkOptionalSelectionsKeepTheStatisticsProjection(t *testing.T) {
	spec := generated.Types["order"]
	document, err := operationBody(spec.BulkDocument, spec.BulkOps[0])
	if err != nil {
		t.Fatal(err)
	}
	// The independently live-qualified list is held separately in the test
	// harness: adding a production omission must not quietly widen that proof.
	if len(bulkOptionalOrderFields) != len(statisticsOptionalOrderFields) {
		t.Fatal("bulk omissions differ from the live-qualified projection")
	}
	for _, field := range statisticsOptionalOrderFields {
		if !bulkOptionalOrderFields[field] {
			t.Fatalf("bulk omission %s is not live-qualified", field)
		}
	}
	got, omitted, err := bulkDocumentWithGrant(document, spec, statisticsOptionalOrderFields)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range statisticsOptionalOrderFields {
		if bulkSelects(got, field) {
			t.Errorf("denied field %s remains in the export", field)
		}
		if slices.Contains(omitted, field) != bulkSelects(document, field) {
			t.Errorf("omission provenance does not match actual selection %s: %v", field, omitted)
		}
	}
	for _, field := range []string{"orders", "id", "updatedAt", "createdAt", "processedAt", "currencyCode", "customer", "email", "cancelledAt", "test", "taxesIncluded", "totalShippingPriceSet", "totalTaxSet", "originalTotalDutiesSet", "subtotalLineItemsQuantity", "currentSubtotalPriceSet", "totalDiscountsSet", "totalRefundedSet", "lineItems", "quantity", "currentQuantity", "isGiftCard", "product", "title", "originalUnitPriceSet", "discountAllocations", "refunds", "duties", "additionalPaymentCollectionUrl"} {
		if !bulkSelects(got, field) {
			t.Errorf("required/sibling selection %s was lost", field)
		}
	}
	changedShape := strings.ReplaceAll(document, "additionalPaymentCollectionUrl vaultedPaymentMethods", "vaultedPaymentMethods")
	if _, _, err := bulkDocumentWithGrant(changedShape, spec, []string{"vaultedPaymentMethods"}); err == nil {
		t.Fatal("a changed inline shape could leave an empty selection without refusing")
	}
}

func TestBulkRequiredAndUnknownDenialsNeverReduceHistory(t *testing.T) {
	for _, field := range []string{"orders", "id", "updatedAt", "lineItems", "refunds", "returns", "quantity", "totalTaxSet", "customer", "email", "test", "title"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t)
			store := h.store(t)
			h.conn.noteDeniedFields(store, generated.Types["order"], []string{field})
			page, err := h.conn.BackfillStore(context.Background(), store, generated.ConceptID("order"), "")
			if err == nil || page.Done || page.NextCursor != "" || len(page.Writes) != 0 || len(h.admin.seen()) != 0 {
				t.Fatalf("required known denial advanced or reached provider: page=%+v err=%v calls=%d", page, err, len(h.admin.seen()))
			}
		})
	}
	for _, messages := range [][]string{
		{"Access denied for fulfillmentOrders field."}, // not selected by the bulk document
		{"Access denied for returns field."},
		{"Access denied for totalTaxSet field."},
		{"Access denied for aFutureField field."},
		{"Access denied for staffMember field.", "Query is invalid"},
		{"Unexpected error: Access denied for staffMember field."},
	} {
		h := newHarness(t)
		items := make([]any, len(messages))
		for n, message := range messages {
			items[n] = map[string]any{"message": message}
		}
		h.admin.reply("ShopifyBulkRun", map[string]any{"bulkOperationRunQuery": map[string]any{"userErrors": items}})
		page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("order"), "")
		var cause *UserError
		if !errors.As(err, &cause) || page.Done || page.NextCursor != "" || len(page.Writes) != 0 || h.admin.countOp("ShopifyBulkRun") != 1 {
			t.Fatalf("unqualified refusal was lost or retried: messages=%v page=%+v err=%v", messages, page, err)
		}
	}
}

func TestBulkAcceptedOperationWithErrorsIsNotSubmittedAgain(t *testing.T) {
	h := newHarness(t)
	reply := startedOperation("gid://shopify/BulkOperation/accepted")
	reply["bulkOperationRunQuery"].(map[string]any)["userErrors"] = []any{map[string]any{"message": "Access denied for staffMember field."}}
	h.admin.reply("ShopifyBulkRun", reply)
	page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("order"), "")
	if err == nil || page.Done || page.NextCursor != "" || h.admin.countOp("ShopifyBulkRun") != 1 {
		t.Fatalf("ambiguous accepted operation was retried or marked complete: %+v, %v", page, err)
	}
}

func TestBulkOuterGraphQLDenialDoesNotAuthorizeAnInnerQueryRetry(t *testing.T) {
	h := newHarness(t)
	h.admin.graphQLError("ShopifyBulkRun", "ACCESS_DENIED", "Access denied for staffMember field.")
	page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("order"), "")
	var cause *AdminError
	if !errors.As(err, &cause) || page.Done || page.NextCursor != "" || h.admin.countOp("ShopifyBulkRun") != 1 || len(h.conn.FieldsDenied(testStoreID)) != 0 {
		t.Fatalf("outer failure was mistaken for a safely refused inner selection: %+v, %v", page, err)
	}
}

func TestBulkOmissionsSurviveCompletionAndAFreshVerdictReader(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	h.conn.noteDeniedFields(store, generated.Types["order"], []string{"staffMember", "fulfillmentOrders"})
	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(context.Background(), store, generated.ConceptID("order"), "")
	if err != nil {
		t.Fatal(err)
	}
	started := decodeBackfillCursor(page.NextCursor)
	if started.Omitted != "staffMember" || started.Rule != originVersionRule || started.At == "" {
		t.Fatalf("started cursor lacks precise selection/version provenance: %+v", started)
	}
	h.admin.reply("ShopifyBulkStatus", linklessCompletion(started.OperationID, "0"))
	page, err = h.conn.BackfillStore(context.Background(), store, generated.ConceptID("order"), page.NextCursor)
	if err != nil || !page.Done {
		t.Fatalf("completed empty export = %+v, %v", page, err)
	}
	completed := decodeBackfillCursor(page.NextCursor)
	if completed.Omitted != started.Omitted || completed.Rule != started.Rule || completed.At != started.At {
		t.Fatalf("completion erased coverage or refund-detail watermark: %+v", completed)
	}
	fresh := newHarness(t)
	fresh.engine.setRows("syncStateById", []map[string]any{{"conceptId": generated.ConceptID("order"), "backfillStatus": "done", "backfillCursor": page.NextCursor}})
	if len(fresh.conn.FieldsDenied(testStoreID)) != 0 {
		t.Fatal("fresh reader unexpectedly inherited the old process cache")
	}
	v, err := fresh.conn.storeVerdict(context.Background(), fresh.store(t))
	if err != nil || !reflect.DeepEqual(v.Withheld, []string{"orders: staff member"}) {
		t.Fatalf("fresh verdict lost exact omitted field: withheld=%v err=%v", v.Withheld, err)
	}
	if len(fresh.conn.FieldsDenied(testStoreID)) != 0 {
		t.Fatal("reading historical exclusions changed current provider knowledge")
	}
}

func TestBulkCursorOmissionsAreBoundedAndDomainSpecific(t *testing.T) {
	for _, tt := range []struct{ concept, cursor string }{
		{"product", `{"omitted":"staffMember"}`},
		{"order", `{"omitted":"totalTaxSet"}`},
		{"order", `{"omitted":"staffMember,staffMember"}`},
		{"order", `{"omitted":["staffMember"]}`},
		{"order", `{"omitted":"` + strings.Repeat("staffMember,", 20) + `"}`},
	} {
		h := newHarness(t)
		page, err := h.conn.BackfillStore(context.Background(), h.store(t), generated.ConceptID(tt.concept), tt.cursor)
		if err == nil || page.Done || len(h.admin.seen()) != 0 {
			t.Fatalf("invalid coverage cursor reached provider or completed: %s %+v %v", tt.cursor, page, err)
		}
		if _, err := bulkFieldsDenied(map[string][]string{}, []map[string]any{{"conceptId": generated.ConceptID(tt.concept), "backfillCursor": tt.cursor}}); err == nil {
			t.Fatalf("health read accepted invalid coverage: %s", tt.cursor)
		}
	}
}

func TestManagedCommerceOrderBackfillReachesTheProvider(t *testing.T) {
	h := newHarness(t)
	// Same effective recorded commerce grant as the investigated managed
	// connection. Only the token/lock seams are local fakes; no provider call.
	withManagedScopes(t, h, "unauthenticated_read_product_listings", "unauthenticated_write_checkouts", "unauthenticated_read_customers", "write_products", "read_inventory", "read_locations", "read_orders", "write_customers", "read_discounts", "read_returns")
	h.conn.connectAppGate = func(context.Context, string) (func(), error) { return func() {}, nil }
	h.conn.stores.secrets = func(context.Context, string) (string, error) {
		return (offlineGrant{AccessToken: "fake-managed-access", ExpiresAt: h.now.Add(time.Hour)}).encoded(), nil
	}
	store := h.store(t)
	if !store.Managed() || !domainInConnection(store, generated.Types["order"]) || len(scopesMissingFor(store, generated.Types["order"])) != 0 {
		t.Fatal("the fixture does not pass the actual managed order scope gates")
	}
	h.admin.userError("ShopifyBulkRun", "bulkOperationRunQuery", "Access denied for orders field. This export is unavailable.")
	page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("order"), "")
	var cause *UserError
	if !memqlsync.IsNotRequested(err) || !errors.As(err, &cause) || page.Done || h.admin.countOp("ShopifyBulkRun") != 1 {
		t.Fatalf("managed order path did not retain its actual provider refusal: %+v %v", page, err)
	}
}

func TestBulkOptionalDenialRetriesRemainBounded(t *testing.T) {
	h := newHarness(t)
	fields := []string{"contract", "paymentTerms", "staffMember", "user", "vaultedPaymentMethods"}
	h.admin.replyWith("ShopifyBulkRun", func(vars map[string]any) any {
		for _, field := range fields {
			if bulkSelects(vars["query"].(string), field) {
				return map[string]any{"bulkOperationRunQuery": map[string]any{"userErrors": []any{map[string]any{"message": "Access denied for " + field + " field."}}}}
			}
		}
		return startedOperation("gid://shopify/BulkOperation/1")
	})
	page, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("order"), "")
	if err == nil || page.Done || page.NextCursor != "" || h.admin.countOp("ShopifyBulkRun") != maxDeniedPrunes+1 {
		t.Fatalf("denial retries escaped their bound: page=%+v err=%v calls=%d", page, err, h.admin.countOp("ShopifyBulkRun"))
	}
}

func TestBulkOmissionsSurviveAnExpiredOperation(t *testing.T) {
	h := newHarness(t)
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Rule: originVersionRule, At: exportStartedAt, Omitted: "staffMember"}
	h.admin.reply("ShopifyBulkStatus", operationStatus(cur.OperationID, "EXPIRED", ""))
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), generated.ConceptID("order"), cur.encode())
	next := decodeBackfillCursor(page.NextCursor)
	if err != nil || page.Done || next.OperationID != "" || next.Restarts != 1 || next.Omitted != cur.Omitted || next.Rule != cur.Rule {
		t.Fatalf("restart lost coverage or its retry boundary: %+v %v", next, err)
	}
}
