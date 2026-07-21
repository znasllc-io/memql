package memql

// description_flip_test.go -- memql#2634: description sourcing flips to ///
// doc-comments with @description fallback (design ruling 3: /// wins, never
// concatenate; @description-only output must be byte-identical to pre-flip).

import (
	"encoding/json"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"strings"
	"testing"
)

func loadFlipProbe(t *testing.T, doc, annot string) *Function {
	t.Helper()
	var lines []string
	if doc != "" {
		lines = append(lines, "/// "+doc)
	}
	if annot != "" {
		lines = append(lines, "@description(\""+annot+"\")")
	}
	lines = append(lines,
		"logic flipProbeLogic {",
		"  args {",
		"    /// The subject arg.",
		"    a string @required",
		"  }",
		"  body {",
		"    return coalesce(args.a, \"\")",
		"  }",
		"}")
	fn, err := tryParseNewFunctionSyntax("flipProbeLogic", "logic", strings.Join(lines, "\n"), "common.logic.memql", dotAccessLoadRegistry())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return fn
}

// The engine Function.Description resolves precedence at the loader copy
// site, so every registry consumer (functions()/tools, MCP descriptors,
// sense hover, function-tools) inherits in one place.
func TestDescriptionFlip_FunctionPrecedence(t *testing.T) {
	if fn := loadFlipProbe(t, "Doc channel.", "Annot channel."); fn.Description != "Doc channel." {
		t.Errorf("both channels: Description = %q, want the /// channel", fn.Description)
	}
	if fn := loadFlipProbe(t, "Doc only.", ""); fn.Description != "Doc only." {
		t.Errorf("///-only: Description = %q", fn.Description)
	}
	if fn := loadFlipProbe(t, "", "Annot only."); fn.Description != "Annot only." {
		t.Errorf("@description-only must be IDENTICAL to pre-flip, got %q", fn.Description)
	}
}

// Args-field /// docs reach the engine args schema and the JSON-schema
// renderers -- the first time arg descriptions reach the runtime at all.
func TestDescriptionFlip_ArgsFieldReachesSchemas(t *testing.T) {
	fn := loadFlipProbe(t, "Doc.", "")
	if fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) == 0 {
		t.Fatal("args schema missing")
	}
	if got := fn.ArgsSchema.Fields[0].Description; got != "The subject arg." {
		t.Errorf("engine args-field Description = %q", got)
	}

	schema := FunctionInputJSONSchema(fn)
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props["a"].(map[string]any)
	if got, _ := prop["description"].(string); got != "The subject arg." {
		t.Errorf("FunctionInputJSONSchema description = %q", got)
	}
}

// Function.clone must carry DocComment (census-found bug) and the resolved
// Description together.
func TestDescriptionFlip_CloneCarriesDocComment(t *testing.T) {
	fn := loadFlipProbe(t, "Doc channel.", "Annot channel.")
	c := fn.clone()
	if c.DocComment != fn.DocComment || c.DocComment == "" {
		t.Errorf("clone dropped DocComment: %q vs %q", c.DocComment, fn.DocComment)
	}
}

// The promote-time catalog match text prefers the /// block via the parser's
// own lexer extraction, with the @description regex as fallback.
func TestDescriptionFlip_CatalogMatchText(t *testing.T) {
	src := "/// Catalog doc channel.\n@description(\"Catalog annot channel.\")\nlogic catProbe {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
	text, err := CatalogMatchText("logic", src)
	if err != nil {
		t.Fatalf("CatalogMatchText: %v", err)
	}
	if !strings.Contains(text, "intent:Catalog doc channel. form:") {
		t.Errorf("catalog intent must carry the /// channel alone (form: keeps raw source, pre-existing), got %q", text)
	}
	if strings.Contains(text, "intent:Catalog annot channel.") {
		t.Errorf("catalog intent must not be the fallback when /// present, got %q", text)
	}
	annotOnly := "@description(\"Catalog annot channel.\")\nlogic catProbe {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
	text2, err := CatalogMatchText("logic", annotOnly)
	if err != nil {
		t.Fatalf("CatalogMatchText: %v", err)
	}
	if !strings.Contains(text2, "intent:Catalog annot channel.") {
		t.Errorf("@description-only catalog text must be unchanged, got %q", text2)
	}
}

// The registry clones at ingress AND egress -- the review blocker: arg docs
// must survive Upsert+Lookup, or both schema renderers serve empty
// descriptions in production while loader-level tests stay green.
func TestDescriptionFlip_ArgDocSurvivesRegistry(t *testing.T) {
	fn := loadFlipProbe(t, "Doc.", "")
	reg := newFunctionRegistry()
	if err := reg.Upsert(fn); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := reg.Get(fn.Name)
	if err != nil || got == nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ArgsSchema == nil || len(got.ArgsSchema.Fields) == 0 || got.ArgsSchema.Fields[0].Description != "The subject arg." {
		t.Fatalf("arg Description dropped across the registry: %+v", got.ArgsSchema)
	}
	schema := FunctionInputJSONSchema(got)
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props["a"].(map[string]any)
	if d, _ := prop["description"].(string); d != "The subject arg." {
		t.Errorf("registry-egress schema description = %q", d)
	}
}

// The SECOND schema renderer (function-tools) emits the description too --
// its own pin so the two schemas cannot silently diverge.
func TestDescriptionFlip_FunctionToolsSchemaDescription(t *testing.T) {
	fn := loadFlipProbe(t, "Doc.", "")
	raw, err := toolInputSchemaFromArgs(fn.ArgsSchema)
	if err != nil {
		t.Fatalf("toolInputSchemaFromArgs: %v", err)
	}
	if !strings.Contains(string(raw), `"description":"The subject arg."`) {
		t.Errorf("function-tools schema missing field description: %s", raw)
	}
}

// The catalog extraction anchors on the construct even when a use/import
// prelude precedes it (authoring slices carry file-top imports).
func TestDescriptionFlip_CatalogWithUsePrelude(t *testing.T) {
	src := "use cognition.concepts.{ space }\n\n/// Prelude doc channel.\nlogic catPreludeProbe {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
	text, err := CatalogMatchText("logic", src)
	if err != nil {
		t.Fatalf("CatalogMatchText: %v", err)
	}
	if !strings.Contains(text, "intent:Prelude doc channel.") {
		t.Errorf("use-prelude must not hide the /// block, got %q", text)
	}
}

// The concept-level JSON schema description agrees with Concept.Description
// (single-point precedence -- no contradictory surfaces within one concept).
func TestDescriptionFlip_ConceptSchemaCoherence(t *testing.T) {
	src := "/// Concept doc channel.\n@description(\"Concept annot channel.\")\nconcept flipCoherenceProbe {\n  ownerUserId string!\n}"
	file := parseKind(t, src)
	for _, def := range file.Definitions {
		if cd, ok := def.(*languageParser.ConceptDecl); ok {
			c, err := concept.BuildConceptFromDecl(cd, "flipCoherenceProbe")
			if err != nil {
				t.Fatal(err)
			}
			if c.Description != "Concept doc channel." {
				t.Errorf("Concept.Description = %q", c.Description)
			}
			var schema map[string]any
			if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil {
				t.Fatalf("schema: %v (keys: %v)", err, c.Schemas)
			}
			if d, _ := schema["description"].(string); d != "Concept doc channel." {
				t.Errorf("schema description = %q, disagrees with Concept.Description", d)
			}
			return
		}
	}
	t.Fatal("concept not parsed")
}
