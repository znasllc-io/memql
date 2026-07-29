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
		"prose":            "only forward deploys are allowed",
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
