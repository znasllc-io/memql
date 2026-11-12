package storestats

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// writersThroughWriteTx is every function of this package that writes. Each
// takes a bun.Tx and writes through it, so each runs inside writeTx, after its
// lock and its generation check. A task that adds a writer adds its name
// here, which is the decision that it goes through writeTx.
var writersThroughWriteTx = []string{
	"ensureStore",
	"keepFloor",
}

// writeCalls are the calls that write: the bun builders, and NewRaw and
// ExecContext among them because a raw statement can be anything.
var writeCalls = map[string]bool{"NewInsert": true, "NewUpdate": true, "NewDelete": true, "NewRaw": true, "ExecContext": true}

// lockOnly is the one function that runs a raw statement and writes nothing:
// the lock itself, which takes a bun.Tx too.
const lockOnly = "lockWrites"

// THE ONE WAY THIS PACKAGE WRITES, read off its source: a transaction is
// opened in writeTx and readTx and nowhere else; writeTx's transaction takes
// the lock before anything else; and every write is made through a bun.Tx
// a function was handed, never through a *bun.DB, by a function on the list
// above. Not a database test.
func TestEveryWritingTransactionBeginsWithTheLock(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{}
	var parsed int
	var writers []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcs[fn.Name.Name] = fn
			if fn.Body == nil {
				continue
			}
			// Each call's innermost function: the declaration, or a
			// function literal inside it.
			var stack []ast.Node
			wrote := false
			ast.Inspect(fn, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if (name == "RunInTx" || name == "BeginTx") && fn.Name.Name != "writeTx" && fn.Name.Name != "readTx" {
					t.Errorf("%s: %s opens a transaction with %s; only writeTx and readTx do", fset.Position(call.Pos()), fn.Name.Name, name)
				}
				if !writeCalls[name] {
					return true
				}
				inner := innermostFunc(stack)
				if inner != fn {
					t.Errorf("%s: %s writes with %s inside a function literal; a write is made by a named function on the list", fset.Position(call.Pos()), fn.Name.Name, name)
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok || !slices.Contains(txParams(fn), recv.Name) {
					t.Errorf("%s: %s writes with %s through something that is not a bun.Tx it was handed; no write goes through a *bun.DB", fset.Position(call.Pos()), fn.Name.Name, name)
				}
				wrote = true
				return true
			})
			if wrote && fn.Name.Name != lockOnly {
				writers = append(writers, fn.Name.Name)
			}
		}
	}
	if parsed < 4 {
		t.Fatalf("parsed only %d files of the package; this gate is not looking at it", parsed)
	}

	for _, name := range []string{"writeTx", "readTx"} {
		fn := funcs[name]
		if fn == nil {
			t.Fatalf("no %s in the package", name)
		}
		if names := calledNames(fn); !names["RunInTx"] && !names["BeginTx"] {
			t.Errorf("%s opens no transaction", name)
		}
	}

	// writeTx's transaction: the first statement of the function it hands
	// RunInTx is the lock.
	if fn := funcs["writeTx"]; fn != nil {
		body := txBody(fn)
		if body == nil || len(body.List) == 0 {
			t.Error("writeTx hands RunInTx no function literal to read its first statement from")
		} else if first := firstCall(body.List[0]); first != "lockWrites" {
			t.Errorf("the first statement writeTx runs in its transaction calls %q, want lockWrites", first)
		}
	}

	sort.Strings(writers)
	want := append([]string{}, writersThroughWriteTx...)
	sort.Strings(want)
	if !slices.Equal(writers, want) {
		t.Errorf("the functions that write are %v, want %v: a writer goes through writeTx and is named in writersThroughWriteTx", writers, want)
	}
}

func innermostFunc(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return stack[i]
		}
	}
	return nil
}

// txParams names the parameters of fn whose type is bun.Tx.
func txParams(fn *ast.FuncDecl) []string {
	var out []string
	for _, field := range fn.Type.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Tx" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "bun" {
			continue
		}
		for _, n := range field.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func calledNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				out[f.Sel.Name] = true
			case *ast.Ident:
				out[f.Name] = true
			}
		}
		return true
	})
	return out
}

// txBody is the body of the function literal writeTx hands RunInTx.
func txBody(fn *ast.FuncDecl) *ast.BlockStmt {
	var body *ast.BlockStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || body != nil {
			return body == nil
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunInTx" {
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.FuncLit); ok {
					body = lit.Body
				}
			}
		}
		return true
	})
	return body
}

// firstCall is the name of the first function a statement calls.
func firstCall(stmt ast.Stmt) string {
	name := ""
	ast.Inspect(stmt, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			return false
		}
		return true
	})
	return name
}
