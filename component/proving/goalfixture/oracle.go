package goalfixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

const sumOracle = `package totals
import "testing"
func TestMemQLEvaluationSumOracle(t *testing.T) {
 cases := [][]int{nil, {}, {9}, {0}, {-4}, {1,2,3}, {-5,2,-8,20}, {100,0,0,7}}
 for n := 0; n < 53; n++ { v := make([]int,n); for i := range v { v[i] = ((i*17+n*31)%101)-50 }; cases=append(cases,v) }
 for _, values := range cases { want := 0; for _,v := range values { want += v }; if got:=Sum(values); got!=want { t.Errorf("Sum(%v)=%d, want %d",values,got,want) } }
}
`

// Used only in a separate oracle copy, never exposed to the worker checkout.
// A reproduction must stop failing when the specified behavior is correct.
const sumReference = `package totals
func Sum(values []int) int { total := 0; for _, value := range values { total += value }; return total }
`

type OracleEvidence struct {
	CriterionID string `json:"criterionId"`
	Verdict     string `json:"verdict"`
	Detail      string `json:"detail"`
	Output      string `json:"output,omitempty"`
}

// ErrOracleDidNotFinish means an oracle's own command reported no exit status,
// so the evaluator measured nothing about the work. Keeping that apart from a
// failure protects the score: a killed `go test` used to answer "fail", which in
// a real evaluation publishes a correct fix as failed (captured 2026-11-01 at
// --cpus=0.1: "go failed: signal: killed" and no test output).
//
// Verify still answers every criterion and saves its evidence: the run's verdict
// is unverified, never fail, and a failure another run reported still stands. It
// starts no further `go test`, since nothing will be scored and another watchdog
// on the same machine buys nothing: at --cpus=0.02 a second kill outlasted the
// package's own -timeout=600s, which panicked before the oracle could say why.
// The GoalRunner then records an observer error, publishes nothing and stops the
// matrix, because a machine that cannot finish its checks would waste the next
// case's model calls too. Nothing can earn a pass this way: a required criterion
// that is not pass already refuses success (goaleval.Result.Validate). The
// published contract needs no fourth verdict, since no result carrying this one
// is ever published.
//
// Known limits: this sees only the go command itself, so a compile the kernel
// kills inside a live `go test` is reported by go as [build failed] and reads as
// a failure of the tree. A tree that keeps the compiler busy past goTestBound
// reads as an oracle that did not finish, never as a pass.
//
// Regression tests: TestOracleThatCouldNotFinishNeverJudgesTheWork,
// TestRegressionOracleDoesNotLetAnUnfinishedRunHideAFailure,
// TestOracleStartsNoFurtherRunAfterOneDidNotFinish and, for the score, proving's
// TestGoalWorkflowScoresNothingWhenItsOracleCouldNotFinish.
var ErrOracleDidNotFinish = errors.New("goal oracle did not finish")

func (w *Workspace) Verify(ctx context.Context, s goaleval.Scenario) ([]goaleval.Assertion, error) {
	files, snapshotErr := w.Snapshot()
	if snapshotErr == nil {
		b, err := json.MarshalIndent(files, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(w.Root, "final-files.json"), b, 0600); err != nil {
			return nil, err
		}
	}
	var assertions []goaleval.Assertion
	var unfinished []string
	testCopy := func(files map[string][]byte, name string, independent bool) (string, string) {
		if len(unfinished) > 0 {
			unfinished = append(unfinished, name+": not run")
			return "not run: an earlier oracle run did not finish", "unverified"
		}
		out, verdict := w.testCopy(ctx, files, name, independent)
		if verdict == "unverified" {
			unfinished = append(unfinished, name+": "+out[strings.LastIndex(out, "\n")+1:])
		}
		return out, verdict
	}
	for _, c := range s.Criteria {
		e := OracleEvidence{CriterionID: c.ID, Verdict: "unverified", Detail: "no executable oracle for this criterion"}
		if snapshotErr != nil {
			e.Verdict, e.Detail = "fail", snapshotErr.Error()
		} else {
			unfinishedBefore := len(unfinished)
			switch c.ID {
			case "preservation":
				e.Verdict, e.Detail = "pass", "preserved tracked, dirty and untracked bytes and tracking state"
				for name, want := range w.Preserved {
					if string(files[name]) != string(want) {
						e.Verdict, e.Detail = "fail", "preserved file changed: "+name
						break
					}
				}
				out, err := Command(ctx, w.Checkout, "git", "status", "--porcelain=v1", "--", "NOTES.txt", "operator-untracked.txt", ".memql-evaluation.json")
				e.Output = out
				switch {
				case errors.Is(err, errUnfinished):
					unfinished = append(unfinished, c.ID+": "+err.Error())
					if e.Verdict == "pass" {
						e.Verdict = "unverified"
					}
				case err != nil || out != " M NOTES.txt\n?? operator-untracked.txt\n":
					e.Verdict, e.Detail = "fail", "unrelated tracked or untracked state changed"
				}
			case "no-fix":
				e.Verdict, e.Detail = "pass", "implementation retains exact base bytes"
				for name, before := range w.Before {
					if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && string(files[name]) != string(before) {
						e.Verdict, e.Detail = "fail", "implementation changed"
					}
				}
				for name := range files {
					if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
						if _, ok := w.Before[name]; !ok {
							e.Verdict, e.Detail = "fail", "added implementation during investigation"
						}
					}
				}
			case "correctness":
				e.Output, e.Verdict = testCopy(files, "correctness", true)
				e.Detail = "independent held-out Sum cases in a separate captured tree"
			case "checks":
				e.Output, e.Verdict = testCopy(files, "checks", false)
				e.Detail = "independent execution of model-authored package tests"
				if !hasTests(files) {
					e.Verdict, e.Detail = "fail", "no model-authored package tests"
				}
			case "regression", "red", "reproduction":
				base := map[string][]byte{}
				for name, b := range w.Before {
					base[name] = b
				}
				for name, b := range files {
					if strings.HasSuffix(name, "_test.go") {
						base[name] = b
					}
				}
				red, status := testCopy(base, c.ID+"-base", false)
				e.Output, e.Detail, e.Verdict = red, "model-authored tests fail behaviorally against untouched base", "fail"
				if hasTests(files) && status == "unverified" {
					e.Verdict = "unverified"
				} else if hasTests(files) && status == "fail" && strings.Contains(red, "--- FAIL: Test") && !strings.Contains(red, "[build failed]") {
					e.Verdict = "pass"
				}
				control := map[string][]byte{"go.mod": w.Before["go.mod"], "totals.go": []byte(sumReference)}
				for name, b := range files {
					if strings.HasSuffix(name, "_test.go") {
						control[name] = b
					}
				}
				greenControl, controlStatus := testCopy(control, c.ID+"-control", false)
				e.Output += "\nCORRECT-BEHAVIOR CONTROL\n" + greenControl
				e.Verdict = settle(e.Verdict, controlStatus)
				e.Detail += "; tests pass against an isolated correct-behavior control"
				if c.ID == "regression" {
					green, status := testCopy(files, "regression-final", false)
					e.Output += "\nFINAL\n" + green
					e.Verdict = settle(e.Verdict, status)
					e.Detail += " and pass against the captured final tree"
				}
			case "refactor":
				e.Verdict, e.Detail = "fail", "Sum must use range with the same exported signature"
				f, err := parser.ParseFile(token.NewFileSet(), "totals.go", files["totals.go"], 0)
				if err == nil {
					for _, decl := range f.Decls {
						fn, ok := decl.(*ast.FuncDecl)
						if !ok || fn.Name.Name != "Sum" || fn.Body == nil || !sumSignature(fn) {
							continue
						}
						ast.Inspect(fn.Body, func(n ast.Node) bool {
							if _, ok := n.(*ast.RangeStmt); ok {
								e.Verdict = "pass"
							}
							return true
						})
					}
				}
			}
			if e.Verdict == "unverified" && len(unfinished) > unfinishedBefore {
				e.Detail = "no conclusion: an oracle run did not finish, which says nothing about the work; it would have established: " + e.Detail
			}
		}
		name := "oracle-" + c.ID + ".json"
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(w.Root, name), append(b, '\n'), 0600); err != nil {
			return nil, err
		}
		assertions = append(assertions, goaleval.Assertion{CriterionID: c.ID, Verdict: e.Verdict, Evidence: []string{filepath.Join(w.Root, name)}})
	}
	if len(unfinished) > 0 {
		return assertions, fmt.Errorf("%w: %s", ErrOracleDidNotFinish, strings.Join(unfinished, "; "))
	}
	return assertions, nil
}

// settle combines two runs of one criterion. A reported failure is a finding
// about the work and decides it, whatever a sibling run could not say; otherwise
// a run that did not finish leaves the criterion open.
func settle(a, b string) string {
	switch {
	case a == "fail" || b == "fail":
		return "fail"
	case a == "unverified" || b == "unverified":
		return "unverified"
	}
	return "pass"
}

func sumSignature(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || fn.Type.TypeParams != nil || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}
	input := fn.Type.Params.List[0]
	slice, ok := input.Type.(*ast.ArrayType)
	if !ok || slice.Len != nil || len(input.Names) > 1 {
		return false
	}
	element, ok := slice.Elt.(*ast.Ident)
	output, outputOK := fn.Type.Results.List[0].Type.(*ast.Ident)
	return ok && element.Name == "int" && outputOK && output.Name == "int" && len(fn.Type.Results.List[0].Names) <= 1
}

func hasTests(files map[string][]byte) bool {
	for name := range files {
		if strings.HasSuffix(name, "_test.go") {
			return true
		}
	}
	return false
}

func (w *Workspace) testCopy(ctx context.Context, files map[string][]byte, name string, independent bool) (string, string) {
	dir, err := os.MkdirTemp(w.Root, "oracle-"+name+"-")
	if err != nil {
		return err.Error(), "unverified"
	}
	for name, b := range files {
		if strings.HasSuffix(name, ".go") || name == "go.mod" {
			if independent && strings.HasSuffix(name, "_test.go") {
				continue
			}
			if err := putFile(dir, name, b); err != nil {
				return err.Error(), "unverified"
			}
		}
	}
	if independent {
		if err := putFile(dir, "memql_evaluation_oracle_test.go", []byte(sumOracle)); err != nil {
			return err.Error(), "unverified"
		}
	}
	out, err := commandWithin(ctx, goTestBound, dir, "go", "test", "-count=1", "-timeout=20s", "-v", ".")
	if errors.Is(err, errUnfinished) {
		return out + "\n" + fmt.Sprint(err), "unverified"
	}
	if err != nil {
		return out + "\n" + fmt.Sprint(err), "fail"
	}
	return out, "pass"
}
