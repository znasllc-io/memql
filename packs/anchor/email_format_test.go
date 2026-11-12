package anchor

import (
	"testing"

	// Core Campaigns declares its signup form at package initialization; it
	// is linked here so the walk below sees every form a default image carries.
	_ "github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/memql"
)

// ONE FIELD DECLARES THE email FORMAT (ruling 54): the wholesale application's
// address. Every other form and read of every anchored pack, and Campaigns'
// signup, keeps the rules it had, so the format changes nothing else.
func TestOnlyTheWholesaleApplicationsAddressDeclaresTheEmailFormat(t *testing.T) {
	Storefront()
	found, forms := false, 0
	for _, entry := range memql.ShopperSurface() {
		var fields []memql.ShopperField
		switch entry.Kind {
		case "form":
			if f := memql.ShopperFormFor(entry.Pack, entry.Name); f != nil {
				fields = f.Fields
				forms++
			}
		case "read":
			if r := memql.ShopperReadFor(entry.Pack, entry.Name); r != nil {
				fields = r.Fields
			}
		}
		for _, field := range fields {
			if field.Format != memql.ShopperFormatEmail {
				continue
			}
			if entry.Pack == "wholesale" && entry.Kind == "form" && entry.Name == "application" && field.Name == "applicantEmail" {
				found = true
				continue
			}
			t.Errorf("%s/%s (%s) declares the email format on %q", entry.Pack, entry.Name, entry.Kind, field.Name)
		}
	}
	if !found {
		t.Error("the wholesale application's applicantEmail does not declare the email format")
	}
	// THE REACHABLE POSITIVE: the walk saw the forms it claims to judge.
	if forms < 4 {
		t.Errorf("the walk saw %d forms, want the wholesale application and acceptance, the review form and the signup", forms)
	}
}
