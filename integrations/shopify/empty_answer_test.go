package shopify

import (
	"slices"
	"sort"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// empty_answer_test.go -- which kinds an answer of nothing may not retire.
//
// What a clean answer of nothing DOES is two halves -- the connector's
// decision and what the runtime's dispatcher stamps on the delivery -- and is
// played through both in the root module
// (component/datasync/shopify_empty_answer_test.go), which this module cannot
// import the runtime to do. What stays here is the connector's alone: the
// list of kinds the rule covers, read off the generated specs.

// THE KINDS READ UNDER THE ORDERS PERMISSION come from the generated specs: a
// kind whose own scopes name read_orders, and every kind that hangs from one.
// The list is pinned here so a regeneration that moves it is read by a person.
func TestTheKindsReadUnderTheOrdersPermissionComeFromTheGeneratedSpecs(t *testing.T) {
	var got []string
	for concept, spec := range generated.Types {
		if readUnderOrders(spec) {
			got = append(got, concept)
		}
		if slices.Contains(spec.Scopes, "read_orders") && !readUnderOrders(spec) {
			t.Errorf("%s reads under read_orders and is not covered", concept)
		}
		if parent := generated.Types[spec.Parent]; parent != nil && readUnderOrders(parent) && !readUnderOrders(spec) {
			t.Errorf("%s hangs from %s, which is order data, and is not covered", concept, spec.Parent)
		}
	}
	sort.Strings(got)
	want := []string{"abandonedCheckout", "fulfillment", "order", "orderLineItem", "orderTransaction", "refund",
		"reverseDelivery", "reverseFulfillmentOrder", "shopifyReturn", "tenderTransaction"}
	if !slices.Equal(got, want) {
		t.Errorf("the kinds read under the orders permission are\n  %v\nwant\n  %v", got, want)
	}
	for _, concept := range []string{"product", "customer", "draftOrder", "fulfillmentOrder"} {
		if readUnderOrders(generated.Types[concept]) {
			t.Errorf("%s is not order data and is covered", concept)
		}
	}
}
