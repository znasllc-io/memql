package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestBuiltinDeclToFunction_RejectsUnknownAnnotation locks in #990: the
// builtin converter no longer silently swallows unknown annotations.
func TestBuiltinDeclToFunction_RejectsUnknownAnnotation(t *testing.T) {
	decl := &languageParser.BuiltinDecl{
		Name: "cognitionScore",
		Attributes: []*languageParser.Attribute{
			{Name: "executor", Value: "integration.cognition.scoreUtterance"},
			{Name: "bogusBuiltinAnno"},
		},
	}

	_, err := builtinDeclToFunction(decl, "dsl/cognition/builtins.memql")
	if err == nil {
		t.Fatalf("expected unknown annotation @bogusBuiltinAnno to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown annotation @bogusBuiltinAnno") {
		t.Fatalf("expected unknown-annotation error naming @bogusBuiltinAnno, got: %v", err)
	}
}

// TestBuiltinDeclToFunction_AcceptsSupported guards against over-rejection.
func TestBuiltinDeclToFunction_AcceptsSupported(t *testing.T) {
	decl := &languageParser.BuiltinDecl{
		Name: "cognitionScore",
		Attributes: []*languageParser.Attribute{
			{Name: "description", Value: "score an utterance"},
			{Name: "executor", Value: "integration.cognition.scoreUtterance"},
		},
	}

	if _, err := builtinDeclToFunction(decl, "dsl/cognition/builtins.memql"); err != nil {
		t.Fatalf("builtinDeclToFunction: %v", err)
	}
}

// TestPromptDeclToPromptDecl_RejectsUnknownAnnotation locks in #990 for prompts.
func TestPromptDeclToPromptDecl_RejectsUnknownAnnotation(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Attributes: []*languageParser.Attribute{
			{Name: "description", Value: "agent reply"},
			{Name: "templateFile", Value: "agentReply.tmpl"},
			{Name: "bogusPromptAnno"},
		},
	}

	_, err := promptDeclToPromptDecl(decl, "dsl/cognition/prompts.memql")
	if err == nil {
		t.Fatalf("expected unknown annotation @bogusPromptAnno to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown annotation @bogusPromptAnno") {
		t.Fatalf("expected unknown-annotation error naming @bogusPromptAnno, got: %v", err)
	}
}

// TestPromptDeclToPromptDecl_AcceptsSupported guards against over-rejection.
//
// @level is in the fixture because four places have to learn the annotation in
// lockstep or the parser refuses it after one of them changes:
// annotations.ByReceiver["Prompt"], annotations.Docs, the PromptDecl AST doc
// comment, and this fixture. It also asserts the CARRY -- the parser fills
// PromptDecl.Level, and a converter that read the annotation without storing
// it would pass every check here while leaving the field a routing rule
// branches on permanently empty.
func TestPromptDeclToPromptDecl_AcceptsSupported(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name:  "agentReply",
		Level: "strong",
		Attributes: []*languageParser.Attribute{
			{Name: "description", Value: "agent reply"},
			{Name: "level", Value: "strong"},
			{Name: "defaultProvider", Value: "chat54Mini"},
			{Name: "templateFile", Value: "agentReply.tmpl"},
		},
	}

	out, err := promptDeclToPromptDecl(decl, "dsl/cognition/prompts.memql")
	if err != nil {
		t.Fatalf("promptDeclToPromptDecl: %v", err)
	}
	if out.level != "strong" {
		t.Errorf("level = %q, want strong -- the level must survive the conversion, or the "+
			"registry hands the router an empty one and every rule matching on level misses", out.level)
	}
}
