package packages

import (
	"strings"
	"testing"
)

// What a source DECLARES, as opposed to what it has deployed.
//
// The catalogue exists because a site row is written only for an app that
// actually deployed, so an app skipped at the confirm gate had no row and was
// invisible in every client surface -- absent from the list, and absent from
// its own source's "apps it produces". A person could therefore choose not to
// deploy one app of a source and then be unable to find it again.

func TestDeclaredFromKeepsEveryNamedDeployable(t *testing.T) {
	rep := &Report{Deployables: []DeployableReport{
		{Name: "storefront", Kind: "spa", Path: "clients/web", BuildPlan: "npm run build"},
		{Name: "admin", Kind: "spa", Path: "clients/admin"},
	}}
	got := declaredFrom(rep)
	if len(got) != 2 {
		t.Fatalf("declaredFrom = %d entries, want 2", len(got))
	}
	if got[0].Name != "storefront" || got[0].Kind != "spa" {
		t.Errorf("first entry = %+v, want storefront/spa", got[0])
	}
}

func TestDeclaredFromCarriesOnlyNameAndKind(t *testing.T) {
	// The catalogue outlives every run, so a run-shaped fact recorded on it
	// would be one nothing ever corrects. Asserted structurally: the JSON a
	// declared entry renders to has exactly the two keys.
	got := declaredFrom(&Report{Deployables: []DeployableReport{
		{Name: "storefront", Kind: "spa", Path: "clients/web", BuildPlan: "npm run build", Output: "dist", Prebuilt: true},
	}})
	lit := jsonLiteral(got)
	for _, absent := range []string{"buildPlan", "path", "output", "prebuilt", "command"} {
		if strings.Contains(lit, absent) {
			t.Errorf("declared entry carries run-shaped field %q: %s", absent, lit)
		}
	}
	for _, want := range []string{"storefront", "spa"} {
		if !strings.Contains(lit, want) {
			t.Errorf("declared entry is missing %q: %s", want, lit)
		}
	}
}

func TestDeclaredFromIsAnArrayNeverNull(t *testing.T) {
	// The trap closeDeployment documents: jsonLiteral sees an interface
	// holding a typed nil slice as non-nil, marshals it, and writes the word
	// `null` -- which the concept's []object refuses, so the write is refused
	// and the package keeps whatever the LAST analysis found.
	for _, rep := range []*Report{nil, {}, {Deployables: nil}} {
		got := declaredFrom(rep)
		if got == nil {
			t.Fatalf("declaredFrom(%+v) returned nil; the store would render null", rep)
		}
		if lit := jsonLiteral(got); lit != "[]" {
			t.Errorf("declaredFrom(%+v) rendered %s, want []", rep, lit)
		}
	}
}

func TestDeclaredFromDropsANamelessDeployable(t *testing.T) {
	// A nameless entry could never be matched against a site's
	// packageDeployableName, so it would list a row nobody could act on.
	got := declaredFrom(&Report{Deployables: []DeployableReport{
		{Name: "", Kind: "spa"},
		{Name: "  ", Kind: "spa"},
		{Name: "web", Kind: "spa"},
	}})
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("declaredFrom kept %+v, want only web", got)
	}
}
