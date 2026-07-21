package memql

// description_flip_test.go -- memql#2634: description sourcing flips to ///
// doc-comments with @description fallback (design ruling 3: /// wins, never
// concatenate; @description-only output must be byte-identical to pre-flip).

import (
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
