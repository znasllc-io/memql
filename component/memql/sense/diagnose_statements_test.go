package sense

// diagnose_statements_test.go -- Sense's diagnostics for a body written in
// statements (edition 2026, epic memql#5370): a refusal of the statement parser
// carries its code and lands on the text it refuses, and the scope rules the
// loader enforces (compiler.CheckBody) are reported where the author breaks
// them -- over the corpus's statement cases, the same verdict the engine
// reaches.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// TestDiagnose_StatementRefusalLandsOnTheAuthorsText: the diagnostic for a
// refused statement body is one error, whose code is the refusal's and whose
// range is the text it refuses. The first three are the inputs the rewriter
// refused before a logic and an automation were written in statements.
func TestDiagnose_StatementRefusalLandsOnTheAuthorsText(t *testing.T) {
	cases := []struct {
		name, src, needle string
		nth               int
		code, want        string
	}{
		{"a body block in an automation", probeTrigger + "automation a {\n  body {\n    x := 1\n  }\n}\n",
			"body", 1, "body_block_retired", "`body { }` is retired in edition 2026"},
		{"an automation with no statement", probeTrigger + "automation noSteps {\n  args {\n    x string\n  }\n}\n",
			"}", 2, "body_empty", "automation noSteps has no statement"},
		{"a logic with no statement", "logic noBody {\n  args {\n    a string\n  }\n}\n",
			"logic", 1, "body_logic_return", "a logic ends with `return <value>`"},
		{"a name read before it is bound", "logic early {\n  y := x + 1\n  x := 1\n  return y\n}\n",
			"x", 1, "body_forward_reference", "reads `x`, which is bound on line 3"},
		{"an argument read bare", probeTrigger + "automation bare {\n  args {\n    limit any\n  }\n  mutation save(n: limit)\n}\n",
			"limit", 2, "body_unknown_name", "write args.limit"},
		{"a publish in a logic", "logic loud {\n  publish \"x.y\" { a: 1 }\n  return 1\n}\n",
			"publish", 1, "body_publish_in_logic", "a logic may not publish"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := errorDiags(New(nil).Diagnose(c.src, "probe/things.memql"))
			if len(errs) != 1 {
				t.Fatalf("want one error diagnostic, got %d: %+v", len(errs), errs)
			}
			d := errs[0]
			if d.Code != c.code || !strings.Contains(d.Message, c.want) {
				t.Fatalf("got %s %q, want %s %q", d.Code, d.Message, c.code, c.want)
			}
			start := nthAt(t, c.src, c.needle, c.nth)
			end := Position{Line: start.Line, Column: start.Column + utf8.RuneCountInString(c.needle)}
			if d.Range.Start != start || d.Range.End != end {
				t.Fatalf("range %d:%d-%d:%d, want %d:%d-%d:%d (the %q the author wrote): %s",
					d.Range.Start.Line, d.Range.Start.Column, d.Range.End.Line, d.Range.End.Column,
					start.Line, start.Column, end.Line, end.Column, c.needle, d.Message)
			}
		})
	}
}

// TestDiagnose_StatementBodyAboveALoweredConstruct: a scope problem below a
// construct the rewriter lowers to fewer lines is still placed on the
// author's text.
func TestDiagnose_StatementBodyAboveALoweredConstruct(t *testing.T) {
	src := `use probe.concepts.{ thing }

query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
}

logic early {
  y := x + 1
  x := 1
  return y
}
`
	errs := errorDiags(New(nil).Diagnose(src, "probe/things.memql"))
	if len(errs) != 1 || errs[0].Code != "body_forward_reference" {
		t.Fatalf("want one body_forward_reference, got %+v", errs)
	}
	if want := nthAt(t, src, "x + 1", 1); errs[0].Range.Start != want {
		t.Fatalf("placed at %d:%d, want %d:%d: %s", errs[0].Range.Start.Line, errs[0].Range.Start.Column, want.Line, want.Column, errs[0].Message)
	}
}

// TestDiagnose_StatementCorpusAgreesWithTheLoader: over every statement case of
// the conformance corpus, Sense reaches the engine's verdict -- the statement
// parser's code on a refuse_parse case, CheckBody's on a refuse_load case whose
// code is one of its own, and no error on a case the engine loads. A refusal
// the load path makes for another reason (an argument declared and unused, an
// unknown construct) needs the loaded workspace and is not Sense's to show here.
func TestDiagnose_StatementCorpusAgreesWithTheLoader(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "test", "conformance", "2026", "statements"))
	if err != nil {
		t.Fatal(err)
	}
	parseCodes, loadCodes := map[string]bool{}, map[string]bool{}
	for _, c := range parser.BodyRefusalCodes() {
		parseCodes[c] = true
	}
	for _, c := range compiler.BodyProblemCodes() {
		loadCodes[c] = true
	}
	loads, refusals := 0, 0
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "expect.json" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var exp struct {
			Cases []struct {
				File, Verdict, Code string
			} `json:"cases"`
		}
		if err := json.Unmarshal(raw, &exp); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, c := range exp.Cases {
			file := filepath.Join(filepath.Dir(path), c.File)
			rel, _ := filepath.Rel(root, file)
			src, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			errs := errorDiags(New(nil).Diagnose(string(src), rel))
			switch {
			case c.Verdict == "load_ok" || c.Verdict == "evaluate":
				loads++
				for _, d := range errs {
					t.Errorf("%s loads, and Sense reports %s at %d:%d: %s", rel, d.Code, d.Range.Start.Line, d.Range.Start.Column, d.Message)
				}
			case (c.Verdict == "refuse_parse" && parseCodes[c.Code]) || (c.Verdict == "refuse_load" && loadCodes[c.Code]):
				refusals++
				found := false
				for _, d := range errs {
					found = found || d.Code == c.Code
				}
				if !found {
					t.Errorf("%s is refused %s, and Sense reports %+v", rel, c.Code, errs)
				}
			}
		}
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		t.Fatal(err)
	}
	// Both halves must have run, or the test proved nothing about them.
	if loads == 0 || refusals == 0 {
		t.Fatalf("checked %d loading cases and %d refusals under %s: the corpus moved", loads, refusals, root)
	}
}
