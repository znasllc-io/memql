package shopify

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// erasure_fields_test.go -- the gate on what a customer's erasure leaves
// behind (rulings 78, 81, 82 and 83).
//
// The comment over the old field list named a gate test that did not exist.
// This is it: the golden file is the reviewed statement of the erasure's reach
// and of every field's fate, and it is rendered again here from the generated
// model and the policy.

const erasureGoldenPath = "testdata/erasure-fields.golden"

// erasureGoldenUpdate is the environment variable that rewrites the golden
// from the model and the policy. READ THE DIFF: it is the review.
const erasureGoldenUpdate = "SHOPIFY_ERASURE_UPDATE_GOLDEN"

func TestTheErasurePolicyMatchesItsGolden(t *testing.T) {
	got := renderErasureGolden()
	if os.Getenv(erasureGoldenUpdate) == "1" {
		if err := os.MkdirAll(filepath.Dir(erasureGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(erasureGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s rewritten -- read its diff, it is the review of what an erasure keeps", erasureGoldenPath)
		return
	}
	want, err := os.ReadFile(erasureGoldenPath)
	if err != nil {
		t.Fatalf("the reviewed statement of what an erasure keeps is missing: %v\n  write it with %s=1 and read it", err, erasureGoldenUpdate)
	}
	if string(want) == got {
		return
	}
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := "<the file ends>", "<the file ends>"
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			t.Fatalf("what an erasure reaches or keeps has changed, at line %d of %s:\n  reviewed: %s\n  now:      %s\n\n"+
				"A regeneration of the mirror or an edit of the policy changed it. If the change is meant, rewrite the file with %s=1 and read the whole diff:\n"+
				"a field that is new is scrubbed unless it is put on the keep-list in erasure_fields.go with the reason it is not about a person.",
				i+1, erasureGoldenPath, w, g, erasureGoldenUpdate)
		}
	}
}

// renderErasureGolden is the golden file: the reach of an erasure, concept by
// concept, and the fate of every field.
func renderErasureGolden() string {
	var b strings.Builder
	b.WriteString(erasureGoldenHeader())
	reach := erasureReach()
	concepts := make([]string, 0, len(generated.Types))
	for concept := range generated.Types {
		concepts = append(concepts, concept)
	}
	sort.Strings(concepts)
	for _, concept := range concepts {
		spec := generated.Types[concept]
		fmt.Fprintf(&b, "\nconcept %s (%s)\n", concept, spec.GraphQLType)
		r := reach[concept]
		switch {
		case len(r.Links) > 0:
			fmt.Fprintf(&b, "  reach: %s\n", strings.Join(r.Links, "; "))
		case len(r.Lists) > 0:
			fmt.Fprintf(&b, "  reach: only through a list of references (%s): the customer's gid is taken out of the list and nothing else in the row changes\n", strings.Join(r.Lists, ", "))
		default:
			b.WriteString("  reach: not reachable\n")
		}
		if len(r.Links) > 0 && len(r.Lists) > 0 {
			fmt.Fprintf(&b, "  a row that is not in scope and lists the customer among its references (%s) has the gid taken out of the list\n", strings.Join(r.Lists, ", "))
		}
		if concept == erasedConcept {
			b.WriteString("  own: the erased customer's own row, which keeps nothing but its structural fields\n")
			b.WriteString("  other: a row of another customer that a link brought into scope\n")
		}
		for _, f := range spec.Fields {
			fmt.Fprintf(&b, "  %-40s %-11s %-34s ", f.Name, f.Kind, f.AdminType)
			if concept == erasedConcept {
				fmt.Fprintf(&b, "own: %s | other: %s\n", describeErasure(erasureDecisionFor(concept, f, true)), describeErasure(erasureDecisionFor(concept, f, false)))
			} else {
				b.WriteString(describeErasure(erasureDecisionFor(concept, f, false)) + "\n")
			}
			// What a kept object or list holds is part of what is kept, so
			// it is part of what is reviewed: a field Shopify adds INSIDE it
			// shows here. Money is one fixed shape and is not repeated.
			if d := erasureDecisionFor(concept, f, false); d.ByList != "" && f.AdminType != "MoneyV2" && f.AdminType != "MoneyBag" {
				if sel := selectionOf(spec, f); sel != "" {
					fmt.Fprintf(&b, "      holds %s\n", sel)
				}
			}
		}
	}
	return b.String()
}

func erasureGoldenHeader() string {
	kept := make([]string, 0, len(erasureKeptScalars))
	for name, why := range erasureKeptScalars {
		kept = append(kept, name+" ("+why+")")
	}
	sort.Strings(kept)
	other := map[string]bool{}
	for _, spec := range generated.Types {
		for _, f := range spec.Fields {
			if f.Kind == generated.KindScalar && erasureKeptScalars[f.AdminType] == "" {
				other[f.AdminType] = true
			}
		}
	}
	scrubbed := make([]string, 0, len(other))
	for name := range other {
		scrubbed = append(scrubbed, name)
	}
	sort.Strings(scrubbed)
	return "# What a customer's erasure (customers/redact) reaches in a store's mirror, and what it leaves.\n" +
		"#\n" +
		"# Rendered by TestTheErasurePolicyMatchesItsGolden from the generated model\n" +
		"# (integrations/shopify/generated/model.go) and the policy (integrations/shopify/erasure_fields.go).\n" +
		"# Never edited by hand: " + erasureGoldenUpdate + "=1 go test ./integrations/shopify/ -run TestTheErasurePolicyMatchesItsGolden\n" +
		"# rewrites it, and its diff is the review.\n" +
		"#\n" +
		"# REACH. The scope starts at the customer's row and the orders the request names, and grows to a fixed\n" +
		"# point by three links and no other:\n" +
		"#   child of X              the row hangs from an in-scope row of X by the model's parent link\n" +
		"#   gid field F naming X    the row holds an in-scope row's gid as the whole value of its gid field F\n" +
		"#   the customer inside object F\n" +
		"#                           the row holds the customer's gid inside its object or list of objects F. The fields\n" +
		"#                           named are those whose selection keeps a customer's gid; the match itself is by value,\n" +
		"#                           in every object and list of objects of every row of the store\n" +
		"# A list of references is not a link. A row that is not in scope and lists the customer among its\n" +
		"# references has the gid taken out of that list, and nothing else in it changes. The data request exports\n" +
		"# exactly the rows in scope.\n" +
		"#\n" +
		"# FIELDS. Every version of a row in scope is rewritten. The structural fields are never touched:\n" +
		"#   " + strings.Join(erasureStructuralFields, ", ") + "\n" +
		"# Every other field, and any key the model does not know, is decided below:\n" +
		"#   kept by kind            a gid, a list of references, an enum, or a scalar whose Admin type is one of:\n" +
		wrapComment("#                           ", kept) +
		"#   kept by the list        on the keep-list of erasure_fields.go, with the reason it describes the sale\n" +
		"#   scrubbed                everything else: a string becomes \"" + RedactionMarker + "\", a list becomes [], and the key of\n" +
		"#                           any other type is removed, so a typed read answers null and the mirror can write the row again\n" +
		"#   kept by the list; scrubbed on a custom item\n" +
		"#                           the one decision that looks at a row's values: a line item (" + erasureCustomItem.Concept + ") with no\n" +
		"#                           " + erasureCustomItem.Reference + " is a custom item, whose titles are what a person typed. It is decided\n" +
		"#                           version by version\n" +
		"# The scalar Admin types the model holds that are not kept by kind:\n" +
		wrapComment("#   ", scrubbed) +
		"# The columns are the field, its kind, its Admin type and the decision. A kept object or list is followed by\n" +
		"# what it holds.\n"
}

// wrapComment joins items with commas into comment lines of about a hundred
// columns, each under the same prefix.
func wrapComment(prefix string, items []string) string {
	var b strings.Builder
	line := prefix
	for i, item := range items {
		if i < len(items)-1 {
			item += ","
		}
		if len(line) > len(prefix) && len(line)+1+len(item) > 104 {
			b.WriteString(line + "\n")
			line = prefix
		}
		if len(line) > len(prefix) {
			line += " "
		}
		line += item
	}
	if len(line) > len(prefix) {
		b.WriteString(line + "\n")
	}
	return b.String()
}

func describeErasure(d erasureDecision) string {
	switch {
	case d.OnCustomItem != "":
		return "kept by the list: " + d.ByList + "; scrubbed on a custom item (a line with no " + erasureCustomItem.Reference + "): " + describeScrub(d.OnCustomItem)
	case d.ByKind != "":
		return "kept by kind: " + d.ByKind
	case d.ByList != "":
		return "kept by the list: " + d.ByList
	}
	return "scrubbed: " + describeScrub(d.Fate)
}

func describeScrub(fate erasureFate) string {
	switch fate {
	case fateMarked:
		return `becomes "` + RedactionMarker + `"`
	case fateEmptied:
		return "becomes []"
	case fateRemoved:
		return "the key is removed"
	}
	return "UNDECIDED (" + string(fate) + ")"
}

// selectionOf is what the mirror selects inside one object or list field, read
// from the concept's own fetch document, where every field of the type is one
// line.
func selectionOf(spec *generated.TypeSpec, f generated.FieldSpec) string {
	for _, line := range strings.Split(spec.FetchDocument, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, f.GraphQL+" {") && !strings.HasPrefix(line, f.GraphQL+"(") {
			continue
		}
		if i := strings.Index(line, "{"); i >= 0 {
			return line[i:]
		}
	}
	return ""
}

// The keep-list is read by a person, so nothing on it may be dead: an entry
// for a field no concept has, for a field its kind already keeps, or for a
// concept no erasure reaches would say something the erasure does not do.
func TestTheKeepListNamesOnlyFieldsAnErasureWouldOtherwiseScrub(t *testing.T) {
	if len(erasureKeepList) == 0 {
		t.Fatal("the keep-list is empty: an erasure would scrub every order's totals")
	}
	reach := erasureReach()
	for concept, fields := range erasureKeepList {
		spec := generated.Types[concept]
		if spec == nil {
			t.Errorf("the keep-list names the concept %q, which the model does not have", concept)
			continue
		}
		if len(reach[concept].Links) == 0 {
			t.Errorf("the keep-list has entries for %q, which no erasure reaches", concept)
		}
		if concept == erasedConcept {
			t.Errorf("the keep-list has entries for %q: the customer's own row keeps nothing, and another customer's row is about a person", concept)
		}
		byName := map[string]generated.FieldSpec{}
		for _, f := range spec.Fields {
			byName[f.Name] = f
		}
		for field, reason := range fields {
			f, ok := byName[field]
			if !ok {
				t.Errorf("the keep-list names %s.%s, which the model does not have", concept, field)
				continue
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("the keep-list keeps %s.%s without saying why it is not about a person", concept, field)
			}
			if _, byKind := erasureKeptByKind(f); byKind {
				t.Errorf("the keep-list names %s.%s, which its kind keeps already: the entry does nothing", concept, field)
			}
			if d := erasureDecisionFor(concept, f, false); d.Fate != fateKept || d.ByList != reason {
				t.Errorf("%s.%s is on the keep-list and is decided %+v", concept, field, d)
			}
		}
	}
}

// The value a scrubbed field is left with has to be one its type accepts, or
// the mirror cannot write the row again: a raw insert merges the stored row
// under what it writes and validates the result (measured, ruling 81). A
// string takes the marker, a list becomes empty, and the key of anything else
// is removed.
func TestAScrubbedFieldIsLeftWithAValueItsTypeAccepts(t *testing.T) {
	cases := []struct {
		name string
		f    generated.FieldSpec
		want erasureFate
	}{
		{"a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "String"}, fateMarked},
		{"a date, carried as a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "DateTime"}, fateMarked},
		{"a reference, carried as a string", generated.FieldSpec{Kind: generated.KindGid, DSLType: "string", AdminType: "Order"}, fateMarked},
		{"a list of strings", generated.FieldSpec{Kind: generated.KindScalarList, DSLType: "[]string", AdminType: "String"}, fateEmptied},
		{"a list of objects", generated.FieldSpec{Kind: generated.KindObjectList, DSLType: "[]object", AdminType: "Attribute"}, fateEmptied},
		{"a list of references", generated.FieldSpec{Kind: generated.KindRefs, DSLType: "[]string", AdminType: "Order"}, fateEmptied},
		{"an object", generated.FieldSpec{Kind: generated.KindObject, DSLType: "object", AdminType: "MailingAddress"}, fateRemoved},
		{"metafields, an object keyed by namespace and key", generated.FieldSpec{Kind: generated.KindMetafields, DSLType: "object", AdminType: "Metafield"}, fateRemoved},
		{"a boolean", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "bool", AdminType: "Boolean"}, fateRemoved},
		{"an integer", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "int", AdminType: "Int"}, fateRemoved},
		{"a float", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "float", AdminType: "Float"}, fateRemoved},
		{"a JSON value", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "any", AdminType: "JSON"}, fateRemoved},
		{"an enum", generated.FieldSpec{Kind: generated.KindEnum, DSLType: `enum("A", "B")`, AdminType: "SomeState"}, fateRemoved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := erasureScrubFate(tc.f); got != tc.want {
				t.Errorf("scrubbed, the field is %q, want %q", got, tc.want)
			}
		})
	}
	// And every field the model has is of a type the erasure has been taught.
	// A type the generator starts to emit next quarter fails here instead of
	// being given whatever the last branch gives.
	for concept, spec := range generated.Types {
		for _, f := range spec.Fields {
			known := f.DSLType == "string" || f.DSLType == "bool" || f.DSLType == "int" || f.DSLType == "float" ||
				f.DSLType == "any" || f.DSLType == "object" || f.DSLType == "[]string" || f.DSLType == "[]object" ||
				strings.HasPrefix(f.DSLType, "enum(")
			if !known {
				t.Errorf("%s.%s is declared %q, a type the erasure has not been taught a value for", concept, f.Name, f.DSLType)
			}
		}
	}
}

// The customer's own row keeps nothing but its structural fields, whatever a
// field's kind: its totals and counts are about a person.
func TestTheCustomersOwnRowKeepsNothing(t *testing.T) {
	spec := generated.Types[erasedConcept]
	for _, f := range spec.Fields {
		if d := erasureDecisionFor(erasedConcept, f, true); d.Fate == fateKept {
			t.Errorf("the erased customer's own row keeps %s (%+v)", f.Name, d)
		}
	}
	plan := erasurePlanFor(spec, true)
	if strings.Join(plan.Keep, ",") != strings.Join(erasureStructuralFields, ",") {
		t.Errorf("the own row's plan keeps %v, want the structural fields and nothing else", plan.Keep)
	}
	for _, name := range []string{"defaultEmailAddress", "defaultPhoneNumber", "image", "tags", "metafields", "note", "firstName", "lastName", "displayName", "numberOfOrders", "amountSpent", "verifiedEmail"} {
		f, ok := fieldNamed(spec, name)
		if !ok {
			t.Errorf("the customer has no field %q; the model moved and this list has to follow it", name)
			continue
		}
		if d := erasureDecisionFor(erasedConcept, f, true); d.Fate == fateKept {
			t.Errorf("the erased customer's own row keeps %s", name)
		}
	}
}

func fieldNamed(spec *generated.TypeSpec, name string) (generated.FieldSpec, bool) {
	for _, f := range spec.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return generated.FieldSpec{}, false
}

// The order keeps the sale and loses the person; so do the rows that hang from
// it. Named here, beside the golden that lists every field, because these are
// the ones a person would ask about first.
func TestAnErasureKeepsTheSaleAndLosesThePerson(t *testing.T) {
	cases := map[string]struct{ kept, scrubbed []string }{
		"order": {
			kept:     []string{"totalPriceSet", "subtotalPriceSet", "currentTotalPriceSet", "totalTaxSet", "currencyCode", "name", "processedAt", "sourceCreatedAt", "cancelledAt", "customerGid", "taxLines", "discountCodes", "paymentGatewayNames", "displayFinancialStatus", "currentSubtotalLineItemsQuantity", "legacyResourceId", "taxExempt"},
			scrubbed: []string{"email", "phone", "billingAddress", "shippingAddress", "displayAddress", "note", "customAttributes", "clientIp", "customerLocale", "customerJourneySummary", "metafields", "tags", "shippingLine", "purchasingEntity", "statusPageUrl", "poNumber"},
		},
		"orderLineItem": {
			kept:     []string{"title", "variantTitle", "name", "sku", "quantity", "currentQuantity", "originalUnitPriceSet", "discountedTotalSet", "taxLines", "productGid", "variantGid"},
			scrubbed: []string{"customAttributes", "lineItemGroup", "image"},
		},
		"orderTransaction": {
			kept:     []string{"amountSet", "kind", "status", "gateway", "formattedGateway", "processedAt", "test", "orderGid"},
			scrubbed: []string{"paymentDetails", "accountNumber", "receiptJson", "paymentId", "device"},
		},
		"fulfillment": {
			kept:     []string{"status", "displayStatus", "deliveredAt", "totalQuantity", "name", "orderGid"},
			scrubbed: []string{"trackingInfo", "shippingLabel"},
		},
		"refund": {
			kept:     []string{"totalRefundedSet", "duties", "processedAt", "orderGid"},
			scrubbed: []string{"note"},
		},
		"fulfillmentOrder": {
			kept:     []string{"orderGid", "status", "orderName", "fulfillAt"},
			scrubbed: []string{"destination", "deliveryMethod"},
		},
		"tenderTransaction": {
			kept:     []string{"amount", "processedAt", "orderGid"},
			scrubbed: []string{"transactionDetails", "remoteReference"},
		},
		"shopifyPaymentsDispute": {
			kept:     []string{"amount", "status", "orderGid", "initiatedAt"},
			scrubbed: []string{"disputeEvidence"},
		},
		"draftOrder": {
			kept:     []string{"totalPriceSet", "currencyCode", "name", "customerGid", "orderGid"},
			scrubbed: []string{"email", "phone", "billingAddress", "shippingAddress", "note2", "customAttributes", "purchasingEntity", "shippingLine"},
		},
		"customerPaymentMethod": {kept: []string{"customerGid", "revokedAt"}, scrubbed: []string{"instrument"}},
		"storeCreditAccount":    {scrubbed: []string{"balance", "owner"}},
	}
	for concept, c := range cases {
		spec := generated.Types[concept]
		if spec == nil {
			t.Errorf("the model has no concept %q", concept)
			continue
		}
		check := func(names []string, wantKept bool) {
			for _, name := range names {
				f, ok := fieldNamed(spec, name)
				if !ok {
					t.Errorf("%s has no field %q; the model moved and this list has to follow it", concept, name)
					continue
				}
				d := erasureDecisionFor(concept, f, false)
				if wantKept && d.Fate != fateKept {
					t.Errorf("an erasure scrubs %s.%s (%s), which describes the sale", concept, name, d.Fate)
				}
				if !wantKept && d.Fate == fateKept {
					t.Errorf("an erasure keeps %s.%s (%+v), which is about the person", concept, name, d)
				}
			}
		}
		check(c.kept, true)
		check(c.scrubbed, false)
	}
}

// The reach, by the three links of ruling 83 and no other.
func TestTheReachOfAnErasureIsTheThreeLinks(t *testing.T) {
	reach := erasureReach()
	has := func(concept, way string) bool {
		for _, w := range reach[concept].Links {
			if w == way {
				return true
			}
		}
		return false
	}
	for concept, way := range map[string]string{
		"customer":               "the customer's row",
		"order":                  "an order the request names",
		"orderLineItem":          "child of order",
		"reverseDelivery":        "child of reverseFulfillmentOrder",
		"storeCreditAccount":     "child of customer",
		"fulfillmentOrder":       "gid field orderGid naming order",
		"tenderTransaction":      "gid field orderGid naming order",
		"shopifyPaymentsDispute": "gid field orderGid naming order",
		"draftOrder":             "gid field orderGid naming order",
		"abandonedCheckout":      "gid field customerGid naming customer",
		"companyContact":         "gid field customerGid naming customer",
		"giftCard":               "the customer inside object recipientAttributes",
	} {
		if !has(concept, way) {
			t.Errorf("%s is not reached as %q; it is reached as %v", concept, way, reach[concept].Links)
		}
	}
	// A list of references is not a link: an order's gid in a company's list
	// of its orders does not bring the company in.
	if has("company", "gid field ordersGids naming order") {
		t.Error("a company is reached through its list of orders, and a list of references is not a link")
	}
	for _, concept := range []string{"product", "productVariant", "collection", "location", "shop", "staffMember", "inventoryItem", "discountNode"} {
		if r := reach[concept]; len(r.Links) != 0 || len(r.Lists) != 0 {
			t.Errorf("an erasure reaches %s: %+v", concept, r)
		}
	}
	if len(generated.Types) != 65 {
		t.Logf("the model now has %d concepts; the golden lists each of them", len(generated.Types))
	}
}

// The one conditional rule (ruling 89). A line item with no product reference
// is a custom item, and its title is free text a person typed: an engraving, a
// name. So the three titles the keep-list keeps on a line of a product are
// scrubbed on a custom line, and nothing else in the policy depends on a row's
// values.
func TestACustomLineItemLosesTheTitlesAProductsLineKeeps(t *testing.T) {
	spec := generated.Types["orderLineItem"]
	conditional := map[string]bool{}
	for _, f := range spec.Fields {
		d := erasureDecisionFor("orderLineItem", f, false)
		if d.OnCustomItem != "" {
			conditional[f.Name] = true
			if d.Fate != fateKept || d.ByList == "" || d.OnCustomItem != fateMarked {
				t.Errorf("%s is decided %+v, want kept by the list on a line of a product and marked on a custom line", f.Name, d)
			}
		}
	}
	if got := strings.Join(sortedSet(conditional), ","); got != "name,title,variantTitle" {
		t.Errorf("the titles scrubbed on a custom item are %q, want name, title and variantTitle", got)
	}
	plan := erasurePlanFor(spec, false)
	if plan.CustomItemReference != "productGid" || strings.Join(sortedStrings(plan.MarkOnCustomItem), ",") != "name,title,variantTitle" {
		t.Errorf("the line item's plan marks %v on a row with no %q, want the three titles on a row with no productGid", plan.MarkOnCustomItem, plan.CustomItemReference)
	}
	for _, kept := range plan.MarkOnCustomItem {
		for _, k := range plan.Keep {
			if k == kept {
				t.Errorf("%s is both kept without condition and marked on a custom item", kept)
			}
		}
	}
	if _, ok := fieldNamed(spec, plan.CustomItemReference); !ok {
		t.Errorf("the line item has no field %q to tell a custom item by", plan.CustomItemReference)
	}
	// It is the ONE conditional rule.
	for concept, other := range generated.Types {
		for _, f := range other.Fields {
			if d := erasureDecisionFor(concept, f, false); d.OnCustomItem != "" && concept != "orderLineItem" {
				t.Errorf("%s.%s is decided by a row's values too: %+v", concept, f.Name, d)
			}
		}
		if p := erasurePlanFor(other, false); concept != "orderLineItem" && (p.CustomItemReference != "" || len(p.MarkOnCustomItem) != 0) {
			t.Errorf("%s has a conditional plan: %+v", concept, p)
		}
	}
}

// ErasureDecision is the policy as another package may read it: one answer for
// a concept and a field, in words that are part of its contract.
func TestTheExportedReadOfThePolicy(t *testing.T) {
	for _, tc := range []struct{ concept, field, want string }{
		{"order", "processedAt", "kept by kind"},
		{"order", "customerGid", "kept by kind"},
		{"order", "displayFinancialStatus", "kept by kind"},
		{"order", "currentSubtotalLineItemsQuantity", "kept by kind"},
		{"order", "totalPriceSet", "kept by the list"},
		{"order", "currencyCode", "kept by the list"},
		{"order", "name", "kept by the list"},
		{"order", "email", "scrubbed"},
		{"order", "billingAddress", "scrubbed"},
		{"order", "tags", "scrubbed"},
		{"orderLineItem", "title", "kept, scrubbed on a custom item"},
		{"orderLineItem", "variantTitle", "kept, scrubbed on a custom item"},
		{"orderLineItem", "name", "kept, scrubbed on a custom item"},
		{"orderLineItem", "sku", "kept by the list"},
		{"orderLineItem", "quantity", "kept by kind"},
		{"order", "updatedAt", "kept as structure"},
		{"order", "gid", "kept as structure"},
		{"order", "storeId", "kept as structure"},
		{"orderLineItem", "parentGid", "kept as structure"},
		{"order", "parentGid", "no such field"},
		// The erased customer's own row keeps nothing but its structure.
		{"customer", "numberOfOrders", "scrubbed"},
		{"customer", "verifiedEmail", "scrubbed"},
		{"customer", "lastOrderGid", "scrubbed"},
		{"customer", "gid", "kept as structure"},
		{"order", "noSuchField", "no such field"},
		{"noSuchConcept", "name", "no such field"},
		{"v1:shopify:order", "totalPriceSet", "kept by the list"},
	} {
		if got := ErasureDecision(tc.concept, tc.field); got != tc.want {
			t.Errorf("ErasureDecision(%q, %q) = %q, want %q", tc.concept, tc.field, got, tc.want)
		}
	}
	// It agrees with the policy the erasure runs on, for every field there is.
	for concept, spec := range generated.Types {
		for _, f := range spec.Fields {
			d := erasureDecisionFor(concept, f, concept == erasedConcept)
			want := "scrubbed"
			switch {
			case d.OnCustomItem != "":
				want = "kept, scrubbed on a custom item"
			case d.ByKind != "":
				want = "kept by kind"
			case d.ByList != "":
				want = "kept by the list"
			}
			if got := ErasureDecision(concept, f.Name); got != want {
				t.Errorf("ErasureDecision(%q, %q) = %q, and the erasure decides %q", concept, f.Name, got, want)
			}
		}
	}
}

// Every field of the model says what its value is. The erasure keeps a date
// and scrubs a name, and both are strings in the mirror: a field that reached
// the model without its Admin type would be scrubbed as a string of no known
// sort, whatever it holds.
func TestEveryFieldOfTheModelCarriesItsAdminType(t *testing.T) {
	for concept, spec := range generated.Types {
		for _, f := range spec.Fields {
			if f.AdminType == "" {
				t.Errorf("%s.%s carries no Admin type: the generator emitted a field without saying what its value is (cmd/shopifyschema, planField)", concept, f.Name)
			}
			if len(f.GidsInside) > 0 && f.Kind != generated.KindObject && f.Kind != generated.KindObjectList {
				t.Errorf("%s.%s is a %s and says it keeps gids inside: only an object or a list of objects can", concept, f.Name, f.Kind)
			}
		}
	}
}
