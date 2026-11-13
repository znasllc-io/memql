package shopify

import (
	"context"
	"regexp"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// A denial of returns used to empty an UNSELECTED page operation in the
// generated Order document. Shopify parses the whole document, so even an
// ordinary order list then failed with unexpected RCURLY. These assertions
// exercise the actual emitted document; independent GraphQL validation also
// checks its syntax and variable uses when qualifying this regression.
func assertDeniedOperationDocument(t *testing.T, document, operation string) {
	t.Helper()
	if regexp.MustCompile(`\{\s*\}`).MatchString(document) {
		t.Fatal("pruned request carries an empty selection set")
	}
	operations := regexp.MustCompile(`(?m)^query ([A-Za-z_][A-Za-z_0-9]*)`).FindAllStringSubmatch(document, -1)
	if len(operations) != 1 || operations[0][1] != operation {
		t.Fatalf("pruned request carries %d operations, want only %s", len(operations), operation)
	}
	body := strings.IndexByte(document, '{')
	if body < 0 {
		t.Fatal("request has no operation body")
	}
	for _, variable := range regexp.MustCompile(`\$[A-Za-z_][A-Za-z_0-9]*`).FindAllString(document[:body], -1) {
		if !strings.Contains(document[body:], variable) {
			t.Errorf("pruned request declares unused variable %s", variable)
		}
	}
}

func TestDeniedOrderChildKeepsTheSelectedOperationValid(t *testing.T) {
	spec := generated.Types["order"]
	for _, tt := range []struct {
		operation string
		denied    []string
	}{
		{spec.FetchOp, []string{"returns", "staffMember"}},
		{spec.ListOp, []string{"returns", "staffMember"}},
		{"ShopifyPageOrderLineItems", []string{"returns", "staffMember"}},
		{"ShopifyPageOrderReturns", []string{"lineItems", "staffMember"}},
	} {
		t.Run(tt.operation, func(t *testing.T) {
			operation := tt.operation
			h := newHarness(t)
			store := h.store(t)
			h.conn.noteDeniedFields(store, spec, tt.denied)
			h.admin.reply(operation, map[string]any{"node": nil})
			vars := map[string]any{"id": orderGID, "after": "page-1", "first": 1, "query": "status:any"}
			resp, err := h.conn.callWithGrant(context.Background(), store, spec, operation, vars)
			if err != nil {
				t.Fatal(err)
			}
			requests := h.admin.seen()
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want one", len(requests))
			}
			assertDeniedOperationDocument(t, requests[0].Query, operation)
			for _, field := range tt.denied {
				if regexp.MustCompile(`(?m)^\s*` + field + `\b`).MatchString(requests[0].Query) {
					t.Errorf("denied selection %s survived in the request", field)
				}
			}
			if strings.HasPrefix(operation, "ShopifyPage") && (!strings.Contains(requests[0].Query, "after: $after") || !strings.Contains(requests[0].Query, "pageInfo { hasNextPage endCursor }")) {
				t.Error("the allowed child page lost its cursor or paging proof")
			}
			if resp.QueryCost != 10 || resp.RequestedCost != 12 {
				t.Fatalf("query costs = %v/%v, want 10/12", resp.QueryCost, resp.RequestedCost)
			}
		})
	}
}

func TestADeniedLaterChildPageIsNotADeletedOrCompleteOrder(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchOrder", map[string]any{"node": map[string]any{
		"__typename": "Order", "id": orderGID, "updatedAt": "2026-08-23T11:00:00Z",
		"returns": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "page-1"}},
	}})
	h.admin.denyField("ShopifyPageOrderReturns", "returns", map[string]any{"node": nil})
	writes, err := h.conn.Apply(context.Background(), delivery("orders/updated", orderGID))
	if !memqlsync.IsNotGranted(err) {
		t.Fatalf("later child refusal = %v, want not granted", err)
	}
	if len(writes) != 0 {
		t.Fatalf("a denied child page produced %d writes; the partial parent is neither a deletion nor a completed read", len(writes))
	}
	if got := len(h.admin.seen()); got != 2 {
		t.Fatalf("requests = %d, want fetch and one refused child page", got)
	}
}

func TestAChildDenialIsLearnedBeforeTheValidMainOperationIsRetried(t *testing.T) {
	h := newHarness(t)
	spec := generated.Types["order"]
	h.admin.denyField(spec.ListOp, "returns", map[string]any{"orders": nil})
	h.admin.denyField(spec.ListOp, "staffMember", map[string]any{"orders": nil})
	h.admin.reply(spec.ListOp, listPageReply("orders", false, "", map[string]any{
		"id": orderGID, "updatedAt": "2026-08-23T11:00:00Z", "name": "#1001",
	}))
	if _, err := h.conn.Reconcile(context.Background(), "v1:shopify:order", h.now); err != nil {
		t.Fatal(err)
	}
	requests := h.admin.seen()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want the denial and one valid retry", len(requests))
	}
	if requests[0].Query != spec.FetchDocument {
		t.Fatal("a store without learned denials did not receive the original document")
	}
	assertDeniedOperationDocument(t, requests[1].Query, spec.ListOp)
	if got := strings.Join(h.conn.FieldsDenied(testStoreID)["v1:shopify:order"], ","); got != "returns,staffMember" {
		t.Fatalf("learned denials = %q", got)
	}
}

func TestADeniedSelectedChildPageIsRefusedBeforeAnInvalidRetry(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "learned now", true: "already learned"}[cached], func(t *testing.T) {
			h := newHarness(t)
			store, spec := h.store(t), generated.Types["order"]
			const operation = "ShopifyPageOrderReturns"
			if cached {
				h.conn.noteDeniedFields(store, spec, []string{"returns"})
			} else {
				h.admin.denyField(operation, "returns", map[string]any{"node": nil})
			}
			_, err := h.conn.callWithGrant(context.Background(), store, spec, operation, map[string]any{"id": orderGID, "after": "page-1"})
			if !memqlsync.IsNotGranted(err) {
				t.Fatalf("selected child refusal = %v, want not granted", err)
			}
			wantRequests := 1
			if cached {
				wantRequests = 0
			}
			if got := len(h.admin.seen()); got != wantRequests {
				t.Errorf("requests = %d, want %d; a denied child must not be re-asked with an empty operation", got, wantRequests)
			}
			if got := strings.Join(h.conn.FieldsDenied(testStoreID)["v1:shopify:order"], ","); got != "returns" {
				t.Fatalf("learned denials = %q, want returns", got)
			}
		})
	}
}

func TestAnUnknownOrderOperationIsRefusedBeforeHTTP(t *testing.T) {
	for _, fields := range [][]string{nil, {"returns", "staffMember"}} {
		h := newHarness(t)
		store, spec := h.store(t), generated.Types["order"]
		h.conn.noteDeniedFields(store, spec, fields)
		_, err := h.conn.callWithGrant(context.Background(), store, spec, "ShopifyUnknownOrderOperation", nil)
		if !memqlsync.IsPermanent(err) {
			t.Fatalf("unknown operation with denials %v = %v, want permanent refusal", fields, err)
		}
		if got := len(h.admin.seen()); got != 0 {
			t.Fatalf("unknown operation sent %d requests", got)
		}
	}
}
