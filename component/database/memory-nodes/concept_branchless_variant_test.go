package memoryNodes

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// memql#3123: `@variant(discriminator="kind")` written WITHOUT its branch block
// was silently dropped -- the discriminator never reached the schema and the
// field built as a bare `type: object`, which validates against nothing.
//
// Everything about @variant keyed off the BRANCH LIST rather than the
// attribute: the discriminator is harvested inside `len(prop.Variants) > 0`,
// and valueAnnotationNames reports @variant from `len(variants)`. So the one
// spelling that declares the intent without the branches was invisible to all
// of them -- including memql#3049's composite guard.
//
// Depth-independent, which is why it is not a regression of #3049: `object`,
// `[]object` and `[][]object` were all equally silent. #3049 made the composite
// case expressible enough to notice; this is what its guard still let through.
//
// @variant is the annotation #3049 called its worst case, precisely because a
// dropped union means a row validates against nothing at all.

// variantSrc builds a one-property concept whose field `f` has the given type
// and annotation text.
func variantSrc(decl string) string {
	return "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept probe {\n  label string @required @description(\"l\")\n  f " + decl + "\n}\n"
}

// branchBlock is a well-formed two-branch union, written once so the accepted
// control and the refused case differ ONLY in whether the branches are present.
const branchBlock = " @variant(discriminator=\"kind\") {\n" +
	"    text {\n      kind string @required\n    }\n" +
	"    image {\n      url string @required\n    }\n  }"

func TestBranchlessVariantIsRefusedAtEveryDepth(t *testing.T) {
	// Every wrapping depth the grammar allows for an object field. All three
	// were equally silent before, so all three are asserted -- a fix applied at
	// one depth would otherwise look complete.
	for _, decl := range []string{
		`object @variant(discriminator="kind") @description("x")`,
		`[]object @variant(discriminator="kind") @description("x")`,
		`[][]object @variant(discriminator="kind") @description("x")`,
		`map[string]object @variant(discriminator="kind") @description("x")`,
	} {
		t.Run(decl, func(t *testing.T) {
			c, err := tryBuildConcept(variantSrc(decl), "v1:aud:branchless")
			if err == nil {
				raw, _ := c.DefinitionSchema()
				t.Fatalf("a branch-less @variant was accepted, and the discriminator is gone:\n  %s\n\n"+
					"The author declared a discriminated union and got a schema that validates "+
					"against nothing (memql#3123).", raw)
			}
			msg := err.Error()
			if !strings.Contains(msg, "@variant") {
				t.Errorf("the refusal does not name the annotation.\n  %v", msg)
			}
			if !strings.Contains(msg, `"f"`) && !strings.Contains(msg, " f ") {
				t.Errorf("the refusal does not name the property, so an author cannot find it.\n  %v", msg)
			}
			if !strings.Contains(msg, "branch") {
				t.Errorf("the refusal does not say what is missing.\n  %v", msg)
			}
		})
	}
}

// The converse. Without it, a guard that refused every @variant would pass the
// test above while deleting the feature.
func TestWellFormedVariantStillBuildsAtEveryDepth(t *testing.T) {
	for _, c := range []struct {
		decl      string
		unionPath []string // where the oneOf is expected to live
	}{
		{"object" + branchBlock, []string{}},
		{"[]object" + branchBlock, []string{"items"}},
		// map[string]object rides on additionalProperties, which the issue
		// called out specifically as a no-regression case.
		{"map[string]object" + branchBlock, []string{"additionalProperties"}},
	} {
		t.Run(c.decl[:min(len(c.decl), 24)], func(t *testing.T) {
			built, err := tryBuildConcept(variantSrc(c.decl), "v1:aud:wellformed")
			if err != nil {
				t.Fatalf("a well-formed @variant was refused: %v", err)
			}
			raw, serr := built.DefinitionSchema()
			if serr != nil {
				t.Fatalf("schema: %v", serr)
			}
			var doc struct {
				Properties map[string]map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			node := doc.Properties["f"]
			for _, seg := range c.unionPath {
				next, ok := node[seg].(map[string]any)
				if !ok {
					t.Fatalf("expected a %q level in the schema for %q, got: %#v", seg, c.decl, node)
				}
				node = next
			}
			oneOf, _ := node["oneOf"].([]any)
			if len(oneOf) != 2 {
				t.Errorf("the union did not survive: expected 2 branches, got %#v", node)
			}
			if node["x-discriminator"] != "kind" {
				t.Errorf("the discriminator did not survive alongside the union: %#v", node)
			}
		})
	}
}

// A field with no @variant at all must be untouched -- the overwhelmingly
// common case, and where a regression would be widest.
func TestPlainObjectFieldIsUnaffected(t *testing.T) {
	for _, decl := range []string{
		`object @description("x")`,
		`[]object @description("x")`,
		`[][]object @description("x")`,
	} {
		if _, err := tryBuildConcept(variantSrc(decl), "v1:aud:plainobj"); err != nil {
			t.Errorf("a field with no @variant was refused: %s -> %v", decl, err)
		}
	}
}

func TestHasVariantAttribute(t *testing.T) {
	if hasVariantAttribute(nil) {
		t.Error("nil attributes reported a @variant")
	}
	if hasVariantAttribute([]*parser.Attribute{nil, {Name: "description"}}) {
		t.Error("a non-variant attribute set reported a @variant")
	}
	if !hasVariantAttribute([]*parser.Attribute{{Name: "description"}, {Name: "variant"}}) {
		t.Error("a @variant attribute was not found")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// tryBuildConcept parses a single-concept fixture and builds it under a
// caller-supplied id, returning the build error rather than failing the test --
// the refusal IS the behaviour under test here.
//
// A distinct id per fixture on purpose: compileSchema caches by concept id, so
// two fixtures sharing one id in a single run can validate against each other's
// schema.
func tryBuildConcept(src, conceptId string) (*Concept, error) {
	file, err := parser.ParseFile(src)
	if err != nil {
		return nil, err
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		return BuildConceptFromDecl(decl, conceptId)
	}
	return nil, errNoConceptInFixture
}

var errNoConceptInFixture = fmt.Errorf("fixture declared no concept, so it measures nothing")
