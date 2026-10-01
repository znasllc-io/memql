package memql

import (
	"reflect"
	"testing"
)

// prompt_array_items_test.go -- a prompt's `[]T` field publishes items of T.
//
// toInputSchema used to publish every array's items as objects whatever the
// declared element, so `inputKeys []string` refused its own strings:
//
//	jsonschema: '/inputKeys/0' does not validate with
//	.../goalComplexityTriage#/properties/inputKeys/items/type:
//	expected object, but got string
//
// Every Ask turn carries a goal input (`conversation`), so triage refused
// every one, the compile treated even "hello" as complex, and the turn sat in
// the author route for minutes. compileGoal and the router's compileRule,
// classifyRequest and composeRoutingPolicy declare `[]string` fields too.

func TestPromptArrayFieldPublishesItsElementType(t *testing.T) {
	cases := []struct {
		elem string
		want map[string]any
	}{
		{"string", map[string]any{"type": "string"}},
		{"int", map[string]any{"type": "integer"}},
		{"number", map[string]any{"type": "number"}},
		{"boolean", map[string]any{"type": "boolean"}},
		{"object", map[string]any{"type": "object"}},
		{"[]string", map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		// `array` written without the shorthand names no element.
		{"", map[string]any{"type": "object"}},
	}
	for _, c := range cases {
		decl := &promptDecl{name: "p", fields: []toolField{{name: "xs", typeName: "array", elementType: c.elem}}}
		schema, err := decl.toInputSchema()
		if err != nil {
			t.Fatal(err)
		}
		prop := schema["properties"].(map[string]any)["xs"].(map[string]any)
		if got := prop["items"]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("[]%s: items %#v, want %#v", c.elem, got, c.want)
		}
	}
}

// The corpus half: the prompts this broke accept what they declare.
func TestCorpusPromptsAcceptTheirDeclaredStringLists(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	cases := map[string]map[string]any{
		"goalComplexityTriage": {"goal": "hello", "inputKeys": []any{"conversation"}},
		"compileGoal":          {"statement": "hello", "now": "2026-09-28T06:24:19Z", "inputKeys": []any{"conversation"}},
		"classifyRequest":      {"request": "hello", "levels": []any{"fast", "strong"}, "ruledOut": []any{"embeddings"}},
		"compileRule": {"sentence": "use the fast lane for triage", "whenKeys": []any{"prompt"},
			"policies": []any{map[string]any{"name": "balanced"}}, "levels": []any{"fast", "strong"}},
	}
	for name, data := range cases {
		p, ok := prompts.Get(name)
		if !ok {
			t.Errorf("prompt %q is not in the corpus", name)
			continue
		}
		if err := p.ValidateData(data); err != nil {
			t.Errorf("prompt %q refused its own declared input: %v", name, err)
		}
	}
}
