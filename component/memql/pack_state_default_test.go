package memql

import (
	"reflect"
	"testing"
)

// A ROW ALWAYS WINS over a declared default, in both directions. The row
// is the operator's explicit act; the default is only what governs in its
// absence.
func TestARowOverridesADeclaredDefaultInBothDirections(t *testing.T) {
	defaults := map[string]bool{"reviews": false, "harness": true}

	if got := DisabledPackDomains(map[string]PackStateRow{}, defaults); !reflect.DeepEqual(got, []string{"reviews"}) {
		t.Fatalf("no rows: got %v, want [reviews] -- the declared default decides", got)
	}

	got := DisabledPackDomains(map[string]PackStateRow{"reviews": {Enabled: true}}, defaults)
	if len(got) != 0 {
		t.Fatalf("a row saying enabled must beat a default saying disabled: got %v, want []", got)
	}

	// harness declares TRUE and its row says false, so the row disables it;
	// reviews declares FALSE and has no row, so the default disables it.
	// Both halves of the fold in one assertion, which is what makes it
	// falsifiable: dropping either source loses exactly one name.
	got = DisabledPackDomains(map[string]PackStateRow{"harness": {Enabled: false}}, defaults)
	if !reflect.DeepEqual(got, []string{"harness", "reviews"}) {
		t.Fatalf("a row saying disabled must beat a default saying enabled, and an "+
			"undeclared-row pack must keep its declared default: got %v, want [harness reviews]", got)
	}
}

// The fresh-database, nothing-declared state is still "every pack loads",
// which is the property that keeps every pack predating declared defaults
// behaving as it did.
func TestNoRowsAndNoDeclarationsDisablesNothing(t *testing.T) {
	if got := DisabledPackDomains(map[string]PackStateRow{}, map[string]bool{}); len(got) != 0 {
		t.Fatalf("got %v, want [] -- absence with no declaration means enabled", got)
	}
	if got := DisabledPackDomains(nil, nil); len(got) != 0 {
		t.Fatalf("got %v, want [] on nil inputs", got)
	}
}

// Sorted, because the set reaches a log line and the module inventory, and
// an unstable order there reads as a flip nobody made.
func TestTheDisabledSetIsSorted(t *testing.T) {
	got := DisabledPackDomains(
		map[string]PackStateRow{"zeta": {Enabled: false}, "alpha": {Enabled: false}},
		map[string]bool{"mid": false},
	)
	if !reflect.DeepEqual(got, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("got %v, want [alpha mid zeta]", got)
	}
}
