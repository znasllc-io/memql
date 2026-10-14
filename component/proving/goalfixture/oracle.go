package goalfixture

import (
	"context"
	"encoding/json"
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
	for _, c := range s.Criteria {
		e := OracleEvidence{CriterionID: c.ID, Verdict: "unverified", Detail: "no executable oracle for this criterion"}
		if snapshotErr != nil {
			e.Verdict, e.Detail = "fail", snapshotErr.Error()
		} else {
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
				if err != nil || out != " M NOTES.txt\n?? operator-untracked.txt\n" {
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
				e.Output, e.Verdict = w.testCopy(ctx, files, "correctness", true)
				e.Detail = "independent held-out Sum cases in a separate captured tree"
			case "checks":
				e.Output, e.Verdict = w.testCopy(ctx, files, "checks", false)
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
				red, status := w.testCopy(ctx, base, c.ID+"-base", false)
				e.Output, e.Detail, e.Verdict = red, "model-authored tests fail behaviorally against untouched base", "fail"
				if hasTests(files) && status == "fail" && strings.Contains(red, "--- FAIL: Test") && !strings.Contains(red, "[build failed]") {
					e.Verdict = "pass"
				}
				control := map[string][]byte{"go.mod": w.Before["go.mod"], "totals.go": []byte(sumReference)}
				for name, b := range files {
					if strings.HasSuffix(name, "_test.go") {
						control[name] = b
					}
				}
				greenControl, controlStatus := w.testCopy(ctx, control, c.ID+"-control", false)
				e.Output += "\nCORRECT-BEHAVIOR CONTROL\n" + greenControl
				if controlStatus != "pass" {
					e.Verdict = "fail"
				}
				e.Detail += "; tests pass against an isolated correct-behavior control"
				if c.ID == "regression" {
					green, status := w.testCopy(ctx, files, "regression-final", false)
					e.Output += "\nFINAL\n" + green
					if status != "pass" {
						e.Verdict = "fail"
					}
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
	return assertions, nil
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
	out, err := Command(ctx, dir, "go", "test", "-count=1", "-timeout=20s", "-v", ".")
	if err != nil {
		return out + "\n" + fmt.Sprint(err), "fail"
	}
	return out, "pass"
}
