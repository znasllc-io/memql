package memql

import (
	"strings"
	"testing"
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
