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
	t.Run("ruling-3-doc-comment-wins", func(t *testing.T) {
		// Both channels present: the /// doc comment is the resolved
		// description. Short /// over a long stale @description must
		// stay silent; the converse must hint. Pins the argument order
		// at every EffectiveDescription call site.
		silent := "/// Short and sweet.\n@description(\"" + long + "\")\n" + logicBody
		file := parseWithDocs(t, silent)
		if diags := descriptionLengthRule(file, silent); len(diags) != 0 {
			t.Fatalf("short /// wins over long @description; must not hint: %+v", diags)
		}
		hints := "/// " + long + "\n@description(\"Short and sweet.\")\n" + logicBody
		file = parseWithDocs(t, hints)
		if diags := descriptionLengthRule(file, hints); len(diags) != 1 {
			t.Fatalf("long /// wins over short @description; must hint, got %+v", diags)
		}
	})
	t.Run("runes-not-bytes", func(t *testing.T) {
		// 150 characters, 300 UTF-8 bytes: under the 200-character
		// target, so no hint -- the target is defined in characters.
		accented := strings.Repeat("é", 150)
		src := "/// " + accented + "\n" + logicBody
		file := parseWithDocs(t, src)
		if diags := descriptionLengthRule(file, src); len(diags) != 0 {
			t.Fatalf("150 runes (300 bytes) is under the character target; must not hint: %+v", diags)
		}
		// 250 characters of the same rune: over target, and the message
		// must report the rune count, not the byte count.
		over := strings.Repeat("é", 250)
		src = "/// " + over + "\n" + logicBody
		file = parseWithDocs(t, src)
		diags := descriptionLengthRule(file, src)
		if len(diags) != 1 {
			t.Fatalf("250 runes must hint, got %+v", diags)
		}
		if !strings.Contains(diags[0].Message, "250 characters") {
			t.Errorf("message must report the rune count (250), got %q", diags[0].Message)
		}
	})
	t.Run("anchors-on-declaration-not-call-site", func(t *testing.T) {
		src := "logic caller {\n" +
			"  args {\n    a string @required\n  }\n" +
			"  body {\n    return lengthProbe(a: args.a)\n  }\n" +
			"}\n\n" +
			"/// " + long + "\n" +
			"logic lengthProbe {\n" +
			"  args {\n    a string @required\n  }\n" +
			"  body {\n    return coalesce(args.a, \"\")\n  }\n" +
			"}\n"
		file := parseWithDocs(t, src)
		diags := descriptionLengthRule(file, src)
		if len(diags) != 1 {
			t.Fatalf("want exactly one hint, got %+v", diags)
		}
		declLine := 1 + strings.Count(src[:strings.Index(src, "logic lengthProbe")], "\n")
		if diags[0].Range.Start.Line != declLine {
			t.Errorf("hint anchors at line %d; the declaration is at line %d (must not anchor on the call site)", diags[0].Range.Start.Line, declLine)
		}
	})
	t.Run("all-describable-kinds-covered", func(t *testing.T) {
		cases := map[string]string{
			"prompt":     "/// " + long + "\nprompt lengthProbe {\n  subject string\n}\n",
			"provider":   "/// " + long + "\nprovider lengthProbe {\n  params {\n    voice \"alloy\"\n  }\n}\n",
			"policy":     "/// " + long + "\n@primary(\"other\")\npolicy lengthProbe {\n}\n",
			"capability": "/// " + long + "\ncapability integration.probe.run {\n  args {\n    workdir string\n  }\n}\n",
			"action":     "use capabilities.probe.{ run }\n/// " + long + "\naction lengthProbe {\n  args {\n    workdir string\n  }\n  capability run(workdir: args.workdir)\n}\n",
		}
		for kind, src := range cases {
			file := parseWithDocs(t, src)
			diags := descriptionLengthRule(file, src)
			if len(diags) != 1 || diags[0].Code != "description-length" {
				t.Errorf("%s: over-target description must hint, got %+v", kind, diags)
			}
		}
	})
	t.Run("args-field-doc-comment-covered", func(t *testing.T) {
		src := "/// Short and sweet.\n" +
			"logic lengthProbe {\n" +
			"  args {\n" +
			"    /// " + long + "\n" +
			"    a string @required\n" +
			"  }\n" +
			"  body {\n    return coalesce(args.a, \"\")\n  }\n" +
			"}\n"
		file := parseWithDocs(t, src)
		diags := descriptionLengthRule(file, src)
		if len(diags) != 1 {
			t.Fatalf("over-target args-field /// must hint, got %+v", diags)
		}
		if !strings.Contains(diags[0].Message, "args field \"a\"") {
			t.Errorf("args-field hint must name the field, got %q", diags[0].Message)
		}
	})
}

// TestDescriptionLengthThroughDiagnose pins the wiring: the hint must be
// emitted by the real Service.Diagnose path (registry-gated semantic phase
// plus the doc-comment side channel on the diagnose parse), not only by the
// rule function in isolation.
func TestDescriptionLengthThroughDiagnose(t *testing.T) {
	long := strings.Repeat("alpha beta gamma delta ", 12)
	src := "/// " + long + "\nlogic lengthProbe {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}\n"
	s := New(&stubRegistry{})
	found := false
	for _, d := range s.Diagnose(src, "probe.memql") {
		if d.Code == "description-length" {
			found = true
			if d.Severity != SeverityHint {
				t.Errorf("severity through Diagnose = %v, want SeverityHint", d.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("Diagnose must emit the description-length hint end-to-end (SetDocComments wiring + rule registration)")
	}
}
