package dslgate

// construct_placement.go -- the construct-misplaced gate (memql#5437).
//
// THE RULE. A construct declared in a file its loader never reads is refused at
// load. Every loader reads every .memql file of a domain except the automation
// loader, which reads a domain's automations from its automations.memql and
// nowhere else (dsl/construct_placement.go states the rule once, and the loader
// and this gate both read it there).
//
// WHY A LOAD REFUSAL. An automation in any other file is not a skip and not a
// warning: the loader never opens the file, so nothing registers the automation
// and nothing reports that it did not. The passes that do read every file --
// the statement-body and sub-automation gates -- check its statements and pass
// it, so the tree lints clean and boots clean with a workflow that never runs.
// Ten automations of the MemQL Cloud control plane lived in the fleet bundle's
// billing.memql and trial.memql that way -- dunning, the trial clock, the
// grace-period teardown -- while the operator docs described them running.
//
// PER FILE. Whether a declaration is where its loader reads it depends on the
// file's own path and text alone, so the gate rides ScanSource, and a
// single-file caller gets the verdict boot does. It reads the file with
// parser.TopLevelStatements rather than a parse, so a file the parser would
// refuse for another reason is still read for what it declares.

import (
	"fmt"
	"path"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// GateConstructMisplaced -- a construct declared in a file its loader never
// reads.
const GateConstructMisplaced Gate = "construct-misplaced"

// CodeConstructMisplaced ends every construct-misplaced refusal, in brackets,
// as the language's rule ids do.
const CodeConstructMisplaced = "construct_misplaced"

// ScanConstructPlacement is this gate alone over one file, for a caller that
// holds a file under a different path than the one boot reads it by: memqllint
// pointed at one domain directory, or at one file in it, whose own root hides
// the domain directory the verdict turns on. p is the file's path within a DSL
// tree, its domain directory included ("acmez/billing.memql").
func ScanConstructPlacement(p, src string) []Violation {
	return scanConstructPlacement(p, src)
}

// scanConstructPlacement runs the gate over one file.
func scanConstructPlacement(p, src string) []Violation {
	var out []Violation
	for _, s := range languageParser.TopLevelStatements(src) {
		if _, restricted := memqldsl.ConstructFile(s.Keyword); !restricted {
			continue
		}
		if memqldsl.LoaderReadsConstruct(s.Keyword, p) {
			continue
		}
		out = append(out, Violation{
			Gate:      GateConstructMisplaced,
			File:      p,
			Line:      s.Line,
			Kind:      s.Keyword,
			Construct: s.Name,
			Detail:    misplacedConstructMessage(s.Keyword, s.Name, p),
		})
	}
	return out
}

// misplacedConstructMessage names the construct, the file it is declared in,
// why its loader never reads that file, and the file it belongs in.
func misplacedConstructMessage(keyword, name, p string) string {
	file, _ := memqldsl.ConstructFile(keyword)
	construct := keyword
	if name != "" {
		construct += " " + name
	}
	home := memqldsl.ConstructHome(keyword, p)
	why := fmt.Sprintf("a domain's %ss load from its %s and from no other file", keyword, file)
	fix := "Move it to " + home
	switch {
	case home == "":
		why = fmt.Sprintf("it is at the root of the tree, in no domain directory, and a domain's %ss load from <domain>/%s", keyword, file)
		fix = fmt.Sprintf("Move it to the %s of its domain", file)
	case path.Base(p) == file:
		why = "the loader skips a directory whose name begins with _ or ."
	}
	return fmt.Sprintf("%s is declared in %s, which the %s loader never reads: %s, so it would load as nothing, with no skip and no warning. %s [%s]",
		construct, p, keyword, why, fix, CodeConstructMisplaced)
}
