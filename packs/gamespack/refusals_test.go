package gamespack

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The refusal codes are a contract: every code the source raises is in
// RefusalCodes, and every code in RefusalCodes is one the source raises.
func TestEveryRefusalCodeIsDeclaredAndRaised(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	raised := map[string]bool{}
	code := regexp.MustCompile(`"(games_[a-z_]+):`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range code.FindAllStringSubmatch(string(src), -1) {
			raised[m[1]] = true
		}
	}
	for c := range raised {
		if !slices.Contains(RefusalCodes, c) {
			t.Errorf("%s is raised but not declared in RefusalCodes", c)
		}
	}
	for _, c := range RefusalCodes {
		if !raised[c] {
			t.Errorf("%s is declared in RefusalCodes but nothing raises it", c)
		}
	}
}

// The list is a set of words a client matches: no code twice, each a games_ word in
// lower case with underscores, and none that a client could mistake for part of another.
func TestRefusalCodesAreDistinctWords(t *testing.T) {
	word := regexp.MustCompile(`^games_[a-z]+(_[a-z]+)*$`)
	seen := map[string]bool{}
	for _, c := range RefusalCodes {
		if !word.MatchString(c) {
			t.Errorf("%q is not a games_ word in lower case", c)
		}
		if seen[c] {
			t.Errorf("%s is declared twice", c)
		}
		seen[c] = true
	}
	for a := range seen {
		for b := range seen {
			if a != b && strings.HasPrefix(b, a+"_") {
				t.Errorf("%s is a prefix of %s: a client matching the shorter code by prefix would take the longer for it", a, b)
			}
		}
	}
}
