package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestBuiltinDeclToFunction_RejectsUnknownAnnotation locks in #990: an unknown
// annotation on a builtin is refused -- since memql#5359 at parse time, by the
// annotation registry, so the converter only ever sees a legal set.
func TestBuiltinDeclToFunction_RejectsUnknownAnnotation(t *testing.T) {
	src := "@executor(\"integration.cognition.scoreUtterance\")\n@bogusBuiltinAnno\nbuiltin cognitionScore {\n}\n"

	_, err := languageParser.ParseBuiltinDecl(src)
	if err == nil {
		t.Fatalf("expected unknown annotation @bogusBuiltinAnno to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown annotation @bogusBuiltinAnno on a builtin") || !strings.HasSuffix(err.Error(), "[annotation_unknown]") {
		t.Fatalf("expected the registry's unknown-annotation refusal naming @bogusBuiltinAnno, got: %v", err)
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

// TestPromptDeclToPromptDecl_RejectsUnknownAnnotation locks in #990 for
// prompts, on the path it now runs on (the parser's registry check).
func TestPromptDeclToPromptDecl_RejectsUnknownAnnotation(t *testing.T) {
	src := "@description(\"agent reply\")\n@templateFile(\"agentReply.tmpl\")\n@bogusPromptAnno\nprompt agentReply {\n  x string\n}\n"

	_, err := languageParser.ParsePromptDecl(src)
	if err == nil {
		t.Fatalf("expected unknown annotation @bogusPromptAnno to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown annotation @bogusPromptAnno on a prompt") || !strings.HasSuffix(err.Error(), "[annotation_unknown]") {
		t.Fatalf("expected the registry's unknown-annotation refusal naming @bogusPromptAnno, got: %v", err)
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
