package memoryNodes

import (
	"encoding/json"
	"strings"
	"testing"
)

// concept_variant_branchless_test.go covers memql#3123 / task #3197: a
// `@variant(discriminator="...")` written WITHOUT its branch block.
//
// Before the fix the annotation was silently dropped at every depth. The
// discriminator was harvested only inside `if len(prop.Variants) > 0`, and
// valueAnnotationNames keyed @variant off the branch list rather than off the
// attribute -- so a branch-less @variant populated neither, and memql#3049's
// composite guard never saw it. The author had said "this field is a
// discriminated union" and got a schema asserting only `type: object`, with no
// error: a row then validates against nothing, which memql#3049 named as the
// worst case of the whole family.
//
// The ruling is to refuse it at load, at any depth. An author who wrote the
// attribute meant a union, and a union with no branches is not one.

// branchlessVariantConcept spells a concept whose sole property carries a
// branch-less @variant at the given declared type, so the depth axis is the
// only thing that varies across the table below.
func branchlessVariantConcept(declType string) []byte {
	return []byte(`
@description("A widget.")
concept Widget {
  body  ` + declType + `  @variant(discriminator="kind")
}
`)
}

// TestParseConcept_BranchlessVariant_RefusedAtEveryDepth is the depth table.
// Each row built a schema that contradicted its declaration in silence before
// the fix; each must now fail to load with the property named.
func TestParseConcept_BranchlessVariant_RefusedAtEveryDepth(t *testing.T) {
	for _, declType := range []string{
		"object",
		"[]object",
		"[][]object",
		"map[string]object",
	} {
		t.Run(declType, func(t *testing.T) {
			_, err := ParseConceptMemQL(branchlessVariantConcept(declType), "v1/test/widget")
			if err == nil {
				t.Fatalf("`body %s @variant(discriminator=\"kind\")` with no branch block loaded without error -- "+
					"the discriminated union is dropped and the row validates against nothing", declType)
			}
			if !strings.Contains(err.Error(), "@variant") {
				t.Errorf("diagnostic does not name the annotation: %v", err)
			}
			if !strings.Contains(err.Error(), "body") {
				t.Errorf("diagnostic does not name the property: %v", err)
			}
		})
	}
}

// TestParseConcept_BranchlessVariant_RefusedWhenNested pins that the refusal
// does not depend on the property sitting at the top level. A nested object
// leaf goes through the same propertyDeclToParsed, so a branch-less @variant
// buried one level down must be refused too.
func TestParseConcept_BranchlessVariant_RefusedWhenNested(t *testing.T) {
	content := []byte(`
@description("A widget.")
concept Widget {
  payload {
    body  object  @variant(discriminator="kind")
  }
}
`)
	_, err := ParseConceptMemQL(content, "v1/test/widget")
	if err == nil {
		t.Fatal("a nested branch-less @variant loaded without error")
	}
	if !strings.Contains(err.Error(), "@variant") || !strings.Contains(err.Error(), "body") {
		t.Errorf("diagnostic does not name the annotation and the property: %v", err)
	}
}

// TestParseConcept_WellFormedVariant_Unaffected is the negative control the
// refusal must not swallow. A scanner that refused every @variant would pass
// the table above while breaking the one live use in the tree
// (identity:identity.credentials), so the well-formed spellings are pinned
// here at each depth the grammar allows.
func TestParseConcept_WellFormedVariant_Unaffected(t *testing.T) {
	t.Run("object", func(t *testing.T) {
		content := []byte(`
@description("A widget.")
concept Widget {
  kind  string!
  body  object!  @variant(discriminator="kind") {
    oauth   { provider string! }
    api_key { keyHash  string! }
  }
}
`)
		c, err := ParseConceptMemQL(content, "v1/test/widget")
		if err != nil {
			t.Fatalf("well-formed @variant failed to load: %v", err)
		}
		body := propertySchema(t, c, "body")
		if _, ok := body["oneOf"]; !ok {
			t.Errorf("well-formed @variant lost its union: %v", body)
		}
	})

	t.Run("[]object", func(t *testing.T) {
		content := []byte(`
@description("A widget.")
concept Widget {
  kind    string!
  blocks  []object  @variant(discriminator="kind") {
    text  { value string! }
    image { url   string! }
  }
}
`)
		c, err := ParseConceptMemQL(content, "v1/test/widget")
		if err != nil {
			t.Fatalf("well-formed []object @variant failed to load: %v", err)
		}
		items, ok := propertySchema(t, c, "blocks")["items"].(map[string]any)
		if !ok {
			t.Fatal("[]object @variant lost its items schema")
		}
		if _, ok := items["oneOf"]; !ok {
			t.Errorf("well-formed []object @variant lost its union: %v", items)
		}
	})

	// The map case is called out explicitly in the acceptance criteria
	// because the union rides on `additionalProperties` with
	// `x-discriminator` there rather than on `items`, so a refusal keyed off
	// the wrong field would break it while the two above stayed green.
	t.Run("map[string]object", func(t *testing.T) {
		content := []byte(`
@description("A widget.")
concept Widget {
  kind   string!
  slots  map[string]object  @variant(discriminator="kind") {
    text  { value string! }
    image { url   string! }
  }
}
`)
		c, err := ParseConceptMemQL(content, "v1/test/widget")
		if err != nil {
			t.Fatalf("well-formed map[string]object @variant failed to load: %v", err)
		}
		addl, ok := propertySchema(t, c, "slots")["additionalProperties"].(map[string]any)
		if !ok {
			t.Fatal("map[string]object @variant lost its additionalProperties schema")
		}
		if _, ok := addl["oneOf"]; !ok {
			t.Errorf("well-formed map @variant lost its union: %v", addl)
		}
		if addl["x-discriminator"] != "kind" {
			t.Errorf("map @variant lost x-discriminator: %v", addl)
		}
	})
}

// propertySchema pulls one property out of a parsed concept's definition
// schema, so each assertion above reads as the thing it is checking rather
// than as three lines of unmarshalling.
func propertySchema(t *testing.T, c *Concept, field string) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties in schema")
	}
	fp, ok := props[field].(map[string]any)
	if !ok {
		t.Fatalf("missing property %q", field)
	}
	return fp
}
