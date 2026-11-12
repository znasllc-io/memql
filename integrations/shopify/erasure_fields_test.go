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
	conceptOf := map[string]string{}
	concepts := make([]string, 0, len(generated.Types))
	for concept, spec := range generated.Types {
		concepts = append(concepts, concept)
		conceptOf[spec.GraphQLType] = concept
	}
	sort.Strings(concepts)
	for _, concept := range concepts {
		spec := generated.Types[concept]
		fmt.Fprintf(&b, "\nconcept %s (%s)\n", concept, spec.GraphQLType)
		r := reach[concept]
		switch {
		case len(r.Links) > 0:
			fmt.Fprintf(&b, "  reach: %s\n", strings.Join(r.Links, "; "))
		case len(r.TakenOut) > 0:
			fmt.Fprintf(&b, "  reach: not in scope; an entry that names the customer is taken out of %s and nothing else in the row changes\n", strings.Join(r.TakenOut, ", "))
		default:
			b.WriteString("  reach: not reachable\n")
		}
		if len(r.Links) > 0 && len(r.TakenOut) > 0 {
			fmt.Fprintf(&b, "  a row that is not in scope has an entry that names the customer taken out of %s\n", strings.Join(r.TakenOut, ", "))
		}
		if concept == erasedConcept {
			b.WriteString("  every customer row in scope is the person's own (the erased customer's, or an earlier record merged into it) and keeps nothing but its structural fields\n")
		}
		for _, f := range spec.Fields {
			d := erasureDecisionFor(concept, f, false)
			fmt.Fprintf(&b, "  %-40s %-11s %-34s %s", f.Name, f.Kind, f.AdminType, describeErasure(d))
			if note := describeLink(concept, f, conceptOf, reach); note != "" {
				b.WriteString("; " + note)
			}
			b.WriteString("\n")
			// What a kept object or list holds is part of what is kept, so
			// it is part of what is reviewed: a field Shopify adds INSIDE it
			// shows here. Money is one fixed shape and is not repeated.
			if d.ByList != "" && f.AdminType != "MoneyV2" && f.AdminType != "MoneyBag" {
				if sel := selectionOf(spec, f); sel != "" {
					fmt.Fprintf(&b, "      holds %s\n", sel)
				}
			}
		}
	}
	return b.String()
}

// describeLink says, of a gid field and of an object that can keep a
// customer's gid, what it means for the reach: every one of them is decided,
// and the golden shows the decision beside the field.
func describeLink(concept string, f generated.FieldSpec, conceptOf map[string]string, reach map[string]conceptReach) string {
	switch f.Kind {
	case generated.KindGid:
		named := conceptOf[f.AdminType]
		if not, ok := erasureNotALink[concept][f.Name]; ok {
			note := "NOT A LINK: " + not.Why
			if not.FromADeletedRow != "" {
				note += "; A LINK FROM A DELETED ROW: " + not.FromADeletedRow
			}
			return note
		}
		if len(reach[named].Links) > 0 {
			return "a link: a row that names an in-scope " + named + " here is in scope"
		}
		return "a link that reaches nothing: no erasure reaches a " + named
	case generated.KindObject, generated.KindObjectList:
		if why, ok := erasureCustomerInside[concept][f.Name]; ok {
			return "THE CUSTOMER'S GID INSIDE IT BRINGS THE ROW INTO SCOPE: " + why
		}
		if why, ok := erasureCustomerTakenOut[concept][f.Name]; ok {
			return "AN ENTRY THAT NAMES THE CUSTOMER IS TAKEN OUT, AND THE ROW IS LEFT: " + why
		}
	case generated.KindRefs:
		if f.AdminType == generated.Types[erasedConcept].GraphQLType {
			return "the customer's gid is taken out of the list in a row that is not in scope"
		}
	}
	return ""
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
		"# point by these links and no other:\n" +
		"#   child of X              the row hangs from an in-scope row of X by the model's parent link\n" +
		"#   gid field F naming X    the row holds an in-scope row of X's gid as the whole value of its gid field F.\n" +
		"#                           Every gid field is such a link unless its line below says NOT A LINK. One of\n" +
		"#                           them is a link \"from a deleted row only\": it is followed from a row whose\n" +
		"#                           newest version is a tombstone, and from no other\n" +
		"#   the customer inside object F\n" +
		"#                           the row holds an in-scope customer's gid as an id inside its object F, and F\n" +
		"#                           is one of the few objects that say who a row is for. Her gid inside any other\n" +
		"#                           object, or as a free value anywhere (a custom attribute, a note, a message in\n" +
		"#                           that same object), is not a link: nothing is matched by value\n" +
		"# A list of references is not a link, and neither is an object that only refers to the customer (a\n" +
		"# metaobject's fields): in a row that is not in scope the entry that names her is taken out -- an entry\n" +
		"# that holds her gid as a whole value anywhere inside it -- and nothing else in the row changes.\n" +
		"#\n" +
		"# A ROW IS DECIDED ON WHAT IT IS NOW. A row is in scope, with everything that hangs from it, when its\n" +
		"# NEWEST version holds a link; the data request exports exactly those rows, each at its newest version.\n" +
		"# An OLDER version that holds the customer's gid in a link field, on a row whose newest version does not\n" +
		"# (an order since moved to another person), is scrubbed by itself: that version only, not the row's\n" +
		"# newest version and not what hangs from the row.\n" +
		"#\n" +
		"# AN ORDER THE REQUEST NAMES is a seed only when its newest version names the customer or names no\n" +
		"# customer. One that names another customer now is that person's: it is decided as a row that moved.\n" +
		"#\n" +
		"# A DELETED CUSTOMER ROW WHOSE LAST ORDER IS IN SCOPE is scrubbed BY ITSELF: nothing is reached from it\n" +
		"# (not its orders, not what names it) and it is never in an export. It may be a record merged into\n" +
		"# the customer, or another deleted person whose stale pointer names an order that is hers now.\n" +
		"#\n" +
		"# TWO THINGS AN ERASURE DOES NOT REACH, accepted in writing:\n" +
		"#   - a row that carries the person only by an email address, with no customer on it: a guest's\n" +
		"#     abandoned checkout\n" +
		"#   - the buyer's message inside a gift card another person holds: the card is its holder's\n" +
		"#\n" +
		"# FIELDS. Every version of a row in scope is rewritten. The structural fields are never touched:\n" +
		"#   " + strings.Join(erasureStructuralFields, ", ") + "\n" +
		"# Every other field, and any key the model does not know, is decided below:\n" +
		"#   kept by kind            a gid, a list of references, an enum, or a scalar whose Admin type is one of:\n" +
		wrapComment("#                           ", kept) +
		"#   kept by the list        on the keep-list of erasure_fields.go, with the reason it describes the sale\n" +
		"#   scrubbed                everything else: a string of free text becomes \"" + RedactionMarker + "\", a list becomes [], and\n" +
		"#                           the key of anything else is removed (an object, a number, a boolean, an enum, and a\n" +
		"#                           string that is a date, a number or a reference), so a typed read answers null, a reader\n" +
		"#                           of dates never meets the marker, and the mirror can write the row again\n" +
		"#   scrubbed although kept by kind\n" +
		"#                           the person's own, on a row that is not her customer row; each says why\n" +
		"#   kept by the list; scrubbed on a custom item\n" +
		"#                           the one decision that looks at a row's values: a line item (" + erasureCustomItem.Concept + ") with no\n" +
		"#                           " + erasureCustomItem.Reference + " is a custom item, whose titles and SKU are what a person typed. It is\n" +
		"#                           decided version by version\n" +
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
	case d.AlthoughKept != "":
		return "scrubbed although kept by kind (" + d.AlthoughKept + "): " + describeScrub(d.Fate)
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
		{"a string of free text", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "String"}, fateMarked},
		{"a URL", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "URL"}, fateMarked},
		{"an enum too long to be one, carried as a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "CurrencyCode"}, fateMarked},
		// A string that is not free text loses its key: a reader comparing
		// dates, or following a reference, must never meet the marker.
		{"a date-time, carried as a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "DateTime"}, fateRemoved},
		{"a date, carried as a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "Date"}, fateRemoved},
		{"a number the API writes as a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "UnsignedInt64"}, fateRemoved},
		{"a decimal the API writes as a string", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "Decimal"}, fateRemoved},
		{"an ID", generated.FieldSpec{Kind: generated.KindScalar, DSLType: "string", AdminType: "ID"}, fateRemoved},
		{"a reference, carried as a string", generated.FieldSpec{Kind: generated.KindGid, DSLType: "string", AdminType: "Order"}, fateRemoved},
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
	// Only free text takes the marker. A date, a count the API writes as a
	// string and a reference lose their keys.
	if got := strings.Join(sortedStrings(plan.Mark), ","); got != "displayName,firstName,lastName,lifetimeDuration,locale,multipassIdentifier,note" {
		t.Errorf("the own row's plan marks %s, want the free-text strings and no date, count or reference", got)
	}
	// A customer's row comes into scope as the erased customer's own, or as
	// an earlier record of the same person that was merged into it. Either is
	// the person's, so there is one plan for the concept and not two.
	other := erasurePlanFor(spec, false)
	if strings.Join(other.Keep, ",") != strings.Join(plan.Keep, ",") || strings.Join(other.Mark, ",") != strings.Join(plan.Mark, ",") || strings.Join(other.Empty, ",") != strings.Join(plan.Empty, ",") {
		t.Errorf("a customer's row that is not the erased one's own is planned %+v, want it scrubbed as the own row is: %+v", other, plan)
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

// reviewedGidLinks is every gid field of the model that IS a link: a row that
// holds an in-scope row's gid in it is in scope. A gid field is one of these
// or is on the not-a-link list of erasure_fields.go, and a field a
// regeneration adds is on neither: the test below fails until a person puts
// it on one. Most name a row no erasure reaches (a product, a location) and
// so reach nothing; they are decided all the same, because what an erasure
// reaches can change under them.
var reviewedGidLinks = []string{
	"abandonedCheckout.customerGid", "article.blogGid", "articleComment.articleGid", "catalog.priceListGid",
	"catalog.publicationGid", "companyContact.customerGid", "customerPaymentMethod.customerGid",
	"draftOrder.customerGid", "draftOrder.orderGid", "fulfillment.locationGid", "fulfillment.orderGid",
	"fulfillmentOrder.orderGid", "giftCard.customerGid", "inventoryLevel.itemGid",
	"inventoryLevel.locationGid", "marketingActivity.marketingEventGid", "metaobject.createdByStaffGid",
	"metaobject.definitionGid", "metaobjectDefinition.createdByStaffGid", "order.customerGid",
	"order.merchantBusinessEntityGid", "order.publicationGid", "order.retailLocationGid",
	"order.staffMemberGid", "orderLineItem.productGid", "orderLineItem.staffMemberGid",
	"orderLineItem.variantGid", "orderTransaction.locationGid", "orderTransaction.orderGid",
	"orderTransaction.parentTransactionGid", "orderTransaction.userGid", "priceList.catalogGid",
	"priceListPrice.variantGid", "productVariant.deliveryProfileGid", "productVariant.inventoryItemGid",
	"productVariant.productGid", "publication.catalogGid", "quantityRule.productVariantGid",
	"refund.orderGid", "refund.sourceReturnGid", "refund.staffMemberGid",
	"reverseDelivery.reverseFulfillmentOrderGid", "reverseFulfillmentOrder.orderGid", "shop.accountOwnerGid",
	"shopifyPaymentsDispute.orderGid", "shopifyPaymentsPayout.businessEntityGid", "shopifyReturn.orderGid",
	"shopifyReturn.staffMemberGid", "subscriptionContract.customerGid",
	"subscriptionContract.customerPaymentMethodGid", "subscriptionContract.originOrderGid",
	"tenderTransaction.orderGid", "tenderTransaction.userGid",
}

// Every gid field of the model is a link or is said not to be one (ruling
// 91). With every gid field a link, a customer whose row named the erased
// customer's order as its last order was scrubbed, and exported to her.
func TestEveryGidFieldIsALinkOrIsSaidNotToBe(t *testing.T) {
	reviewed := map[string]bool{}
	for _, field := range reviewedGidLinks {
		if reviewed[field] {
			t.Errorf("%s is listed twice among the reviewed links", field)
		}
		reviewed[field] = true
	}
	inModel := map[string]bool{}
	for concept, spec := range generated.Types {
		for _, f := range spec.Fields {
			if f.Kind != generated.KindGid {
				continue
			}
			name := concept + "." + f.Name
			inModel[name] = true
			not, isNot := erasureNotALink[concept][f.Name]
			switch {
			case isNot && reviewed[name]:
				t.Errorf("%s is both a reviewed link and on the not-a-link list", name)
			case isNot && strings.TrimSpace(not.Why) == "":
				t.Errorf("%s is on the not-a-link list without the reason", name)
			case !isNot && !reviewed[name]:
				t.Errorf("%s is a gid field nobody has decided: put it on reviewedGidLinks (a row that names an in-scope %s is in scope) or on erasureNotALink with the reason", name, f.AdminType)
			}
		}
	}
	for name := range reviewed {
		if !inModel[name] {
			t.Errorf("the reviewed links name %s, which is not a gid field of the model", name)
		}
	}
	for concept, fields := range erasureNotALink {
		for field := range fields {
			if !inModel[concept+"."+field] {
				t.Errorf("the not-a-link list names %s.%s, which is not a gid field of the model", concept, field)
			}
		}
	}
	// The list, exactly: a change to it is a change to whose rows an erasure
	// takes.
	var not []string
	for concept, fields := range erasureNotALink {
		for field, entry := range fields {
			name := concept + "." + field
			if entry.FromADeletedRow != "" {
				name += " (a link from a deleted row)"
			}
			not = append(not, name)
		}
	}
	want := []string{
		"company.mainContactGid", "companyContact.companyGid", "companyLocation.companyGid",
		"customer.lastOrderGid (a link from a deleted row)", "giftCard.lineItemGid", "giftCard.orderGid", "orderLineItem.contractGid",
	}
	if got := sortedStrings(not); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the not-a-link list is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The customer's gid inside an object is a link only in the fields a reviewed
// list names, never by value anywhere: an order with a custom attribute whose
// value was the erased customer's gid was scrubbed with its transaction, and
// exported to her. Every object the generated model says can keep a
// customer's gid is on one of two lists: the row is about that person, or the
// entry that names her is taken out and the row is left.
func TestEveryObjectThatCanHoldACustomerIsDecided(t *testing.T) {
	customerType := generated.Types[erasedConcept].GraphQLType
	inModel := map[string]bool{}
	for concept, spec := range generated.Types {
		for _, f := range spec.Fields {
			holds := false
			for _, inside := range f.GidsInside {
				holds = holds || inside == customerType
			}
			if !holds {
				continue
			}
			name := concept + "." + f.Name
			inModel[name] = true
			link, isLink := erasureCustomerInside[concept][f.Name]
			out, isOut := erasureCustomerTakenOut[concept][f.Name]
			switch {
			case isLink && isOut:
				t.Errorf("%s is on both object lists", name)
			case !isLink && !isOut:
				t.Errorf("%s can keep a customer's gid and nobody has decided what that means: put it on erasureCustomerInside (the row is about her) or on erasureCustomerTakenOut (the entry goes, the row stays), with the reason", name)
			case strings.TrimSpace(link+out) == "":
				t.Errorf("%s is decided without the reason", name)
			}
		}
	}
	var links, outs []string
	for concept, fields := range erasureCustomerInside {
		for field := range fields {
			links = append(links, concept+"."+field)
			if !inModel[concept+"."+field] {
				t.Errorf("erasureCustomerInside names %s.%s, which the model does not say can keep a customer's gid", concept, field)
			}
		}
	}
	for concept, fields := range erasureCustomerTakenOut {
		for field := range fields {
			outs = append(outs, concept+"."+field)
			// A free-form value is on this list too; the model cannot say
			// what it holds (TestEveryFreeFormValueIsDecided).
			if spec := generated.Types[concept]; spec != nil {
				for _, f := range spec.Fields {
					if f.Name == field && f.Kind == generated.KindScalar && f.AdminType == "JSON" {
						inModel[concept+"."+field] = true
					}
				}
			}
			if !inModel[concept+"."+field] {
				t.Errorf("erasureCustomerTakenOut names %s.%s, which the model does not say can keep a customer's gid", concept, field)
			}
		}
	}
	if got := strings.Join(sortedStrings(links), ","); got != "draftOrder.purchasingEntity,giftCard.recipientAttributes,order.purchasingEntity" {
		t.Errorf("the objects that make a row hers are %s", got)
	}
	if got := strings.Join(sortedStrings(outs), ","); got != "metaobject.fields,metaobject.thumbnailField,metaobject.values" {
		t.Errorf("the objects an entry is taken out of are %s", got)
	}
}

// A free-form value can hold anything, a customer's gid included, and the
// generated model cannot say what is inside one: its Admin type is JSON. So
// each such field is decided by a person: an entry that names her is taken
// out of it (erasureCustomerTakenOut), or it is said why it is left
// (erasureFreeFormLeft). A regenerated model that adds one fails here until
// somebody decides. A metaobject's values was the one nobody had decided: it
// holds the object's fields a second time, and kept her gid after her erasure.
func TestEveryFreeFormValueIsDecided(t *testing.T) {
	seen := map[string]bool{}
	for concept, spec := range generated.Types {
		for _, f := range spec.Fields {
			if f.Kind != generated.KindScalar || f.AdminType != "JSON" {
				continue
			}
			name := concept + "." + f.Name
			seen[name] = true
			_, out := erasureCustomerTakenOut[concept][f.Name]
			why, left := erasureFreeFormLeft[concept][f.Name]
			switch {
			case out && left:
				t.Errorf("%s is both taken out of and said to be left", name)
			case !out && !left:
				t.Errorf("%s is a free-form value nobody has decided: put it on erasureCustomerTakenOut (an entry that names her is taken out) or on erasureFreeFormLeft with the reason", name)
			case left && strings.TrimSpace(why) == "":
				t.Errorf("%s is left without the reason", name)
			}
		}
	}
	for concept, fields := range erasureFreeFormLeft {
		for field := range fields {
			if !seen[concept+"."+field] {
				t.Errorf("erasureFreeFormLeft names %s.%s, which is not a free-form value of the model", concept, field)
			}
		}
	}
	if len(seen) != 2 || !seen["metaobject.values"] || !seen["orderTransaction.receiptJson"] {
		t.Errorf("the model's free-form values are %v; this test was written for a metaobject's values and a transaction's receipt", seen)
	}
}

// The reach, by the links of ruling 91 and no other.
func TestTheReachOfAnErasureIsItsLinks(t *testing.T) {
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
	// No path from a customer reaches a company, a company's location, or
	// another person's customer row.
	for _, concept := range []string{"company", "companyLocation"} {
		if r := reach[concept]; len(r.Links) != 0 {
			t.Errorf("an erasure reaches %s: %v", concept, r.Links)
		}
	}
	if got := strings.Join(reach["companyContact"].Links, "; "); got != "gid field customerGid naming customer" {
		t.Errorf("a company's contact is reached as %q, want only as the erased person's own contact row", got)
	}
	if got := strings.Join(reach["customer"].Links, "; "); got != "the customer's row; gid field lastOrderGid naming order, from a deleted row only, which is scrubbed by itself" {
		t.Errorf("a customer's row is reached as %q, want only as the erased customer's own and as an earlier record merged into it", got)
	}
	if got := strings.Join(reach["giftCard"].Links, "; "); got != "gid field customerGid naming customer; the customer inside object recipientAttributes" {
		t.Errorf("a gift card is reached as %q, want only as its holder's or its recipient's", got)
	}
	if got := strings.Join(reach["orderLineItem"].Links, "; "); got != "child of order" {
		t.Errorf("a line item is reached as %q, want only as a line of its order", got)
	}
	// An entry that names her is taken out of a metaobject, and the
	// metaobject is not hers.
	if r := reach["metaobject"]; len(r.Links) != 0 || strings.Join(r.TakenOut, ",") != "fields,thumbnailField,values" {
		t.Errorf("a metaobject is reached as %+v, want no link and an entry taken out of fields and thumbnailField", r)
	}
	for _, concept := range []string{"product", "productVariant", "collection", "location", "shop", "staffMember", "inventoryItem", "discountNode"} {
		if r := reach[concept]; len(r.Links) != 0 || len(r.TakenOut) != 0 {
			t.Errorf("an erasure reaches %s: %+v", concept, r)
		}
	}
	reachable := 0
	for _, r := range reach {
		if len(r.Links) > 0 {
			reachable++
		}
	}
	if reachable != 19 {
		t.Errorf("an erasure reaches %d concepts, want 19: the customer, the order and its tree, and the rows of either", reachable)
	}
}

// The reach is told once. The scope's statements and the golden are both made
// from erasureLinksOf, so what the golden says an erasure can reach is what
// the scope looks for.
func TestTheLinksAreEnumeratedOnce(t *testing.T) {
	order := erasureLinksOf(generated.Types["order"])
	var got []string
	for _, l := range order {
		got = append(got, l.Kind+" "+l.Field+" -> "+l.Names)
	}
	if strings.Join(got, "; ") != "gid customerGid -> customer; object purchasingEntity -> customer" {
		t.Errorf("an order's links are %q: a reference to staff, a location or a publication names a row no erasure reaches and is left out", strings.Join(got, "; "))
	}
	customer := erasureLinksOf(generated.Types["customer"])
	if len(customer) != 1 || customer[0].Field != "lastOrderGid" || !customer[0].FromADeletedRow || customer[0].Names != "order" {
		t.Errorf("a customer's links are %+v, want its last order, from a deleted row only", customer)
	}
	if links := erasureLinksOf(generated.Types["company"]); len(links) != 0 {
		t.Errorf("a company has links: %+v", links)
	}
	if links := erasureLinksOf(generated.Types["companyLocation"]); len(links) != 0 {
		t.Errorf("a company's location has links: %+v", links)
	}
	line := erasureLinksOf(generated.Types["orderLineItem"])
	if len(line) != 1 || line[0].Kind != "parent" || line[0].Names != "order" {
		t.Errorf("a line item's links are %+v, want its order and nothing else", line)
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
	if got := strings.Join(sortedSet(conditional), ","); got != "name,sku,title,variantTitle" {
		t.Errorf("the fields scrubbed on a custom item are %q, want the three titles and the SKU, which somebody typed too", got)
	}
	plan := erasurePlanFor(spec, false)
	if plan.CustomItemReference != "productGid" || strings.Join(sortedStrings(plan.MarkOnCustomItem), ",") != "name,sku,title,variantTitle" {
		t.Errorf("the line item's plan marks %v on a row with no %q, want the three titles and the SKU on a row with no productGid", plan.MarkOnCustomItem, plan.CustomItemReference)
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
		{"orderLineItem", "sku", "kept, scrubbed on a custom item"},
		// Her consent, as a boolean on the order: scrubbed although its kind
		// would keep it.
		{"order", "customerAcceptsMarketing", "scrubbed"},
		{"orderLineItem", "quantity", "kept by kind"},
		{"order", "updatedAt", "kept as structure"},
		{"order", "gid", "kept as structure"},
		{"order", "storeId", "kept as structure"},
		// The account a row belongs to is the engine's own stamp, on every
		// mirror concept: structure, on the erased customer's own row too.
		{"order", "accountId", "kept as structure"},
		{"customer", "accountId", "kept as structure"},
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

// A short list of fields is scrubbed although their kind would keep them: a
// boolean or a date that is the person's own, on a row that is not her
// customer row. Each says why, and each is a field its kind would keep.
func TestSomeFieldsAreScrubbedAlthoughTheirKindWouldKeepThem(t *testing.T) {
	var listed []string
	for concept, fields := range erasureScrubbedAlthoughKept {
		spec := generated.Types[concept]
		if spec == nil {
			t.Errorf("the list names the concept %q, which the model does not have", concept)
			continue
		}
		for field, why := range fields {
			listed = append(listed, concept+"."+field)
			f, ok := fieldNamed(spec, field)
			if !ok {
				t.Errorf("the list names %s.%s, which the model does not have", concept, field)
				continue
			}
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s.%s is scrubbed although kept by kind, and nothing says why", concept, field)
			}
			if _, byKind := erasureKeptByKind(f); !byKind {
				t.Errorf("%s.%s is on the list and its kind does not keep it: the entry does nothing", concept, field)
			}
			if d := erasureDecisionFor(concept, f, false); d.Fate == fateKept || d.AlthoughKept != why {
				t.Errorf("%s.%s is decided %+v, want scrubbed for the reason on the list", concept, field, d)
			}
		}
	}
	if got := strings.Join(sortedStrings(listed), ","); got != "order.customerAcceptsMarketing" {
		t.Errorf("the fields scrubbed although their kind would keep them are %s", got)
	}
	f, _ := fieldNamed(generated.Types["order"], "customerAcceptsMarketing")
	if got := erasureDecisionFor("order", f, false).Fate; got != fateRemoved {
		t.Errorf("her consent on an order is %q, want its key removed", got)
	}
}
