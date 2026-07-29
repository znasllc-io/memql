package parser

import (
	"fmt"
	"strings"
	"testing"
)

// The invariant: COMMENT CONTENT MUST NEVER CHANGE THE COMPILED AUTOMATION
// (memql#2906).
//
// A sweep rather than fixtures, because the defect this pins was mis-diagnosed
// by fixtures. #2906 reported three payload-shaped cases -- a commented-out
// `args`, `step`, and `logic` -- and as written none of the three reproduced.
// The real failing surface was two classes neither of those rows describes:
//
//	A. ANY comment inside a `step { }` block refused the load, regardless of
//	   form or content. A plain prose line failed exactly as a commented-out
//	   construct did. This was the large majority of failures.
//	B. A commented-out `args {` or `step X {` header that STARTS A LINE failed
//	   brace matching, because the header was located on raw text while
//	   matchBraceInBody blanks -- so the two could never agree, and the error
//	   named a block that does not exist in the file.
//
// Sweeping positions x forms x payloads is what surfaced that. A fixture set
// aimed at the three reported payloads would have passed while leaving the
// dominant class broken.
//
// Comparing OUTPUT, not just error-vs-nil: a comment that changes the compiled
// automation without erroring is the worse defect, and only an equality check
// against a comment-free control catches it.
//
// The comparison strips comments and collapses whitespace on BOTH sides rather
// than demanding byte equality. Authored comments are deliberately PRESERVED
// in the emitted source -- that is what comment_blank.go's detect-on-blanked /
// slice-from-original split exists to guarantee -- so a preamble comment
// legitimately appears in the output and a byte comparison would fail on
// correct behaviour. What must not differ is the compiled automation, and
// after removing comment text the two are the same program or they are not.
func compiledForm(s string) string {
	return strings.Join(strings.Fields(BlankComments(s)), " ")
}
func TestAutomationSource_CommentContentNeverChangesTheCompiledAutomation(t *testing.T) {
	// %s is where the comment is spliced in.
	positions := map[string]string{
		"preamble":       "%s\n@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\nautomation a {\n  args {\n    x string @required\n  }\n  step s {\n    logic l { x: args.x }\n  }\n}",
		"after trigger":  "@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\n%s\nautomation a {\n  args {\n    x string @required\n  }\n  step s {\n    logic l { x: args.x }\n  }\n}",
		"body top":       "@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\nautomation a {\n%s\n  args {\n    x string @required\n  }\n  step s {\n    logic l { x: args.x }\n  }\n}",
		"between blocks": "@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\nautomation a {\n  args {\n    x string @required\n  }\n%s\n  step s {\n    logic l { x: args.x }\n  }\n}",
		"in step":        "@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\nautomation a {\n  args {\n    x string @required\n  }\n  step s {\n%s\n    logic l { x: args.x }\n  }\n}",
		"in step after":  "@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\nautomation a {\n  args {\n    x string @required\n  }\n  step s {\n    logic l { x: args.x }\n%s\n  }\n}",
		"body end":       "@trigger(event=\"e\", concept=\"v1:a:b\", partition=\"*\")\nautomation a {\n  args {\n    x string @required\n  }\n  step s {\n    logic l { x: args.x }\n  }\n%s\n}",
	}

	// Payloads chosen to include the ORDINARY case (prose), which is what
	// class A actually broke on, alongside the construct-shaped ones #2906
	// reported.
	payloads := map[string]string{
		"prose": "only forward deploys are allowed",
		// Earns the */ skip guard below. Without a payload that actually
		// contains */ the guard never fires, and its comment reads as if
		// cases were being excluded when none were.
		"closer":           "a note ending with */ inside it",
		"args header":      "args {\n  ghost string\n}",
		"step header":      "step ghost {\n  logic g { a: 1 }\n}",
		"logic header":     "logic ghost { a: 1 }",
		"unbalanced brace": "step ghost {",
	}

	forms := map[string]func(string) string{
		"line":            func(p string) string { return "// " + strings.ReplaceAll(p, "\n", "\n// ") },
		"block inline":    func(p string) string { return "/* " + strings.ReplaceAll(p, "\n", " ") + " */" },
		"block multiline": func(p string) string { return "/*\n" + p + "\n*/" },
	}

	for posName, tmpl := range positions {
		// The control is the same template with the comment removed entirely.
		want, err := NormaliseAutomationSource(fmt.Sprintf(tmpl, ""))
		if err != nil {
			t.Fatalf("control for position %q does not compile, so this position proves nothing: %v", posName, err)
		}
		for formName, wrap := range forms {
			for payName, payload := range payloads {
				// A payload containing */ cannot be wrapped in a block
				// comment -- the */ closes it and the rest is real code.
				if strings.Contains(payload, "*/") && formName != "line" {
					continue
				}
				name := fmt.Sprintf("%s/%s/%s", posName, formName, payName)
				t.Run(name, func(t *testing.T) {
					got, err := NormaliseAutomationSource(fmt.Sprintf(tmpl, wrap(payload)))
					if err != nil {
						t.Fatalf("a comment refused the load. Comment content must never change the "+
							"compiled automation, and refusing it is the loudest way to change it "+
							"(memql#2906).\n  error: %v", err)
					}
					if compiledForm(got) != compiledForm(want) {
						t.Errorf("a comment CHANGED the compiled automation without erroring -- the "+
							"worse half of memql#2906, since nothing surfaces it.\n  with comment:\n%s\n  control:\n%s",
							got, want)
					}
				})
			}
		}
	}
}

// TestAutomationSource_CommentsInsideAHeaderLine covers the axis the sweep
// above cannot reach: it splices every comment onto its own LINE, so nothing
// ever places one INSIDE a `step X {` or `args {` header.
//
// That gap was not theoretical. The first cut of the memql#2906 fix located
// step headers on the blanked view but re-extracted the name from raw text, so
// any comment adjacent to a header produced `step block: missing name` for a
// step whose name is plainly present -- and the 105-case sweep stayed green
// throughout, because line-granularity positions cannot express it.
func TestAutomationSource_CommentsInsideAHeaderLine(t *testing.T) {
	const ctl = `@trigger(event="e", concept="v1:a:b", partition="*")
automation a {
  args {
    x string @required
  }
  step s {
    logic l { x: args.x }
  }
}`
	want, err := NormaliseAutomationSource(ctl)
	if err != nil {
		t.Fatalf("control: %v", err)
	}

	variants := map[string]string{
		"before step keyword": "  /*c*/step s {",
		"after step keyword":  "  step /*c*/ s {",
		"before brace":        "  step s /*c*/ {",
		"before args keyword": "  /*c*/args {",
		"after args keyword":  "  args /*c*/ {",
	}
	for name, header := range variants {
		t.Run(name, func(t *testing.T) {
			var src string
			if strings.Contains(header, "args") {
				src = strings.Replace(ctl, "  args {", header, 1)
			} else {
				src = strings.Replace(ctl, "  step s {", header, 1)
			}
			got, err := NormaliseAutomationSource(src)
			if err != nil {
				t.Fatalf("a comment inside the header line refused the load (memql#2906).\n  error: %v", err)
			}
			if compiledForm(got) != compiledForm(want) {
				t.Errorf("a comment inside the header line changed the compiled automation.\n  got:\n%s\n  control:\n%s", got, want)
			}
		})
	}
}

// TestAutomationSource_NonASCIIWhitespaceAtAStepBodyEdge covers the other axis
// the sweep misses: it never varies the whitespace at a step body's leading
// edge.
//
// trimCommentEdges replaced a strings.TrimSpace call, and TrimSpace trims
// unicode.IsSpace -- U+00A0, U+2003 and friends -- not just ASCII. A first cut
// that hand-rolled an ASCII-only class made these compile on main and error
// here, because parseAutomationSteps dispatches forEach/switch on the leading
// token of the trimmed result (memql#2906 review).
func TestAutomationSource_NonASCIIWhitespaceAtAStepBodyEdge(t *testing.T) {
	for _, sp := range []struct{ name, ch string }{
		{"U+00A0 no-break space", " "},
		{"U+2003 em space", " "},
		{"U+3000 ideographic space", "　"},
	} {
		t.Run(sp.name, func(t *testing.T) {
			ctl := `@trigger(event="e", concept="v1:a:b", partition="*")
automation a {
  step s {
    forEach item in args.x {
      logic l { x: item }
    }
  }
}`
			want, err := NormaliseAutomationSource(ctl)
			if err != nil {
				t.Fatalf("control: %v", err)
			}
			// Same source, but the step body starts with a non-ASCII space.
			src := strings.Replace(ctl, "  step s {\n", "  step s {\n"+sp.ch, 1)
			got, err := NormaliseAutomationSource(src)
			if err != nil {
				t.Fatalf("a non-ASCII leading space refused the load; strings.TrimSpace accepted it "+
					"before, so this is a regression not a hardening (memql#2906).\n  error: %v", err)
			}
			if compiledForm(got) != compiledForm(want) {
				t.Errorf("a non-ASCII leading space changed the compiled automation -- the forEach "+
					"dispatch silently fell through to the plain-call translator.\n  got:\n%s\n  control:\n%s", got, want)
			}
		})
	}
}
