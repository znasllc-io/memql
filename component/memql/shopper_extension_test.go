package memql

import (
	"strings"
	"testing"
)

// shopper_extension_test.go -- the extension registry (design record
// 2026-09-21, section 4.2).
//
// Every test here names the production change that would make it fail,
// because a registry whose refusals are untested is a registry whose
// refusals are comments.

// declareWholesaleForm puts a pack form in the registry for an extension to
// attach to. The real one is registered by packs/wholesalepack at init; this
// package cannot import it (the pack imports THIS one), so the tests declare
// an equivalent.
func declareWholesaleForm(t *testing.T) {
	t.Helper()
	RegisterShopperForm(ShopperForm{
		Pack:      "wholesale",
		Name:      "application",
		Construct: "wholesaleSubmitApplication",
		Kind:      ShopperReadKindBuiltin,
		Fields: []ShopperField{
			{Name: "companyName", Required: true, MaxLength: 200},
			{Name: "applicantName", Required: true, MaxLength: 120},
			{Name: "applicantEmail", Required: true, MaxLength: 320},
		},
		RedirectOK:    "/wholesale/thank-you",
		RedirectError: "/wholesale/problem",
	})
}

func fyloExtension() ShopperExtension {
	return ShopperExtension{
		Domain:    "fylo",
		Pack:      "wholesale",
		Form:      "application",
		Construct: "recordFyloApplicationDetail",
		Fields: []ShopperField{
			{Name: "ein", MaxLength: 20},
			{Name: "address", Required: true, MaxLength: 200},
		},
		Description: "Fylo's own fields on a wholesale application.",
	}
}

func TestShopperExtensionRegistersAndResolves(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)

	if err := RegisterShopperExtension(fyloExtension()); err != nil {
		t.Fatalf("a valid extension was refused: %v", err)
	}
	got := ShopperExtensionFor("wholesale", "application")
	if got == nil {
		t.Fatal("the extension did not resolve; a declared extension must be reachable by (pack, form)")
	}
	if got.Construct != "recordFyloApplicationDetail" {
		t.Errorf("Construct = %q, want recordFyloApplicationDetail", got.Construct)
	}
	if len(got.Fields) != 2 {
		t.Errorf("Fields = %d, want 2", len(got.Fields))
	}
}

func TestShopperExtensionIsAbsentForARouteNobodyExtended(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)

	if got := ShopperExtensionFor("wholesale", "application"); got != nil {
		t.Fatalf("an unextended route resolved an extension: %+v", got)
	}
}

// REFUSAL 1. An extension of a route nobody serves is a declaration whose
// author believes something false -- and a DISABLED pack declares no forms,
// so this is also what a cluster sees when reviews is switched off.
func TestShopperExtensionRefusesAnUndeclaredForm(t *testing.T) {
	ResetShopperSurfaceForTest()

	err := RegisterShopperExtension(fyloExtension())
	if err == nil {
		t.Fatal("an extension of an undeclared form was accepted")
	}
	if !strings.Contains(err.Error(), "wholesale/application") {
		t.Errorf("the refusal does not name the route it could not find: %v", err)
	}
}

// REFUSAL 2. Ordering between two extensions would be resolved by nothing.
func TestShopperExtensionRefusesASecondExtensionOnOneRoute(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)
	if err := RegisterShopperExtension(fyloExtension()); err != nil {
		t.Fatalf("the first extension was refused: %v", err)
	}

	second := fyloExtension()
	second.Domain = "acme"
	second.Construct = "recordAcmeDetail"
	err := RegisterShopperExtension(second)
	if err == nil {
		t.Fatal("a second extension on one route was accepted; which one runs would depend on load order")
	}
	// BOTH DOMAINS, because the operator has to know which two declarations
	// to reconcile and one name sends them looking for the other.
	if !strings.Contains(err.Error(), "fylo") || !strings.Contains(err.Error(), "acme") {
		t.Errorf("the refusal does not name both declaring domains: %v", err)
	}
}

// REFUSAL 4. The load-bearing half of D9 -- a shopper who could supply
// storeId could aim a row at a store they are not on.
func TestShopperExtensionRefusesAServerStampedFieldName(t *testing.T) {
	for _, name := range []string{"storeId", "siteId", "ownerUserId", "id", "createdBy"} {
		t.Run(name, func(t *testing.T) {
			ResetShopperSurfaceForTest()
			declareWholesaleForm(t)

			ext := fyloExtension()
			ext.Fields = append(ext.Fields, ShopperField{Name: name, MaxLength: 40})
			err := RegisterShopperExtension(ext)
			if err == nil {
				t.Fatalf("an extension declaring the stamped field %q was accepted", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name the field: %v", err)
			}
		})
	}
}

// REFUSAL 5. A pack adding a field to its own form declares it.
func TestShopperExtensionRefusesThePackExtendingItself(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)

	ext := fyloExtension()
	ext.Domain = "wholesale"
	err := RegisterShopperExtension(ext)
	if err == nil {
		t.Fatal("a pack extending its own form was accepted")
	}
}

// REFUSAL 6. A name declared by both is ambiguous about whose rule applies,
// and the client can read the pack's value off the row it relates to.
func TestShopperExtensionRefusesAFieldThePackAlreadyDeclares(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)

	ext := fyloExtension()
	ext.Fields = append(ext.Fields, ShopperField{Name: "companyName", MaxLength: 50})
	err := RegisterShopperExtension(ext)
	if err == nil {
		t.Fatal("an extension redeclaring one of the pack's own fields was accepted")
	}
	if !strings.Contains(err.Error(), "companyName") {
		t.Errorf("the refusal does not name the colliding field: %v", err)
	}
}

func TestShopperExtensionRefusesADeclarationWithNoFields(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)

	ext := fyloExtension()
	ext.Fields = nil
	if err := RegisterShopperExtension(ext); err == nil {
		t.Fatal("an extension declaring no fields was accepted; it adds nothing and runs a mutation with no input")
	}
}

func TestShopperExtensionRefusesAnEmptyConstruct(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)

	ext := fyloExtension()
	ext.Construct = ""
	if err := RegisterShopperExtension(ext); err == nil {
		t.Fatal("an extension naming no construct was accepted")
	}
}

// THE LOAD-TIME PROPERTY. A pack's declaration is written once at init; an
// extension's is rebuilt every time the DSL tree loads, so a domain removed
// from a bundle stops extending on the next boot rather than lingering.
func TestResetShopperExtensionsClearsWithoutTouchingPackForms(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)
	if err := RegisterShopperExtension(fyloExtension()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ResetShopperExtensions()

	if got := ShopperExtensionFor("wholesale", "application"); got != nil {
		t.Error("the extension survived a reload; a bundle that dropped the domain would keep extending")
	}
	if ShopperFormFor("wholesale", "application") == nil {
		t.Error("the pack's own form was cleared by an extension reload; the two registries have different lifetimes")
	}
}

// The surface is meant to be READ: an operator asking what the public can
// reach must see the client's fields too, or the inventory understates it.
func TestShopperSurfaceReportsExtensions(t *testing.T) {
	ResetShopperSurfaceForTest()
	declareWholesaleForm(t)
	if err := RegisterShopperExtension(fyloExtension()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var found *ShopperSurfaceEntry
	for _, e := range ShopperSurface() {
		if e.Kind == "extension" {
			entry := e
			found = &entry
		}
	}
	if found == nil {
		t.Fatal("ShopperSurface() does not report extensions; the module inventory would understate the public surface")
	}
	if found.Pack != "wholesale" || found.Name != "application" {
		t.Errorf("the extension entry names %s/%s, want wholesale/application", found.Pack, found.Name)
	}
}
