package memoryNodes

// memql#2909 item 3: "worth auditing which other property types the two
// disagree on. I only measured `boolean`."
//
// The audit found sixteen. This file is that audit, kept executable so it stays
// true: the accepted set and the corrected set are both pinned, and adding a
// type to the builder without deciding what it means for authors now fails
// here rather than being discovered by a bundle at boot.
//
// It said "twelve" until review round 14 counted. The table had sixteen
// entries and four of them -- long, decimal, str, time -- were in no docs
// table and driven by no test. An asserted count, in the PR whose whole
// subject is vocabularies asserted rather than measured. The size assertion
// below exists so the next entry cannot drift the same way.

import (
	"strings"
	"testing"
)

// acceptedPropertyTypes is the set an AUTHOR can write BARE, which is not the
// same as buildPropertySchema's case labels: `map` and `enum` are cases there
// but neither is writable bare -- they are reached only through
// `map[string]<type>` and `enum("a", "b")`, which the parser lowers to those
// kinds. The parameterised forms are measured in component/memql's
// TestConceptPropertyTypes_AcceptedAndRejectedSets, which is where a decl can
// be built.
//
// An earlier version of this comment claimed no property syntax produces
// `map` at all. It does -- `map[string]string` builds fine -- and the claim
// reached the author-facing error text and the docs before review caught it.
// Kept as a note because the mistake is the one this whole issue is about:
// a vocabulary asserted from reading a switch rather than measured.
var acceptedPropertyTypes = []string{
	"string", "bool", "int", "float", "datetime", "array", "object", "any", "",
}

// TestPropertyTypeSuggestion_CorrectsEveryPlausibleMisspelling pins the
// sixteen. Each was verified to be REJECTED by the schema builder, so each one
// silently dropped a whole concept before memql#2909.
func TestPropertyTypeSuggestion_CorrectsEveryPlausibleMisspelling(t *testing.T) {
	for wrong, want := range propertyTypeSuggestions {
		t.Run(wrong, func(t *testing.T) {
			got := suggestPropertyType(wrong)
			if !strings.Contains(got, "did you mean") {
				t.Fatalf("%q is in the suggestion table but produced no suggestion: %q", wrong, got)
			}
			if !strings.Contains(got, `"`+want+`"`) {
				t.Errorf("%q should be corrected to %q; got %q", wrong, want, got)
			}
			// The correction has to be a type the builder actually takes,
			// or the error sends the author from one failure to another.
			var ok bool
			for _, a := range acceptedPropertyTypes {
				if a == want {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%q is suggested for %q but is not itself an accepted type", want, wrong)
			}
		})
	}
}

// TestPropertyTypeSuggestion_UnknownSpellingListsTheVocabulary covers the
// fallback: something not in the table still has to tell the author what IS
// allowed, since a bare "unknown type" leaves them guessing.
func TestPropertyTypeSuggestion_UnknownSpellingListsTheVocabulary(t *testing.T) {
	got := suggestPropertyType("frobnicate")
	if strings.Contains(got, "did you mean") {
		t.Errorf("an unrecognised spelling must not claim a correction: %q", got)
	}
	for _, want := range []string{"string", "bool", "int", "float", "datetime", "object", "any", "array", "enum"} {
		if !strings.Contains(got, want) {
			t.Errorf("the fallback must list %q as accepted; got %q", want, got)
		}
	}
}

// TestPropertyTypeSuggestion_IsCaseAndSpaceInsensitive -- the parser hands the
// type through as written, so `Boolean` and ` boolean ` reach here unchanged.
func TestPropertyTypeSuggestion_IsCaseAndSpaceInsensitive(t *testing.T) {
	for _, in := range []string{"Boolean", "BOOLEAN", " boolean ", "Integer"} {
		if !strings.Contains(suggestPropertyType(in), "did you mean") {
			t.Errorf("%q should still be recognised as a misspelling; got %q", in, suggestPropertyType(in))
		}
	}
}

// TestAcceptedPropertyTypes_AreNotAlsoSuggestions guards the obvious
// contradiction: a type cannot be both accepted and corrected.
func TestAcceptedPropertyTypes_AreNotAlsoSuggestions(t *testing.T) {
	for _, a := range acceptedPropertyTypes {
		if want, bad := propertyTypeSuggestions[a]; bad {
			t.Errorf("%q is accepted by the builder but the table corrects it to %q", a, want)
		}
	}
}

// TestPropertyTypeSuggestions_SizeIsPinned is a drift gate, not a behaviour
// test. The correction table is restated in three places a test cannot reach
// from here -- the table in docs/public/language/memql.md, and the independent
// rejected-spelling map in component/memql's
// TestConceptPropertyTypes_AcceptedAndRejectedSets -- so adding an entry here
// silently leaves those two behind. That is exactly what happened: the table
// grew to sixteen while both restatements stayed at twelve.
func TestPropertyTypeSuggestions_SizeIsPinned(t *testing.T) {
	const want = 16
	if got := len(propertyTypeSuggestions); got != want {
		t.Errorf("the correction table has %d entries, not %d. If you added or removed a "+
			"spelling, update BOTH restatements in the same change:\n"+
			"  - the correction table in docs/public/language/memql.md\n"+
			"  - the rejected map in TestConceptPropertyTypes_AcceptedAndRejectedSets "+
			"(component/memql/lint_parity_concept_build_test.go)\n"+
			"then update `want` here.", got, want)
	}
}
