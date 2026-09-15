package memql

import (
	"github.com/znasllc-io/memql/component/language/annotations"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestPromptFieldRefusesUnknownAnnotation closes the tolerance half of D16.
// prompt_converter's default arm was a comment saying the annotation was
// swallowed, so `@requred` -- one r -- on a prompt field loaded clean and the
// field was simply not required, reported to nobody. An args field has
// refused this since #991; a prompt body IS the schema handed to the model,
// so it needs the same gate.
func TestPromptFieldRefusesUnknownAnnotation(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Fields: []*languageParser.PromptField{{
			Name:       "space",
			Type:       "object",
			Attributes: []*languageParser.Attribute{{Name: "requred"}},
		}},
	}
	_, err := promptDeclToPromptDecl(decl, "dsl/agents/prompts.memql")
	if err == nil {
		t.Fatal("an unknown prompt field annotation should be refused")
	}
	if !strings.Contains(err.Error(), "requred") {
		t.Errorf("refusal should name the offending annotation, got: %v", err)
	}
}

// TestPromptFieldRefusesRetiredAnnotation routes the field ledger through the
// prompt body, so a retired name refuses with the rewrite named from every
// body it can appear in rather than only from a concept.
func TestPromptFieldRefusesRetiredAnnotation(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Fields: []*languageParser.PromptField{{
			Name:       "space",
			Type:       "object",
			Attributes: []*languageParser.Attribute{{Name: "unique"}},
		}},
	}
	_, err := promptDeclToPromptDecl(decl, "dsl/agents/prompts.memql")
	if err == nil {
		t.Fatal("a retired field annotation should be refused on a prompt field")
	}
	if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
		t.Errorf("refusal should name the rewrite, got: %v", err)
	}
}

// TestPromptFieldKeepsItsSchemaAnnotations guards against over-rejection. A
// prompt or tool field's @default IS the JSON-Schema default the model reads,
// which is exactly why memql#5375 retired @default on a CONCEPT field and
// kept it here -- the two receivers were doing different things under one
// name, and only one of them was doing nothing.
func TestPromptFieldKeepsItsSchemaAnnotations(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Fields: []*languageParser.PromptField{{
			Name: "tone",
			Type: "string",
			Attributes: []*languageParser.Attribute{
				{Name: "required"},
				{Name: "description", Value: "how the reply should read"},
				{Name: "default", Value: "neutral"},
			},
		}},
	}
	if _, err := promptDeclToPromptDecl(decl, "dsl/agents/prompts.memql"); err != nil {
		t.Fatalf("the schema annotations must still be accepted: %v", err)
	}
}

// TestBuiltinFieldRefusesUnknownAnnotation is the builtin half. The field
// loop kept @required off the typed field and dropped every other annotation
// without a word, so a @description on a builtin field vanished from the
// schema both SDKs generate.
func TestBuiltinFieldRefusesUnknownAnnotation(t *testing.T) {
	decl := &languageParser.BuiltinDecl{
		Name: "workbenchDispatchHost",
		Attributes: []*languageParser.Attribute{
			{Name: "executor", Value: "integration.workbench.dispatchHost"},
		},
		Fields: []*languageParser.BuiltinField{{
			Name:       "runId",
			Type:       "string",
			Attributes: []*languageParser.Attribute{{Name: "bogusFieldAnno"}},
		}},
	}
	_, err := builtinDeclToFunction(decl, "dsl/workbench/builtins.memql")
	if err == nil {
		t.Fatal("an unknown builtin field annotation should be refused")
	}
	if !strings.Contains(err.Error(), "bogusFieldAnno") {
		t.Errorf("refusal should name the offending annotation, got: %v", err)
	}
}

// TestBuiltinFieldKeepsItsSchemaAnnotations is the over-rejection guard for
// the builtin body, which is the one this could plausibly break: @required
// was the ONLY annotation it read before, so every other name in the
// allow-list is newly accepted here rather than newly refused.
func TestBuiltinFieldKeepsItsSchemaAnnotations(t *testing.T) {
	decl := &languageParser.BuiltinDecl{
		Name: "workbenchDispatchHost",
		Attributes: []*languageParser.Attribute{
			{Name: "executor", Value: "integration.workbench.dispatchHost"},
		},
		Fields: []*languageParser.BuiltinField{{
			Name:     "action",
			Type:     "string",
			Required: true,
			Attributes: []*languageParser.Attribute{
				{Name: "required"},
				{Name: "description", Value: "exec / fs_read / fs_write"},
				{Name: "enum", Value: "exec"},
			},
		}},
	}
	if _, err := builtinDeclToFunction(decl, "dsl/workbench/builtins.memql"); err != nil {
		t.Fatalf("the schema annotations must be accepted on a builtin field: %v", err)
	}
}

// TestOneFieldAllowListForEveryBody pins the single-surface property itself.
// Three separate hand-written lists is the state D16 replaced; since #5359 the
// one surface is the annotation registry, and the cheapest way back to three
// lists is a literal added beside it.
//
// The shared core is what a field body means as a SCHEMA -- a description, a
// default the model reads, an enum, and whether it is required. @autoInjected
// is deliberately tool-only, so this asserts a shared core rather than three
// identical sets.
func TestOneFieldAllowListForEveryBody(t *testing.T) {
	for _, r := range []annotations.Receiver{annotations.ToolField, annotations.PromptField, annotations.BuiltinField} {
		// The shared core. @default is NOT in it: a tool and a prompt field
		// carry one, a builtin field does not, and neither does an args field
		// (it is retired there, never applied). D16 is about a field body
		// being a SCHEMA, not about the three sets being identical.
		for _, want := range []string{"required", "description", "enum"} {
			if _, ok := annotations.Lookup(r, want); !ok {
				t.Errorf("%s should accept @%s -- its body IS the schema handed to the model", r, want)
			}
		}
		// The retired names must NOT be reachable from a field body, or the
		// registry's retirement refusal is unreachable there.
		for _, unwanted := range []string{"unique", "immutable", "enabled", "latestMode"} {
			if _, ok := annotations.Lookup(r, unwanted); ok {
				t.Errorf("%s must not accept retired @%s", r, unwanted)
			}
		}
	}
}
