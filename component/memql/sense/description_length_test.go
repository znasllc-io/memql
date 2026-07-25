package sense

// #2703: the ruling-5 editorial length hint -- HINT severity, ruling-3
// resolved description, never fails a gate. Tested at the rule level (the
// sibling authoring-rule idiom): Diagnose runs the rule registry-gated.

import (
	"strconv"
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
	long := strings.Repeat("alpha beta gamma delta ", 24) // ~552 chars, over the 500 target
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
		// 400 characters, 800 UTF-8 bytes: under the 500-character
		// target, so no hint -- the target is defined in characters.
		accented := strings.Repeat("é", 400)
		src := "/// " + accented + "\n" + logicBody
		file := parseWithDocs(t, src)
		if diags := descriptionLengthRule(file, src); len(diags) != 0 {
			t.Fatalf("400 runes (800 bytes) is under the character target; must not hint: %+v", diags)
		}
		// 600 characters of the same rune: over target, and the message
		// must report the rune count, not the byte count.
		over := strings.Repeat("é", 600)
		src = "/// " + over + "\n" + logicBody
		file = parseWithDocs(t, src)
		diags := descriptionLengthRule(file, src)
		if len(diags) != 1 {
			t.Fatalf("600 runes must hint, got %+v", diags)
		}
		if !strings.Contains(diags[0].Message, "600 characters") {
			t.Errorf("message must report the rune count (600), got %q", diags[0].Message)
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
	long := strings.Repeat("alpha beta gamma delta ", 24)
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

// TestDescriptionLengthTargetIsConsistent pins the three places the editorial
// target is stated to ONE number (#2759). The rule enforces
// descriptionLengthTarget, but two author-facing doc strings quote it
// independently -- the @description annotation doc and the doc-comment
// next-rule -- and both are what an author actually reads in completion and
// hover. Before this pin they were plain prose that could silently disagree
// with the diagnostic, advising one length while the editor flagged another.
func TestDescriptionLengthTargetIsConsistent(t *testing.T) {
	want := "~" + strconv.Itoa(descriptionLengthTarget) + " characters"

	t.Run("@description annotation doc", func(t *testing.T) {
		doc, ok := AnnotationDocs["description"]
		if !ok {
			t.Fatal("no doc registered for @description")
		}
		if !strings.Contains(doc, want) {
			t.Errorf("annotation doc must quote %q (the live target); got: %s", want, doc)
		}
	})

	t.Run("doc-comment next-rule doc", func(t *testing.T) {
		var docs []string
		for _, rule := range dslSpec.NextRules {
			if strings.Contains(rule.Doc, "editorial length target") {
				docs = append(docs, rule.Doc)
			}
		}
		if len(docs) == 0 {
			t.Fatal("no next-rule quotes the editorial length target; the pin has lost its subject")
		}
		for _, doc := range docs {
			if !strings.Contains(doc, want) {
				t.Errorf("next-rule doc must quote %q (the live target); got: %s", want, doc)
			}
		}
	})
}
