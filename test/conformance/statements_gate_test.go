package conformance

// statements_gate_test.go -- the statement cells cover the statement language
// (epic memql#5370, task memql#5374; D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// test/conformance/2026/statements/<construct>/<form>/ holds, for `logic` and
// for `automation` and every form the statement parser accepts
// (parser.BodyStatementForms), a case that runs -- load_ok, or for a logic an
// evaluate that calls it -- and a case that is refused. A form a construct
// refuses outright (a logic's publish) holds that refusal instead of a run.
// Beside the forms: scope/ (the scope rules, one of each way and one that
// runs), body/ (a body as a whole) and retired/ (one refusal per retired body
// form the construct could be written in, by its code). Nothing else.
//
// The lists are read from the parser, so a new form or a new retired code
// fails here until its cells exist.

import (
	"encoding/json"
	"os"
	"path"
	"sort"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

const statementCells = "2026/statements"

// statementRefusedForms are the forms a construct refuses outright, with the
// code its refusal carries: the cell holds that refusal and no run.
var statementRefusedForms = map[string]map[string]string{
	"logic": {
		"publish":   "body_publish_in_logic",  // D14: a logic may not publish
		"onSurface": "body_call_not_in_logic", // a surface names where an action runs, and a logic calls no action
	},
}

// statementRetiredOnlyIn are the retired forms only one construct was ever
// written in: a trigger, a step block and the terse header belong to an
// automation.
var statementRetiredOnlyIn = map[string]string{
	"trigger_partition_retired":        "automation",
	"trigger_schedule_synonym_retired": "automation",
	"body_step_retired":                "automation",
	"body_terse_retired":               "automation",
}

// statementRetiredAtTheFlip are the retired forms the transitional dispatch
// still sends to the legacy rewriter (TestTransitionalDispatchIsExact): they
// load until the tree is migrated, so their refusals cannot be cells yet.
// The flip (plan Task 13) deletes the dispatch and empties this map.
var statementRetiredAtTheFlip = map[string][]string{
	"automation": {"body_step_retired", "body_terse_retired"},
	"logic":      {"body_block_retired"},
}

func TestStatementCellsCoverEveryForm(t *testing.T) {
	var missing []string
	top, err := os.ReadDir(statementCells)
	if err != nil {
		t.Fatalf("read %s: %v", statementCells, err)
	}
	for _, e := range top {
		if e.IsDir() && e.Name() != "automation" && e.Name() != "logic" {
			missing = append(missing, statementCells+"/"+e.Name()+": a directory for neither logic nor automation")
		}
	}

	forms := langparser.BodyStatementForms()
	allowed := map[string]bool{"scope": true, "body": true, "retired": true}
	for _, f := range forms {
		allowed[f] = true
	}
	for _, construct := range []string{"automation", "logic"} {
		base := path.Join(statementCells, construct)
		entries, err := os.ReadDir(base)
		if err != nil {
			t.Fatalf("read %s: %v", base, err)
		}
		for _, e := range entries {
			if e.IsDir() && !allowed[e.Name()] {
				missing = append(missing, base+"/"+e.Name()+": not a statement form, scope, body or retired")
			}
		}

		for _, form := range forms {
			cs := statementCases(t, path.Join(base, form))
			if code, refused := statementRefusedForms[construct][form]; refused {
				if !cs.refusedWith(code) {
					missing = append(missing, path.Join(base, form)+": the refusal ["+code+"]")
				}
				continue
			}
			if !cs.runs(construct) {
				missing = append(missing, path.Join(base, form)+": a case that runs")
			}
			if !cs.refused() {
				missing = append(missing, path.Join(base, form)+": a refusal")
			}
		}

		scope := statementCases(t, path.Join(base, "scope"))
		if !scope.runs(construct) || !scope.refused() {
			missing = append(missing, path.Join(base, "scope")+": a case that runs and a refusal")
		}
		if !statementCases(t, path.Join(base, "body")).refused() {
			missing = append(missing, path.Join(base, "body")+": a refusal")
		}

		retired := statementCases(t, path.Join(base, "retired"))
		deferred := map[string]bool{}
		for _, code := range statementRetiredAtTheFlip[construct] {
			deferred[code] = true
		}
		for _, code := range statementRetiredCodes() {
			if only, ok := statementRetiredOnlyIn[code]; (ok && only != construct) || deferred[code] {
				continue
			}
			if !retired.refusedWith(code) {
				missing = append(missing, path.Join(base, "retired")+": the refusal ["+code+"]")
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("the statement cells are missing %d case(s):\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// statementRetiredCodes are the parser's retired-form codes.
func statementRetiredCodes() []string {
	var out []string
	for _, code := range langparser.BodyRefusalCodes() {
		if strings.HasSuffix(code, "_retired") {
			out = append(out, code)
		}
	}
	return out
}

type statementDirCases []corpusCase

// statementCases reads a cell directory's cases; none when it has no
// expect.json (the gate then names what it lacks).
func statementCases(t *testing.T, dir string) statementDirCases {
	t.Helper()
	data, err := os.ReadFile(path.Join(dir, "expect.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s/expect.json: %v", dir, err)
	}
	var ef corpusExpectFile
	if err := json.Unmarshal(data, &ef); err != nil {
		t.Fatalf("%s/expect.json: %v", dir, err)
	}
	return ef.Cases
}

// runs reports a case that runs: one that loads, or a logic an evaluate calls.
func (cs statementDirCases) runs(construct string) bool {
	for _, c := range cs {
		if c.Verdict == verdictLoadOK || (construct == "logic" && c.Verdict == verdictEvaluate && c.Call != "") {
			return true
		}
	}
	return false
}

func (cs statementDirCases) refused() bool {
	for _, c := range cs {
		if c.Verdict == verdictRefuseParse || c.Verdict == verdictRefuseLoad {
			return true
		}
	}
	return false
}

func (cs statementDirCases) refusedWith(code string) bool {
	for _, c := range cs {
		if (c.Verdict == verdictRefuseParse || c.Verdict == verdictRefuseLoad) && c.Code == code {
			return true
		}
	}
	return false
}
