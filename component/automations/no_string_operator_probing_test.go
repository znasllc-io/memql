package automations

// no_string_operator_probing_test.go -- the edition-2026 flip's acceptance
// check for this tree, as a test (memql#5367).
//
// The string evaluator that lived here decided what a piece of text MEANT by
// searching it for operators: `strings.Contains(t, "==")` to call it a
// predicate, `strings.Index(cond, "&&")` to split a conjunction,
// `strings.SplitN(part, "==", 2)` to find a comparison's sides. Every such
// probe is a second, private reading of the grammar, and the two readings
// drifted -- a quoted "==" inside a string literal split a condition in two,
// an operator inside a nested call was read at the top level. Edition 2026
// parses every expression once, at load, into a tree (PrepareExpressions),
// and nothing in the runtime reads expression text again.
//
// So: no non-test Go file under component/automations may hand an operator
// literal -- "==", "!=", ">=", "<=", "&&", "||" -- to a strings / bytes
// search or split. An operator is read off an AST node (ast.BinaryExpr.Op),
// never searched for in text.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// probedOperators are the operator spellings a probe searches text for.
var probedOperators = []string{"==", "!=", ">=", "<=", "&&", "||"}

// probingFuncs are the strings / bytes functions that search, split or
// rewrite text by a substring: handing one an operator is operator probing.
var probingFuncs = map[string]bool{
	"Contains": true, "ContainsAny": true, "Count": true, "Cut": true,
	"Index": true, "IndexAny": true, "LastIndex": true, "LastIndexAny": true,
	"Split": true, "SplitN": true, "SplitAfter": true, "SplitAfterN": true,
	"HasPrefix": true, "HasSuffix": true, "TrimPrefix": true, "TrimSuffix": true,
	"CutPrefix": true, "CutSuffix": true, "Replace": true, "ReplaceAll": true,
	"EqualFold": true,
}

// probeArg unwraps a conversion -- `[]byte("==")`, `string(op)` -- to the
// argument it converts.
func probeArg(e ast.Expr) ast.Expr {
	for {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return e
		}
		switch fun := call.Fun.(type) {
		case *ast.ArrayType:
		case *ast.Ident:
			if fun.Name != "string" {
				return e
			}
		default:
			return e
		}
		e = call.Args[0]
	}
}

// holdsOperator reports whether a string literal's value contains an
// operator spelling.
func holdsOperator(lit *ast.BasicLit) bool {
	if lit == nil || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	for _, op := range probedOperators {
		if strings.Contains(v, op) {
			return true
		}
	}
	return false
}

// operatorProbes returns one "file:line: call" per operator probe in the
// non-test Go files under root, and how many files it parsed. It reads three
// spellings of the operator argument: a string literal written in the call,
// a name bound to one (a const or var, at any scope in the file), and the
// value variable of a range over a literal list holding one -- the
// `for _, op := range []string{"==", "!="}` shape the string evaluator used.
func operatorProbes(root string) (probes []string, files int, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		files++

		// Names bound to an operator literal, and range variables over a
		// literal list holding one. Scope is ignored: a name that is an
		// operator anywhere in the file is suspect everywhere in it, which
		// errs toward reporting.
		bound := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if i < len(x.Values) {
						if lit, ok := x.Values[i].(*ast.BasicLit); ok && holdsOperator(lit) {
							bound[name.Name] = true
						}
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || i >= len(x.Rhs) {
						continue
					}
					if lit, ok := x.Rhs[i].(*ast.BasicLit); ok && holdsOperator(lit) {
						bound[id.Name] = true
					}
				}
			case *ast.RangeStmt:
				list, ok := x.X.(*ast.CompositeLit)
				if !ok {
					return true
				}
				for _, el := range list.Elts {
					if lit, ok := el.(*ast.BasicLit); ok && holdsOperator(lit) {
						if id, ok := x.Value.(*ast.Ident); ok {
							bound[id.Name] = true
						}
						break
					}
				}
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !probingFuncs[sel.Sel.Name] {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || (pkg.Name != "strings" && pkg.Name != "bytes") {
				return true
			}
			for _, arg := range call.Args {
				switch a := probeArg(arg).(type) {
				case *ast.BasicLit:
					if !holdsOperator(a) {
						continue
					}
				case *ast.Ident:
					if !bound[a.Name] {
						continue
					}
				default:
					continue
				}
				probes = append(probes, fmt.Sprintf("%s: %s.%s(...)",
					fset.Position(call.Pos()), pkg.Name, sel.Sel.Name))
				break
			}
			return true
		})
		return nil
	})
	sort.Strings(probes)
	return probes, files, err
}

// TestNoStringOperatorProbing is the gate: no operator probe in the tree.
func TestNoStringOperatorProbing(t *testing.T) {
	probes, files, err := operatorProbes(".")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// The tree holds dozens of Go files (this package and steps/): a count
	// this low means the walk went blind, and a clean result would mean
	// nothing.
	if files < 40 {
		t.Fatalf("scanned only %d non-test Go files under component/automations -- the walk is broken, not the tree", files)
	}
	if len(probes) > 0 {
		t.Errorf("%d operator probe(s) in component/automations -- text searched for an operator is a second, private reading of the grammar (memql#5367). Read the operator off the parsed node instead (PrepareExpressions caches it):\n  %s",
			len(probes), strings.Join(probes, "\n  "))
	}
}

// TestNoStringOperatorProbingCatchesAProbe is the gate's negative control: a
// tree holding each spelling of a probe -- a literal argument, a bound name, a
// range over a literal list, through strings and through bytes, in a nested
// package -- must be reported probe by probe, and a comparison of a parsed
// operator must not be. A scan that finds nothing here finds nothing anywhere.
func TestNoStringOperatorProbingCatchesAProbe(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("probe.go", `package probe

import (
	"bytes"
	"strings"
)

const andOp = "&&"

func literal(s string) bool { return strings.Contains(s, "==") }

func split(s string) []string { return strings.SplitN(s, " != ", 2) }

func named(s string) int { return strings.Index(s, andOp) }

func ranged(s string) bool { for _, op := range []string{">=", "<="} { if strings.HasPrefix(s, op) { return true } }; return false }

func local(b []byte) bool { or := "||"; return bytes.Contains(b, []byte(or)) }

func converted(b []byte) bool { return bytes.Contains(b, []byte("||")) }

// Not probes: an operator read off a parsed node, and a search for text
// that holds no operator.
func fine(op, s string) bool { return op == "==" || strings.Contains(s, "=") }
`)
	write("nested/deeper.go", `package deeper

import "strings"

func deep(s string) bool { return strings.Contains(s, "&&") }
`)
	write("probe_test.go", `package probe

import "strings"

func inATest(s string) bool { return strings.Contains(s, "==") }
`)

	probes, files, err := operatorProbes(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if files != 2 {
		t.Errorf("parsed %d files, want 2 (a _test.go file is not scanned)", files)
	}
	want := []string{"literal", "split", "named", "ranged", "local", "converted", "deep"}
	for _, fn := range want {
		found := false
		for _, p := range probes {
			if strings.Contains(p, probeLineOf(t, root, fn)) {
				found = true
			}
		}
		if !found {
			t.Errorf("the probe in func %s was not reported; got:\n  %s", fn, strings.Join(probes, "\n  "))
		}
	}
	for _, p := range probes {
		if strings.Contains(p, probeLineOf(t, root, "fine")) {
			t.Errorf("a non-probe was reported: %s", p)
		}
	}
}

// probeLineOf is "file:line:" of the named func in the negative control's
// fixture -- each written on one line -- so a reported probe can be matched
// to the func holding it.
func probeLineOf(t *testing.T, root, fn string) string {
	t.Helper()
	for _, rel := range []string{"probe.go", filepath.Join("nested", "deeper.go")} {
		path := filepath.Join(root, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "func "+fn+"(") {
				return fmt.Sprintf("%s:%d:", path, i+1)
			}
		}
	}
	t.Fatalf("no func %s in the fixture", fn)
	return ""
}
