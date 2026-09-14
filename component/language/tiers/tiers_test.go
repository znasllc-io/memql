package tiers

import (
	"regexp"
	"testing"
)

// A position's value is its corpus directory name, so it has to be a plain
// lowerCamel identifier, unique, and the list has to hold every one of the
// eleven the record names plus the refine clause the expression-language epic
// adds -- a position missing here is a position the conformance corpus stops
// asking for.
func TestPositionsAreTheTwelveCorpusDirectories(t *testing.T) {
	ident := regexp.MustCompile(`^[a-z][A-Za-z]*$`)
	got := Positions()
	if len(got) != 12 {
		t.Fatalf("Positions() has %d entries, want the record's 11 plus queryRefine: %v", len(got), got)
	}
	seen := map[Position]bool{}
	for _, p := range got {
		if !ident.MatchString(string(p)) {
			t.Errorf("position %q is not a lowerCamel directory name", p)
		}
		if seen[p] {
			t.Errorf("position %q listed twice", p)
		}
		seen[p] = true
	}
}
