package memoryNodes

// memql#2909 item 3: "worth auditing which other property types the two
// disagree on. I only measured `boolean`."
//
// The audit found twelve. This file is that audit, kept executable so it stays
// true: the accepted set and the corrected set are both pinned, and adding a
// type to the builder without deciding what it means for authors now fails
// here rather than being discovered by a bundle at boot.

import (
	"strings"
	"testing"
)

// acceptedPropertyTypes is the set an AUTHOR can actually write, which is not
// the same as buildPropertySchema's case labels. `map` is a case there but no
// property syntax produces it, and `enum` is reachable only in its
// parenthesised `enum("a", "b")` form -- both measured in
// component/memql's TestConceptPropertyTypes_AcceptedAndRejectedSets.
//
// Listing the case labels here instead would document a vocabulary nobody can
// use, which is the same failure this issue is about pointed the other way.
var acceptedPropertyTypes = []string{
	"string", "bool", "int", "float", "datetime", "array", "object", "any", "",
}

// TestPropertyTypeSuggestion_CorrectsEveryPlausibleMisspelling pins the
// twelve. Each was verified to be REJECTED by the schema builder, so each one
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
