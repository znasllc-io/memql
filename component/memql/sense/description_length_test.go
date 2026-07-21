package sense

// #2703: the ruling-5 editorial length hint -- HINT severity, ruling-3
// resolved description, never fails a gate. Tested at the rule level (the
// sibling authoring-rule idiom): Diagnose runs the rule registry-gated.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// parseWithDocs mirrors the real diagnose pipeline: struct-form lowering,
// then a doc-comment-wired parse.
func parseWithDocs(t *testing.T, src string) *parser.File {
	t.Helper()
	normalised, err := parser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	lex := parser.NewLexer(normalised)
	tokens, err := lex.Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := parser.NewParser(tokens)
	p.SetDocComments(lex.DocComments())
	node, err := p.Parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	file, ok := node.(*parser.File)
	if !ok || file == nil {
		t.Fatalf("want *parser.File, got %T", node)
	}
	return file
}

func TestDescriptionLengthRule(t *testing.T) {
	long := strings.Repeat("alpha beta gamma delta ", 12) // ~276 chars
	logicBody := "logic lengthProbe {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}\n"

	t.Run("over-target-hints", func(t *testing.T) {
		src := "/// " + long + "\n" + logicBody
		file := parseWithDocs(t, src)
		diags := descriptionLengthRule(file, src)
		if len(diags) != 1 || diags[0].Code != "description-length" {
			t.Fatalf("over-target description must produce the length hint, got %+v", diags)
		}
		if diags[0].Severity != SeverityHint {
			t.Errorf("severity = %v, want SeverityHint (never an error)", diags[0].Severity)
		}
	})
	t.Run("under-target-silent", func(t *testing.T) {
		src := "/// Short and sweet.\n" + logicBody
		file := parseWithDocs(t, src)
		if diags := descriptionLengthRule(file, src); len(diags) != 0 {
			t.Fatalf("under-target description must not hint: %+v", diags)
		}
	})
	t.Run("fallback-annotation-counts-too", func(t *testing.T) {
		src := "@description(\"" + long + "\")\n" + logicBody
		file := parseWithDocs(t, src)
		if diags := descriptionLengthRule(file, src); len(diags) != 1 {
			t.Fatalf("the @description fallback resolves too and must hint when over target, got %+v", diags)
		}
	})
}
