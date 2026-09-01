package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ===========================================================================
// EVERY PAYLOAD NARROWING GETS AN ANSWER (memql#4779)
// ===========================================================================
//
// A `case float64:` or `case int64:` arm of an `any` type switch that returns
// a bare `int(x)` is the defect this gate exists to stop arriving again. Go
// leaves the out-of-range conversion IMPLEMENTATION-DEFINED, and on amd64 the
// hardware answers with the integer indefinite value: measured in memql#4778,
// int(1e30) is -9223372036854775808. A count becomes hugely negative, which
// inverts every `> 0` guard and every ordering the field exists to express.
//
// WHY A GATE RATHER THAN ANOTHER SWEEP. CodeQL's go/incorrect-integer-conversion
// is TAINT-DRIVEN: it flags a narrowing only where it can trace the value back
// to a strconv.Parse*, and every site in this tree is the same shape -- a small
// `func …(v any) int` decoding a JSON-ish payload field -- with no parser
// upstream to trace. So a green scan says nothing about this class. Four rounds
// of fixing it each fixed exactly the site the scanner named, and
// `intFromAnyLoose` -- written 2026-05-21 -- survived three of them, because no
// round asked what else looked like it. docs/internal/ops/codeql-alert-triage.md
// states that as "what recurs is not the bug, it is the SWEEP". This is the
// sweep becoming a gate, which is the only version of it that does not have to
// run a fifth time.
//
// WHY THE ROOT PACKAGE. Three reasons, the same ones staged_read_site_
// classification_test.go states: root `go test ./...` covers the root module
// only (memql#4032), so a gate in component/memql is invisible to the command
// contributors run; component/memql's suite is db-gated and SKIPS without a
// database, and a gate whose default outcome is a quiet skip is a vacuous pass
// wearing a different hat; and the root package runs uncached under both
// RUN_GO and RUN_GATES, which is exactly the change class that adds an arm.
//
// # Honest limits
//
// This is a SYNTACTIC gate over one shape, and it cannot see:
//
//   - a narrowing whose value reached a local variable before the conversion,
//     outside the case clause it came from;
//   - a conversion in a `case int:` arm, or a bare `v.(float64)` assertion --
//     neither is the shape, and widening the detector to them reported noise
//     at a rate that would have made an allowlist the real interface;
//   - `uint32(x.GetNumberValue())` and friends, which narrow a float64 with no
//     case clause anywhere near them. component/identity/webauthn/store.go's
//     signCount is the sharpest of those and is recorded here rather than
//     gated, because a detector wide enough to catch it also catches every
//     protobuf field width in the tree.
//
// What it does cover is the shape that recurred four times, which is the one
// worth spending a gate on.

// narrowingArmTypes are the case types whose value cannot be assumed to fit an
// int. `json.Number` is included because `.Int64()` hands back an int64 and
// three arms dropped the error and converted anyway -- the same defect one
// level down, and invisible to the grep in the triage doc.
var narrowingArmTypes = map[string]bool{
	"float64":     true,
	"int64":       true,
	"json.Number": true,
}

// narrowingConversions are the destination types that can lose information
// from a float64 / int64 source.
//
// `int64` is in the list, which looks wrong for a 64-bit build and is not:
// float64 -> int64 is the implementation-defined conversion itself, so
// `float64(int64(n)) == n` -- the integrality test seven files wrote -- has an
// UNDEFINED result for an out-of-range n. It answers "not a whole number" for
// 1e30 on amd64, which is at least the safe direction, but it is undefined
// rather than merely surprising and `n == math.Trunc(n)` is the exact total
// test that needs no conversion at all.
var narrowingConversions = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
}

// narrowingVerdictMarker is how a site declares that its answer was decided
// rather than defaulted. The vocabulary is closed and the casing is exact, so
// a marker that stopped being recognised fails loudly instead of silently
// pre-authorising the file it sits in.
//
//	// narrowing: SATURATE -- <why the order matters here>
//	// narrowing: ZERO     -- <why callers read 0 as unset>
//	// narrowing: DEFAULT  -- <the default, and why saturating is worse>
//	// narrowing: GUARDED  -- <the inline bound, and why it is not core/num's>
const narrowingVerdictMarker = "narrowing:"

var narrowingVerdicts = map[string]bool{
	"SATURATE": true, "ZERO": true, "DEFAULT": true, "GUARDED": true,
}

// narrowingScanSkipPrefixes are paths the gate does not read.
//
// Generated protobuf bindings decode wire fields at widths the .proto file
// fixed, which is a format rule rather than a payload narrowing, and nobody
// edits them by hand anyway.
var narrowingScanSkipPrefixes = []string{
	"component/grpc/gen/",
	"component/node/gen/",
	"component/bus/gen/",
	"sdk/go/client/generated_",
}

type narrowingSite struct {
	file    string
	line    int
	armType string
	// convert is the destination type of the offending conversion.
	convert string
	// verdict is the declared answer, or "" when the site declared none.
	verdict string
}

func (s narrowingSite) String() string {
	return s.file + ":" + strconv.Itoa(s.line) + "  case " + s.armType + ": -> " + s.convert + "(...)"
}

// findNarrowingSites reports every unguarded narrowing arm in one file.
//
// It takes SOURCE rather than a path, precisely so the self-tests below can
// drive it over a mutated copy of a real file without writing that copy
// anywhere the sweep would then find it.
func findNarrowingSites(path string, src []byte) ([]narrowingSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	// Verdict markers, by line, so a marker anywhere in the enclosing
	// function's doc comment covers the arms inside it. Comments are collected
	// once and matched by proximity rather than by exact line: a decoder
	// declares its answer in its doc comment, above the switch.
	verdictAt := map[int]string{}
	for _, group := range file.Comments {
		text := group.Text()
		idx := strings.Index(text, narrowingVerdictMarker)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(text[idx+len(narrowingVerdictMarker):])
		word := rest
		if cut := strings.IndexAny(rest, " \t\n-"); cut >= 0 {
			word = rest[:cut]
		}
		verdictAt[fset.Position(group.End()).Line] = word
	}

	var out []narrowingSite
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		bound := boundVarName(sw)
		if bound == "" {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			armType := narrowingArmOf(clause)
			if armType == "" {
				continue
			}
			// Names that carry the arm's value: the bound variable, plus
			// anything assigned from an expression mentioning it (the
			// `n, _ := v.Int64()` shape).
			carriers := map[string]bool{bound: true}
			collectCarriers(clause, carriers)
			for _, stmt := range clause.Body {
				ast.Inspect(stmt, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						return true
					}
					ident, ok := call.Fun.(*ast.Ident)
					if !ok || !narrowingConversions[ident.Name] {
						return true
					}
					arg, ok := call.Args[0].(*ast.Ident)
					if !ok || !carriers[arg.Name] {
						return true
					}
					line := fset.Position(call.Pos()).Line
					out = append(out, narrowingSite{
						file:    path,
						line:    line,
						armType: armType,
						convert: ident.Name,
						verdict: nearestVerdict(verdictAt, line),
					})
					return true
				})
			}
		}
		return true
	})
	return out, nil
}

// boundVarName returns `v` from `switch v := x.(type)`, or "" for the
// unassigned `switch x.(type)` form, which carries no value to narrow.
func boundVarName(sw *ast.TypeSwitchStmt) string {
	assign, ok := sw.Assign.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 {
		return ""
	}
	ident, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}

// narrowingArmOf returns the arm's type name when the clause names exactly one
// of the types that cannot be assumed to fit an int. A multi-type clause
// (`case int64, float64:`) counts as its first listed type.
func narrowingArmOf(clause *ast.CaseClause) string {
	for _, expr := range clause.List {
		name := typeName(expr)
		if narrowingArmTypes[name] {
			return name
		}
	}
	return ""
}

func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
	}
	return ""
}

// collectCarriers adds every name assigned within the clause from an
// expression that mentions a name already carrying the arm's value.
func collectCarriers(clause *ast.CaseClause, carriers map[string]bool) {
	// Two passes, because `i, _ := n.Int64()` may follow the assignment that
	// made `n` a carrier, in either order inside an if-statement's init.
	for range 2 {
		ast.Inspect(clause, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			mentions := false
			for _, rhs := range assign.Rhs {
				ast.Inspect(rhs, func(n ast.Node) bool {
					if ident, ok := n.(*ast.Ident); ok && carriers[ident.Name] {
						mentions = true
					}
					return true
				})
			}
			if !mentions {
				return true
			}
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" {
					carriers[ident.Name] = true
				}
			}
			return true
		})
	}
}

// nearestVerdict finds a marker in the comment group closest ABOVE the site,
// within the span a doc comment can plausibly cover.
func nearestVerdict(verdictAt map[int]string, line int) string {
	best, bestLine := "", -1
	for at, word := range verdictAt {
		if at <= line && at > bestLine && line-at <= narrowingVerdictSpan {
			best, bestLine = word, at
		}
	}
	return best
}

// narrowingVerdictSpan is how far below its marker a site may sit. A decoder
// is a type switch of at most a dozen arms, so 60 lines covers a doc comment
// above the longest of them and does not reach the next function.
const narrowingVerdictSpan = 60

func TestEveryPayloadNarrowingCarriesAnAnswer(t *testing.T) {
	files := narrowingTrackedGoFiles(t)

	var offenders []narrowingSite
	armsFound, routed := 0, 0
	badVerdict := map[string]string{}

	for _, rel := range files {
		src, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		armsFound += strings.Count(string(src), "\tcase float64:") +
			strings.Count(string(src), "\tcase int64:")
		routed += strings.Count(string(src), "num.Clamp") +
			strings.Count(string(src), "num.Int64") +
			strings.Count(string(src), "num.Float64")
		sites, err := findNarrowingSites(rel, src)
		if err != nil {
			// A file the gate cannot parse is a file it did not examine, and
			// a checker that hides what it could not read makes its own pass
			// a claim about the tool rather than about the tree.
			t.Errorf("%s: could not be parsed, so this gate did not examine it: %v", rel, err)
			continue
		}
		for _, site := range sites {
			switch {
			case site.verdict == "":
				offenders = append(offenders, site)
			case !narrowingVerdicts[site.verdict]:
				badVerdict[site.String()] = site.verdict
			}
		}
	}

	t.Logf("\n=== payload narrowings, re-derived at this tree (memql#4779) ===\n"+
		"  go files swept         %d\n"+
		"  float64/int64 arms     %d\n"+
		"  narrowings via core/num %d\n"+
		"  unanswered narrowings  %d\n",
		len(files), armsFound, routed, len(offenders))

	// --- the vacuous-pass guards ------------------------------------------
	//
	// Every one of these is a way for the gate to report a clean tree having
	// measured nothing: run from the wrong directory, run without git, a
	// detector that quietly stopped matching, or a repository that moved its
	// decoders somewhere this does not look.
	if len(files) < 300 {
		t.Fatalf("swept only %d tracked non-test Go files; this gate cannot pass vacuously", len(files))
	}
	if armsFound < 100 {
		t.Fatalf("found only %d float64/int64 case arms, and the tree had 156 when this gate "+
			"landed. Either the decoders moved somewhere this does not look or the scan is "+
			"reading the wrong tree -- both report clean while measuring nothing", armsFound)
	}
	if routed < 40 {
		t.Fatalf("only %d narrowings route through core/num, and %d did when this gate landed. "+
			"An empty offender list means nothing if the answers stopped being called",
			routed, 66)
	}

	if len(badVerdict) > 0 {
		keys := make([]string, 0, len(badVerdict))
		for k := range badVerdict {
			keys = append(keys, k+" (declared "+badVerdict[k]+")")
		}
		sort.Strings(keys)
		t.Errorf("these sites declared a verdict outside the closed set "+
			"{SATURATE, ZERO, DEFAULT, GUARDED}:\n  %s", strings.Join(keys, "\n  "))
	}

	if len(offenders) > 0 {
		lines := make([]string, 0, len(offenders))
		for _, site := range offenders {
			lines = append(lines, site.String())
		}
		sort.Strings(lines)
		t.Errorf("these narrowings decide what an out-of-range payload number means, and "+
			"none of them says which answer it picked.\n\n"+
			"DECIDE it -- do not reflexively reach for the first helper. The three answers "+
			"are all correct somewhere and each states something different about the field:\n"+
			"  num.ClampInt64 / num.ClampFloat64   saturate; the field is an ORDERING\n"+
			"  num.Int64OrZero / num.Float64OrZero  0; callers read 0 as \"unset\"\n"+
			"  num.Int64Or / num.Float64Or          the caller's default; the site has one\n\n"+
			"Then say so in the decoder's doc comment, in this vocabulary:\n"+
			"  // narrowing: SATURATE -- <why the order matters here>\n"+
			"  // narrowing: ZERO     -- <why callers read 0 as unset>\n"+
			"  // narrowing: DEFAULT  -- <the default, and why saturating is worse>\n"+
			"  // narrowing: GUARDED  -- <the inline bound, and why it is not core/num's>\n\n"+
			"%d unanswered:\n  %s",
			len(offenders), strings.Join(lines, "\n  "))
	}
}

func narrowingTrackedGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		skip := false
		for _, prefix := range narrowingScanSkipPrefixes {
			if strings.HasPrefix(rel, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	return files
}

// ===========================================================================
// The gate's own controls. Both halves matter, and they fail in opposite
// directions: without the first, the detector could match nothing and every
// run would be green; without the second, it could match everything and every
// run would be red for the wrong reason.
// ===========================================================================

// narrowingProbe is a decoder in the shape the whole class takes, written out
// twice -- once as the defect and once as the fix. Real files cannot serve as
// the offending half any more (that is what the sweep was), so the offender is
// synthetic and the SPARED half is checked against real source below.
const narrowingProbeOffender = `package probe

func decode(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}
`

const narrowingProbeAnswered = `package probe

import "github.com/znasllc-io/memql/core/num"

// narrowing: SATURATE -- the field is an ordering.
func decode(v any) int {
	switch n := v.(type) {
	case float64:
		return num.ClampFloat64(n)
	case int64:
		return num.ClampInt64(n)
	}
	return 0
}
`

func TestNarrowingDetectorFiresOnTheDefectAndSparesTheFix(t *testing.T) {
	offending, err := findNarrowingSites("probe.go", []byte(narrowingProbeOffender))
	if err != nil {
		t.Fatalf("parse offender probe: %v", err)
	}
	if len(offending) != 2 {
		t.Fatalf("the detector found %d narrowings in a file with two, so an empty "+
			"offender list over the real tree is a statement about the detector rather "+
			"than about the tree: %v", len(offending), offending)
	}
	for _, site := range offending {
		if site.verdict != "" {
			t.Errorf("%s carried verdict %q in a file with no marker in it", site, site.verdict)
		}
	}

	answered, err := findNarrowingSites("probe.go", []byte(narrowingProbeAnswered))
	if err != nil {
		t.Fatalf("parse answered probe: %v", err)
	}
	if len(answered) != 0 {
		t.Errorf("the detector flagged %d sites in a file that narrows only through "+
			"core/num; it is reading something other than the conversion: %v",
			len(answered), answered)
	}
}

// TestNarrowingMarkerFiresAndSpares pins the marker vocabulary in both
// directions, against real comment text from the sweep. A marker regexp that
// quietly stopped recognising its own words would let every site through.
func TestNarrowingMarkerFiresAndSpares(t *testing.T) {
	mustFire := map[string]string{
		"// narrowing: SATURATE -- the field is an ORDERING.":     "SATURATE",
		"// narrowing: ZERO -- callers read 0 as unset.":          "ZERO",
		"// narrowing: DEFAULT -- the site already names one.":    "DEFAULT",
		"// narrowing: GUARDED -- num.WholeInt64 IS the guard.":   "GUARDED",
		"// narrowing: SATURATE\n// -- wrapped across two lines.": "SATURATE",
	}
	for comment, want := range mustFire {
		src := "package probe\n\n" + comment + "\nfunc decode(v any) int {\n" +
			"\tswitch n := v.(type) {\n\tcase float64:\n\t\treturn int(n)\n\t}\n\treturn 0\n}\n"
		sites, err := findNarrowingSites("probe.go", []byte(src))
		if err != nil {
			t.Fatalf("parse %q: %v", comment, err)
		}
		if len(sites) != 1 {
			t.Fatalf("expected one site for %q, got %d", comment, len(sites))
		}
		if sites[0].verdict != want {
			t.Errorf("marker %q read as verdict %q, want %q", comment, sites[0].verdict, want)
		}
	}

	// Text that mentions the subject without ruling on it must NOT read as a
	// verdict -- otherwise a prose paragraph about narrowing would silently
	// pre-authorize the function beneath it.
	mustSpare := []string{
		"// This function does some narrowing of the payload.",
		"// See memql#4779 for the narrowing class.",
		"// narrowings are decided per site.",
	}
	for _, comment := range mustSpare {
		src := "package probe\n\n" + comment + "\nfunc decode(v any) int {\n" +
			"\tswitch n := v.(type) {\n\tcase float64:\n\t\treturn int(n)\n\t}\n\treturn 0\n}\n"
		sites, err := findNarrowingSites("probe.go", []byte(src))
		if err != nil {
			t.Fatalf("parse %q: %v", comment, err)
		}
		if len(sites) != 1 {
			t.Fatalf("expected one site for %q, got %d", comment, len(sites))
		}
		if sites[0].verdict != "" {
			t.Errorf("prose %q was read as verdict %q; a paragraph about narrowing is "+
				"not a ruling on one", comment, sites[0].verdict)
		}
	}
}

// TestNarrowingDetectorReadsTheCarriedValueOnly guards the precision the
// detector needs to be usable: a conversion inside a float64 arm that does NOT
// narrow the arm's own value is not this defect, and flagging it would make
// the allowlist the real interface.
func TestNarrowingDetectorReadsTheCarriedValueOnly(t *testing.T) {
	src := `package probe

func decode(v any, other []byte) int {
	switch n := v.(type) {
	case float64:
		_ = n
		return int(len(other))
	}
	return 0
}
`
	sites, err := findNarrowingSites("probe.go", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sites) != 0 {
		t.Errorf("flagged %v; int(len(other)) does not narrow the arm's value", sites)
	}

	// ...and the `n, _ := v.Int64()` shape DOES carry it, one hop away. Three
	// json.Number arms dropped that error and converted anyway, and the grep in
	// the triage doc could not see any of them.
	carried := `package probe

import "encoding/json"

func decode(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
`
	sites, err = findNarrowingSites("probe.go", []byte(carried))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sites) != 1 {
		t.Errorf("found %d sites in the json.Number shape, want 1: %v", len(sites), sites)
	}
}
