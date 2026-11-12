package memql

import (
	"context"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func validForm() ShopperForm {
	return ShopperForm{
		Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields:        []ShopperField{{Name: "body", Required: true, MaxLength: 4000}},
		RedirectOK:    "/reviews/thanks",
		RedirectError: "/reviews/error",
	}
}

func expectPanic(t *testing.T, containing string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", containing)
		}
		if msg, ok := r.(string); ok && !strings.Contains(msg, containing) {
			t.Fatalf("panic = %q, want it to contain %q", msg, containing)
		}
	}()
	fn()
}

// THE LOAD-BEARING HALF OF D9. A form that could declare storeId could aim
// a row at a store the shopper is not on, which is exactly the preview/live
// confusion storeId exists to prevent.
func TestAFormMayNotDeclareAServerStampedField(t *testing.T) {
	for _, name := range []string{"storeId", "siteId", "ownerUserId", "createdBy", "id", "actor"} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(ResetShopperSurfaceForTest)
			f := validForm()
			f.Fields = []ShopperField{{Name: name}}
			expectPanic(t, "stamped server-side", func() { RegisterShopperForm(f) })
		})
	}
}

func TestADuplicateRouteIsRefusedRatherThanLastWins(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	RegisterShopperForm(validForm())
	expectPanic(t, "already declared", func() { RegisterShopperForm(validForm()) })
}

func TestAFormAndAReadMayNotShareARouteName(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	RegisterShopperForm(validForm())
	expectPanic(t, "already declared as a FORM", func() {
		RegisterShopperRead(ShopperRead{Pack: "reviews", Name: "review", Construct: "x",
			Kind: ShopperReadKindBuiltin, Fields: []ShopperField{{Name: "q"}}})
	})
}

// The 303 is on the request's critical path, so a target that is not
// site-relative is an open redirect on a merchant's own origin.
func TestARedirectMustBeSiteRelative(t *testing.T) {
	for _, bad := range []string{
		"https://elsewhere.example/thanks",
		"//elsewhere.example/thanks",
		"/reviews/../../admin",
		"reviews/thanks",
		"",
		"/thanks\r\nSet-Cookie: x=1",
	} {
		t.Run(bad, func(t *testing.T) {
			t.Cleanup(ResetShopperSurfaceForTest)
			f := validForm()
			f.RedirectOK = bad
			expectPanic(t, "RedirectOK", func() { RegisterShopperForm(f) })
		})
	}
}

func TestARouteNameMustBeASinglePathSegment(t *testing.T) {
	for _, bad := range []string{"has/slash", "Has-Hyphen", "has.dot", "", "9leading", strings.Repeat("a", 41)} {
		t.Run("name="+bad, func(t *testing.T) {
			t.Cleanup(ResetShopperSurfaceForTest)
			f := validForm()
			f.Name = bad
			expectPanic(t, "path segment", func() { RegisterShopperForm(f) })
		})
	}
}

// A route with no fields accepts nothing, so it is unusable rather than
// open -- but it is still a declaration nobody meant to make.
func TestARouteWithNoFieldsIsRefused(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	f := validForm()
	f.Fields = nil
	expectPanic(t, "declares no fields", func() { RegisterShopperForm(f) })
}

func TestARouteWithNoConstructIsRefused(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	f := validForm()
	f.Construct = ""
	expectPanic(t, "declares no Construct", func() { RegisterShopperForm(f) })
}

func TestAFieldMayNotBeBothNumericAndEnum(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	f := validForm()
	f.Fields = []ShopperField{{Name: "rating", Numeric: true, Enum: []string{"1", "2"}}}
	expectPanic(t, "Numeric and an Enum", func() { RegisterShopperForm(f) })
}

func TestADuplicateFieldNameIsRefused(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	f := validForm()
	f.Fields = []ShopperField{{Name: "body"}, {Name: "body"}}
	expectPanic(t, "twice", func() { RegisterShopperForm(f) })
}

func TestAReadMustNameAKnownKind(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	expectPanic(t, "is neither", func() {
		RegisterShopperRead(ShopperRead{Pack: "reviews", Name: "published", Construct: "x",
			Kind: "automation", Fields: []ShopperField{{Name: "q"}}})
	})
}

func TestLookupIsByPackAndName(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	RegisterShopperForm(validForm())
	if ShopperFormFor("reviews", "review") == nil {
		t.Fatal("the registered form must resolve")
	}
	if ShopperFormFor("reviews", "nope") != nil {
		t.Fatal("an unregistered name must not resolve")
	}
	if ShopperFormFor("wholesale", "review") != nil {
		t.Fatal("a name registered under another pack must not resolve")
	}
	if ShopperReadFor("reviews", "review") != nil {
		t.Fatal("a form must not resolve as a read")
	}
}

// An UNDECLARED pack reaches nothing. This is the property the whole file
// exists for, and it is asserted rather than assumed.
func TestAnEmptyRegistryReachesNothing(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	ResetShopperSurfaceForTest()
	if len(ShopperSurface()) != 0 {
		t.Fatal("an empty registry must expose nothing")
	}
	if ShopperFormFor("anything", "atall") != nil || ShopperReadFor("anything", "atall") != nil {
		t.Fatal("nothing resolves against an empty registry")
	}
}

func TestTheSurfaceIsSortedAndComplete(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	RegisterShopperForm(validForm())
	RegisterShopperRead(ShopperRead{Pack: "reviews", Name: "published", Construct: "reviewsPublished",
		Kind: ShopperReadKindBuiltin, Fields: []ShopperField{{Name: "productHandle", Required: true}}})
	got := ShopperSurface()
	if len(got) != 2 {
		t.Fatalf("ShopperSurface() = %d entries, want 2", len(got))
	}
	if got[0].Kind != "form" || got[1].Kind != "read" {
		t.Fatalf("expected form before read, got %v then %v", got[0].Kind, got[1].Kind)
	}
}

func TestAFieldWithNoBoundStillHasOne(t *testing.T) {
	f := ShopperField{Name: "body"}
	if f.EffectiveMaxLength() != shopperFieldDefaultMaxLength {
		t.Fatalf("an unbounded field must fall back to the package default, got %d",
			f.EffectiveMaxLength())
	}
	if (ShopperField{Name: "body", MaxLength: 10}).EffectiveMaxLength() != 10 {
		t.Fatal("a declared bound must be honoured")
	}
}

// accountId IS STAMPED FROM THE SITE'S ORGANIZATION, never posted. A form that
// could declare it could tie a row to an organization the site is not in, and
// the row tier would then admit that organization's people to it. It is refused
// as a declared form field and as an extension's field, exactly as storeId is,
// and the guide lists it among the stamped names.
func TestAccountIdIsAReservedShopperName(t *testing.T) {
	t.Cleanup(ResetShopperSurfaceForTest)
	t.Cleanup(ResetShopperExtensions)
	f := validForm()
	f.Fields = []ShopperField{{Name: "accountId"}}
	expectPanic(t, "stamped server-side", func() { RegisterShopperForm(f) })

	RegisterShopperForm(validForm())
	err := RegisterShopperExtension(ShopperExtension{Domain: "fylo", Pack: "reviews", Form: "review",
		Construct: "recordFyloReviewDetail", Fields: []ShopperField{{Name: "accountId"}}})
	if err == nil || !strings.Contains(err.Error(), "stamped server-side") {
		t.Fatalf("an extension declaring accountId as a field was not refused as stamped: %v", err)
	}

	listed := false
	for _, name := range ReservedShopperFieldNames() {
		listed = listed || name == "accountId"
	}
	if !listed {
		t.Fatalf("accountId is not among the reserved shopper names: %v", ReservedShopperFieldNames())
	}
}

// AN EXTENSION ROW IS SHOWN BY ITS FIELD NAMES IN PLAIN WORDS (X14): every
// declared field but the stamped names, labelled by its name split at its case
// boundaries, and the concept's display card picks the two facts a list shows.
func TestAnExtensionRowIsLabelledByItsFieldNamesInPlainWords(t *testing.T) {
	src := `@version("1.0.0")
@description("A client's own fields on an application.")
@displayCard(primary="taxNumber", secondary="tradeName")
concept acmeDetail {
  ownerUserId    string!
  storeId        string!
  submissionId   string
  accountId      string
  applicationId  string!
  taxNumber      string!
  tradeName      string
}
`
	file, err := langparser.ParseFile(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var concept *memoryNodes.Concept
	for _, def := range file.Definitions {
		if decl, ok := def.(*langparser.ConceptDecl); ok {
			if concept, err = memoryNodes.BuildConceptFromDecl(decl, "v1:acme:acmeDetail"); err != nil {
				t.Fatalf("build: %v", err)
			}
		}
	}
	if concept == nil {
		t.Fatal("no concept in the source")
	}
	row := extensionRowOf(concept, map[string]any{
		"ownerUserId": "v1:identity:user:u1", "storeId": "store-1", "submissionId": "sub-1", "accountId": "acct-1",
		"applicationId": "sub-1", "taxNumber": "12-3456789", "tradeName": "Acme Trade",
	})
	if row.Concept != "v1:acme:acmeDetail" || row.Primary != "12-3456789" || row.Secondary != "Acme Trade" {
		t.Fatalf("row = %+v, want the concept and its display card's two facts", row)
	}
	labels := map[string]string{}
	for _, f := range row.Fields {
		labels[f.Name] = f.Label
	}
	for _, stamped := range []string{"storeId", "submissionId", "ownerUserId", "accountId"} {
		if _, shown := labels[stamped]; shown {
			t.Errorf("%s is a stamped name and is shown: %+v", stamped, row.Fields)
		}
	}
	for name, want := range map[string]string{"taxNumber": "Tax number", "tradeName": "Trade name", "applicationId": "Application id"} {
		if labels[name] != want {
			t.Errorf("%s is labelled %q, want %q", name, labels[name], want)
		}
	}
	for name, want := range map[string]string{"ein": "Ein", "shopifyCustomerID": "Shopify customer id", "vatNumber2": "Vat number 2"} {
		if got := extensionFieldLabel(name); got != want {
			t.Errorf("extensionFieldLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// THE EVIDENCE AN ACCEPTANCE RECORDS IS STAMPED, NEVER POSTED: a form that
// could declare clientAddress or userAgent would let a shopper write their own.
func TestClientAddressAndUserAgentAreReservedShopperNames(t *testing.T) {
	for _, name := range []string{"clientAddress", "userAgent"} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(ResetShopperSurfaceForTest)
			f := validForm()
			f.Fields = []ShopperField{{Name: name}}
			expectPanic(t, "stamped server-side", func() { RegisterShopperForm(f) })
			r := ShopperRead{Pack: "reviews", Name: "draft", Construct: "x", Kind: ShopperReadKindBuiltin,
				Fields: []ShopperField{{Name: name}}}
			expectPanic(t, "stamped server-side", func() { RegisterShopperRead(r) })
		})
	}
	names := strings.Join(ReservedShopperFieldNames(), ",")
	if !strings.Contains(names, "clientAddress") || !strings.Contains(names, "userAgent") {
		t.Fatalf("the reserved names %s leave out the evidence", names)
	}
}

// THE EVIDENCE RIDES THE CONTEXT, so only server code can set it: a context
// with none answers false, and the value set is the value read.
func TestShopperEvidenceRidesTheContext(t *testing.T) {
	if _, ok := ShopperEvidenceFrom(context.Background()); ok {
		t.Fatal("a bare context carries evidence")
	}
	ev := ShopperEvidence{ClientAddress: "198.51.100.7", UserAgent: "Example Browser/1.0"}
	got, ok := ShopperEvidenceFrom(ContextWithShopperEvidence(context.Background(), ev))
	if !ok || got != ev {
		t.Fatalf("the context carries %+v (%v), want %+v", got, ok, ev)
	}
	if ShopperClientHeader != "X-Memql-Shopper-Client" {
		t.Fatalf("ShopperClientHeader = %q", ShopperClientHeader)
	}
}

// A FIELD'S FORMAT IS A NAMED RULE (ruling 53): sha256 accepts exactly 64
// hexadecimal characters of either case and nothing else, an unknown format is
// refused at registration, and a format does not combine with Numeric or Enum.
func TestAFieldFormatIsANamedRule(t *testing.T) {
	f := ShopperField{Name: "termsSha256", Format: ShopperFormatSHA256}
	for value, want := range map[string]bool{
		strings.Repeat("a", 64): true, strings.Repeat("A", 64): true, strings.Repeat("0", 64): true,
		strings.Repeat("a", 63): false, strings.Repeat("a", 65): false, strings.Repeat("g", 64): false, "": false,
	} {
		if got := f.MatchesFormat(value); got != want {
			t.Errorf("MatchesFormat(%q) = %v, want %v", value, got, want)
		}
	}
	if !(ShopperField{Name: "free"}).MatchesFormat("anything") {
		t.Error("a field with no format refuses a value")
	}
	for name, field := range map[string]ShopperField{
		"unknown": {Name: "x", Format: "uuid"},
		"numeric": {Name: "x", Format: ShopperFormatSHA256, Numeric: true},
		"enum":    {Name: "x", Format: ShopperFormatSHA256, Enum: []string{"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(ResetShopperSurfaceForTest)
			form := validForm()
			form.Fields = []ShopperField{field}
			expectPanic(t, "format", func() { RegisterShopperForm(form) })
		})
	}
}
