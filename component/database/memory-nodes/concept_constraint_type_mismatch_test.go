package memoryNodes

import (
	"encoding/json"
	"strings"
	"testing"
)

// concept_constraint_type_mismatch_test.go covers memql#3124 / task #3198: a
// value constraint the element's TYPE cannot carry.
//
// memql#3049 established that a schema contradicting its declaration in silence
// is the wrong outcome, and memql#3104 enforced it for a COMPOSITE element
// ([][]T, []map[string]T, map[string][]T). The same silence remained one level
// up, where the element is perfectly ordinary but simply cannot carry the
// keyword: `[]int @pattern("^[0-9]+$")` emitted `pattern` onto an integer
// element, where JSON Schema ignores it outright. The author gets no
// enforcement and no error.
//
// It is not limited to wrapped fields -- a plain `object @minLength(3)` built
// too -- so it predates memql#2951 and memql#3049 both.

// mismatchConcept spells a one-property concept, so the declaration under test
// is the only thing that varies across the tables below. `kind` is present
// because the @variant rows need a discriminator sibling.
func mismatchConcept(decl string) []byte {
	return []byte(`
@description("A widget.")
concept Widget {
  kind  string!
  ` + decl + `
}
`)
}

// TestParseConcept_TypeMismatchedConstraint_Refused is the mismatch-class
// table. Every row built a schema emitting a keyword the validator ignores for
// that type; each must now fail to load.
//
// The classes are the ones enumerated in the acceptance criteria --
// @pattern / @minLength / @maxLength on a non-string, @minimum / @maximum on a
// non-number -- plus @variant on a non-object, which the probe found dropping a
// whole union on `[]string` and `int` for exactly the same reason. It is
// classified IN rather than left out: it is the same failure mode in the same
// code path, and memql#3123 (task #3197) has just refused the other spelling
// that dropped a union in silence.
func TestParseConcept_TypeMismatchedConstraint_Refused(t *testing.T) {
	cases := []struct {
		decl string
		// want is a fragment of the declaration the diagnostic must quote
		// back, so a message that names the wrong field fails here.
		wantAnnotation string
	}{
		// @pattern / @minLength / @maxLength on a non-string element.
		{`body []object @pattern("^a")`, "@pattern"},
		{`body []int    @pattern("^a")`, "@pattern"},
		{`body []bool   @pattern("^a")`, "@pattern"},
		{`body []float  @minLength(3)`, "@minLength"},
		{`body []object @minLength(3)`, "@minLength"},
		{`body []bool   @maxLength(3)`, "@maxLength"},
		// The unwrapped spellings, which predate the wrapping rules entirely.
		{`body object   @minLength(3)`, "@minLength"},
		{`body bool     @pattern("^a")`, "@pattern"},
		{`body int      @maxLength(3)`, "@maxLength"},

		// @minimum / @maximum on a non-number element.
		{`body []string @minimum(3)`, "@minimum"},
		{`body []bool   @maximum(3)`, "@maximum"},
		{`body []object @minimum(3)`, "@minimum"},
		{`body string   @maximum(3)`, "@maximum"},
		{`body bool     @minimum(3)`, "@minimum"},
		// enum and datetime both EMIT type:string, so they carry the string
		// constraints (pinned green below) and not the numeric ones.
		{`body enum("a","b") @minimum(3)`, "@minimum"},
		{`body datetime      @maximum(3)`, "@maximum"},

		// @variant on a non-object element: the union is discarded, not
		// merely misplaced.
		{`body []string @variant(discriminator="kind") { a { x string! } }`, "@variant"},
		{`body int      @variant(discriminator="kind") { a { x string! } }`, "@variant"},
	}

	for _, tc := range cases {
		t.Run(strings.Join(strings.Fields(tc.decl), " "), func(t *testing.T) {
			_, err := ParseConceptMemQL(mismatchConcept(tc.decl), "v1/test/widget")
			if err == nil {
				t.Fatalf("`%s` loaded without error -- the keyword is emitted and the validator ignores it",
					strings.Join(strings.Fields(tc.decl), " "))
			}
			if !strings.Contains(err.Error(), tc.wantAnnotation) {
				t.Errorf("diagnostic does not name %s: %v", tc.wantAnnotation, err)
			}
			if !strings.Contains(err.Error(), "body") {
				t.Errorf("diagnostic does not name the property: %v", err)
			}
		})
	}
}

// TestParseConcept_TypeMismatchedConstraint_NestedLeafRefused pins that the
// refusal does not depend on the property sitting at the top level.
func TestParseConcept_TypeMismatchedConstraint_NestedLeafRefused(t *testing.T) {
	content := []byte(`
@description("A widget.")
concept Widget {
  settings {
    retries  int  @pattern("^[0-9]+$")
  }
}
`)
	_, err := ParseConceptMemQL(content, "v1/test/widget")
	if err == nil {
		t.Fatal("a nested int leaf carrying @pattern loaded without error")
	}
	if !strings.Contains(err.Error(), "@pattern") || !strings.Contains(err.Error(), "retries") {
		t.Errorf("diagnostic does not name the annotation and the property: %v", err)
	}
}

// TestParseConcept_WellMatchedConstraint_Unaffected is the negative control.
// A refusal that fired on every constraint would pass the table above while
// breaking most of the tree, so every carrying combination is pinned here --
// including the two the acceptance criteria name explicitly ([]string @pattern,
// []int @minimum) and the two string-typed non-`string` kinds, enum and
// datetime, which are the ones a coarser rule would get wrong.
func TestParseConcept_WellMatchedConstraint_Unaffected(t *testing.T) {
	for _, decl := range []string{
		`body []string @pattern("^a")`,
		`body []string @minLength(3) @maxLength(8)`,
		`body string   @maxLength(3)`,
		`body []int    @minimum(3)`,
		`body []float  @maximum(3)`,
		`body int      @minimum(1) @maximum(50)`,
		// enum and datetime emit type:string, so the string constraints
		// reach the schema and are asserted -- these must NOT be refused.
		`body enum("a","b") @pattern("^a")`,
		`body datetime      @maxLength(64)`,
		`body []enum("a","b") @minLength(1)`,
		// @variant on an object element, at both depths the grammar allows.
		`body []object @variant(discriminator="kind") { a { x string! } }`,
		`body map[string]object @variant(discriminator="kind") { a { x string! } }`,
		`body object   @variant(discriminator="kind") { a { x string! } }`,
	} {
		t.Run(strings.Join(strings.Fields(decl), " "), func(t *testing.T) {
			if _, err := ParseConceptMemQL(mismatchConcept(decl), "v1/test/widget"); err != nil {
				t.Fatalf("well-matched constraint refused: %v", err)
			}
		})
	}
}

// TestParseConcept_AnyTypeConstraint_LeftOut records the one keyword/type pair
// deliberately NOT classified as a mismatch, with its reason, rather than
// leaving a reader to wonder whether it was missed.
//
// `any` declares no type at all, so the emitted schema is `{"pattern": "^a"}`
// with no `type` beside it -- and JSON Schema's own rule is that `pattern`
// applies to strings and is ignored for every other instance type. That is not
// the engine contradicting the declaration; it is the declaration saying "any
// JSON value" and the constraint applying exactly where it can. An author who
// wrote `any` opted into that.
func TestParseConcept_AnyTypeConstraint_LeftOut(t *testing.T) {
	c, err := ParseConceptMemQL(mismatchConcept(`body any @pattern("^a")`), "v1/test/widget")
	if err != nil {
		t.Fatalf("`any @pattern` should load: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	body := schema["properties"].(map[string]any)["body"].(map[string]any)
	if body["pattern"] != "^a" {
		t.Errorf("`any @pattern` lost its pattern: %v", body)
	}
	if _, hasType := body["type"]; hasType {
		t.Errorf("`any` grew a type, which would make the carve-out wrong: %v", body)
	}
}
