package vendorname

import (
	"strings"
	"testing"
)

// None of these tests writes a banned literal. They take it from Banned(),
// which is the point: this package's own .go file is the ONLY place in the
// repository allowed to contain one, and the repo-wide sweep exempts exactly
// that file. A test that spelled a name out would need a second exemption and
// would be the first copy of the fact this package exists to prevent.

func TestBannedIsNonEmptyAndFullyDescribed(t *testing.T) {
	names := Banned()
	if len(names) == 0 {
		t.Fatal("Banned() is empty -- the sweep that reads it would then assert nothing")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n.Text == "" {
			t.Error("a banned entry has no Text")
		}
		if n.What == "" {
			t.Errorf("banned %q has no What -- a failure would name it without saying what it is", n.Text)
		}
		if n.Text != strings.ToLower(n.Text) {
			t.Errorf("banned %q is not lowercase; FirstIn lowercases the haystack, not the needle", n.Text)
		}
		if seen[n.Text] {
			t.Errorf("banned %q appears twice", n.Text)
		}
		seen[n.Text] = true
	}
}

func TestFirstInFindsEveryBannedName(t *testing.T) {
	for _, n := range Banned() {
		for _, spelling := range []string{
			n.Text,
			strings.ToUpper(n.Text),
			"prefix-" + n.Text + "-suffix",
		} {
			got, ok := FirstIn(spelling)
			if !ok {
				t.Errorf("FirstIn(%q) found nothing, want %q", spelling, n.Text)
				continue
			}
			if got.Text != n.Text {
				t.Errorf("FirstIn(%q) = %q, want %q", spelling, got.Text, n.Text)
			}
		}
	}
}

// The org name is NOT the domain, and the registry is the product's. Both are
// one character away from a banned entry, so a careless widening of the list
// breaks here rather than in CI on an unrelated PR.
func TestFirstInDoesNotMatchTheOrgOrTheProductRegistry(t *testing.T) {
	for _, clean := range []string{
		"github.com/znasllc-io/memql",
		"ghcr.io/znasllc-io/memql-bff:0.19.6",
		"acrmemql.azurecr.io/memql-identity",
		"id-memql-mail",
		"",
		"example.com",
	} {
		if got, ok := FirstIn(clean); ok {
			t.Errorf("FirstIn(%q) matched %q -- the org, the product registry and the "+
				"naming convention must all stay legal", clean, got.Text)
		}
	}
}

func TestBannedIsACopy(t *testing.T) {
	first := Banned()
	original := first[0].Text
	first[0].Text = "mutated"
	if Banned()[0].Text != original {
		t.Error("Banned() handed out the package's own slice; one caller can now edit every other caller's list")
	}
}
