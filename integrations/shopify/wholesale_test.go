package shopify

import (
	"strings"
	"testing"
)

// wholesale_test.go -- the decisions in the B2B write path that can be
// exercised without a store (epic memql#5533, issues memql#5558 and
// memql#5559).
//
// The Admin calls themselves need a development store and live in
// wholesale_live_test.go, which self-skips without credentials. What is
// here is everything that is a function over values: the plan reading, the
// reference round trip, the ceiling refusal and the two normalisations that
// reach a merchant's live store.

// THE CEILING IS A CATALOG COUNT, NOT A TIER GATE. Since 2026-04-02
// companies, payment terms, volume pricing and up to three catalogs are
// available BELOW Plus (the connector record, 1.3), so an adapter that
// refused a Basic store outright would refuse a store that can do this.
func TestPlanHasUnlimitedCatalogs(t *testing.T) {
	for _, plan := range []string{"Shopify Plus", "plus", "Plus Partner Sandbox", "SHOPIFY PLUS"} {
		if !PlanHasUnlimitedCatalogs(plan) {
			t.Errorf("%q must read as Plus: the display name has been spelled several ways "+
				"and telling a merchant who pays for Plus that they have hit a limit that "+
				"does not apply to them is the worse failure", plan)
		}
	}
	for _, plan := range []string{"Basic", "Shopify", "Advanced", "Development", "Partner test store"} {
		if PlanHasUnlimitedCatalogs(plan) {
			t.Errorf("%q must not read as Plus", plan)
		}
	}
}

// AN EMPTY PLAN IS NOT PLUS, which is the fail-closed reading: an unknown
// plan gets the ceiling rather than escaping it.
func TestAnUnknownPlanGetsTheCeiling(t *testing.T) {
	if PlanHasUnlimitedCatalogs("") {
		t.Fatal("an unknown plan must not be treated as Plus")
	}
	if PlanHasUnlimitedCatalogs("   ") {
		t.Fatal("a blank plan must not be treated as Plus")
	}
}

// THE REFERENCE ROUND-TRIPS, because it is the only thing that lets a
// revoke find what a grant created.
func TestWholesaleRefsRoundTrip(t *testing.T) {
	want := WholesaleAccountRefs{
		CompanyGID:         "gid://shopify/Company/1",
		CompanyLocationGID: "gid://shopify/CompanyLocation/2",
		CompanyContactGID:  "gid://shopify/CompanyContact/3",
		CatalogGID:         "gid://shopify/Catalog/4",
		PriceListGID:       "gid://shopify/PriceList/5",
		CustomerGID:        "gid://shopify/Customer/6",
	}
	got := DecodeWholesaleRefs(want.Encode())
	if got != want {
		t.Fatalf("round trip lost something:\n got %+v\nwant %+v", got, want)
	}
}

// A MISSING KEY IS EMPTY, NOT AN ERROR. A reference written by an older
// build carries fewer keys, and refusing to parse it would make an
// entitlement unrevokable through the pack -- worse than revoking what can
// be found.
func TestPartialRefsDecodeToWhatIsThere(t *testing.T) {
	got := DecodeWholesaleRefs("company=gid://shopify/Company/1;catalog=gid://shopify/Catalog/4")
	if got.CompanyGID != "gid://shopify/Company/1" {
		t.Fatalf("company = %q", got.CompanyGID)
	}
	if got.CatalogGID != "gid://shopify/Catalog/4" {
		t.Fatalf("catalog = %q", got.CatalogGID)
	}
	if got.PriceListGID != "" {
		t.Fatalf("an absent key must decode to empty, got %q", got.PriceListGID)
	}
	if empty := DecodeWholesaleRefs(""); empty != (WholesaleAccountRefs{}) {
		t.Fatalf("an empty reference must decode to zero, got %+v", empty)
	}
}

// THE CEILING REFUSAL NAMES THE STORE, THE PLAN AND THE REMEDY, because
// the person reading it is a merchant whose buyer is waiting.
func TestCeilingErrorSaysWhatToDo(t *testing.T) {
	err := &WholesaleCeilingError{StoreID: "store-live", Plan: "Basic", Limit: 3}
	msg := err.Error()
	for _, want := range []string{"store-live", "Basic", "Plus", "upgrade"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must mention %q: %s", want, msg)
		}
	}
	if !err.Refused() {
		t.Fatal("a ceiling will fail identically for ever and must be marked refused, " +
			"or the pack will retry it as though it were an outage")
	}
	// An unknown plan still produces a readable sentence.
	unknown := (&WholesaleCeilingError{StoreID: "s", Limit: 3}).Error()
	if strings.Contains(unknown, "on  and") {
		t.Fatalf("an empty plan must not leave a gap in the sentence: %s", unknown)
	}
}

// A TAG IS NOT FREE TEXT. It reaches a merchant's live store, and an empty
// one on a tagsRemove would take every tag the customer has.
func TestNormaliseTag(t *testing.T) {
	if got := normaliseTag(""); got != WholesaleTagDefault {
		t.Fatalf("an empty tag must default, got %q", got)
	}
	if got := normaliseTag("   "); got != WholesaleTagDefault {
		t.Fatalf("a blank tag must default, got %q", got)
	}
	if got := normaliseTag("  acme-trade "); got != "acme-trade" {
		t.Fatalf("a tag must be trimmed, got %q", got)
	}
}

// THE DEFAULT TAG IS PREFIXED WITH OURS, so this connector never removes a
// tag somebody else put there.
func TestTheDefaultTagIsOurs(t *testing.T) {
	if !strings.HasPrefix(WholesaleTagDefault, "memql-") {
		t.Fatalf("the default tag must be recognisably ours, got %q", WholesaleTagDefault)
	}
}

// THE NAME SPLIT IS WRONG FOR PLENTY OF NAMES AND THERE IS NO CORRECT
// ANSWER AVAILABLE. The pack collects ONE name field because that is the
// minimum an application needs; inventing a second on the form to satisfy
// Shopify's schema would be the pack drifting toward one integration's
// shape. This pins the behaviour so nobody "improves" it into a guess.
func TestSplitName(t *testing.T) {
	for _, tc := range []struct{ in, first, last string }{
		{"Sam Rivers", "Sam", "Rivers"},
		{"Maria del Carmen Garcia", "Maria del Carmen", "Garcia"},
		{"Prince", "Prince", ""},
		{"  Sam   Rivers  ", "Sam", "Rivers"},
		{"", "", ""},
	} {
		first, last := splitName(tc.in)
		if first != tc.first || last != tc.last {
			t.Errorf("splitName(%q) = (%q, %q), want (%q, %q)",
				tc.in, first, last, tc.first, tc.last)
		}
	}
}

// NOTHING IN THIS FILE'S WRITE PATH DELETES. Revoking sets a catalog to
// DRAFT; a merchant who revokes trade terms is ending a discount, not
// erasing a customer, and deleting a company would take its order history's
// association with it. The assertion is over the mutation documents
// themselves, because that is where a "tidy-up" would appear.
func TestTheWholesaleWritePathDeletesNothing(t *testing.T) {
	for name, document := range map[string]string{
		"companyCreate":       companyCreateMutation,
		"catalogCreate":       catalogCreateMutation,
		"catalogDraft":        catalogDraftMutation,
		"priceListCreate":     priceListCreateMutation,
		"companyAssign":       companyAssignContactMutation,
		"customerCreate":      customerCreateMutation,
		"tagsAdd":             tagsAddMutation,
		"tagsRemove":          tagsRemoveMutation,
		"catalogsCountQuery":  catalogsCountQuery,
		"companySearchQuery":  companySearchQuery,
		"customerSearchQuery": customerSearchQuery,
	} {
		lower := strings.ToLower(document)
		for _, banned := range []string{"delete", "destroy"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s contains %q. The wholesale write path never deletes: revoking "+
					"sets the catalog to DRAFT, and a company carries the buyer's order "+
					"history", name, banned)
			}
		}
	}
	if !strings.Contains(catalogDraftMutation, "catalogUpdate") {
		t.Fatal("the revoke must be a catalogUpdate to DRAFT")
	}
}
