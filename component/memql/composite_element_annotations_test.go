package memql

// memql#3049: a value-constraining annotation on a COMPOSITE element --
// [][]T, []map[string]T, map[string][]T -- was silently inert.
//
// memql#2951 moved value constraints down onto the element of a wrapped type,
// which is right one level down. One level further it lands on the inner array
// or map, where JSON Schema ignores `pattern`/`minimum`/`minLength`/... and
// where propertyToJSONSchema's array and map branches never read `variants` at
// all -- so a discriminated union was discarded outright rather than merely
// misplaced. Measured before the fix, all four ACCEPTED data the declaration
// forbids:
//
//	[][]string @pattern       {"f":[["ZZZ"]]}            ACCEPTED
//	[]map[string]string @pat  {"f":[{"a":"ZZZ"}]}        ACCEPTED
//	[][]int @minimum(3)       {"f":[[1]]}                ACCEPTED
//	[][]object @variant       {"f":[[{"nonsense":1}]]}   ACCEPTED
//
// #2951's own definition of done required such an annotation to apply to
// elements OR be rejected when the field is wrapped. On a composite element the
// code did NEITHER -- it built a schema contradicting the declaration, in
// silence, which that issue names as the worst of the three options.
//
// These tests pin the chosen resolution (reject, loudly) AND the two things it
// must not break: the composite TYPE is still legal on its own, and a
// single-wrapped annotation still behaves exactly as #2951 made it behave.

import (
	"encoding/json"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// buildCompositeField builds a one-property concept whose field `f` carries the
// given type-and-annotation text, and returns f's JSON-Schema subtree.
func buildCompositeField(t *testing.T, decl string) (map[string]any, error) {
	t.Helper()
	src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept probe {\n  label string @required @description(\"l\")\n  f " + decl + "\n}\n"
	decls := ExtractConceptDecls(src)
	if len(decls) == 0 {
		t.Fatalf("fixture for %q did not parse into a concept decl, so it measures nothing", decl)
	}
	c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
	if err != nil {
		return nil, err
	}
	raw, serr := c.DefinitionSchema()
	if serr != nil {
		t.Fatalf("DefinitionSchema for %q: %v", decl, serr)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %q: %v", decl, err)
	}
	return doc.Properties["f"], nil
}

// variantBlock is the two-branch discriminated union used by the @variant
// cases, written once so the single-wrapped control and the composite case
// under test differ ONLY in how deeply the field is wrapped.
const variantBlock = " @variant(discriminator=\"kind\") {\n" +
	"    text {\n      kind string @required\n    }\n" +
	"    image {\n      url string @required\n    }\n  }"

// TestCompositeElement_ValueAnnotationsAreRejected is the core of memql#3049.
//
// The refusal is asserted through its MESSAGE, not merely through `err != nil`.
// The whole complaint was that the old behaviour left an author with no signal;
// refusing with "invalid property" would fix the schema and leave the author in
// the same place, so the message has to name the annotation, quote the
// declaration back, and say what to write instead.
func TestCompositeElement_ValueAnnotationsAreRejected(t *testing.T) {
	for _, c := range []struct {
		name, decl, wantAnnotation, wantType string
	}{
		{"array of arrays, pattern", `[][]string @pattern("^[a-z]+$")`, "@pattern", "[][]string"},
		{"array of maps, pattern", `[]map[string]string @pattern("^[a-z]+$")`, "@pattern", "[]map[string]string"},
		{"map of arrays, pattern", `map[string][]string @pattern("^[a-z]+$")`, "@pattern", "map[string][]string"},
		{"array of arrays, minimum", `[][]int @minimum(3)`, "@minimum", "[][]int"},
		{"array of arrays, maximum", `[][]int @maximum(3)`, "@maximum", "[][]int"},
		{"array of arrays, minLength", `[][]string @minLength(2)`, "@minLength", "[][]string"},
		{"map of arrays, maxLength", `map[string][]string @maxLength(2)`, "@maxLength", "map[string][]string"},
		{"map of maps, pattern", `map[string]map[string]string @pattern("^[a-z]+$")`, "@pattern", "map[string]map[string]string"},
		// The worst case: this one did not merely misplace the constraint, it
		// discarded the union, so the field accepted [[{"nonsense":1}]].
		{"array of arrays, variant", "[][]object" + variantBlock, "@variant", "[][]object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildCompositeField(t, c.decl)
			if err == nil {
				b, _ := json.Marshal(got)
				t.Fatalf("`%s` still builds, emitting %s.\nA value constraint on a composite "+
					"element is inert: this schema contradicts the declaration in silence, which "+
					"is exactly what memql#2951's definition of done forbade.", c.decl, b)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantAnnotation) {
				t.Errorf("the refusal must name the offending annotation %s.\n  got: %v", c.wantAnnotation, msg)
			}
			if !strings.Contains(msg, c.wantType) {
				t.Errorf("the refusal must quote the declaration back as `%s`, or the author has to "+
					"guess which field it means.\n  got: %v", c.wantType, msg)
			}
			if !strings.Contains(msg, "Single-wrap") {
				t.Errorf("the refusal must say what to write instead -- refusing without a remedy "+
					"leaves the author where the silent version left them.\n  got: %v", msg)
			}
			if !strings.Contains(msg, `"f"`) {
				t.Errorf("the refusal must name the property.\n  got: %v", msg)
			}
		})
	}
}

// TestCompositeElement_TypeAloneStillBuilds guards the blast radius: memql#3049
// refuses an ANNOTATION on a composite element, not the composite element.
//
// #2951 is what made these shapes keep their inner type instead of collapsing
// to the outer one, and that has to survive untouched -- a fix that refused
// `[][]string` outright would silently un-ship #2951.
func TestCompositeElement_TypeAloneStillBuilds(t *testing.T) {
	for _, c := range []struct {
		decl string
		want map[string]any
	}{
		{`[][]string @description("x")`, map[string]any{
			"description": "x",
			"type":        "array",
			"items":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}},
		{`[]map[string]int @description("x")`, map[string]any{
			"description": "x",
			"type":        "array",
			"items":       map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer"}},
		}},
		{`map[string][]string @description("x")`, map[string]any{
			"description":          "x",
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}},
	} {
		t.Run(c.decl, func(t *testing.T) {
			got, err := buildCompositeField(t, c.decl)
			if err != nil {
				t.Fatalf("`%s` must still build -- memql#3049 refuses a value ANNOTATION on a "+
					"composite element, never the type itself (that is memql#2951's guarantee).\n  got: %v",
					c.decl, err)
			}
			wantJSON, _ := json.Marshal(c.want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("`%s` lowered to the wrong schema.\n  want: %s\n  got:  %s", c.decl, wantJSON, gotJSON)
			}
		})
	}
}

// TestCompositeElement_FieldMarkersAreUnaffected is the other half of the blast
// radius. Field markers describe the FIELD, stay on the wrapper, and are
// unaffected by how deeply the element is wrapped -- so the new check must not
// catch a single one of them.
//
// Worth its own test because the rejection reads the same parsedProperty the
// markers land on: a check written over "any annotation" rather than over the
// value-constraint set would refuse `[][]string!` and break declarations that
// were never ambiguous.
func TestCompositeElement_FieldMarkersAreUnaffected(t *testing.T) {
	for _, decl := range []string{
		`[][]string @required @description("x")`,
		`[][]string @unique @description("x")`,
		`[][]string @immutable @description("x")`,
		`[][]string @secret @description("x")`,
		`[][]string @pii @description("x")`,
		`[][]string @internal @description("x")`,
		`[][]string @serverSet @description("x")`,
		`map[string][]int @required @immutable @description("x")`,
	} {
		t.Run(decl, func(t *testing.T) {
			if _, err := buildCompositeField(t, decl); err != nil {
				t.Errorf("`%s` must still build. Field markers describe the field, not the value, "+
					"so wrapping depth is irrelevant to them.\n  got: %v", decl, err)
			}
		})
	}
}

// TestSingleWrappedElement_ValueAnnotationsStillApply pins memql#2951's
// behaviour one level down, which memql#3049 must leave exactly as it found it.
//
// This is the test that catches an over-broad fix. Refusing every value
// annotation on any wrapped type would pass every assertion above and quietly
// revert #2951; only this one fails.
func TestSingleWrappedElement_ValueAnnotationsStillApply(t *testing.T) {
	t.Run("pattern lands on the element", func(t *testing.T) {
		got, err := buildCompositeField(t, `[]string @pattern("^[a-z]+$")`)
		if err != nil {
			t.Fatalf("`[]string @pattern(...)` must still build (memql#2951): %v", err)
		}
		items, _ := got["items"].(map[string]any)
		if items["pattern"] != "^[a-z]+$" {
			t.Errorf("the pattern must sit on the element, where it constrains each entry.\n  got: %#v", got)
		}
		if _, onWrapper := got["pattern"]; onWrapper {
			t.Errorf("the pattern must not stay on the array, where JSON Schema ignores it.\n  got: %#v", got)
		}
	})

	t.Run("minimum lands on the element", func(t *testing.T) {
		got, err := buildCompositeField(t, `[]int @minimum(3)`)
		if err != nil {
			t.Fatalf("`[]int @minimum(3)` must still build (memql#2951): %v", err)
		}
		items, _ := got["items"].(map[string]any)
		if items["minimum"] == nil {
			t.Errorf("the minimum must sit on the element.\n  got: %#v", got)
		}
	})

	t.Run("map values carry the constraint", func(t *testing.T) {
		got, err := buildCompositeField(t, `map[string]string @pattern("^[a-z]+$")`)
		if err != nil {
			t.Fatalf("`map[string]string @pattern(...)` must still build (memql#2951): %v", err)
		}
		values, _ := got["additionalProperties"].(map[string]any)
		if values["pattern"] != "^[a-z]+$" {
			t.Errorf("the pattern must sit on the map's value schema.\n  got: %#v", got)
		}
	})

	t.Run("variant keeps its union and discriminator", func(t *testing.T) {
		got, err := buildCompositeField(t, "[]object"+variantBlock)
		if err != nil {
			t.Fatalf("`[]object @variant(...)` must still build (memql#2951): %v", err)
		}
		items, _ := got["items"].(map[string]any)
		oneOf, _ := items["oneOf"].([]any)
		if len(oneOf) != 2 {
			t.Fatalf("the union must survive on the element, with both branches.\n  got: %#v", got)
		}
		if items["x-discriminator"] != "kind" {
			t.Errorf("the discriminator must survive alongside the union.\n  got: %#v", items)
		}
	})
}
