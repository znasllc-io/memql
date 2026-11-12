package shopify

import (
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// erasure_fields.go -- what a customer's erasure reaches, and what it leaves
// in a row it reaches.
//
// # Default deny
//
// The mirror is generated from Shopify's schema, which marks nothing as
// personal. So the rule runs the other way round: a field SURVIVES an erasure
// only when its kind says it cannot hold a person's words, or when it is on
// the keep-list below with the reason it describes the sale and not the
// person. Everything else goes, and a field a regeneration adds next quarter
// goes too until somebody decides otherwise.
//
// # The gate
//
// testdata/erasure-fields.golden lists every concept, how an erasure can come
// to a row of it, and every field with its decision. erasure_fields_test.go
// renders it again from the generated model and this file and fails on any
// difference: a regeneration cannot change what an erasure does without a
// person reading the diff. The old field list named such a test in its comment
// and had none, and the customer's email address and phone number were on
// neither.

// erasedConcept is the concept of the row an erasure is about.
const erasedConcept = "customer"

// requestedConcept is the concept of the rows a privacy request can name
// beside the customer (orders_to_redact, orders_requested).
const requestedConcept = "order"

// erasureFate is what an erasure does with one field of a row in its scope.
type erasureFate string

const (
	// fateKept leaves the field as it is.
	fateKept erasureFate = "kept"
	// fateMarked is a string's: it becomes RedactionMarker.
	fateMarked erasureFate = "marked"
	// fateEmptied is a list's: it becomes the empty list.
	fateEmptied erasureFate = "emptied"
	// fateRemoved is every other type's: the key is taken out of the
	// payload, and a typed read answers null for it.
	fateRemoved erasureFate = "removed"
)

// erasureDecision is the decision for one field, with the reason a kept field
// is kept.
type erasureDecision struct {
	Fate erasureFate
	// ByKind is why the field's kind keeps it ("a date-time"), ByList why
	// the keep-list does. Both are empty for a scrubbed field.
	ByKind string
	ByList string
	// OnCustomItem is set for a field that is kept on a line of a product
	// and scrubbed on a custom line (erasureCustomItem): what it is left as
	// there.
	OnCustomItem erasureFate
	// AlthoughKept is why a field its kind would keep is scrubbed all the
	// same (erasureScrubbedAlthoughKept). Empty for every other field.
	AlthoughKept string
}

// erasureStructuralFields are the fields every mirror row carries for itself
// (the mapper writes them; the generator's mirrorOwnFields is the same list).
// An erasure never touches them: they say which row this is and when it was
// read, and nothing about a person.
var erasureStructuralFields = []string{
	"storeId", "gid", "updatedAt", "versionSource", "syncedAt", "versionReadAt", "deleted", "parentGid",
}

// erasureKeptScalars are the Admin scalar types a field is kept for, with what
// each is. A value of one of them is a figure or a moment, not a person's
// words. The list is CLOSED: a scalar type that is not on it (String, URL,
// HTML, JSON, an enum too long to be carried as one, and whatever Shopify
// names next) is scrubbed unless the field is on the keep-list. Several of
// these are carried as strings in the mirror (generated.FieldSpec.DSLType),
// which is why the Admin type is read and not the declared one.
var erasureKeptScalars = map[string]string{
	"Boolean":       "a boolean",
	"Int":           "an integer",
	"BigInt":        "an integer",
	"Float":         "a float",
	"Decimal":       "a decimal",
	"Money":         "a money amount",
	"UnsignedInt64": "an unsigned 64-bit number",
	"Date":          "a date",
	"DateTime":      "a date-time",
	"ID":            "an ID",
}

// erasureKeptByKind reports whether a field's kind keeps it in a row that is
// not the erased customer's own, and why.
func erasureKeptByKind(f generated.FieldSpec) (string, bool) {
	switch f.Kind {
	case generated.KindGid:
		return "a reference to another row", true
	case generated.KindRefs:
		return "a list of references to other rows", true
	case generated.KindEnum:
		return "an enum", true
	case generated.KindScalar:
		why, ok := erasureKeptScalars[f.AdminType]
		return why, ok
	}
	return "", false
}

// erasureScrubFate is what a scrubbed field is left as: a value its declared
// type accepts. It has to be one. A raw insert on an existing row merges the
// stored payload under what it writes and validates the result, and the mapper
// leaves out an object, a number, a boolean or an enum the origin answers null
// for; so a null, or the string marker, stored in such a field made every
// later write of the row fail ("expected object, but got null"), which is what
// the old erasure left behind in every address it scrubbed.
//
// ONLY FREE TEXT TAKES THE MARKER. A string that is not free text -- a date, a
// number the API writes as a string, a reference -- loses its key like a
// number does: a reader that compares dates, sums a count or follows a
// reference must never meet "[redacted]" where it expects one.
func erasureScrubFate(f generated.FieldSpec) erasureFate {
	switch {
	case f.DSLType == "string":
		if f.Kind == generated.KindGid || (f.Kind == generated.KindScalar && erasureKeptScalars[f.AdminType] != "") {
			return fateRemoved
		}
		return fateMarked
	case strings.HasPrefix(f.DSLType, "[]"):
		return fateEmptied
	}
	return fateRemoved
}

// erasureDecisionFor decides one field of a row in an erasure's scope. own is
// set for the erased customer's own row, which keeps nothing: its totals and
// counts are about a person.
//
// EVERY ROW OF THE CUSTOMER CONCEPT IS DECIDED AS THE OWN ROW. A customer's
// row comes into scope as the erased customer's own, or as an earlier record
// of the same person that was merged into it (erasureNotALink, the last
// order); another person's customer row is never in scope. So there is one
// answer for the concept, and a caller that passes own false for it is told
// the same.
func erasureDecisionFor(concept string, f generated.FieldSpec, own bool) erasureDecision {
	if own || concept == erasedConcept {
		return erasureDecision{Fate: erasureScrubFate(f)}
	}
	if why, ok := erasureScrubbedAlthoughKept[concept][f.Name]; ok {
		return erasureDecision{Fate: erasureScrubFate(f), AlthoughKept: why}
	}
	if why, ok := erasureKeptByKind(f); ok {
		return erasureDecision{Fate: fateKept, ByKind: why}
	}
	if why, ok := erasureKeepList[concept][f.Name]; ok {
		d := erasureDecision{Fate: fateKept, ByList: why}
		if concept == erasureCustomItem.Concept {
			for _, typed := range erasureCustomItem.Fields {
				if typed == f.Name {
					d.OnCustomItem = erasureScrubFate(f)
				}
			}
		}
		return d
	}
	return erasureDecision{Fate: erasureScrubFate(f)}
}

// erasureScrubbedAlthoughKept is the fields an erasure scrubs although their
// kind would keep them: a boolean or a date that is the person's own, on a row
// that is not her customer row. Short on purpose, and each entry says why.
var erasureScrubbedAlthoughKept = map[string]map[string]string{
	"order": {
		"customerAcceptsMarketing": "the person's consent to marketing, as the order recorded it",
	},
}

// erasureCustomItem is the ONE rule of the policy that looks at a row's
// values. A line item with no product reference is a custom item, and what
// names it is free text a person typed: an engraving, a name, a made-up SKU.
// So whatever the keep-list says of a line's titles and its SKU, a line whose
// Reference is empty has them scrubbed as well. It is decided version by
// version: the version of a line that names a product keeps them, and one
// that names none loses them.
//
// The four fields are free-text strings (a test holds that), so on a custom
// item each becomes the marker.
var erasureCustomItem = struct {
	Concept, Reference string
	Fields             []string
}{Concept: "orderLineItem", Reference: "productGid", Fields: []string{"title", "name", "variantTitle", "sku"}}

// erasurePlan is the decisions of one concept as the scrub applies them: the
// payload keys it leaves, those it marks and those it empties. Every other
// key of the payload is removed, a key the model does not know included.
type erasurePlan struct {
	Keep  []string
	Mark  []string
	Empty []string
	// MarkOnCustomItem are the keys kept in a row that holds a value in
	// CustomItemReference and marked in a row that does not.
	MarkOnCustomItem    []string
	CustomItemReference string
}

// erasurePlanFor is a concept's plan, for the customer's own row or any other.
func erasurePlanFor(spec *generated.TypeSpec, own bool) erasurePlan {
	plan := erasurePlan{Keep: append([]string(nil), erasureStructuralFields...)}
	if spec == nil {
		return plan
	}
	for _, f := range spec.Fields {
		d := erasureDecisionFor(spec.Concept, f, own)
		switch {
		case d.OnCustomItem != "":
			plan.MarkOnCustomItem = append(plan.MarkOnCustomItem, f.Name)
			plan.CustomItemReference = erasureCustomItem.Reference
		case d.Fate == fateKept:
			plan.Keep = append(plan.Keep, f.Name)
		case d.Fate == fateMarked:
			plan.Mark = append(plan.Mark, f.Name)
		case d.Fate == fateEmptied:
			plan.Empty = append(plan.Empty, f.Name)
		}
	}
	return plan
}

// ErasureDecision answers what a customer's erasure does with one field of a
// row it reaches, for a concept by its name ("order") or its id
// ("v1:shopify:order") and a field by its name in the mirror. It is for tests
// and audits: a package that depends on a field of the mirror can prove here
// that the field survives an erasure. It reads the policy and changes nothing.
//
// The answer is one of:
//
//	"kept by kind"                     a reference, an enum, or a scalar whose Admin type is a figure or a moment
//	"kept by the list"                 on the keep-list, as describing the sale and not the person
//	"kept, scrubbed on a custom item"  kept on a line item of a product, scrubbed on a line with no product
//	"kept as structure"                one of the fields a mirror row carries for itself (storeId, gid, updatedAt, ...)
//	"scrubbed"                         everything else
//	"no such field"                    the concept or the field is not in the generated model
//
// For the customer concept it answers for the erased customer's own row, which
// keeps nothing but its structure.
func ErasureDecision(concept, field string) string {
	spec := generated.Types[generated.ConceptFromID(strings.TrimSpace(concept))]
	if spec == nil {
		return "no such field"
	}
	for _, name := range erasureStructuralFields {
		if name == field && (name != "parentGid" || spec.Parent != "") {
			return "kept as structure"
		}
	}
	for _, f := range spec.Fields {
		if f.Name != field {
			continue
		}
		d := erasureDecisionFor(spec.Concept, f, spec.Concept == erasedConcept)
		switch {
		case d.OnCustomItem != "":
			return "kept, scrubbed on a custom item"
		case d.ByKind != "":
			return "kept by kind"
		case d.ByList != "":
			return "kept by the list"
		}
		return "scrubbed"
	}
	return "no such field"
}

// The reasons on the keep-list. Each says why the field is not about a person.
const (
	keepMoney        = "an amount of the sale"
	keepCurrency     = "the currency of the sale"
	keepTaxLines     = "tax lines: titles, rates and amounts"
	keepDiscountCode = "the discount codes used on the sale"
)

// erasureKeepList is the string, object and list fields that survive an
// erasure although their kind does not keep them, by concept, each with the
// reason it describes the sale and not the person.
//
// WHAT MAY BE HERE: money, currency codes, the number of an order and of the
// documents made from it, product and variant titles and SKUs, tax lines,
// discount codes and how they were applied, the names of payment gateways, and
// a status carried as a string. Nothing else. A field that cannot be shown to
// be about the sale is not put here, and an object or a list is put here only
// when EVERYTHING it holds is about the sale (the golden prints what each
// holds): an order's shipping line is the title and the amounts of the
// shipping and also the phone number at the delivery address, so it goes, and
// the shipping's amounts stay in the order's totals.
//
// The customer's own row has no entry and can have none. Neither has a row
// that is about a buyer and not about a sale (a company, its contacts and
// locations, a store credit account, a payment method): what they hold are a
// buyer's balances and details.
var erasureKeepList = map[string]map[string]string{
	"order": keepFields(
		keep(keepMoney,
			"cartDiscountAmountSet", "currentCartDiscountAmountSet", "currentShippingPriceSet", "currentSubtotalPriceSet",
			"currentTotalAdditionalFeesSet", "currentTotalDiscountsSet", "currentTotalDutiesSet", "currentTotalPriceSet",
			"currentTotalTaxSet", "netPaymentSet", "originalTotalAdditionalFeesSet", "originalTotalDutiesSet",
			"originalTotalPriceSet", "refundDiscrepancySet", "subtotalPriceSet", "totalCapturableSet", "totalDiscountsSet",
			"totalOutstandingSet", "totalPriceSet", "totalReceivedSet", "totalRefundedSet", "totalRefundedShippingSet",
			"totalShippingPriceSet", "totalTaxSet", "totalTipReceivedSet"),
		keep("amounts of the sale: the cash rounding of its payments and refunds", "totalCashRoundingAdjustment"),
		keep("fees charged on the order: names, amounts and tax lines", "additionalFees"),
		keep(keepCurrency, "currencyCode", "presentmentCurrencyCode"),
		keep("the order's number", "name"),
		keep(keepTaxLines, "taxLines", "currentTaxLines"),
		keep(keepDiscountCode, "discountCode", "discountCodes"),
		keep("how each discount was applied to the order, and its value", "discountApplications"),
		keep("the names of the payment gateways", "paymentGatewayNames"),
	),
	"orderLineItem": keepFields(
		keep(keepMoney,
			"discountedTotalSet", "discountedUnitPriceAfterAllDiscountsSet", "discountedUnitPriceSet", "originalTotalSet",
			"originalUnitPriceSet", "priceAfterAllDiscountsBeforeTaxesSet", "totalDiscountSet",
			"unfulfilledDiscountedTotalSet", "unfulfilledOriginalTotalSet"),
		keep("the product's title", "title"),
		keep("the variant's title", "variantTitle"),
		keep("the product's and the variant's titles", "name"),
		keep("the variant's SKU", "sku"),
		keep(keepTaxLines, "taxLines"),
		keep("the amount each discount took off the line", "discountAllocations"),
		keep("duties on the goods: customs codes, amounts and tax lines", "duties"),
	),
	"orderTransaction": keepFields(
		keep(keepMoney, "amountRoundingSet", "amountSet", "maximumRefundableV2", "totalUnsettledSet"),
		keep("amounts of the sale: the currency exchange adjustment", "currencyExchangeAdjustment"),
		keep("the transaction's fees: names, rates and amounts", "fees"),
		keep(keepCurrency, "settlementCurrency"),
		keep("the payment gateway's name", "gateway", "formattedGateway"),
		keep("a status: the transaction's error code", "errorCode"),
	),
	"refund": keepFields(
		keep(keepMoney, "totalRefundedSet"),
		keep("refunded duties: amounts and the goods' customs codes", "duties"),
	),
	"fulfillment": keepFields(
		keep("the fulfilment's number, made from the order's", "name"),
	),
	"shopifyReturn": keepFields(
		keep("the return's number, made from the order's", "name"),
		keep("amounts of the sale: the return's shipping fees", "returnShippingFees"),
	),
	"fulfillmentOrder": keepFields(
		keep("the order's number", "orderName"),
	),
	"tenderTransaction": keepFields(
		keep(keepMoney, "amount"),
		keep("the name of the payment method, as a gateway's", "paymentMethod"),
	),
	"shopifyPaymentsDispute": keepFields(
		keep(keepMoney, "amount"),
	),
	"draftOrder": keepFields(
		keep(keepMoney,
			"amountDueLaterSet", "amountDueNowSet", "lineItemsSubtotalPrice", "subtotalPriceSet", "totalDiscountsSet",
			"totalLineItemsPriceSet", "totalPriceSet", "totalShippingPriceSet", "totalTaxSet"),
		keep(keepCurrency, "currencyCode", "presentmentCurrencyCode"),
		keep("the draft order's number", "name"),
		keep(keepTaxLines, "taxLines"),
		keep(keepDiscountCode, "discountCodes"),
		keep("the discounts the platform applied: titles, codes and amounts", "platformDiscounts"),
	),
	"abandonedCheckout": keepFields(
		keep(keepMoney,
			"subtotalPriceSet", "totalDiscountSet", "totalDutiesSet", "totalLineItemsPriceSet", "totalPriceSet", "totalTaxSet"),
		keep("the checkout's number", "name"),
		keep(keepTaxLines, "taxLines"),
		keep(keepDiscountCode, "discountCodes"),
	),
	"giftCard": keepFields(
		keep("an amount of the sale: the value the card was sold with", "initialValue"),
	),
	"subscriptionContract": keepFields(
		keep("an amount of the sale: the delivery price of the subscription", "deliveryPrice"),
		keep(keepCurrency, "currencyCode"),
	),
}

// keptFields is one reason and the fields it keeps.
type keptFields struct {
	why    string
	fields []string
}

func keep(why string, fields ...string) keptFields { return keptFields{why: why, fields: fields} }

// keepFields is one concept's entries, field to reason.
func keepFields(groups ...keptFields) map[string]string {
	out := map[string]string{}
	for _, g := range groups {
		for _, f := range g.fields {
			out[f] = g.why
		}
	}
	return out
}

// notALink is why a gid field is not a link, and the one case in which it is
// one after all.
type notALink struct {
	// Why is why a row that names an in-scope row in this field is not in
	// scope for it.
	Why string
	// FromADeletedRow, when set, says why the field IS a link from a row that
	// is deleted (its newest version is a tombstone).
	FromADeletedRow string
}

// erasureNotALink is the gid fields that are NOT links (ruling 91). Every
// other gid field of the model is one: a row that holds an in-scope row's gid
// as the whole value of it is in scope.
//
// A link says "this row is OF that row". The fields here say something else:
// a row that points at one of its own parts, a row that names where it
// belongs, a pointer that goes stale. Followed, each of them reaches somebody
// who never asked: with every gid field a link, a customer whose row named
// the erased customer's order as his last order was scrubbed with her, and
// exported to her.
var erasureNotALink = map[string]map[string]notALink{
	"company": {
		"mainContactGid": {Why: "the company points at one of its contacts; a company is not scrubbed because one contact was"},
	},
	"companyContact": {
		"companyGid": {Why: "it names the company the contact belongs to; followed, it would reach every colleague"},
	},
	"companyLocation": {
		"companyGid": {Why: "it names the company the location belongs to; followed, it would reach the company's addresses"},
	},
	"customer": {
		"lastOrderGid": {
			Why:             "another person's row can name an order that has since moved to the erased customer; a live customer row is another person",
			FromADeletedRow: "a deleted customer whose last order is in scope was merged into the erased one: the same person's earlier record",
		},
	},
	"giftCard": {
		"orderGid":    {Why: "the card is its holder's, not its buyer's; the buyer's message inside a card somebody else holds stays"},
		"lineItemGid": {Why: "the card is its holder's, not its buyer's; the line it was bought on is the buyer's"},
	},
	"orderLineItem": {
		"contractGid": {Why: "a line belongs to its order and is reached with it; the subscription it bills is not what makes it hers"},
	},
}

// erasureCustomerInside is the object fields in which the customer's gid makes
// the ROW hers: the object says who the row is for. The customer's gid is a
// link nowhere else inside an object, and never by value: an order whose
// custom attribute had her gid as its value was scrubbed, and exported to
// her.
var erasureCustomerInside = map[string]map[string]string{
	"order": {
		"purchasingEntity": "who bought: the order is the buyer's, as its customer field says of it",
	},
	"draftOrder": {
		"purchasingEntity": "who the draft is for: the draft is that buyer's",
	},
	"giftCard": {
		"recipientAttributes": "who the card is sent to, with the message to her: the card is the recipient's",
	},
}

// erasureCustomerTakenOut is the object fields in which the customer's gid
// names her in a row that is NOT hers: the entry that names her is taken out
// and the rest of the row is left, exactly as a list of references is
// treated.
var erasureCustomerTakenOut = map[string]map[string]string{
	"metaobject": {
		"fields":         "a merchant's own object with a field that refers to her: the object is the merchant's, the reference goes",
		"thumbnailField": "the one field a metaobject shows for itself; when it refers to her it goes, and the object stays",
	},
}

// Link kinds.
const (
	linkParent = "parent"
	linkGid    = "gid"
	linkObject = "object"
)

// erasureLink is one way a row of a concept comes into an erasure's scope: it
// hangs from an in-scope row of Names (parent), it holds an in-scope row of
// Names' gid as the whole value of Field (gid), or it holds the customer's gid
// inside Field (object).
type erasureLink struct {
	Kind, Field, Names string
	// FromADeletedRow says the link holds only from a row whose newest
	// version is a tombstone.
	FromADeletedRow bool
}

// way is the link in the golden's words.
func (l erasureLink) way() string {
	switch l.Kind {
	case linkParent:
		return "child of " + l.Names
	case linkObject:
		return "the customer inside object " + l.Field
	}
	way := "gid field " + l.Field + " naming " + l.Names
	if l.FromADeletedRow {
		way += ", from a deleted row only"
	}
	return way
}

// conceptReach is how an erasure can come to a row of one concept.
type conceptReach struct {
	// Links are the ways a row of the concept comes into scope.
	Links []string
	// TakenOut are the fields through which a row of the concept can name the
	// customer without being in scope: the entry that names her is taken out
	// and nothing else in the row changes.
	TakenOut []string
}

// erasureModel is the reach of an erasure, worked out once from the generated
// model and the lists above.
type erasureModel struct {
	links    map[string][]erasureLink
	takeOuts map[string][]string
	reach    map[string]conceptReach
}

var (
	erasureModelOnce sync.Once
	erasureModelIs   erasureModel
)

// theErasureModel is THE ONE ENUMERATION of what an erasure can reach. The
// scope's statements are built from its links (Connector.erasureScopeOf) and
// the golden is rendered from its reach, so what the golden says an erasure
// reaches is what the scope looks for.
//
// A concept's links are: its parent link; each of its gid fields that is not
// on erasureNotALink (one that is a link from a deleted row only says so);
// and each of its object fields on erasureCustomerInside. Only a link that
// names a concept an erasure can reach is kept: a reference to a product or
// to a member of staff names a row no erasure comes to, and is nothing to
// follow. The reach starts at the customer's row and the orders a request
// names and grows by those links to a fixed point.
func theErasureModel() erasureModel {
	erasureModelOnce.Do(func() {
		conceptOf := map[string]string{}
		concepts := make([]string, 0, len(generated.Types))
		for concept, spec := range generated.Types {
			conceptOf[spec.GraphQLType] = concept
			concepts = append(concepts, concept)
		}
		sort.Strings(concepts)
		customerType := generated.Types[erasedConcept].GraphQLType

		every := map[string][]erasureLink{}
		takeOuts := map[string][]string{}
		for _, concept := range concepts {
			spec := generated.Types[concept]
			if spec.Parent != "" {
				every[concept] = append(every[concept], erasureLink{Kind: linkParent, Names: spec.Parent})
			}
			for _, f := range spec.Fields {
				switch f.Kind {
				case generated.KindGid:
					named := conceptOf[f.AdminType]
					if named == "" {
						continue
					}
					not, isNot := erasureNotALink[concept][f.Name]
					if isNot && not.FromADeletedRow == "" {
						continue
					}
					every[concept] = append(every[concept], erasureLink{Kind: linkGid, Field: f.Name, Names: named, FromADeletedRow: isNot})
				case generated.KindObject, generated.KindObjectList:
					if _, ok := erasureCustomerInside[concept][f.Name]; ok {
						every[concept] = append(every[concept], erasureLink{Kind: linkObject, Field: f.Name, Names: erasedConcept})
					}
					if _, ok := erasureCustomerTakenOut[concept][f.Name]; ok {
						takeOuts[concept] = append(takeOuts[concept], f.Name)
					}
				case generated.KindRefs:
					// A list of references is never a link. One that can name
					// a customer has her gid taken out of it.
					if f.AdminType == customerType {
						takeOuts[concept] = append(takeOuts[concept], f.Name)
					}
				}
			}
		}

		reachable := map[string]bool{erasedConcept: true, requestedConcept: true}
		for grew := true; grew; {
			grew = false
			for _, concept := range concepts {
				if reachable[concept] {
					continue
				}
				for _, l := range every[concept] {
					if reachable[l.Names] {
						reachable[concept] = true
						grew = true
						break
					}
				}
			}
		}

		m := erasureModel{links: map[string][]erasureLink{}, takeOuts: takeOuts, reach: map[string]conceptReach{}}
		for _, concept := range concepts {
			var r conceptReach
			switch concept {
			case erasedConcept:
				r.Links = append(r.Links, "the customer's row")
			case requestedConcept:
				r.Links = append(r.Links, "an order the request names")
			}
			for _, l := range every[concept] {
				if !reachable[l.Names] {
					continue
				}
				m.links[concept] = append(m.links[concept], l)
				r.Links = append(r.Links, l.way())
			}
			r.TakenOut = takeOuts[concept]
			if len(r.Links) > 0 || len(r.TakenOut) > 0 {
				m.reach[concept] = r
			}
		}
		erasureModelIs = m
	})
	return erasureModelIs
}

// erasureLinksOf is the links by which a row of the concept comes into an
// erasure's scope, in the model's order: the parent link, then its fields.
func erasureLinksOf(spec *generated.TypeSpec) []erasureLink {
	if spec == nil {
		return nil
	}
	return theErasureModel().links[spec.Concept]
}

// erasureTakeOutsOf is the fields of the concept an entry naming the customer
// is taken out of, in a row that is not in scope.
func erasureTakeOutsOf(spec *generated.TypeSpec) []string {
	if spec == nil {
		return nil
	}
	return theErasureModel().takeOuts[spec.Concept]
}

// erasureReach is what an erasure can reach, concept by concept: the ways a
// row comes into scope and the fields an entry is taken out of. A concept
// that is not in it is not reachable.
func erasureReach() map[string]conceptReach {
	return theErasureModel().reach
}
