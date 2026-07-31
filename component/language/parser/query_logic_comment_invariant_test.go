package parser

import (
	"fmt"
	"strings"
	"testing"
)

// query_logic_comment_invariant_test.go -- memql#2948.
//
// The invariant, identical to the automation lane's (memql#2906): COMMENT
// CONTENT MUST NEVER CHANGE THE COMPILED CONSTRUCT.
//
// #2906 fixed this class in the automation lane and, via helpers every lane
// shares (rewriteEachBlock's outer header, extractArgsBlock), fixed part of it
// for query and logic too. What it did not reach were the two lanes' OWN
// locators and, in the query lane, a second and unrelated comment-blindness:
//
//	A. parseStructQueryBody's field loop rejected any line it did not
//	   recognise. A comment is not a recognised field, so ONE explanatory
//	   sentence anywhere in a struct-form query failed the load with
//	   `unknown struct-query field on line "/*"`. This is the ordinary case --
//	   prose, not commented-out constructs -- and it was the dominant class in
//	   the automation lane too.
//	B. parseStructQueryBody's inner `args {` locator and emitLogic's
//	   `body {` locator both ran their regexp on RAW source while
//	   matchBraceInBody matches on the BLANKED view. The two can never agree
//	   about a header inside a comment: the header IS in the raw text, its `{`
//	   is NOT in the blanked view -- so the error named a block the file does
//	   not contain.
//
// Sweeping positions x forms x payloads rather than writing fixtures, for the
// reason #2906 recorded: a fixture set aimed at the reported payloads passes
// while the dominant class stays broken.
//
// Comparing OUTPUT, not merely error-vs-nil. A comment that changes the
// compiled construct WITHOUT erroring is the worse half of this defect,
// because nothing surfaces it. compiledForm (defined in
// automation_comment_invariant_test.go) strips comments and collapses
// whitespace on both sides: authored comments are deliberately preserved in
// the emitted source, so byte equality would fail on correct behaviour.

func TestQuerySource_CommentContentNeverChangesTheCompiledQuery(t *testing.T) {
	// %s is where the comment is spliced in.
	positions := map[string]string{
		"preamble":      "%s\n@description(\"d\")\nquery widget q {\n  args {\n    id string @required\n  }\n  filter id==args.id\n  shape widgetCard\n}",
		"body top":      "@description(\"d\")\nquery widget q {\n%s\n  args {\n    id string @required\n  }\n  filter id==args.id\n  shape widgetCard\n}",
		"inside args":   "@description(\"d\")\nquery widget q {\n  args {\n%s\n    id string @required\n  }\n  filter id==args.id\n  shape widgetCard\n}",
		"after args":    "@description(\"d\")\nquery widget q {\n  args {\n    id string @required\n  }\n%s\n  filter id==args.id\n  shape widgetCard\n}",
		"between field": "@description(\"d\")\nquery widget q {\n  args {\n    id string @required\n  }\n  filter id==args.id\n%s\n  shape widgetCard\n}",
		"body end":      "@description(\"d\")\nquery widget q {\n  args {\n    id string @required\n  }\n  filter id==args.id\n  shape widgetCard\n%s\n}",
	}

	payloads := map[string]string{
		// The ORDINARY case, and the one class A actually broke on.
		"prose":  "only active widgets are returned here",
		"closer": "a note ending with */ inside it",
		// Construct-shaped payloads: these are what class B needs -- a
		// commented-out header that STARTS A LINE.
		"args header":      "args {\n  ghost string\n}",
		"filter line":      "filter ghost==args.ghost",
		"shape line":       "shape ghostCard",
		"unbalanced brace": "args {",
	}

	forms := map[string]func(string) string{
		"line":            func(p string) string { return "// " + strings.ReplaceAll(p, "\n", "\n// ") },
		"block inline":    func(p string) string { return "/* " + strings.ReplaceAll(p, "\n", " ") + " */" },
		"block multiline": func(p string) string { return "/*\n" + p + "\n*/" },
	}

	for posName, tmpl := range positions {
		want, err := NormaliseQuerySource(fmt.Sprintf(tmpl, ""))
		if err != nil {
			t.Fatalf("control for position %q does not compile, so this position proves nothing: %v", posName, err)
		}
		for formName, wrap := range forms {
			for payName, payload := range payloads {
				// A payload containing */ cannot be wrapped in a block
				// comment -- the */ closes it and the rest becomes real code.
				if strings.Contains(payload, "*/") && formName != "line" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%s", posName, formName, payName), func(t *testing.T) {
					got, err := NormaliseQuerySource(fmt.Sprintf(tmpl, wrap(payload)))
					if err != nil {
						t.Fatalf("a comment refused the load. Comment content must never change the "+
							"compiled query, and refusing it is the loudest way to change it "+
							"(memql#2948).\n  error: %v", err)
					}
					if compiledForm(got) != compiledForm(want) {
						t.Errorf("a comment CHANGED the compiled query without erroring -- the worse "+
							"half of memql#2948, since nothing surfaces it.\n  with comment:\n%s\n  control:\n%s",
							got, want)
					}
				})
			}
		}
	}
}

func TestLogicSource_CommentContentNeverChangesTheCompiledLogic(t *testing.T) {
	positions := map[string]string{
		"preamble":    "%s\n@description(\"d\")\nlogic l {\n  args {\n    x string @required\n  }\n  body {\n    return f({ a: args.x })\n  }\n}",
		"body top":    "@description(\"d\")\nlogic l {\n%s\n  args {\n    x string @required\n  }\n  body {\n    return f({ a: args.x })\n  }\n}",
		"inside args": "@description(\"d\")\nlogic l {\n  args {\n%s\n    x string @required\n  }\n  body {\n    return f({ a: args.x })\n  }\n}",
		"after args":  "@description(\"d\")\nlogic l {\n  args {\n    x string @required\n  }\n%s\n  body {\n    return f({ a: args.x })\n  }\n}",
		"inside body": "@description(\"d\")\nlogic l {\n  args {\n    x string @required\n  }\n  body {\n%s\n    return f({ a: args.x })\n  }\n}",
		"body end":    "@description(\"d\")\nlogic l {\n  args {\n    x string @required\n  }\n  body {\n    return f({ a: args.x })\n  }\n%s\n}",
	}

	payloads := map[string]string{
		"prose":            "this step is idempotent and safe to retry",
		"closer":           "a note ending with */ inside it",
		"body header":      "body {\n  return ghost()\n}",
		"args header":      "args {\n  ghost string\n}",
		"unbalanced brace": "body {",
	}

	forms := map[string]func(string) string{
		"line":            func(p string) string { return "// " + strings.ReplaceAll(p, "\n", "\n// ") },
		"block inline":    func(p string) string { return "/* " + strings.ReplaceAll(p, "\n", " ") + " */" },
		"block multiline": func(p string) string { return "/*\n" + p + "\n*/" },
	}

	for posName, tmpl := range positions {
		want, err := NormaliseLogicSource(fmt.Sprintf(tmpl, ""))
		if err != nil {
			t.Fatalf("control for position %q does not compile, so this position proves nothing: %v", posName, err)
		}
		for formName, wrap := range forms {
			for payName, payload := range payloads {
				if strings.Contains(payload, "*/") && formName != "line" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%s", posName, formName, payName), func(t *testing.T) {
					got, err := NormaliseLogicSource(fmt.Sprintf(tmpl, wrap(payload)))
					if err != nil {
						t.Fatalf("a comment refused the load. Comment content must never change the "+
							"compiled logic, and refusing it is the loudest way to change it "+
							"(memql#2948).\n  error: %v", err)
					}
					if compiledForm(got) != compiledForm(want) {
						t.Errorf("a comment CHANGED the compiled logic without erroring -- the worse "+
							"half of memql#2948, since nothing surfaces it.\n  with comment:\n%s\n  control:\n%s",
							got, want)
					}
				})
			}
		}
	}
}

// TestQuerySource_TrailingCommentOnAFieldLine covers the axis the sweep cannot
// reach: it splices every comment onto its OWN line, so nothing ever puts one
// at the end of a real field. That is the shape most likely to corrupt a value
// rather than refuse the load, since the field parsers take the rest of the
// line verbatim.
func TestQuerySource_TrailingCommentOnAFieldLine(t *testing.T) {
	const control = "@description(\"d\")\nquery widget q {\n  filter id==args.id\n  shape widgetCard\n}"

	want, err := NormaliseQuerySource(control)
	if err != nil {
		t.Fatalf("control does not compile: %v", err)
	}

	for name, src := range map[string]string{
		"line comment after filter": "@description(\"d\")\nquery widget q {\n  filter id==args.id // only this one\n  shape widgetCard\n}",
		"block comment after shape": "@description(\"d\")\nquery widget q {\n  filter id==args.id\n  shape widgetCard /* the card projection */\n}",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NormaliseQuerySource(src)
			if err != nil {
				t.Fatalf("a trailing comment refused the load: %v", err)
			}
			if compiledForm(got) != compiledForm(want) {
				t.Errorf("a trailing comment leaked into a field value.\n  got:\n%s\n  control:\n%s", got, want)
			}
		})
	}
}

// TestQuerySource_CommentMarkersInsideStringsSurvive is the counterweight to
// reading field values from the blanked view: BlankComments must be string-
// aware, or this fix would silently truncate a legitimate value. If this fails,
// the blanked-view read is unsafe and the fix must slice from raw instead.
func TestQuerySource_CommentMarkersInsideStringsSurvive(t *testing.T) {
	const src = "@description(\"d\")\nquery widget q {\n  filter path==\"a//b\" && note==\"ends /* here\"\n  shape widgetCard\n}"

	got, err := NormaliseQuerySource(src)
	if err != nil {
		t.Fatalf("a string containing comment markers refused the load: %v", err)
	}
	for _, fragment := range []string{`a//b`, `ends /* here`} {
		if !strings.Contains(got, fragment) {
			t.Errorf("string literal %q was damaged by comment blanking.\n  got:\n%s", fragment, got)
		}
	}
}
