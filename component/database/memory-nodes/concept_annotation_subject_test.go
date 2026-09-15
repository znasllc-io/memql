package memoryNodes

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// TestConceptRefusalsNameWhatTheyRefuse: a registry refusal on a concept
// names the concept, and one on a field or on the body names the field or the
// relationship -- the refusal's own message says only "on a concept field" or
// "on a concept body", which in a concept of forty fields and six
// relationships names nothing (memql#5359). The code stays last and reachable.
func TestConceptRefusalsNameWhatTheyRefuse(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
		want []string
	}{
		{
			name: "a field",
			src:  "concept probe {\n  title string @maxLength(\"zz\")\n}\n",
			code: annotations.CodeForm,
			want: []string{`translate concept probe: property "title": @maxLength on a concept field takes one number`},
		},
		{
			name: "a relationship",
			src:  "concept probe {\n  ownerUserId string\n  @relationship(type=\"parent\", field=\"ownerUserId\", target=user, direction=\"outgoing\", bogus=\"x\")\n}\n",
			code: annotations.CodeKey,
			want: []string{`translate concept probe: the relationship on field "ownerUserId": @relationship on a concept body has no key bogus`},
		},
		{
			name: "the concept",
			src:  "@displayCard(bogus=\"x\")\nconcept probe {\n  title string\n}\n",
			code: annotations.CodeKey,
			want: []string{`translate concept probe: @displayCard on a concept has no key bogus`},
		},
	}
	for _, tc := range cases {
		_, err := buildConceptFromSource(t, tc.src)
		var ref *annotations.Refusal
		if err == nil || !errors.As(err, &ref) || ref.Code != tc.code || !strings.HasSuffix(err.Error(), "["+tc.code+"]") {
			t.Errorf("%s: want a %s refusal with its code last, got: %v", tc.name, tc.code, err)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: the refusal must read %q: %v", tc.name, w, err)
			}
		}
	}
}

// TestConceptFieldSpellingsTheReadersDroppedAreRefused pins the spellings the
// concept translator used to read and no longer does (memql#5359): a keyword
// `value=` on a numeric bound or a description, a quoted number, and a bare
// word other than true/false as a @default. The registry refuses each before
// the translator runs; if a placement ever took one again, the reader that
// understood it is gone and the value would be dropped in silence.
func TestConceptFieldSpellingsTheReadersDroppedAreRefused(t *testing.T) {
	for _, ann := range []string{
		`@minLength(value=5)`, `@maxLength(value=5)`, `@minimum(value=1)`, `@maximum(value=9)`,
		`@minLength("5")`, `@maximum("9")`,
		`@description(value="x")`, `@pattern(value="^a")`,
	} {
		src := "concept probe {\n  count int " + ann + "\n}\n"
		_, err := buildConceptFromSource(t, src)
		var ref *annotations.Refusal
		if err == nil || !errors.As(err, &ref) || ref.Code != annotations.CodeForm {
			t.Errorf("%s on a concept field: want an annotation_form refusal, got: %v", ann, err)
		}
	}
	// @default is refused a rung EARLIER since epic memql#5375 retired it on a
	// concept field: the name never reaches the form check, so the refusal is
	// annotation_retired whatever shape it was written in. Sharper, not weaker.
	for _, ann := range []string{`@default(open)`, `@default(value="x")`, `@default("open")`} {
		src := "concept probe {\n  count int " + ann + "\n}\n"
		_, err := buildConceptFromSource(t, src)
		var ref *annotations.Refusal
		if err == nil || !errors.As(err, &ref) || ref.Code != annotations.CodeRetired {
			t.Errorf("%s on a concept field: want an annotation_retired refusal, got: %v", ann, err)
		}
	}
}
