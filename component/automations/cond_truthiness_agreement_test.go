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
// `cond(args.allowed, "Y", "N")` was evaluated by two code paths depending on
// the shape of the logic body it sat in -- component/memql for a single
// `return cond(...)`, this package's string evaluator for `x := cond(...)` --
// and they carried SEPARATE truthiness rules: the strings "false" and "0" read
// truthy on one path and falsy on the other. Those are exactly the shapes a
// JSON, HTTP or MCP caller sends for a stringified boolean, so a gate opened
// or closed depending on the body's shape.
//
// Edition 2026 retires cond() for `p ? a : b`, and every body shape evaluates
// through EvalExpr. There is no truthiness at all (D8, expr_eval.go): a
// condition is a bool, or absent (false), or it is refused as
// condition_not_boolean. A stringified boolean therefore never opens a gate
// -- it fails the evaluation, naming its type.

// ternaryConditionCases is the input set memql#2963 asked to be measured,
// with the v1 answer: "Y" / "N", or "refused".
var ternaryConditionCases = []struct {
	name string
	in   any
	want string
}{
	{"nil", nil, "N"},
	{"bool false", false, "N"},
	{"bool true", true, "Y"},
	{"empty string", "", "refused"},
	{`the string "false"`, "false", "refused"},
	{`the string "0"`, "0", "refused"},
	{`the string "true"`, "true", "refused"},
	{"zero", 0, "refused"},
	{"one", 1, "refused"},
	{"non-empty string", "nonempty", "refused"},
	{"non-zero float", 2.5, "refused"},
}

// TestTernaryConditionIsBooleanOnly drives a logic step's ternary over a run
// holding each input, and asserts the v1 answer: only a bool (or absence)
// decides a branch.
func TestTernaryConditionIsBooleanOnly(t *testing.T) {
	for _, tc := range ternaryConditionCases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvaluator()
			ev.SetCustom("args", map[string]any{"allowed": tc.in})
			got, err := evalV1(ev, `args.allowed ? "Y" : "N"`)
			answer := fmt.Sprint(got)
			if err != nil {
				if !strings.Contains(err.Error(), "condition_not_boolean") && !strings.Contains(err.Error(), "must be boolean") {
					t.Fatalf("allowed=%#v: %v, want a condition_not_boolean refusal", tc.in, err)
				}
				answer = "refused"
			}
			if answer != tc.want {
				t.Errorf("`args.allowed ? \"Y\" : \"N\"` with allowed=%#v = %s, want %s", tc.in, answer, tc.want)
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
