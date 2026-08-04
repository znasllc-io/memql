package automations

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// memqlRoot is the package that owns the shared rule, relative to this one.
const memqlRoot = "../memql"

// singleCall reports the sole call expression on the right-hand side of an
// assignment, if that is what it is.
func singleCall(rhs []ast.Expr) (*ast.CallExpr, bool) {
	if len(rhs) != 1 {
		return nil, false
	}
	call, ok := rhs[0].(*ast.CallExpr)
	return call, ok
}

// isSharedRuleCall reports whether the call is `memql.IsTruthy(...)` -- the one
// spelling a variable named isTruthy is allowed to be assigned from.
func isSharedRuleCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "IsTruthy" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "memql"
}

// cond_truthiness_agreement_test.go -- memql#2963.
//
// `cond(args.allowed, "Y", "N")` is evaluated by two different code paths
// depending on the shape of the logic body it sits in:
//
//	single-statement   `return cond(...)`        -> component/memql, evalCollCond
//	multi-statement    `x := cond(...)  return x` -> this package, evaluateCondLocally
//
// They used to carry SEPARATE truthiness rules, and the divergence was real
// rather than theoretical. Measured across the full input set before the fix:
//
//	input        single   multi
//	nil          N        N
//	false        N        N
//	true         Y        Y
//	""           N        N
//	"false"      Y        N     <- diverged
//	"0"          Y        N     <- diverged
//	"true"       Y        Y
//	0            N        N
//	1            Y        Y
//	"nonempty"   Y        Y
//	2.5          Y        Y
//
// Two inputs, and both of them the shape a JSON, HTTP or MCP caller sends for a
// stringified boolean. A gate written `return cond(args.allowed, true, false)`
// therefore opened on the string "false" in one body shape and closed in the
// other.
//
// There is one implementation now (memql.IsTruthy, strict). This test is the
// gate that keeps it that way, and it is the sibling of
// TestPositionalBuiltinEvaluatorsAgree, whose own premise is exactly this:
// "the same source must produce the same value either way." That test covers
// concat and coalesce, not cond.
//
// Direction of the ruling: STRICT. The permissive rule is the one that fails
// OPEN on a gate, and an author who writes "false" means false.

// condTruthinessCases is the input set memql#2963 asks to be measured. Shared
// by both halves of this file so neither can quietly test a narrower set.
var condTruthinessCases = []struct {
	name string
	in   any
	want string // "Y" (truthy) or "N" (falsy)
}{
	{"nil", nil, "N"},
	{"bool false", false, "N"},
	{"bool true", true, "Y"},
	{"empty string", "", "N"},
	{`the string "false"`, "false", "N"},
	{`the string "0"`, "0", "N"},
	{`the string "true"`, "true", "Y"},
	{"zero", 0, "N"},
	{"one", 1, "Y"},
	{"non-empty string", "nonempty", "Y"},
	{"non-zero float", 2.5, "Y"},
}

// The multi-statement path, driven through the evaluator a logic body's
// intermediate step actually uses.
func TestCondTruthinessMultiStatementPath(t *testing.T) {
	for _, tc := range condTruthinessCases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvaluator()
			ev.SetCustom("args", map[string]any{"allowed": tc.in})
			got, handled, err := tryEvaluateBuiltinLocally(`cond(args.allowed, "Y", "N")`, ev)
			if err != nil {
				t.Fatalf("evaluating cond with allowed=%#v: %v", tc.in, err)
			}
			if !handled {
				t.Fatalf("cond was not handled locally for allowed=%#v, so this measures nothing", tc.in)
			}
			if fmt.Sprint(got) != tc.want {
				t.Errorf("multi-statement `cond(args.allowed, \"Y\", \"N\")` with allowed=%#v = %v, want %s",
					tc.in, got, tc.want)
			}
		})
	}
}

// TestCondTruthinessPathMatchesTheSharedRule is the agreement itself, and WHAT
// it compares is the whole point: for every input, the answer the
// multi-statement PATH ACTUALLY PRODUCES must equal the answer memql.IsTruthy
// gives. Path output against rule output -- not the rule against a table.
//
// That distinction is the difference between a gate and a tautology. An earlier
// spelling asserted only `memql.IsTruthy(tc.in) == want`, which says nothing
// whatever about the path: repoint evaluator.go's call site at a local
// permissive rule and the two genuinely disagree, yet that version PASSED,
// because it never evaluated the path. This version fails, because it
// evaluates both sides and compares them.
//
// This package cannot drive the single-statement shape, and the reason is
// structural rather than a missing harness: `return cond(...)` alone compiles
// to an automation with no steps ("automation must have at least one step"),
// which is exactly WHY that shape takes component/memql's path instead. It is
// pinned there by TestCond_TruthinessIsPinnedIndependently over the same
// values, and TestThereIsOnlyOneTruthinessRuleImplementation below is what
// stops a second rule appearing for either side to drift onto.
func TestCondTruthinessPathMatchesTheSharedRule(t *testing.T) {
	for _, tc := range condTruthinessCases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvaluator()
			ev.SetCustom("args", map[string]any{"allowed": tc.in})
			got, handled, err := tryEvaluateBuiltinLocally(`cond(args.allowed, "Y", "N")`, ev)
			if err != nil {
				t.Fatalf("evaluating cond with allowed=%#v: %v", tc.in, err)
			}
			if !handled {
				t.Fatalf("cond was not handled locally for allowed=%#v, so this measures nothing", tc.in)
			}

			pathSaysTruthy := fmt.Sprint(got) == "Y"
			ruleSaysTruthy := memql.IsTruthy(tc.in)
			if pathSaysTruthy != ruleSaysTruthy {
				t.Errorf("the multi-statement PATH and the shared RULE disagree on %#v: "+
					"path chose %q (truthy=%v), memql.IsTruthy says truthy=%v.\n\n"+
					"That is the divergence memql#2963 was filed about, reopened. cond's branch "+
					"must come from one rule on every path -- and the direction matters: the "+
					"permissive spelling (any non-empty string is truthy) fails OPEN on a gate "+
					"written `cond(args.allowed, true, false)`.",
					tc.in, got, pathSaysTruthy, ruleSaysTruthy)
			}
		})
	}
}

// The duplicate is gone, not merely aligned. Two implementations that happen to
// agree today is the state memql#2963 was filed about -- they agreed on nine of
// eleven inputs, which is exactly why nobody noticed for as long as they didn't.
//
// This is a SOURCE SCAN, and it has to be. The behavioural anchor it replaces
// asserted `!memql.IsTruthy("false")` and nothing else -- a strict subset of
// what the table above already covers -- and it passed with a duplicate rule
// reintroduced AND wired into the production call site. It could not have
// failed: it never looked at anything except the one function it wanted to
// vouch for. Its own comment claimed "nothing here can assert 'no such function
// exists' at runtime"; that is untrue, and this repo already does exactly this
// scan in component/automations/actor_envelope_invariant_test.go,
// component/database/memory-nodes/concept_rowauthz_test.go and
// component/identity/admin/route_gate_test.go.
//
// What a truthiness rule looks like, and why the shape is worth matching on: a
// function whose body type-switches a value and answers bool. The historical
// ones were `isTruthy`, and a local one shadows the shared rule at every call
// site in the package with no compile error -- which is how the divergence
// survived long enough to be filed.
func TestThereIsOnlyOneTruthinessRuleImplementation(t *testing.T) {
	// component/memql is in the root set deliberately. It is the package that
	// OWNS the shared rule, and it hosted one of the two duplicates this issue
	// removed (mutation_templates.go's evalCondition arm) -- so a scan that
	// covers only the consumer packages leaves the likeliest home for the next
	// duplicate unwatched.
	roots := []string{".", "./steps", memqlRoot}
	var offenders []string
	scanned := 0

	for _, root := range roots {
		fset := token.NewFileSet()
		pkgs, err := goparser.ParseDir(fset, root, nil, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", root, err)
		}
		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				if strings.HasSuffix(path, "_test.go") {
					continue
				}
				scanned++
				ast.Inspect(file, func(n ast.Node) bool {
					switch d := n.(type) {
					case *ast.FuncDecl:
						if !strings.EqualFold(d.Name.Name, "isTruthy") {
							return true
						}
						// The one permitted declaration is the shared rule
						// itself. Matched exactly, package-qualified, and only
						// as a plain function: a lowercase `isTruthy` beside it
						// is still a duplicate, an `IsTruthy` anywhere else is
						// still a duplicate, and a METHOD of either spelling is
						// a duplicate wherever it lives.
						if d.Name.Name == "IsTruthy" && root == memqlRoot && d.Recv == nil {
							return true
						}
						kind := "func"
						if d.Recv != nil {
							kind = "method"
						}
						offenders = append(offenders,
							fmt.Sprintf("%s:%d %s %s", path, fset.Position(d.Pos()).Line, kind, d.Name.Name))
					case *ast.AssignStmt:
						// `isTruthy := ...` -- the inline spelling that carried
						// its own rule in steps/mutation.go, complete with a
						// multi-type `case int, int64, float64: v != 0` that
						// left v as `any` and read int64(0) as TRUE.
						for _, lhs := range d.Lhs {
							id, ok := lhs.(*ast.Ident)
							if ok && strings.EqualFold(id.Name, "isTruthy") {
								if call, isCall := singleCall(d.Rhs); isCall && isSharedRuleCall(call) {
									continue // `isTruthy := memql.IsTruthy(x)` is the point
								}
								offenders = append(offenders,
									fmt.Sprintf("%s:%d local %s", path, fset.Position(id.Pos()).Line, id.Name))
							}
						}
					}
					return true
				})
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned no non-test Go files, so this gate measures nothing")
	}
	if len(offenders) > 0 {
		t.Errorf("a second truthiness rule exists in this package tree:\n  %s\n\n"+
			"cond's branch must come from memql.IsTruthy and nothing else (memql#2963). A "+
			"local rule shadows the shared one at every call site in its package with no "+
			"compile error -- which is how the original divergence survived. If this is a "+
			"deliberate new rule, three documents change with it: "+
			"docs/public/language/functions.md, dsl/_reference/_logic.memql, and this test.",
			strings.Join(offenders, "\n  "))
	}

	// Belt and braces on the direction of the shared rule itself, kept because
	// it is the one property the scan above cannot express.
	for _, in := range []any{"false", "0"} {
		if memql.IsTruthy(in) {
			t.Errorf("memql.IsTruthy(%q) is true. That is the permissive spelling this package "+
				"used to reject, and it is the one that opens a gate handed a stringified "+
				"boolean (memql#2963).", in)
		}
	}
}
