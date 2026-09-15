package tiers

// docs_test.go -- the language reference's "Where each expression runs" table
// is generated from this package's manifest, and this test holds the two
// together. The table is not restated by hand: when the manifest changes, run
//
//	go test github.com/znasllc-io/memql/component/language/tiers -run TestWhereEachExpressionRunsIsPublished -update-docs
//
// and the rows between the markers in docs/public/language/memql.md are
// rewritten from Rules() and PredicateAdmission.

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
)

var updateDocs = flag.Bool("update-docs", false, "rewrite the generated table in docs/public/language/memql.md")

const (
	referencePath   = "../../../docs/public/language/memql.md"
	positionsRegion = "where each expression runs"
)

// TestWhereEachExpressionRunsIsPublished: the reference's table is the
// manifest, row for row.
func TestWhereEachExpressionRunsIsPublished(t *testing.T) {
	want, err := renderPositionTable()
	if err != nil {
		t.Fatal(err)
	}
	checkGeneratedRegion(t, referencePath, positionsRegion, want,
		"go test github.com/znasllc-io/memql/component/language/tiers -run TestWhereEachExpressionRunsIsPublished -update-docs")
}

// writtenAs is how an author meets each position. It is the one hand-written
// column: the manifest knows what a position admits, not what it looks like.
// A position without a spelling here fails the render, so a new position
// cannot reach the reference without one.
var writtenAs = map[Position]string{
	PositionQueryFilter:         "`filter row => ...` in a query, and the query a tool's `@handler(query=...)` runs",
	PositionSort:                "`sort \"row.createdAt\", \"desc\"`",
	PositionSpecBody:            "`spec c name = row => ...`, `trait name = row => ...`",
	PositionRowAuthzArgument:    "`@rowAuthz(owner=\"ownerUserId\")`",
	PositionAutomationCondition: "an automation's `if` and `else if` conditions, a `for` filter, a `switch` subject and a precondition's `check:`",
	PositionTriggerFilter:       "`@filter(row => ...)` over the triggering row",
	PositionLogicBody:           "a statement or a `return` in a logic body",
	PositionMutationValue:       "`key: <value>` in `insert`, `update` or `stamp`",
	PositionBeforeWriteValue:    "a row.<field> = <expression> value in a before-write automation",
	PositionStepArgument:        "an argument of a call statement in an automation",
	PositionToolDefault:         "a tool field's `@default(\"...\")`",
	PositionPromptInput:         "a value bound into a prompt's input",
	PositionQueryRefine:         "`refine row => ...` after `paginate`",
}

// kindSpelling is how the table names a node kind: the way an author writes
// it. Every kind has one (TestEveryKindHasATableSpelling), so a kind the
// manifest refuses or restricts is always named in words, never as an
// internal identifier.
var kindSpelling = map[ast.NodeKind]string{
	ast.KindIdent:          "a name",
	ast.KindMember:         "`.f`",
	ast.KindOptionalMember: "`.?f`",
	ast.KindCall:           "a function call",
	ast.KindMethodCall:     "a method call",
	ast.KindConstructCall:  "a construct call",
	ast.KindNot:            "`!`",
	ast.KindNegate:         "unary `-`",
	ast.KindArithmetic:     "arithmetic",
	ast.KindCoalesce:       "`??`",
	ast.KindComparison:     "a comparison",
	ast.KindIn:             "`in`",
	ast.KindStartsWith:     "`startsWith`",
	ast.KindAnd:            "`&&`",
	ast.KindOr:             "`\\|\\|`",
	ast.KindTernary:        "`c ? a : b`",
	ast.KindLambda:         "a lambda",
	ast.KindList:           "a list literal",
	ast.KindMap:            "a map literal",
	ast.KindLiteral:        "a literal",
	ast.KindNil:            "`nil`",
	ast.KindParen:          "parentheses",
}

func TestEveryKindHasATableSpelling(t *testing.T) {
	for _, k := range ast.AllNodeKinds() {
		if kindSpelling[k] == "" {
			t.Errorf("node kind %q has no spelling in kindSpelling: the generated table cannot name it", k)
		}
	}
	for p := range writtenAs {
		if TierOf(p) == "" {
			t.Errorf("writtenAs names %q, which is not a position", p)
		}
	}
}

// renderPositionTable renders one row per position, in Positions() order,
// from the manifest's own answers.
func renderPositionTable() (string, error) {
	var b strings.Builder
	b.WriteString("| Position | Written as | Runs | Takes | Applies a spec or trait |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, p := range Positions() {
		spelled, ok := writtenAs[p]
		if !ok {
			return "", fmt.Errorf("position %s has no writtenAs spelling", p)
		}
		takes, err := takesCell(p)
		if err != nil {
			return "", err
		}
		runs := "In process"
		if TierOf(p) == TierP {
			runs = "Pushed down to SQL"
		}
		preds := "No"
		if PredicateAdmission(p) != Refused {
			preds = "Yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", p, spelled, runs, takes, preds)
	}
	return b.String(), nil
}

// takesCell says what a position admits, grouped from Rules(): the kinds it
// refuses, the kinds and functions it admits only on values that do not read
// the row, and -- where the functions do not follow one of the three tier
// rules -- each function by name.
func takesCell(p Position) (string, error) {
	var refused, planOnly, admitted []string
	fnAdmissions := map[functions.Tier]map[Admission][]string{}
	for _, r := range Rules() {
		if r.Position != p {
			continue
		}
		if r.Function != "" {
			f, ok := catalogByKey[r.Function]
			if !ok {
				return "", fmt.Errorf("rule for %s names function %q, which the catalog does not hold", p, r.Function)
			}
			if fnAdmissions[f.Tier] == nil {
				fnAdmissions[f.Tier] = map[Admission][]string{}
			}
			fnAdmissions[f.Tier][r.Admission] = append(fnAdmissions[f.Tier][r.Admission], r.Function)
			continue
		}
		spelled := kindSpelling[r.Kind]
		if spelled == "" {
			return "", fmt.Errorf("node kind %q has no table spelling", r.Kind)
		}
		switch r.Admission {
		case Refused:
			refused = append(refused, spelled)
		case PlanConstantOnly:
			planOnly = append(planOnly, spelled)
		case Admitted:
			admitted = append(admitted, spelled)
		}
	}

	// A position that admits literals and nothing else is a literal position.
	if len(admitted) == 1 && admitted[0] == kindSpelling[ast.KindLiteral] && len(planOnly) == 0 {
		if only(fnAdmissions, Refused) {
			return "A literal, and nothing else", nil
		}
		return "", fmt.Errorf("position %s admits only literals but calls functions: the table has no words for that", p)
	}

	// Functions: every tier admitted, or pushed-down functions admitted and
	// in-process functions only on plan constants; anything else is named.
	var fnNotes []string
	switch {
	case only(fnAdmissions, Admitted):
	case tierIs(fnAdmissions, functions.TierP, Admitted) && tierIs(fnAdmissions, functions.TierM, PlanConstantOnly):
		planOnly = append(planOnly, "an in-process function")
	default:
		for _, tier := range []functions.Tier{functions.TierP, functions.TierM} {
			for _, a := range []Admission{Refused, PlanConstantOnly} {
				keys := append([]string(nil), fnAdmissions[tier][a]...)
				sort.Strings(keys)
				for _, k := range keys {
					fnNotes = append(fnNotes, fmt.Sprintf("`%s` %s", k, a))
				}
			}
		}
	}

	cell := "Every expression"
	if len(refused) > 0 {
		cell += " except " + joinWords(refused)
	}
	if len(planOnly) > 0 {
		cell += "; " + joinWords(planOnly) + " only on values that do not read the row"
	}
	if len(fnNotes) > 0 {
		cell += "; " + strings.Join(fnNotes, ", ")
	}
	return cell, nil
}

// only reports whether every function, of every tier, has admission a.
func only(fa map[functions.Tier]map[Admission][]string, a Admission) bool {
	for _, byAdmission := range fa {
		for got, keys := range byAdmission {
			if got != a && len(keys) > 0 {
				return false
			}
		}
	}
	return true
}

// tierIs reports whether every function of one tier has admission a.
func tierIs(fa map[functions.Tier]map[Admission][]string, tier functions.Tier, a Admission) bool {
	for got, keys := range fa[tier] {
		if got != a && len(keys) > 0 {
			return false
		}
	}
	return true
}

// joinWords joins a list the way the table's prose does: "a, b and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// checkGeneratedRegion compares the content between a region's markers with
// want, or -- under -update-docs -- rewrites it.
func checkGeneratedRegion(t *testing.T, path, region, want, command string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	begin := "<!-- BEGIN GENERATED: " + region + "."
	end := "<!-- END GENERATED: " + region + " -->"
	text := string(raw)
	b := strings.Index(text, begin)
	if b < 0 {
		t.Fatalf("%s has no %q marker", path, begin)
	}
	open := b + strings.Index(text[b:], "\n") + 1
	e := strings.Index(text[open:], end)
	if e < 0 {
		t.Fatalf("%s has no %q marker after %q", path, end, begin)
	}
	got := text[open : open+e]
	body := "\n" + want + "\n"
	if got == body {
		return
	}
	if *updateDocs {
		if err := os.WriteFile(path, []byte(text[:open]+body+text[open+e:]), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote the %q region of %s", region, path)
		return
	}
	t.Errorf("the %q table in %s is not what the code says; regenerate it with\n\t%s\nwant:\n%s\ngot:\n%s", region, path, command, body, got)
}
