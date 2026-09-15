package parser_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The invariant: COMMENT CONTENT MUST NEVER CHANGE THE COMPILED AUTOMATION
// (memql#2906), for the statement body the parser reads as written (epic
// memql#5370).
//
// A sweep rather than fixtures, because fixtures mis-diagnosed #2906: its
// reported payloads did not reproduce, and the real failures were whole
// classes (any comment inside a block; a commented-out header opening a line)
// that only positions x forms x payloads surfaced. The comparison is of the
// COMPILED body, against a comment-free control, because a comment that
// changes what runs without erroring is the worse defect.

// compiledAutomation is an automation's trigger, args and compiled statement
// steps, comparable across sources that differ only in comments.
func compiledAutomation(t *testing.T, src string) string {
	t.Helper()
	f, err := parser.ParseFile(src)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	for _, d := range f.Definitions {
		fn, ok := d.(*ast.FunctionDef)
		if !ok {
			continue
		}
		auto, ok := fn.Body.(*ast.AutomationDef)
		if !ok || auto.Body == nil {
			t.Fatalf("not a statement body:\n%s", src)
		}
		var args []string
		if fn.ArgsSchema != nil {
			for _, a := range fn.ArgsSchema.Fields {
				args = append(args, a.Name+" "+a.Type)
			}
		}
		var attrs []string
		for _, a := range fn.Attributes {
			attrs = append(attrs, fmt.Sprintf("%s %v %v", a.Name, a.Value, a.Args))
		}
		steps, problems := compiler.CompileBody("automation", fn.Name, nil, auto.Body)
		if len(problems) > 0 {
			t.Fatalf("compile: %v\n%s", problems, src)
		}
		out, err := json.Marshal(map[string]any{"name": fn.Name, "attributes": attrs, "args": args, "steps": steps})
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	t.Fatalf("no automation in:\n%s", src)
	return ""
}

func TestStatementBody_CommentContentNeverChangesTheCompiledAutomation(t *testing.T) {
	const ctl = `@trigger(event="e", concept="v1:a:b")
automation a {
  args {
    x  string!
  }
  s := logic l(x: args.x)
  if args.x != "" {
    logic m(x: args.x)
  }
}`
	want := compiledAutomation(t, ctl)

	// %s is where the comment is spliced in; every position is a line of its
	// own except the header's, which only an inline block comment can take.
	positions := map[string]string{
		"preamble":           "%s\n" + ctl,
		"after the trigger":  strings.Replace(ctl, "automation a {", "%s\nautomation a {", 1),
		"body top":           strings.Replace(ctl, "automation a {", "automation a {\n%s", 1),
		"inside args":        strings.Replace(ctl, "  args {", "  args {\n%s", 1),
		"after args":         strings.Replace(ctl, "  s := logic", "%s\n  s := logic", 1),
		"between statements": strings.Replace(ctl, "  if args.x", "%s\n  if args.x", 1),
		"inside a block":     strings.Replace(ctl, "    logic m(", "%s\n    logic m(", 1),
		"body end":           strings.Replace(ctl, "  }\n}", "  }\n%s\n}", 1),
		"in the header line": strings.Replace(ctl, "automation a {", "automation %s a {", 1),
	}
	payloads := map[string]string{
		"prose": "only forward deploys are allowed",
		// Earns the */ skip below: a payload that closes a block comment.
		"closer":           "a note ending with */ inside it",
		"args header":      "args {\n  ghost string\n}",
		"a statement":      "ghost := logic g(a: 1)",
		"a block":          "if ghost {\n  logic g()\n}",
		"unbalanced brace": "if ghost {",
	}
	forms := map[string]func(string) string{
		"line":            func(p string) string { return "// " + strings.ReplaceAll(p, "\n", "\n// ") },
		"block inline":    func(p string) string { return "/* " + strings.ReplaceAll(p, "\n", " ") + " */" },
		"block multiline": func(p string) string { return "/*\n" + p + "\n*/" },
	}

	for pname, pos := range positions {
		for payName, payload := range payloads {
			for formName, form := range forms {
				if strings.HasPrefix(formName, "block") && strings.Contains(payload, "*/") {
					continue // the payload would end the comment early: not a comment
				}
				if pname == "in the header line" && formName != "block inline" {
					continue // a line comment or a newline would end the header
				}
				t.Run(pname+"/"+payName+"/"+formName, func(t *testing.T) {
					src := fmt.Sprintf(pos, form(payload))
					if got := compiledAutomation(t, src); got != want {
						t.Errorf("a comment changed the compiled automation.\n  source:\n%s\n  got:  %s\n  want: %s", src, got, want)
					}
				})
			}
		}
	}
}
