package memql

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// loadedSchemaEngine builds a real engine with the full embedded DSL tree
// loaded (concepts + functions), mirroring the cluster bootstrap. DB-free.
func loadedSchemaEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts; DSL tree did not load")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the full DSL tree failed: %v", err)
	}
	return eng
}

// TestMCPSchemaSingleSourceOfTruth is the #1713 contract guard: for EVERY
// @mcp-promoted function, the inputSchema the typed MCP wrapper advertises
// (MCPPromotedFunctionTools) and the inputSchema describeFunction/help reports
// (functionHelpPayload) MUST be byte-identical, because both render through the
// single source of truth FunctionInputJSONSchema. The two contracts therefore
// cannot drift.
//
// This FAILS before #1713: describeFunction emitted only a flat `argsSchema`
// list and no JSON-Schema `inputSchema` at all, so a connector consumer reading
// describeFunction saw a different (and incomplete) contract than the typed
// wrapper advertised.
func TestMCPSchemaSingleSourceOfTruth(t *testing.T) {
	eng := loadedSchemaEngine(t)

	wrappers := eng.MCPPromotedFunctionTools()
	if len(wrappers) == 0 {
		t.Fatal("no @mcp-promoted functions found; expected libraryArtifactById et al.")
	}

	sawArtifact := false
	for _, w := range wrappers {
		name, _ := w["name"].(string)
		wrapSchema, _ := w["inputSchema"].(map[string]any)
		if wrapSchema == nil {
			t.Errorf("promoted tool %q advertises no inputSchema", name)
			continue
		}

		fn, err := eng.functions.Get(name)
		if err != nil || fn == nil {
			t.Errorf("promoted tool %q absent from the function registry: %v", name, err)
			continue
		}

		help := functionHelpPayload(fn)
		helpSchema, _ := help["inputSchema"].(map[string]any)
		if helpSchema == nil {
			t.Errorf("describeFunction payload for %q carries no inputSchema -- source-of-truth drift (#1713)", name)
			continue
		}

		// The two consumer surfaces must be identical...
		if !reflect.DeepEqual(wrapSchema, helpSchema) {
			t.Errorf("schema drift for %q between typed wrapper and describeFunction:\n  wrapper=%#v\n  help   =%#v", name, wrapSchema, helpSchema)
		}
		// ...and both must be exactly what the single builder produces.
		if canonical := FunctionInputJSONSchema(fn); !reflect.DeepEqual(wrapSchema, canonical) {
			t.Errorf("wrapper schema for %q is not the FunctionInputJSONSchema output:\n  wrapper  =%#v\n  canonical=%#v", name, wrapSchema, canonical)
		}

		if name == "libraryArtifactById" {
			sawArtifact = true
			// The reported bug: artifactId silently dropped. The complete,
			// correct schema must declare artifactId AND mark it required.
			props, _ := wrapSchema["properties"].(map[string]any)
			if _, ok := props["artifactId"]; !ok {
				t.Errorf("libraryArtifactById schema missing artifactId property: %#v", wrapSchema)
			}
			required, _ := wrapSchema["required"].([]any)
			found := false
			for _, r := range required {
				if r == "artifactId" {
					found = true
				}
			}
			if !found {
				t.Errorf("libraryArtifactById must mark artifactId required: %#v", wrapSchema)
			}
		}
	}

	if !sawArtifact {
		t.Error("libraryArtifactById not present among @mcp-promoted functions (the reported buggy wrapper)")
	}
}

// TestFunctionInputJSONSchemaCompleteness pins that the single builder carries
// every declared constraint through to the JSON-Schema (not just the bare
// type), so connector consumers see a complete contract (#1713 "complete &
// consistent tool schemas").
func TestFunctionInputJSONSchemaCompleteness(t *testing.T) {
	addl := false
	minV := 1.0
	maxV := 10.0
	fn := &Function{
		Name:         "rich",
		FunctionKind: "query",
		ArgsSchema: &ArgsSchemaConfig{
			AdditionalProperties: &addl,
			Fields: []*FunctionArgsField{
				{Name: "id", Type: "string", Optional: false, Format: "uuid", Pattern: "^x", MaxLength: 36},
				{Name: "count", Type: "number", Optional: true, Minimum: &minV, Maximum: &maxV},
				{Name: "kind", Type: "string", Optional: true, Enum: []any{"a", "b"}},
				{Name: "tags", Type: "array", Optional: true, Items: &FunctionArgsField{Type: "string"}},
				{Name: "nested", Type: "object", Optional: true, Nested: []*FunctionArgsField{
					{Name: "inner", Type: "string", Optional: false},
				}},
			},
		},
	}

	s := FunctionInputJSONSchema(fn)
	props := s["properties"].(map[string]any)

	idP := props["id"].(map[string]any)
	if idP["format"] != "uuid" || idP["pattern"] != "^x" || idP["maxLength"] != 36 {
		t.Errorf("id constraints not carried: %#v", idP)
	}
	countP := props["count"].(map[string]any)
	if countP["minimum"] != 1.0 || countP["maximum"] != 10.0 {
		t.Errorf("count numeric bounds not carried: %#v", countP)
	}
	kindP := props["kind"].(map[string]any)
	if enum, _ := kindP["enum"].([]any); len(enum) != 2 {
		t.Errorf("kind enum not carried: %#v", kindP)
	}
	tagsP := props["tags"].(map[string]any)
	if items, _ := tagsP["items"].(map[string]any); items["type"] != "string" {
		t.Errorf("tags items schema not carried: %#v", tagsP)
	}
	nestedP := props["nested"].(map[string]any)
	nProps, _ := nestedP["properties"].(map[string]any)
	if _, ok := nProps["inner"]; !ok {
		t.Errorf("nested object properties not carried: %#v", nestedP)
	}
	if req, _ := nestedP["required"].([]any); len(req) != 1 || req[0] != "inner" {
		t.Errorf("nested required not carried: %#v", nestedP)
	}
	if s["additionalProperties"] != false {
		t.Errorf("top-level additionalProperties not carried: %#v", s)
	}
	if req, _ := s["required"].([]any); len(req) != 1 || req[0] != "id" {
		t.Errorf("top-level required = %v, want [id]", s["required"])
	}
}
