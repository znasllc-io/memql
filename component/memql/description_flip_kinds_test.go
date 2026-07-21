package memql

// description_flip_kinds_test.go -- memql#2634 per-kind precedence pins:
// every converter resolves /// over @description (ruling 3), with the
// annotation-only shape identical to pre-flip output.

import (
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func parseKind(t *testing.T, src string) *languageParser.File {
	t.Helper()
	normalised, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	file, err := languageParser.ParseFile(normalised)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return file
}

func TestDescriptionFlip_ShapeToolSpecPromptPolicy(t *testing.T) {
	t.Run("shape", func(t *testing.T) {
		file := parseKind(t, "/// Shape doc.\n@row\n@description(\"Shape annot.\")\nshape candidate flipCard {\n  status\n}")
		for _, def := range file.Definitions {
			if sd, ok := def.(*languageParser.ShapeDecl); ok {
				conv, err := shapeDeclToShapeDefinition(sd, "test")
				if err != nil {
					t.Fatal(err)
				}
				if conv.Description != "Shape doc." {
					t.Errorf("shape Description = %q, want the /// channel", conv.Description)
				}
				return
			}
		}
		t.Fatal("shape not parsed")
	})
	t.Run("spec", func(t *testing.T) {
		decl, err := languageParser.ParseSpecDecl("/// Spec doc.\n@description(\"Spec annot.\")\nspec actorEnvelope flipSpec {\n  return role == \"admin\"\n}")
		if err != nil {
			t.Fatal(err)
		}
		spec, err := specDeclToSpec(decl, "test")
		if err != nil {
			t.Fatal(err)
		}
		if spec.Description != "Spec doc." {
			t.Errorf("spec Description = %q, want the /// channel", spec.Description)
		}
	})
	t.Run("tool", func(t *testing.T) {
		decl, err := languageParser.ParseToolDecl("/// Tool doc.\n@handler(type=\"function\", name=\"flipTool\")\n@description(\"Tool annot.\")\ntool flipTool {\n  role string!\n}")
		if err != nil {
			t.Fatal(err)
		}
		tools, err := toolDeclToTool(decl, "test")
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) == 0 || tools[0].Description != "Tool doc." {
			t.Errorf("tool Description want the /// channel, got %+v", tools)
		}
	})
	t.Run("annotation-only-identical", func(t *testing.T) {
		decl, err := languageParser.ParseSpecDecl("@description(\"Only annot.\")\nspec actorEnvelope flipSpec2 {\n  return role == \"admin\"\n}")
		if err != nil {
			t.Fatal(err)
		}
		spec, err := specDeclToSpec(decl, "test")
		if err != nil {
			t.Fatal(err)
		}
		if spec.Description != "Only annot." {
			t.Errorf("annotation-only must be identical to pre-flip, got %q", spec.Description)
		}
	})
}

// Prompt, provider, and policy converters resolve ruling-3 precedence too --
// each was a surviving mutant in the round-1 review.
func TestDescriptionFlip_PromptProviderPolicy(t *testing.T) {
	t.Run("prompt", func(t *testing.T) {
		file := parseKind(t, "/// Prompt doc.\n@description(\"Prompt annot.\")\n@templateFile(\"prompts/x.tmpl\")\nprompt flipPrompt {\n}")
		for _, def := range file.Definitions {
			if pd, ok := def.(*languageParser.PromptDecl); ok {
				out, err := promptDeclToPromptDecl(pd, "test")
				if err != nil {
					t.Fatal(err)
				}
				if got := languageParser.EffectiveDescription(out.docComment, out.description); got != "Prompt doc." {
					t.Errorf("prompt resolved description = %q", got)
				}
				return
			}
		}
		t.Fatal("prompt not parsed")
	})
	t.Run("policy", func(t *testing.T) {
		decl, err := languageParser.ParsePolicyDecl("/// Policy doc.\n@primary(\"p\")\n@description(\"Policy annot.\")\npolicy flipPolicy {\n}")
		if err != nil {
			t.Fatal(err)
		}
		pol, err := policyDeclToPolicyConfig(decl)
		if err != nil {
			t.Fatal(err)
		}
		if pol.Description != "Policy doc." {
			t.Errorf("policy Description = %q", pol.Description)
		}
	})
	t.Run("provider", func(t *testing.T) {
		decl, err := languageParser.ParseProviderDecl("/// Provider doc.\n@base\n@type(\"Anthropic\")\n@description(\"Provider annot.\")\nprovider flipProvider {\n  auth {\n    apiKey env(\"X\")\n  }\n}")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := providerDeclToProviderConfig(decl)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Description != "Provider doc." {
			t.Errorf("provider Description = %q", cfg.Description)
		}
	})
}
